package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/zanescope/vohive/internal/global"
)

var ErrJobNotFound = errors.New("update job not found")

// Coordinator is the injectable API boundary used by HTTP handlers. It never
// performs an in-process binary replacement: Start persists an exact target and
// dispatches the independent systemd/procd update worker.
type Coordinator interface {
	Capabilities(context.Context) (Capabilities, error)
	Check(context.Context, CheckRequest) (Candidate, error)
	Start(context.Context, UpdateRequest) (TransactionState, error)
	State(context.Context, string) (TransactionState, error)
}

type LocalCoordinator struct {
	DeploymentFile string
	Resolver       ReleaseResolver
	Launcher       JobLauncher
	Now            func() time.Time
	validateScope  func(RuntimePaths) error
	containerized  func() bool
	buildVersion   func() string
}

func NewLocalCoordinator(deploymentFile string, resolver ReleaseResolver, launcher JobLauncher) *LocalCoordinator {
	if deploymentFile == "" {
		deploymentFile = DefaultDeploymentPath
	}
	if launcher == nil {
		launcher = ServiceJobLauncher{}
	}
	return &LocalCoordinator{DeploymentFile: deploymentFile, Resolver: resolver, Launcher: launcher, Now: time.Now}
}

func (c *LocalCoordinator) Capabilities(_ context.Context) (Capabilities, error) {
	deployment, paths, err := c.deployment()
	if err != nil {
		return Capabilities{}, err
	}
	if err := c.validateProductionScope(paths); err != nil {
		return Capabilities{}, err
	}
	capabilities := DetectCapabilities(deployment)
	if c.Resolver == nil {
		capabilities.CanCheck = false
		capabilities.CanUpdate = false
		capabilities.Reason = ErrSignatureUnavailable.Error()
	}
	return capabilities, nil
}

func (c *LocalCoordinator) Check(ctx context.Context, request CheckRequest) (Candidate, error) {
	deployment, paths, err := c.deployment()
	if err != nil {
		return Candidate{}, err
	}
	if err := c.validateProductionScope(paths); err != nil {
		return Candidate{}, err
	}
	if c.Resolver == nil {
		return Candidate{}, ErrSignatureUnavailable
	}
	currentVersion := c.effectiveCurrentVersion(deployment)
	if !validVersion(currentVersion) {
		return Candidate{
			HasUpdate: false, CurrentVer: deployment.CurrentVersion,
		}, nil
	}
	if request.Channel == "" {
		request.Channel = deployment.Channel
	}
	request.CurrentVersion = currentVersion
	request.GOOS = runtime.GOOS
	request.GOARCH = runtime.GOARCH
	return c.Resolver.Check(ctx, request)
}

func (c *LocalCoordinator) Start(ctx context.Context, request UpdateRequest) (TransactionState, error) {
	deployment, paths, err := c.deployment()
	if err != nil {
		return TransactionState{}, err
	}
	if err := c.validateProductionScope(paths); err != nil {
		return TransactionState{}, err
	}
	capabilities := DetectCapabilities(deployment)
	if !capabilities.CanUpdate {
		return TransactionState{}, fmt.Errorf("%w: %s", ErrUpdateUnsupported, capabilities.Reason)
	}
	if c.Resolver == nil || c.Launcher == nil {
		return TransactionState{}, errors.New("update coordinator is not fully configured")
	}
	if !validVersion(deployment.CurrentVersion) {
		return TransactionState{}, fmt.Errorf("%w: %q", ErrNonReleaseBuild, deployment.CurrentVersion)
	}
	if request.Channel == "" {
		request.Channel = deployment.Channel
	}
	if _, err := ParseChannel(string(request.Channel)); err != nil {
		return TransactionState{}, err
	}
	// The UI must submit the exact tag returned by Check. This prevents stable
	// or beta from moving between user confirmation and the service restart.
	if request.Version == "" || !validVersion(request.Version) {
		return TransactionState{}, fmt.Errorf("%w: an exact signed target version is required", ErrInvalidUpdateRequest)
	}
	candidate, err := c.Resolver.Check(ctx, CheckRequest{
		Channel:        request.Channel,
		Version:        request.Version,
		CurrentVersion: deployment.CurrentVersion,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
	})
	if err != nil {
		return TransactionState{}, err
	}
	if !candidate.HasUpdate || normalizeVersion(candidate.LatestVer) != normalizeVersion(request.Version) {
		return TransactionState{}, fmt.Errorf("%w: %s", ErrTargetNotApplicable, request.Version)
	}
	lock, err := AcquireUpdateLock(paths.LockFile())
	if err != nil {
		return TransactionState{}, err
	}
	if err := guardManualRecovery(paths); err != nil {
		_ = lock.Release()
		return TransactionState{}, err
	}
	if previous, stateErr := LoadState(paths.StateFile()); stateErr == nil && !previous.Phase.Terminal() {
		_ = lock.Release()
		return TransactionState{}, ErrUpdateLocked
	} else if stateErr != nil && !os.IsNotExist(stateErr) {
		_ = lock.Release()
		return TransactionState{}, stateErr
	}
	now := c.now().UTC()
	if request.JobID == "" {
		request.JobID = fmt.Sprintf("%s-%d", now.Format("20060102T150405.000000000Z"), os.Getpid())
	}
	state := TransactionState{
		Schema: 1, ID: request.JobID, Operation: "update", Phase: PhaseChecking,
		CurrentVersion: normalizeVersion(deployment.CurrentVersion),
		TargetVersion:  normalizeVersion(request.Version),
		StartedAt:      now, UpdatedAt: now,
	}
	if err := SaveRequest(paths.RequestFile(), request); err != nil {
		_ = lock.Release()
		return TransactionState{}, err
	}
	if err := atomicWriteJSON(paths.StateFile(), state, 0o600); err != nil {
		_ = lock.Release()
		return TransactionState{}, err
	}
	if err := lock.Release(); err != nil {
		return TransactionState{}, err
	}
	if err := c.Launcher.Launch(ctx, deployment.InstallType); err != nil {
		state.Phase = PhaseFailed
		state.Error = err.Error()
		state.UpdatedAt = c.now().UTC()
		_ = atomicWriteJSON(paths.StateFile(), state, 0o600)
		return state, err
	}
	return state, nil
}

func (c *LocalCoordinator) State(_ context.Context, jobID string) (TransactionState, error) {
	_, paths, err := c.deployment()
	if err != nil {
		return TransactionState{}, err
	}
	if err := c.validateProductionScope(paths); err != nil {
		return TransactionState{}, err
	}
	state, err := LoadState(paths.StateFile())
	if os.IsNotExist(err) {
		return TransactionState{}, ErrJobNotFound
	}
	if err != nil {
		return TransactionState{}, err
	}
	if jobID != "" && state.ID != jobID {
		return TransactionState{}, ErrJobNotFound
	}
	return state, nil
}

func (c *LocalCoordinator) deployment() (Deployment, RuntimePaths, error) {
	deployment, err := DiscoverDeployment(c.DeploymentFile)
	if err != nil {
		return Deployment{}, RuntimePaths{}, err
	}
	paths := PathsFor(c.DeploymentFile, deployment)
	if err := paths.Validate(); err != nil {
		return Deployment{}, RuntimePaths{}, err
	}
	return deployment, paths, nil
}

func (c *LocalCoordinator) validateProductionScope(paths RuntimePaths) error {
	if c.validateScope != nil {
		return c.validateScope(paths)
	}
	return ValidateProductionScope(paths)
}

func (c *LocalCoordinator) effectiveCurrentVersion(deployment Deployment) string {
	if validVersion(deployment.CurrentVersion) {
		return deployment.CurrentVersion
	}
	containerized := c.containerized
	if containerized == nil {
		containerized = func() bool {
			_, err := os.Stat("/.dockerenv")
			return err == nil
		}
	}
	if !containerized() {
		return deployment.CurrentVersion
	}
	buildVersion := c.buildVersion
	if buildVersion == nil {
		buildVersion = func() string { return global.Version }
	}
	return buildVersion()
}

func (c *LocalCoordinator) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
