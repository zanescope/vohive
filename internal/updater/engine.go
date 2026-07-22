package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// UpdaterVersion is injected into vohivectl release builds.
var UpdaterVersion = "unknown"

type Quiescer interface{ WaitForQuiescence(context.Context) error }

type Engine struct {
	DeploymentFile string
	Resolver       ReleaseResolver
	HTTPClient     *http.Client
	Service        ServiceController
	Ready          ReadyChecker
	Quiescer       Quiescer
	Now            func() time.Time
	ReadyTimeout   time.Duration
	ReadyInterval  time.Duration
	StableFor      time.Duration
	// BootRecovery is set only by the boot-ordered recover service. It allows
	// clearing an orphan lock after confirming the managed service is inactive.
	BootRecovery bool

	installerRecovery func(context.Context, RuntimePaths, string) error
	validateScope     func(RuntimePaths) error
}

func NewEngine(deploymentFile string, resolver ReleaseResolver) (*Engine, error) {
	deployment, err := DiscoverDeployment(deploymentFile)
	if err != nil {
		return nil, err
	}
	if deploymentFile == "" {
		deploymentFile = DefaultDeploymentPath
	}
	return &Engine{
		DeploymentFile: deploymentFile, Resolver: resolver,
		HTTPClient: &http.Client{Timeout: 30 * time.Minute},
		Service:    NewServiceController(deployment.InstallType, nil), Ready: HTTPReadyChecker{},
		Now: time.Now, ReadyTimeout: 90 * time.Second, ReadyInterval: 2 * time.Second, StableFor: 30 * time.Second,
	}, nil
}

func (e *Engine) Update(ctx context.Context, request UpdateRequest) (state TransactionState, resultErr error) {
	deployment, paths, err := e.load()
	if err != nil {
		return TransactionState{}, err
	}
	originalDeployment := deployment
	capabilities := DetectCapabilities(deployment)
	if !capabilities.CanUpdate {
		return TransactionState{}, fmt.Errorf("%w: %s", ErrUpdateUnsupported, capabilities.Reason)
	}
	if !validVersion(deployment.CurrentVersion) {
		return TransactionState{}, fmt.Errorf("%w: %q", ErrNonReleaseBuild, deployment.CurrentVersion)
	}
	if e.Resolver == nil || e.Service == nil || e.Ready == nil {
		return TransactionState{}, errors.New("resolver, service controller, and readiness checker are required")
	}
	if !validJobID(request.JobID) {
		return TransactionState{}, fmt.Errorf("invalid update job id %q", request.JobID)
	}
	channel, err := ParseChannel(string(request.Channel))
	if err != nil {
		return TransactionState{}, err
	}
	if channel == ChannelPinned && request.Version == "" {
		return TransactionState{}, errors.New("pinned channel requires an exact version")
	}

	lock, err := AcquireUpdateLock(paths.LockFile())
	if err != nil {
		return TransactionState{}, err
	}
	defer func() {
		if releaseErr := lock.Release(); resultErr == nil && releaseErr != nil {
			resultErr = releaseErr
		}
	}()
	if err := guardEngineUpdateStart(paths, request.JobID); err != nil {
		return TransactionState{}, err
	}

	now := e.now().UTC()
	jobID := request.JobID
	if jobID == "" {
		jobID = fmt.Sprintf("%s-%d", now.Format("20060102T150405.000000000Z"), os.Getpid())
	}
	state = TransactionState{
		Schema: 1, ID: jobID, Operation: "update", Phase: PhaseChecking,
		CurrentVersion: normalizeVersion(deployment.CurrentVersion), StartedAt: now, UpdatedAt: now,
	}
	if err := e.saveState(paths, &state, PhaseChecking, nil); err != nil {
		return state, err
	}

	candidate, err := e.Resolver.Check(ctx, CheckRequest{
		Channel: channel, Version: request.Version, CurrentVersion: deployment.CurrentVersion,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	})
	if err != nil {
		return e.fail(paths, state, err)
	}
	state.TargetVersion = normalizeVersion(candidate.Manifest.Version)
	if compareVersions(deployment.CurrentVersion, candidate.Manifest.Version) >= 0 {
		return e.fail(paths, state, fmt.Errorf("target %s is not newer than current %s; use rollback for downgrades", candidate.Manifest.Version, deployment.CurrentVersion))
	}
	if err := validateUpgradeCompatibility(deployment.CurrentVersion, candidate.Manifest); err != nil {
		return e.fail(paths, state, err)
	}

	artifactPath := filepath.Join(paths.DownloadsDir(), candidate.Artifact.Name)
	state.ArtifactPath = artifactPath
	if err := e.saveState(paths, &state, PhaseDownloading, nil); err != nil {
		return state, err
	}
	if err := DownloadArtifact(ctx, e.HTTPClient, candidate, artifactPath); err != nil {
		return e.fail(paths, state, err)
	}
	if err := e.saveState(paths, &state, PhaseVerifying, nil); err != nil {
		return state, err
	}
	if err := VerifyFileSHA256(artifactPath, candidate.Artifact.SHA256); err != nil {
		return e.fail(paths, state, err)
	}
	stagingPath, err := stageRelease(paths, candidate.Manifest.Version, artifactPath, candidate.Artifact)
	if err != nil {
		return e.fail(paths, state, err)
	}
	state.StagingPath = stagingPath
	defer func() { _ = removeStagingPath(paths, stagingPath) }()

	if err := e.saveState(paths, &state, PhaseWaitingQuiesce, nil); err != nil {
		return state, err
	}
	if e.Quiescer != nil {
		if err := e.Quiescer.WaitForQuiescence(ctx); err != nil {
			return e.fail(paths, state, err)
		}
	}
	if err := e.saveState(paths, &state, PhaseStopping, nil); err != nil {
		return state, err
	}
	if err := e.Service.Stop(ctx); err != nil {
		return e.fail(paths, state, fmt.Errorf("stop service: %w", err))
	}

	restartUnchanged := func(original error) (TransactionState, error) {
		if startErr := e.Service.Start(context.Background()); startErr != nil {
			return e.manualRecovery(paths, state, original, fmt.Errorf("restart unchanged service: %w", startErr))
		}
		return e.fail(paths, state, original)
	}
	if err := e.saveState(paths, &state, PhaseBackingUp, nil); err != nil {
		return restartUnchanged(err)
	}
	backupPath, err := createBackup(paths, deployment.CurrentVersion, e.now())
	if err != nil {
		return restartUnchanged(fmt.Errorf("create update backup: %w", err))
	}
	state.BackupPath = backupPath

	previousTarget, previousVersion, err := ensureCurrentPointer(paths, deployment)
	if err != nil {
		return restartUnchanged(err)
	}
	state.PreviousTarget, state.PreviousVersion = previousTarget, previousVersion
	state.PreviousLastGoodTarget, err = optionalVersionPointer(paths, paths.LastGoodLink())
	if err != nil {
		return restartUnchanged(err)
	}
	releasePath := filepath.Join(paths.ReleasesDir(), sanitizeVersion(candidate.Manifest.Version))
	reused, err := prepareReleasePromotion(paths, stagingPath, releasePath, state.ID, previousTarget, state.PreviousLastGoodTarget)
	if err != nil {
		return restartUnchanged(err)
	}
	if reused {
		state.StagingPath = ""
	} else {
		state.InstalledReleasePath = releasePath
	}
	// Persist ownership of the target slot before the atomic rename. Recovery
	// can then remove only the directory carrying this transaction's marker.
	if err := e.saveState(paths, &state, PhaseSwitching, nil); err != nil {
		return restartUnchanged(err)
	}
	if !reused {
		if err := installStagedRelease(paths, stagingPath, releasePath); err != nil {
			return restartUnchanged(err)
		}
		state.StagingPath = ""
		if err := e.saveState(paths, &state, PhaseSwitching, nil); err != nil {
			return e.rollbackFailedUpdate(paths, originalDeployment, state, err, false)
		}
	}
	if err := switchVersionPointer(paths, paths.CurrentLink(), releasePath); err != nil {
		return e.rollbackFailedUpdate(paths, originalDeployment, state, err, false)
	}
	if err := switchVersionPointer(paths, paths.LastGoodLink(), previousTarget); err != nil {
		return e.rollbackFailedUpdate(paths, originalDeployment, state, err, false)
	}

	if err := e.saveState(paths, &state, PhaseStarting, nil); err != nil {
		return e.rollbackFailedUpdate(paths, originalDeployment, state, err, false)
	}
	if err := e.Service.Start(ctx); err != nil {
		return e.rollbackFailedUpdate(paths, originalDeployment, state, fmt.Errorf("start new version: %w", err), false)
	}
	if err := e.saveState(paths, &state, PhaseVerifyingService, nil); err != nil {
		return e.rollbackFailedUpdate(paths, originalDeployment, state, err, false)
	}
	if err := waitReady(ctx, e.Ready, deployment.ReadyURL, e.readyTimeout(), e.readyInterval(), e.stableFor()); err != nil {
		return e.rollbackFailedUpdate(paths, originalDeployment, state, err, false)
	}

	deployment.LastGoodVersion = normalizeVersion(previousVersion)
	deployment.CurrentVersion = normalizeVersion(candidate.Manifest.Version)
	deployment.Channel = channel
	if err := SaveDeployment(paths.DeploymentFile, deployment); err != nil {
		return e.rollbackFailedUpdate(paths, originalDeployment, state, fmt.Errorf("save deployment metadata: %w", err), false)
	}
	state.ControlTouched = true
	if err := e.saveState(paths, &state, PhaseVerifyingService, nil); err != nil {
		return e.rollbackFailedUpdate(paths, originalDeployment, state, err, false)
	}
	if err := updateControlBinary(paths, releasePath); err != nil {
		return e.rollbackFailedUpdate(paths, originalDeployment, state, fmt.Errorf("update control binary: %w", err), true)
	}
	if err := e.saveState(paths, &state, PhaseCompleted, nil); err != nil {
		return e.rollbackFailedUpdate(paths, originalDeployment, state, err, true)
	}
	_ = os.Remove(artifactPath)
	return state, nil
}

func (e *Engine) Backup(ctx context.Context) (string, error) {
	deployment, paths, err := e.load()
	if err != nil {
		return "", err
	}
	if deployment.InstallType == InstallPortable {
		return "", ErrPortableUnsupported
	}
	lock, err := AcquireUpdateLock(paths.LockFile())
	if err != nil {
		return "", err
	}
	defer lock.Release()
	if err := guardEngineUpdateStart(paths, ""); err != nil {
		return "", err
	}
	if err := e.Service.Stop(ctx); err != nil {
		return "", err
	}
	backupPath, backupErr := createBackup(paths, deployment.CurrentVersion, e.now())
	startErr := e.Service.Start(context.Background())
	if backupErr != nil {
		return "", backupErr
	}
	if startErr != nil {
		return backupPath, startErr
	}
	return backupPath, nil
}

func (e *Engine) Rollback(ctx context.Context) (TransactionState, error) {
	deployment, paths, err := e.load()
	if err != nil {
		return TransactionState{}, err
	}
	originalDeployment := deployment
	if deployment.LastGoodVersion == "" {
		return TransactionState{}, errors.New("no last-good version is recorded")
	}
	lock, err := AcquireUpdateLock(paths.LockFile())
	if err != nil {
		return TransactionState{}, err
	}
	defer lock.Release()
	if err := guardEngineUpdateStart(paths, ""); err != nil {
		return TransactionState{}, err
	}
	now := e.now().UTC()
	state := TransactionState{Schema: 1, ID: fmt.Sprintf("%s-%d", now.Format("20060102T150405.000000000Z"), os.Getpid()), Operation: "rollback", Phase: PhaseRollingBack,
		CurrentVersion: deployment.CurrentVersion, TargetVersion: deployment.LastGoodVersion, StartedAt: now, UpdatedAt: now}
	if err := e.saveState(paths, &state, PhaseRollingBack, nil); err != nil {
		return state, err
	}
	target, err := optionalVersionPointer(paths, paths.LastGoodLink())
	if err != nil {
		return e.fail(paths, state, err)
	}
	if target == "" {
		return e.fail(paths, state, errors.New("last-good version pointer is missing"))
	}
	targetBackup, err := latestBackupForVersion(paths, deployment.LastGoodVersion)
	if err != nil {
		return e.fail(paths, state, err)
	}
	state.TargetBackupPath = targetBackup
	currentTarget, _, err := currentVersionTarget(paths)
	if err != nil {
		return e.fail(paths, state, err)
	}
	state.PreviousTarget, state.PreviousVersion = currentTarget, normalizeVersion(deployment.CurrentVersion)
	state.PreviousLastGoodTarget, err = optionalVersionPointer(paths, paths.LastGoodLink())
	if err != nil {
		return e.fail(paths, state, err)
	}
	if err := e.Service.Stop(ctx); err != nil {
		return e.fail(paths, state, err)
	}
	currentBackup, err := createBackup(paths, deployment.CurrentVersion, e.now())
	if err != nil {
		if startErr := e.Service.Start(context.Background()); startErr != nil {
			return e.manualRecovery(paths, state, err, startErr)
		}
		return e.fail(paths, state, err)
	}
	state.BackupPath = currentBackup
	if err := e.saveState(paths, &state, PhaseRollingBack, nil); err != nil {
		return e.restoreRollbackOriginal(paths, originalDeployment, state, err)
	}
	if err := restoreBackup(paths, targetBackup); err != nil {
		return e.restoreRollbackOriginal(paths, originalDeployment, state, err)
	}
	if err := switchVersionPointer(paths, paths.CurrentLink(), target); err != nil {
		return e.restoreRollbackOriginal(paths, originalDeployment, state, err)
	}
	if err := switchVersionPointer(paths, paths.LastGoodLink(), currentTarget); err != nil {
		return e.restoreRollbackOriginal(paths, originalDeployment, state, err)
	}
	if err := e.Service.Start(ctx); err != nil {
		return e.restoreRollbackOriginal(paths, originalDeployment, state, err)
	}
	if err := waitReady(ctx, e.Ready, deployment.ReadyURL, e.readyTimeout(), e.readyInterval(), e.stableFor()); err != nil {
		return e.restoreRollbackOriginal(paths, originalDeployment, state, err)
	}
	deployment.CurrentVersion, deployment.LastGoodVersion = deployment.LastGoodVersion, deployment.CurrentVersion
	if err := SaveDeployment(paths.DeploymentFile, deployment); err != nil {
		return e.restoreRollbackOriginal(paths, originalDeployment, state, err)
	}
	if err := e.saveState(paths, &state, PhaseRolledBack, nil); err != nil {
		return e.restoreRollbackOriginal(paths, originalDeployment, state, err)
	}
	return state, nil
}

func (e *Engine) Recover(ctx context.Context, startService bool) (TransactionState, error) {
	deployment, paths, err := e.load()
	if err != nil {
		return TransactionState{}, err
	}
	state, err := LoadState(paths.StateFile())
	if os.IsNotExist(err) {
		if !e.BootRecovery {
			return TransactionState{}, nil
		}
		// The installer can be interrupted after O_EXCL lock acquisition but
		// before its first state write. Boot recovery may clear that orphan only
		// after the same live-worker and service-inactive checks used below.
		_, _, recoveryErr := prepareRecoveryState(ctx, paths, e.Service, true, TransactionState{Phase: PhaseFailed})
		return TransactionState{}, recoveryErr
	}
	if err != nil {
		return TransactionState{}, err
	}
	serviceKnownInactive, terminal, err := prepareRecoveryState(ctx, paths, e.Service, e.BootRecovery, state)
	if err != nil {
		return state, err
	}
	if terminal {
		return state, nil
	}

	if !serviceKnownInactive {
		active, activeErr := e.Service.Active(ctx)
		if activeErr != nil {
			return state, activeErr
		}
		if active {
			return state, errors.New("refusing recovery while VoHive is active")
		}
	}

	// A manual recovery that starts the restored service must hold the same
	// lock recognized by guard-start. Boot recovery never starts VoHive itself;
	// the init system starts it only after the terminal state is durable.
	if startService {
		recoveryLock, lockErr := AcquireUpdateLock(paths.LockFile())
		if lockErr != nil {
			return state, lockErr
		}
		defer recoveryLock.Release()
	}

	if state.BackupPath == "" || (state.PreviousTarget == "" && state.Operation != "install") {
		_ = removeStagingPath(paths, state.StagingPath)
		interruptedErr := errors.New("interrupted before version switch")
		if cleanupErr := cleanupInstalledRelease(paths, state); cleanupErr != nil {
			return e.manualRecovery(paths, state, interruptedErr, cleanupErr)
		}
		if startService {
			state, err = e.startRestoredService(paths, state, deployment.ReadyURL, interruptedErr)
			if err != nil {
				return state, err
			}
		}
		if saveErr := e.saveState(paths, &state, PhaseFailed, interruptedErr); saveErr != nil {
			return state, saveErr
		}
		return state, nil
	}

	if restoreErr := e.restoreOriginalState(paths, state, state.ControlTouched); restoreErr != nil {
		return e.manualRecovery(paths, state, errors.New("recover interrupted update"), restoreErr)
	}
	if cleanupErr := cleanupInstalledRelease(paths, state); cleanupErr != nil {
		return e.manualRecovery(paths, state, errors.New("recover interrupted update"), cleanupErr)
	}
	if startService {
		state, err = e.startRestoredService(paths, state, deployment.ReadyURL, errors.New("recover interrupted update"))
		if err != nil {
			return state, err
		}
	}
	_ = removeStagingPath(paths, state.StagingPath)
	if err := e.saveState(paths, &state, PhaseRolledBack, errors.New("interrupted update was recovered")); err != nil {
		return state, err
	}
	return state, nil
}

func prepareRecoveryState(ctx context.Context, paths RuntimePaths, service ServiceController, boot bool, state TransactionState) (bool, bool, error) {
	serviceKnownInactive := false
	if lockInfo, lockErr := os.Lstat(paths.LockFile()); lockErr == nil {
		if !boot {
			return false, false, ErrUpdateLocked
		}
		if lockInfo.Mode()&os.ModeSymlink == 0 {
			if workerActive, _ := updateLockProcessActive(paths.LockFile()); workerActive {
				return false, false, ErrUpdateLocked
			}
		}
		active, activeErr := service.Active(ctx)
		if activeErr != nil {
			return false, false, activeErr
		}
		if active {
			return false, false, errors.New("refusing boot recovery while VoHive is active")
		}
		if err := os.Remove(paths.LockFile()); err != nil {
			return false, false, err
		}
		serviceKnownInactive = true
	} else if !os.IsNotExist(lockErr) {
		return false, false, lockErr
	}
	if state.Phase == PhaseManualRecovery {
		return serviceKnownInactive, false, ErrManualRecoveryRequired
	}
	return serviceKnownInactive, state.Phase.Terminal(), nil
}

func (e *Engine) restoreOriginalState(paths RuntimePaths, state TransactionState, restoreControl bool) error {
	var restoreErrors []error
	if state.PreviousTarget == "" && state.Operation == "install" {
		if err := restoreOptionalPointer(paths, paths.CurrentLink(), ""); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("remove new current pointer: %w", err))
		}
	} else if state.PreviousTarget == "" {
		restoreErrors = append(restoreErrors, errors.New("previous version target is missing"))
	} else if err := switchVersionPointer(paths, paths.CurrentLink(), state.PreviousTarget); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("restore current pointer: %w", err))
	}
	if err := restoreOptionalPointer(paths, paths.LastGoodLink(), state.PreviousLastGoodTarget); err != nil {
		restoreErrors = append(restoreErrors, fmt.Errorf("restore last-good pointer: %w", err))
	}
	if state.BackupPath == "" {
		restoreErrors = append(restoreErrors, errors.New("transaction backup path is missing"))
	} else {
		if err := restoreBackup(paths, state.BackupPath); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore config and data: %w", err))
		}
		if err := restoreDeploymentFromBackup(paths, state.BackupPath); err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("restore deployment metadata: %w", err))
		}
		if restoreControl {
			if err := restoreControlBinary(paths, state.BackupPath); err != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restore control binary: %w", err))
			}
		}
		if state.Operation == "install" {
			restoreInstaller := e.installerRecovery
			if restoreInstaller == nil {
				restoreInstaller = restoreInstallerManagedFiles
			}
			if err := restoreInstaller(context.Background(), paths, state.BackupPath); err != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restore installer-managed files: %w", err))
			}
		}
	}
	return errors.Join(restoreErrors...)
}

func (e *Engine) startRestoredService(paths RuntimePaths, state TransactionState, readyURL string, original error) (TransactionState, error) {
	if err := e.Service.Start(context.Background()); err != nil {
		return e.manualRecovery(paths, state, original, fmt.Errorf("start restored service: %w", err))
	}
	if err := waitReady(context.Background(), e.Ready, readyURL, e.readyTimeout(), e.readyInterval(), e.stableFor()); err != nil {
		stopErr := e.Service.Stop(context.Background())
		if stopErr != nil {
			err = errors.Join(err, fmt.Errorf("stop unhealthy restored service: %w", stopErr))
		}
		return e.manualRecovery(paths, state, original, fmt.Errorf("restored service did not become healthy: %w", err))
	}
	return state, nil
}

func (e *Engine) restoreRollbackOriginal(paths RuntimePaths, deployment Deployment, state TransactionState, original error) (TransactionState, error) {
	if stopErr := e.Service.Stop(context.Background()); stopErr != nil {
		return e.manualRecovery(paths, state, original, fmt.Errorf("stop rollback target: %w", stopErr))
	}
	if restoreErr := e.restoreOriginalState(paths, state, false); restoreErr != nil {
		return e.manualRecovery(paths, state, original, restoreErr)
	}
	var err error
	state, err = e.startRestoredService(paths, state, deployment.ReadyURL, original)
	if err != nil {
		return state, err
	}
	if saveErr := e.saveState(paths, &state, PhaseFailed, original); saveErr != nil {
		return state, errors.Join(
			fmt.Errorf("rollback attempt failed and original version was restored: %w", original),
			fmt.Errorf("persist restored state: %w", saveErr),
		)
	}
	return state, fmt.Errorf("rollback attempt failed and original version was restored: %w", original)
}

func (e *Engine) rollbackFailedUpdate(paths RuntimePaths, deployment Deployment, state TransactionState, original error, controlTouched bool) (TransactionState, error) {
	_ = e.saveState(paths, &state, PhaseRollingBack, original)
	if err := e.Service.Stop(context.Background()); err != nil {
		return e.manualRecovery(paths, state, original, fmt.Errorf("stop failed update: %w", err))
	}
	if restoreErr := e.restoreOriginalState(paths, state, controlTouched || state.ControlTouched); restoreErr != nil {
		return e.manualRecovery(paths, state, original, restoreErr)
	}
	if cleanupErr := cleanupInstalledRelease(paths, state); cleanupErr != nil {
		return e.manualRecovery(paths, state, original, cleanupErr)
	}
	var err error
	state, err = e.startRestoredService(paths, state, deployment.ReadyURL, original)
	if err != nil {
		return state, err
	}
	state.RollbackError = ""
	if err := e.saveState(paths, &state, PhaseRolledBack, original); err != nil {
		return state, errors.Join(
			fmt.Errorf("update failed and original version was restored: %w", original),
			fmt.Errorf("persist rollback state: %w", err),
		)
	}
	return state, fmt.Errorf("update failed and was rolled back: %w", original)
}
func (e *Engine) load() (Deployment, RuntimePaths, error) {
	path := e.DeploymentFile
	if path == "" {
		path = DefaultDeploymentPath
	}
	deployment, err := DiscoverDeployment(path)
	if err != nil {
		return Deployment{}, RuntimePaths{}, err
	}
	if err := deployment.Validate(); err != nil {
		return Deployment{}, RuntimePaths{}, err
	}
	paths := PathsFor(path, deployment)
	if err := paths.Validate(); err != nil {
		return Deployment{}, RuntimePaths{}, err
	}
	validateScope := e.validateScope
	if validateScope == nil {
		validateScope = ValidateProductionScope
	}
	if err := validateScope(paths); err != nil {
		return Deployment{}, RuntimePaths{}, err
	}
	return deployment, paths, nil
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
func (e *Engine) readyTimeout() time.Duration {
	if e.ReadyTimeout > 0 {
		return e.ReadyTimeout
	}
	return 90 * time.Second
}
func (e *Engine) readyInterval() time.Duration {
	if e.ReadyInterval > 0 {
		return e.ReadyInterval
	}
	return 2 * time.Second
}
func (e *Engine) stableFor() time.Duration {
	if e.StableFor >= 0 {
		return e.StableFor
	}
	return 30 * time.Second
}
func (e *Engine) saveState(paths RuntimePaths, state *TransactionState, phase Phase, phaseErr error) error {
	state.Phase = phase
	state.UpdatedAt = e.now().UTC()
	if phaseErr != nil {
		state.Error = phaseErr.Error()
	}
	return atomicWriteJSON(paths.StateFile(), state, 0o600)
}
func (e *Engine) fail(paths RuntimePaths, state TransactionState, err error) (TransactionState, error) {
	_ = e.saveState(paths, &state, PhaseFailed, err)
	return state, err
}
func (e *Engine) manualRecovery(paths RuntimePaths, state TransactionState, original, rollback error) (TransactionState, error) {
	state.RollbackError = rollback.Error()
	_ = e.saveState(paths, &state, PhaseManualRecovery, original)
	return state, fmt.Errorf("%w; manual recovery required: %v", original, rollback)
}

func validateUpgradeCompatibility(current string, manifest ReleaseManifest) error {
	if manifest.MinUpdaterVersion != "" {
		if !validVersion(UpdaterVersion) {
			return fmt.Errorf("updater build %q has no valid release version", UpdaterVersion)
		}
		if compareVersions(UpdaterVersion, manifest.MinUpdaterVersion) < 0 {
			return fmt.Errorf("release requires updater %s or newer", manifest.MinUpdaterVersion)
		}
	}
	if manifest.MinDirectUpgrade != "" && compareVersions(current, manifest.MinDirectUpgrade) < 0 {
		if len(manifest.UpgradeVia) > 0 {
			return fmt.Errorf("current %s must first upgrade via %s", current, strings.Join(manifest.UpgradeVia, ", "))
		}
		return fmt.Errorf("current %s is below minimum direct upgrade version %s", current, manifest.MinDirectUpgrade)
	}
	return nil
}

func stageRelease(paths RuntimePaths, version, artifactPath string, artifact Artifact) (string, error) {
	if err := os.MkdirAll(paths.ReleasesDir(), 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(paths.ReleasesDir(), ".staging-"+sanitizeVersion(version)+"-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = removeStagingPath(paths, staging)
		}
	}()
	format := strings.ToLower(artifact.Format)
	if format == "" && strings.HasSuffix(artifact.Name, ".tar.gz") {
		format = "tar.gz"
	}
	switch format {
	case "tar.gz", "tgz":
		err = extractReleaseArchive(artifactPath, staging)
	case "binary":
		err = copyFile(artifactPath, filepath.Join(staging, "vohive"), 0o755)
	default:
		err = fmt.Errorf("unsupported artifact format %q", artifact.Format)
	}
	if err != nil {
		return "", err
	}
	if err := validateStagedRelease(staging); err != nil {
		return "", err
	}
	ok = true
	return staging, nil
}

func extractReleaseArchive(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	allowed := map[string]bool{"vohive": true, "vohivectl": true, "LICENSE": true}
	seen := map[string]bool{}
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Clean(header.Name))
		name = strings.TrimPrefix(name, "./")
		if !allowed[name] || seen[name] {
			return fmt.Errorf("release archive contains unexpected or duplicate entry %q", header.Name)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("release archive entry %q is not regular", header.Name)
		}
		if header.Size < 0 || header.Size > 1<<30 || total+header.Size > 2<<30 {
			return errors.New("release archive exceeds safety limit")
		}
		total += header.Size
		mode := os.FileMode(0o644)
		if name == "vohive" || name == "vohivectl" {
			mode = 0o755
		}
		out, err := os.OpenFile(filepath.Join(destination, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(out, reader, header.Size)
		syncErr := out.Sync()
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		seen[name] = true
	}
	if !seen["vohive"] || !seen["vohivectl"] {
		return errors.New("release archive must contain vohive and vohivectl")
	}
	return nil
}

func validateStagedRelease(path string) error {
	for _, name := range []string{"vohive", "vohivectl"} {
		info, err := os.Stat(filepath.Join(path, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%s is not an executable regular file", name)
		}
	}
	return nil
}

const releaseTransactionMarker = ".vohive-transaction"

// prepareReleasePromotion either marks a new target slot as owned by this
// transaction or safely reuses the exact last-good slot after comparing its
// binaries with the freshly verified staging directory.
func prepareReleasePromotion(paths RuntimePaths, staging, release, transactionID, currentTarget, lastGoodTarget string) (bool, error) {
	if err := validateReleaseDirParent(paths, staging); err != nil {
		return false, err
	}
	if !strings.HasPrefix(filepath.Base(staging), ".staging-") {
		return false, fmt.Errorf("refusing unmanaged staging directory %q", staging)
	}
	if err := validateStagedRelease(staging); err != nil {
		return false, err
	}
	if err := validateReleaseDirParent(paths, release); err != nil {
		return false, err
	}
	info, err := os.Lstat(release)
	if os.IsNotExist(err) {
		return false, writeReleaseTransactionMarker(staging, transactionID)
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("release path is not a managed directory: %s", release)
	}
	if currentTarget != "" && filepath.Clean(release) == filepath.Clean(currentTarget) {
		return false, fmt.Errorf("target release is already current: %s", release)
	}
	if lastGoodTarget == "" || filepath.Clean(release) != filepath.Clean(lastGoodTarget) {
		return false, fmt.Errorf("release directory already exists and is not last-good: %s", release)
	}
	if err := validateStagedRelease(release); err != nil {
		return false, fmt.Errorf("validate reusable last-good release: %w", err)
	}
	if err := compareReleaseBinaries(staging, release); err != nil {
		return false, err
	}
	if err := removeStagingPath(paths, staging); err != nil {
		return false, err
	}
	return true, nil
}

func writeReleaseTransactionMarker(staging, transactionID string) error {
	if err := validateJobID(transactionID); err != nil || transactionID == "" {
		if err == nil {
			err = errors.New("transaction id is required")
		}
		return err
	}
	marker := filepath.Join(staging, releaseTransactionMarker)
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, transactionID+"\n")
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func compareReleaseBinaries(staging, existing string) error {
	for _, name := range []string{"vohive", "vohivectl"} {
		stagedHash, err := releaseFileSHA256(filepath.Join(staging, name))
		if err != nil {
			return err
		}
		existingHash, err := releaseFileSHA256(filepath.Join(existing, name))
		if err != nil {
			return err
		}
		if stagedHash != existingHash {
			return fmt.Errorf("existing last-good %s does not match the verified target", name)
		}
	}
	return nil
}

func releaseFileSHA256(path string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return result, err
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func installStagedRelease(paths RuntimePaths, staging, release string) error {
	if err := validateReleaseDirParent(paths, release); err != nil {
		return err
	}
	if _, err := os.Stat(release); err == nil {
		return fmt.Errorf("release directory already exists: %s", release)
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(staging, release)
}
func cleanupInstalledRelease(paths RuntimePaths, state TransactionState) error {
	release := state.InstalledReleasePath
	if release == "" {
		return nil
	}
	if err := validateReleaseDirParent(paths, release); err != nil {
		return err
	}
	info, err := os.Lstat(release)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to clean non-directory release path %q", release)
	}
	for _, link := range []string{paths.CurrentLink(), paths.LastGoodLink()} {
		target, err := optionalVersionPointer(paths, link)
		if err != nil {
			return err
		}
		if target != "" && filepath.Clean(target) == filepath.Clean(release) {
			return fmt.Errorf("refusing to clean release still referenced by %s", link)
		}
	}
	marker := filepath.Join(release, releaseTransactionMarker)
	markerInfo, err := os.Lstat(marker)
	if err != nil {
		return fmt.Errorf("read release transaction marker: %w", err)
	}
	if !markerInfo.Mode().IsRegular() || markerInfo.Size() > 256 {
		return errors.New("release transaction marker is not a small regular file")
	}
	markerData, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	if string(markerData) != state.ID+"\n" {
		return errors.New("release transaction marker does not match update state")
	}
	if err := os.RemoveAll(release); err != nil {
		return err
	}
	if dir, err := os.Open(paths.ReleasesDir()); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func removeStagingPath(paths RuntimePaths, path string) error {
	if path == "" {
		return nil
	}
	root, err := filepath.Abs(paths.ReleasesDir())
	if err != nil {
		return err
	}
	candidate, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || strings.Contains(rel, string(filepath.Separator)) || !strings.HasPrefix(rel, ".staging-") {
		return fmt.Errorf("refusing unsafe staging path %q", path)
	}
	return os.RemoveAll(path)
}

func ensureCurrentPointer(paths RuntimePaths, deployment Deployment) (string, string, error) {
	if target, _, err := currentVersionTarget(paths); err == nil {
		return target, normalizeVersion(deployment.CurrentVersion), nil
	}
	if deployment.Layout != LayoutV1 {
		return "", "", errors.New("current version pointer is missing")
	}
	legacy := filepath.Join(paths.InstallRoot, "bin", "vohive")
	if _, err := os.Stat(legacy); os.IsNotExist(err) {
		legacy = filepath.Join(paths.InstallRoot, "vohive")
	}
	info, err := os.Stat(legacy)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", errors.New("legacy VoHive binary was not found")
	}
	version := normalizeVersion(deployment.CurrentVersion)
	release := filepath.Join(paths.ReleasesDir(), sanitizeVersion(version))
	if err := os.MkdirAll(release, 0o755); err != nil {
		return "", "", err
	}
	if err := copyFile(legacy, filepath.Join(release, "vohive"), 0o755); err != nil {
		return "", "", err
	}
	if err := switchVersionPointer(paths, paths.CurrentLink(), release); err != nil {
		return "", "", err
	}
	return release, version, nil
}

func currentVersionTarget(paths RuntimePaths) (string, string, error) {
	target, err := os.Readlink(paths.CurrentLink())
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(paths.InstallRoot, target)
	}
	target = filepath.Clean(target)
	if err := validateReleaseDir(paths, target); err != nil {
		return "", "", err
	}
	return target, filepath.Base(target), nil
}
func optionalVersionPointer(paths RuntimePaths, link string) (string, error) {
	target, err := os.Readlink(link)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(paths.InstallRoot, target)
	}
	target = filepath.Clean(target)
	if err := validateReleaseDir(paths, target); err != nil {
		return "", err
	}
	return target, nil
}
func restoreOptionalPointer(paths RuntimePaths, link, target string) error {
	if target != "" {
		return switchVersionPointer(paths, link, target)
	}
	if filepath.Clean(link) != filepath.Clean(paths.CurrentLink()) && filepath.Clean(link) != filepath.Clean(paths.LastGoodLink()) {
		return errors.New("refusing unmanaged pointer removal")
	}
	info, err := os.Lstat(link)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return errors.New("refusing to remove non-symlink version pointer")
	}
	return os.Remove(link)
}
func switchVersionPointer(paths RuntimePaths, link, target string) error {
	if err := validateReleaseDir(paths, target); err != nil {
		return err
	}
	if filepath.Clean(link) != filepath.Clean(paths.CurrentLink()) && filepath.Clean(link) != filepath.Clean(paths.LastGoodLink()) {
		return errors.New("refusing unmanaged version pointer")
	}
	rel, err := filepath.Rel(paths.InstallRoot, target)
	if err != nil {
		return err
	}
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(rel, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
func validateReleaseDir(paths RuntimePaths, path string) error {
	if err := validateReleaseDirParent(paths, path); err != nil {
		return err
	}
	info, err := os.Stat(filepath.Join(path, "vohive"))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("release vohive binary is not regular")
	}
	return nil
}
func validateReleaseDirParent(paths RuntimePaths, path string) error {
	root, err := filepath.Abs(paths.ReleasesDir())
	if err != nil {
		return err
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.Contains(rel, string(filepath.Separator)) {
		return fmt.Errorf("release path %q is outside managed root", path)
	}
	return nil
}

func updateControlBinary(paths RuntimePaths, release string) error {
	source := filepath.Join(release, "vohivectl")
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("release control binary is not an executable regular file")
	}
	if err := os.MkdirAll(paths.ControlDir(), 0o755); err != nil {
		return err
	}
	return copyFile(source, filepath.Join(paths.ControlDir(), "vohivectl"), 0o755)
}

func restoreControlBinary(paths RuntimePaths, backupPath string) error {
	return restoreControlFromBackup(paths, backupPath)
}
func restoreDeploymentFromBackup(paths RuntimePaths, backup string) error {
	if err := validateBackupPath(paths, backup); err != nil {
		return err
	}
	metadataData, err := os.ReadFile(filepath.Join(backup, "metadata.json"))
	if err != nil {
		return err
	}
	var metadata BackupMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return err
	}
	if !metadata.DeploymentPresent {
		if info, err := os.Lstat(paths.DeploymentFile); err == nil {
			if info.IsDir() {
				return errors.New("refusing to remove a deployment metadata path that is a directory")
			}
			return os.Remove(paths.DeploymentFile)
		} else if !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := os.ReadFile(filepath.Join(backup, "deployment.json"))
	if err != nil {
		return err
	}
	var deployment Deployment
	if err := json.Unmarshal(data, &deployment); err != nil {
		return err
	}
	return SaveDeployment(paths.DeploymentFile, deployment)
}
func latestBackupForVersion(paths RuntimePaths, version string) (string, error) {
	entries, err := os.ReadDir(paths.BackupsDir())
	if err != nil {
		return "", err
	}
	var best string
	var bestTime time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(paths.BackupsDir(), entry.Name())
		data, err := os.ReadFile(filepath.Join(path, "metadata.json"))
		if err != nil {
			continue
		}
		var metadata BackupMetadata
		if json.Unmarshal(data, &metadata) != nil || normalizeVersion(metadata.SourceVersion) != normalizeVersion(version) {
			continue
		}
		if best == "" || metadata.CreatedAt.After(bestTime) {
			best, bestTime = path, metadata.CreatedAt
		}
	}
	if best == "" {
		return "", fmt.Errorf("no compatible backup exists for %s", version)
	}
	return best, nil
}
