package hostconfig

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
)

const (
	defaultHelperUnit        = "vohive-host-config.service"
	helperTimeout            = 45 * time.Second
	cleanupIncompleteWarning = "配置已生效且 udev 规则已重新加载，但收尾清理未完整结束；请先使用当前安装器执行 --repair，再进行下一次宿主机配置变更。"
)

type Launcher interface {
	Launch(context.Context) error
}

type LocalCoordinatorOptions struct {
	Manager         *mmisolation.Manager
	RulePath        string
	StateDir        string
	CapabilityProbe func() Capability
	Launcher        Launcher
	BootIDProvider  func() (string, error)
}

type LocalCoordinator struct {
	manager                 *mmisolation.Manager
	managerErr              error
	rulePath                string
	stateDir                string
	capabilityProbe         func() Capability
	launcher                Launcher
	bootIDProvider          func() (string, error)
	operationMu             sync.Mutex
	manualAttentionMu       sync.RWMutex
	manualAttentionFallback bool
}

func NewLocalCoordinator() *LocalCoordinator {
	return NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{})
}

func NewLocalCoordinatorWithOptions(options LocalCoordinatorOptions) *LocalCoordinator {
	rulePath := strings.TrimSpace(options.RulePath)
	if rulePath == "" {
		rulePath = mmisolation.DefaultRulePath
	}
	stateDir := strings.TrimSpace(options.StateDir)
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	manager := options.Manager
	var managerErr error
	if manager == nil {
		manager, managerErr = mmisolation.New(mmisolation.Options{RulePath: rulePath})
	}
	probe := options.CapabilityProbe
	if probe == nil {
		probe = ProbeCapability
	}
	launcher := options.Launcher
	if launcher == nil {
		launcher = systemdLauncher{}
	}
	bootIDProvider := options.BootIDProvider
	if bootIDProvider == nil {
		bootIDProvider = readHostBootID
	}
	return &LocalCoordinator{
		manager:         manager,
		managerErr:      managerErr,
		rulePath:        filepath.Clean(rulePath),
		stateDir:        filepath.Clean(stateDir),
		capabilityProbe: probe,
		launcher:        launcher,
		bootIDProvider:  bootIDProvider,
	}

}

func (c *LocalCoordinator) Status(ctx context.Context, targets []Target) (Status, error) {
	return c.status(ctx, targets, nil)
}

func (c *LocalCoordinator) status(_ context.Context, targets []Target, activeRequest *WorkerRequest) (Status, error) {
	if c == nil {
		return Status{}, fmt.Errorf("initialize ModemManager isolation manager: nil coordinator")
	}
	if c.manager == nil {
		return Status{}, fmt.Errorf("initialize ModemManager isolation manager: %w", c.managerErr)
	}
	capability := c.capabilityProbe()
	if !capability.Supported {
		devices := make([]DeviceStatus, 0, len(targets))
		for _, target := range targets {
			devices = append(devices, DeviceStatus{
				DeviceID:      target.ID,
				Name:          target.Name,
				ControlDevice: target.ControlDevice,
				SelectorKind:  "none",
				Reason:        capability.Reason,
			})
		}
		status := Status{
			Status:       StateUnsupported,
			Reason:       capability.Reason,
			RulePath:     c.rulePath,
			TotalDevices: len(targets),
			Devices:      devices,
		}
		return c.applyManualAttention(status, activeRequest), nil
	}

	plan, err := buildPlanSnapshot(c.manager, planInputsFromTargets(targets))
	if err != nil {
		return Status{}, err
	}
	ruleStatus := plan.RuleStatus
	status := Status{
		RulePath:        c.rulePath,
		Revision:        ruleStatus.Revision,
		PlanRevision:    plan.Revision,
		ManagedByVoHive: ruleStatus.State == mmisolation.StateManaged,
		CanUninstall:    ruleStatus.State == mmisolation.StateManaged,
		TotalDevices:    len(targets),
		Devices:         make([]DeviceStatus, 0, len(targets)),
	}

	installed := make(map[string]mmisolation.Matcher, len(ruleStatus.Entries))
	for _, entry := range ruleStatus.Entries {
		installed[entry.TargetID] = entry.Matcher
	}
	resolvedCount := 0
	firstResolutionError := ""
	targetMetadata := make(map[string]Target, len(targets))
	for _, target := range targets {
		targetMetadata[target.ID+"\x00"+target.USBPath] = target
	}
	for _, resolution := range plan.Resolutions {
		target := targetMetadata[resolution.ID+"\x00"+resolution.USBPath]
		device := DeviceStatus{
			DeviceID:      resolution.ID,
			Name:          target.Name,
			ControlDevice: target.ControlDevice,
			SelectorKind:  "none",
		}
		if !resolution.Resolved {
			device.Reason = resolution.ResolutionError
			if firstResolutionError == "" {
				firstResolutionError = resolution.ResolutionError
			}
			status.Devices = append(status.Devices, device)
			continue
		}
		matcher := resolution.Matcher
		resolvedCount++
		switch matcher.Kind {
		case mmisolation.MatcherSerial:
			device.SelectorKind = "usb_serial"
			device.SelectorValue = matcher.Serial
		case mmisolation.MatcherKernelPath:
			device.SelectorKind = "usb_path"
			device.SelectorValue = matcher.KernelPath
		}
		if current, ok := installed[resolution.ID]; ok && current == matcher {
			device.Covered = true
			status.CoveredDevices++
		}
		status.Devices = append(status.Devices, device)
	}

	switch ruleStatus.State {
	case mmisolation.StateForeign:
		status.Status = StateForeign
		status.Reason = ruleStatus.Reason
	case mmisolation.StateTampered:
		status.Status = StateModified
		status.Reason = ruleStatus.Reason
	case mmisolation.StateAbsent:
		if resolvedCount != len(targets) {
			status.Status = StatePartial
			status.Reason = firstResolutionError
		} else {
			status.Status = StateAbsent
			if len(targets) == 0 {
				status.Reason = "没有已配置的 QMI 设备可生成隔离规则"
			}
		}
	case mmisolation.StateManaged:
		if resolvedCount != len(targets) {
			status.Status = StatePartial
			status.Reason = firstResolutionError
		} else if len(ruleStatus.Entries) == len(targets) && status.CoveredDevices == len(targets) {
			status.Status = StateCurrent
		} else {
			status.Status = StateStale
			if len(targets) == 0 {
				status.Reason = "规则仍包含设备，但当前没有已配置的 QMI 设备"
			}
		}
	default:
		return Status{}, fmt.Errorf("unknown managed rule state %q", ruleStatus.State)
	}

	status.CanInstall = len(targets) > 0 &&
		resolvedCount == len(targets) &&
		(ruleStatus.State == mmisolation.StateAbsent || ruleStatus.State == mmisolation.StateManaged) &&
		status.Status != StateCurrent
	return c.applyManualAttention(status, activeRequest), nil
}

func (c *LocalCoordinator) Apply(_ context.Context, action Action, targets []Target, expectedRevision, expectedPlanRevision string) (Status, error) {
	if c == nil {
		return Status{}, ErrUnsupported
	}
	if !c.operationMu.TryLock() {
		return Status{}, ErrOperationBusy
	}
	defer c.operationMu.Unlock()

	before, err := c.Status(context.Background(), targets)
	if err != nil {
		return Status{}, err
	}
	if before.Status == StateUnsupported {
		return before, fmt.Errorf("%w: %s", ErrUnsupported, before.Reason)
	}
	expectedRevision = strings.TrimSpace(expectedRevision)
	if expectedRevision == "" || expectedRevision != before.Revision {
		return before, fmt.Errorf("%w: expected %q, found %q",
			mmisolation.ErrRevisionConflict, expectedRevision, before.Revision)
	}
	expectedPlanRevision = strings.TrimSpace(expectedPlanRevision)
	if expectedPlanRevision == "" || expectedPlanRevision != before.PlanRevision {
		return before, fmt.Errorf("%w: expected %q, found %q",
			ErrPlanConflict, expectedPlanRevision, before.PlanRevision)
	}
	if before.Status == StateForeign {
		return before, mmisolation.ErrForeignRule
	}
	if before.ManualAttention {
		return before, ErrOperationIndeterminate
	}
	if before.Status == StateModified {
		return before, mmisolation.ErrTamperedRule
	}
	switch action {
	case ActionInstall:
		if !before.CanInstall {
			return before, fmt.Errorf("%w: %s", ErrNotInstallable, before.Reason)
		}
	case ActionUninstall:
		if !before.CanUninstall {
			return before, fmt.Errorf("%w: %s", ErrNotUninstallable, before.Reason)
		}
	default:
		return before, fmt.Errorf("%w: unknown action %q", mmisolation.ErrInvalidRequest, action)
	}

	requestID, err := newWorkerRequestID()
	if err != nil {
		return Status{}, err
	}
	bootID, err := c.currentBootID()
	if err != nil {
		return before, fmt.Errorf("read host boot ID before starting host configuration: %w", err)
	}
	request := WorkerRequest{
		Schema:               workerSchema,
		ID:                   requestID,
		Action:               action,
		BootID:               bootID,
		ExpectedRevision:     expectedRevision,
		ExpectedPlanRevision: expectedPlanRevision,
	}
	request.Targets = make([]mmisolation.Target, 0, len(targets))
	for _, target := range targets {
		request.Targets = append(request.Targets, mmisolation.Target{ID: target.ID, USBPath: target.USBPath})
	}

	requestPath := workerRequestPath(c.stateDir)
	resultPath := workerResultPath(c.stateDir)
	if err := createJSONNoReplace(requestPath, request, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return before, ErrOperationBusy
		}
		return Status{}, fmt.Errorf("persist host configuration request: %w", err)
	}
	if err := removeRegularMetadata(resultPath); err != nil {
		return c.returnIndeterminateFromRequest(
			before, targets, request,
			"prior worker result metadata could not be cleared after operation ownership was acquired",
		)
	}

	operationContext, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()
	launchErr := c.launcher.Launch(operationContext)
	workerResult, resultErr := loadWorkerResult(resultPath)
	if resultErr == nil && (workerResult.ID != requestID || workerResult.Action != action || workerResult.BootID != request.BootID) {
		resultErr = ErrWorkerResultStale
	}
	if resultErr != nil {
		detail := "the privileged helper did not produce a trusted result"
		if launchErr != nil {
			detail = "the privileged helper failed without producing a trusted result"
		}
		return c.returnIndeterminateFromRequest(before, targets, request, detail)
	}
	workerErr := workerResultError(workerResult)
	committed := workerResult.Result.Changed && workerResult.Result.Reloaded
	if workerResult.Result.ReloadIndeterminate {
		return c.returnIndeterminate(before, targets, workerResult,
			"the privileged worker could not confirm the udev reload outcome")
	}
	if workerResult.Result.Changed && !workerResult.Result.Reloaded {
		if workerErr != nil {
			return before, workerErr
		}
		return before, fmt.Errorf("%w: changed result omitted both reload confirmation and indeterminate state", ErrWorkerResult)
	}
	if !workerResult.Result.Changed && workerResult.Result.Reloaded {
		return before, fmt.Errorf("%w: privileged worker reported reload without a rule change", ErrWorkerResult)
	}
	if workerErr != nil {
		if !committed {
			if err := c.releaseWorkerOperation(request, workerResult); err != nil {
				return c.returnIndeterminate(before, targets, workerResult,
					"a trusted no-change worker result could not be safely finalized")
			}
			return before, workerErr
		}
		if workerResult.WarningCode != workerWarningCleanupIncomplete {
			return c.returnIndeterminate(before, targets, workerResult,
				"the privileged worker reported an unclassified error after changing the host configuration")
		}
	}
	if committed {
		if err := c.verifyCommittedPostcondition(action, workerResult.Result); err != nil {
			return c.returnIndeterminate(before, targets, workerResult,
				"the host configuration no longer matches the privileged worker result")
		}
	}

	after, err := c.status(context.Background(), targets, &request)
	if err != nil {
		return Status{}, err
	}
	if after.ManualAttention || after.Status == StateForeign || after.Status == StateModified {
		return c.returnIndeterminate(before, targets, workerResult,
			"the post-operation host configuration requires manual attention")
	}
	after.RequiresReplug = committed
	if committed {
		postconditionMatches := false
		switch action {
		case ActionInstall:
			postconditionMatches = after.ManagedByVoHive && after.Revision == workerResult.Result.Revision
		case ActionUninstall:
			postconditionMatches = !after.ManagedByVoHive && after.Revision == mmisolation.AbsentRevision
		}
		if !postconditionMatches {
			return c.returnIndeterminate(after, targets, workerResult,
				"the final host configuration snapshot no longer matches the privileged worker result")
		}
		switch action {
		case ActionInstall:
			after.Reason = "隔离规则已安装；请在维护窗口逐台重新插拔目标设备"
		case ActionUninstall:
			after.Reason = "隔离规则已卸载；逐台重新插拔后 ModemManager 才能重新接管目标设备"
		}
		after.Status = StatePendingReplug
	}
	if committed && workerResult.WarningCode == workerWarningCleanupIncomplete {
		after.Warning = cleanupIncompleteWarning
	}
	if err := c.releaseWorkerOperation(request, workerResult); err != nil {
		return c.returnIndeterminate(after, targets, workerResult,
			"the trusted host configuration result could not be safely finalized")
	}
	return after, nil
}

func (c *LocalCoordinator) verifyCommittedPostcondition(action Action, result mmisolation.Result) error {
	if c == nil || c.manager == nil {
		return errors.New("ModemManager isolation manager is unavailable")
	}
	if !result.Changed || !result.Reloaded || result.ReloadIndeterminate {
		return errors.New("worker result is not a confirmed committed change")
	}
	switch action {
	case ActionInstall:
		if result.State != mmisolation.StateManaged || result.Revision == "" || result.Revision == mmisolation.AbsentRevision {
			return errors.New("install result does not identify a managed rule revision")
		}
	case ActionUninstall:
		if result.State != mmisolation.StateAbsent || result.Revision != mmisolation.AbsentRevision {
			return errors.New("uninstall result does not identify the absent rule state")
		}
	default:
		return errors.New("worker result action is invalid")
	}
	observed, err := c.manager.Inspect()
	if err != nil {
		return fmt.Errorf("inspect committed host configuration: %w", err)
	}
	if action == ActionInstall &&
		(observed.State != mmisolation.StateManaged || observed.Revision != result.Revision) {
		return errors.New("installed rule state or revision differs from the worker result")
	}
	if action == ActionUninstall &&
		(observed.State != mmisolation.StateAbsent || observed.Revision != mmisolation.AbsentRevision) {
		return errors.New("uninstalled rule is no longer absent")
	}
	return nil
}

func (c *LocalCoordinator) returnIndeterminate(
	fallback Status,
	targets []Target,
	workerResult WorkerResult,
	detail string,
) (Status, error) {
	baseErr := fmt.Errorf("%w: %s", ErrOperationIndeterminate, detail)
	markerErr := c.recordManualAttention(workerResult.Action, workerResult.ID, workerResult)
	after, statusErr := c.Status(context.Background(), targets)
	if statusErr != nil {
		return fallback, errors.Join(baseErr, markerErr,
			fmt.Errorf("inspect post-operation host configuration: %w", statusErr))
	}
	return after, errors.Join(baseErr, markerErr)
}

func (c *LocalCoordinator) returnIndeterminateFromRequest(
	fallback Status,
	targets []Target,
	request WorkerRequest,
	detail string,
) (Status, error) {
	evidence := WorkerResult{
		Schema: workerSchema, ID: request.ID, Action: request.Action,
		BootID: request.BootID, ManualAttention: true,
	}
	return c.returnIndeterminate(fallback, targets, evidence, detail)
}

func (c *LocalCoordinator) consumeWorkerRequest(expected WorkerRequest) error {
	current, exists, err := loadOptionalWorkerRequest(workerRequestPath(c.stateDir))
	if err != nil || !exists {
		return errors.Join(errors.New("operation journal is missing or invalid"), err)
	}
	if !workerRequestsEqual(current, expected) {
		return errors.New("operation journal changed before cleanup")
	}
	return removeRegularMetadata(workerRequestPath(c.stateDir))
}

func (c *LocalCoordinator) discardWorkerResult(expected WorkerResult) error {
	current, exists, err := loadOptionalWorkerResult(workerResultPath(c.stateDir))
	if err != nil || !exists {
		if err != nil {
			return err
		}
		return errors.New("trusted worker result is missing")
	}
	if current.ID != expected.ID || current.Action != expected.Action || current.BootID != expected.BootID {
		return errors.New("worker result changed before cleanup")
	}
	return removeRegularMetadata(workerResultPath(c.stateDir))
}

// releaseWorkerOperation clears the trusted result first and the request
// journal last. Keeping request.json until the final step preserves both the
// cross-process ownership lock and crash-recovery evidence throughout result
// classification and postcondition verification.
func (c *LocalCoordinator) releaseWorkerOperation(request WorkerRequest, result WorkerResult) error {
	if err := c.discardWorkerResult(result); err != nil {
		return fmt.Errorf("discard trusted worker result: %w", err)
	}
	return c.consumeWorkerRequest(request)
}

func workerRequestsEqual(left, right WorkerRequest) bool {
	if left.Schema != right.Schema || left.ID != right.ID || left.Action != right.Action ||
		left.BootID != right.BootID || left.ExpectedRevision != right.ExpectedRevision ||
		left.ExpectedPlanRevision != right.ExpectedPlanRevision || len(left.Targets) != len(right.Targets) {
		return false
	}
	for index := range left.Targets {
		if left.Targets[index] != right.Targets[index] {
			return false
		}
	}
	return true
}

func newWorkerRequestID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func removeRegularMetadata(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("host configuration metadata path is unsafe")
	}
	return os.Remove(path)
}

type systemdLauncher struct{}

func (systemdLauncher) Launch(ctx context.Context) error {
	output, err := exec.CommandContext(
		ctx,
		"systemctl",
		"--no-ask-password",
		"start",
		defaultHelperUnit,
	).CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}
