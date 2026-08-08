package hostconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
)

const sensitiveCleanupError = "remove privileged backup /var/lib/vohive/private-token: permission denied"

type committedCleanupWarningLauncher struct {
	requestPath string
	manager     *mmisolation.Manager
}

func (l committedCleanupWarningLauncher) Launch(ctx context.Context) error {
	if err := RunWorker(ctx, l.requestPath, l.manager); err != nil {
		return err
	}
	request, err := loadWorkerRequest(l.requestPath)
	if err != nil {
		return err
	}
	result, err := loadWorkerResult(workerResultPath(filepath.Dir(l.requestPath)))
	if err != nil {
		return err
	}
	return persistWorkerResult(
		l.requestPath,
		request,
		result.Result,
		fmt.Errorf("%w: %s", mmisolation.ErrCleanupIncomplete, sensitiveCleanupError),
	)
}

type indeterminateWorkerLauncher struct {
	requestPath  string
	mutate       func()
	afterPersist func()
	result       *mmisolation.Result
}

func (l indeterminateWorkerLauncher) Launch(_ context.Context) error {
	request, err := loadWorkerRequest(l.requestPath)
	if err != nil {
		return err
	}
	if l.mutate != nil {
		l.mutate()
	}
	workerResult := mmisolation.Result{
		State:               mmisolation.StateManaged,
		Changed:             true,
		Reloaded:            false,
		ReloadIndeterminate: true,
	}
	if l.result != nil {
		workerResult = *l.result
	}
	persistErr := persistWorkerResult(
		l.requestPath,
		request,
		workerResult,
		errors.New(sensitiveCleanupError),
	)
	if l.afterPersist != nil {
		l.afterPersist()
	}
	return persistErr
}

type runWorkerThenMutateLauncher struct {
	requestPath string
	manager     *mmisolation.Manager
	mutate      func()
}

func (l runWorkerThenMutateLauncher) Launch(ctx context.Context) error {
	if err := RunWorker(ctx, l.requestPath, l.manager); err != nil {
		return err
	}
	if l.mutate != nil {
		l.mutate()
	}
	return nil
}

func filepathDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}

func TestCoordinatorReturnsPendingReplugForCommittedInstallCleanupError(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "COMMITTED-INSTALL")
	manager := layout.manager(t, &recordingRunner{})
	launcher := committedCleanupWarningLauncher{
		requestPath: workerRequestPath(layout.stateDir),
		manager:     manager,
	}
	coordinator := newTestCoordinator(layout, manager, launcher, supportedCapability)
	preview, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}

	status, err := coordinator.Apply(
		context.Background(), ActionInstall, []Target{target},
		preview.Revision, preview.PlanRevision,
	)
	if err != nil {
		t.Fatalf("Apply(install) returned committed cleanup error: %v", err)
	}
	assertCommittedWarningStatus(t, status, ActionInstall)
	if !status.ManagedByVoHive || status.Revision == mmisolation.AbsentRevision {
		t.Fatalf("committed install status = %+v", status)
	}
}

func TestCoordinatorReturnsPendingReplugForCommittedUninstallCleanupError(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "COMMITTED-UNINSTALL")
	manager := layout.manager(t, &recordingRunner{})
	if _, err := manager.Install(context.Background(), mmisolation.Request{
		Targets: []mmisolation.Target{{ID: target.ID, USBPath: target.USBPath}},
	}); err != nil {
		t.Fatal(err)
	}
	launcher := committedCleanupWarningLauncher{
		requestPath: workerRequestPath(layout.stateDir),
		manager:     manager,
	}
	coordinator := newTestCoordinator(layout, manager, launcher, supportedCapability)
	preview, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}

	status, err := coordinator.Apply(
		context.Background(), ActionUninstall, []Target{target},
		preview.Revision, preview.PlanRevision,
	)
	if err != nil {
		t.Fatalf("Apply(uninstall) returned committed cleanup error: %v", err)
	}
	assertCommittedWarningStatus(t, status, ActionUninstall)
	if status.ManagedByVoHive || status.Revision != mmisolation.AbsentRevision {
		t.Fatalf("committed uninstall status = %+v", status)
	}
}

func TestCoordinatorFinalSnapshotMismatchEntersManualAttention(t *testing.T) {
	for _, action := range []Action{ActionInstall, ActionUninstall} {
		t.Run(string(action), func(t *testing.T) {
			layout := newHostConfigTestLayout(t)
			target := layout.addUSBDevice(t, "1-2", "2c7c", "FINAL-SNAPSHOT")
			manager := layout.manager(t, &recordingRunner{})
			if action == ActionUninstall {
				if _, err := manager.Install(context.Background(), mmisolation.Request{
					Targets: []mmisolation.Target{{ID: target.ID, USBPath: target.USBPath}},
				}); err != nil {
					t.Fatal(err)
				}
			}

			probeCalls := 0
			probe := func() Capability {
				probeCalls++
				if probeCalls == 3 {
					if err := os.Remove(workerResultPath(layout.stateDir)); err != nil {
						t.Fatal(err)
					}
					layout.writeManagedRule(t, []mmisolation.Entry{{
						TargetID: target.ID,
						Matcher: mmisolation.Matcher{
							Kind: mmisolation.MatcherKernelPath, VendorID: "2c7c", KernelPath: "1-2",
						},
					}})
				}
				return supportedCapability()
			}
			launcher := runWorkerThenMutateLauncher{
				requestPath: workerRequestPath(layout.stateDir),
				manager:     manager,
			}
			coordinator := NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
				Manager: manager, RulePath: layout.rulePath, StateDir: layout.stateDir,
				CapabilityProbe: probe,
				Launcher:        launcher,
				BootIDProvider:  func() (string, error) { return mustHostBootID(t), nil },
			})
			preview, err := coordinator.Status(context.Background(), []Target{target})
			if err != nil {
				t.Fatal(err)
			}

			status, err := coordinator.Apply(
				context.Background(), action, []Target{target},
				preview.Revision, preview.PlanRevision,
			)
			if !errors.Is(err, ErrOperationIndeterminate) {
				t.Fatalf("Apply(%s) error = %v, want ErrOperationIndeterminate", action, err)
			}
			if status.Status != StateModified || !status.ManualAttention ||
				status.CanInstall || status.CanUninstall || status.RequiresReplug || status.Warning != "" {
				t.Fatalf("final snapshot mismatch was not locked: %+v", status)
			}
		})
	}
}

func assertCommittedWarningStatus(t *testing.T, status Status, action Action) {
	t.Helper()
	if status.Status != StatePendingReplug || !status.RequiresReplug {
		t.Fatalf("committed status lost pending replug: %+v", status)
	}
	if status.Warning != cleanupIncompleteWarning {
		t.Fatalf("warning = %q, want fixed cleanup warning", status.Warning)
	}
	if strings.Contains(status.Warning, "/var/lib") || strings.Contains(status.Warning, "private-token") {
		t.Fatalf("warning leaked privileged cleanup detail: %q", status.Warning)
	}
	if action == ActionInstall && !strings.Contains(status.Reason, "已安装") {
		t.Fatalf("install reason = %q", status.Reason)
	}
	if action == ActionUninstall && !strings.Contains(status.Reason, "已卸载") {
		t.Fatalf("uninstall reason = %q", status.Reason)
	}
}

func TestCoordinatorLocksRollbackReloadIndeterminateWithoutDiskChange(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "ROLLBACK-UNKNOWN")
	manager := layout.manager(t, &recordingRunner{})
	result := mmisolation.Result{
		State: mmisolation.StateAbsent, Revision: mmisolation.AbsentRevision,
		ReloadIndeterminate: true,
	}
	launcher := indeterminateWorkerLauncher{
		requestPath: workerRequestPath(layout.stateDir),
		result:      &result,
	}
	coordinator := NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
		Manager: manager, RulePath: layout.rulePath, StateDir: layout.stateDir,
		CapabilityProbe: supportedCapability,
		Launcher:        launcher,
		BootIDProvider:  func() (string, error) { return "boot-0001", nil },
	})
	preview, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}

	status, err := coordinator.Apply(
		context.Background(), ActionInstall, []Target{target},
		preview.Revision, preview.PlanRevision,
	)
	if !errors.Is(err, ErrOperationIndeterminate) {
		t.Fatalf("Apply() error = %v, want ErrOperationIndeterminate", err)
	}
	if status.Status != StateModified || !status.ManualAttention || status.ManagedByVoHive ||
		status.Revision != mmisolation.AbsentRevision || status.CanInstall || status.CanUninstall {
		t.Fatalf("unchanged disk state did not retain reload-indeterminate lock: %+v", status)
	}
}

func TestCoordinatorFailsClosedAndReturnsPostStatusWithoutConfirmedReload(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "INDETERMINATE")
	manager := layout.manager(t, &recordingRunner{})
	launcher := indeterminateWorkerLauncher{
		requestPath: workerRequestPath(layout.stateDir),
		mutate: func() {
			layout.writeManagedRule(t, []mmisolation.Entry{{
				TargetID: target.ID,
				Matcher: mmisolation.Matcher{
					Kind: mmisolation.MatcherSerial, VendorID: "2c7c", Serial: "INDETERMINATE",
				},
			}})
		},
	}
	bootID := "boot-0001"
	coordinator := NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
		Manager: manager, RulePath: layout.rulePath, StateDir: layout.stateDir,
		CapabilityProbe: supportedCapability,
		Launcher:        launcher,
		BootIDProvider:  func() (string, error) { return bootID, nil },
	})
	preview, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}

	status, err := coordinator.Apply(
		context.Background(), ActionInstall, []Target{target},
		preview.Revision, preview.PlanRevision,
	)
	if !errors.Is(err, ErrOperationIndeterminate) {
		t.Fatalf("Apply() error = %v, want ErrOperationIndeterminate", err)
	}
	if strings.Contains(err.Error(), "private-token") {
		t.Fatalf("indeterminate API error leaked worker detail: %v", err)
	}
	if status.Status != StateModified || !status.ManagedByVoHive || status.Revision == mmisolation.AbsentRevision {
		t.Fatalf("indeterminate operation was not locked on the post-operation state: %+v", status)
	}
	if !status.ManualAttention || status.CanInstall || status.CanUninstall {
		t.Fatalf("indeterminate operation did not require manual attention: %+v", status)
	}
	if status.RequiresReplug || status.Warning != "" {
		t.Fatalf("indeterminate operation was presented as committed: %+v", status)
	}
	if !strings.Contains(status.Reason, "重启宿主机") ||
		!strings.Contains(status.Reason, "udevadm control --reload-rules") {
		t.Fatalf("manual-attention recovery guidance = %q", status.Reason)
	}
	markerInfo, err := os.Stat(manualAttentionPath(layout.stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if markerInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("manual-attention marker mode = %o, want private", markerInfo.Mode().Perm())
	}

	repeated, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Status != StateModified || !repeated.ManualAttention || repeated.CanInstall || repeated.CanUninstall {
		t.Fatalf("manual-attention lock was lost on status refresh: %+v", repeated)
	}

	bootID = "boot-0002"
	recovered, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != StateCurrent || recovered.ManualAttention || !recovered.ManagedByVoHive {
		t.Fatalf("new boot did not clear manual-attention lock: %+v", recovered)
	}
	if _, err := os.Stat(manualAttentionPath(layout.stateDir)); !os.IsNotExist(err) {
		t.Fatalf("manual-attention marker still exists after boot change: %v", err)
	}
}

func TestCoordinatorRecoversManualAttentionFromWorkerResultAcrossInstances(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "FALLBACK-LOCK")
	manager := layout.manager(t, &recordingRunner{})
	markerPath := manualAttentionPath(layout.stateDir)
	launcher := indeterminateWorkerLauncher{
		requestPath: workerRequestPath(layout.stateDir),
		mutate: func() {
			layout.writeManagedRule(t, []mmisolation.Entry{{
				TargetID: target.ID,
				Matcher: mmisolation.Matcher{
					Kind: mmisolation.MatcherSerial, VendorID: "2c7c", Serial: "FALLBACK-LOCK",
				},
			}})
		},
		afterPersist: func() {
			if err := os.Symlink("missing-target", markerPath); err != nil {
				t.Fatal(err)
			}
		},
	}
	bootID := "boot-0001"
	coordinator := NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
		Manager: manager, RulePath: layout.rulePath, StateDir: layout.stateDir,
		CapabilityProbe: supportedCapability,
		Launcher:        launcher,
		BootIDProvider:  func() (string, error) { return bootID, nil },
	})
	preview, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}

	_, err = coordinator.Apply(
		context.Background(), ActionInstall, []Target{target},
		preview.Revision, preview.PlanRevision,
	)
	if !errors.Is(err, ErrOperationIndeterminate) {
		t.Fatalf("Apply() error = %v, want ErrOperationIndeterminate", err)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}

	status, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StateModified || !status.ManualAttention || status.CanInstall || status.CanUninstall {
		t.Fatalf("persisted fallback did not keep status locked: %+v", status)
	}
	if !strings.Contains(status.Reason, workerResultPath(layout.stateDir)) {
		t.Fatalf("fallback recovery guidance = %q", status.Reason)
	}

	restarted := NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
		Manager: manager, RulePath: layout.rulePath, StateDir: layout.stateDir,
		CapabilityProbe: supportedCapability,
		BootIDProvider:  func() (string, error) { return bootID, nil },
	})
	restartedStatus, err := restarted.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if restartedStatus.Status != StateModified || !restartedStatus.ManualAttention {
		t.Fatalf("new coordinator lost same-boot fallback: %+v", restartedStatus)
	}

	if err := os.Remove(workerResultPath(layout.stateDir)); err != nil {
		t.Fatal(err)
	}
	stillLocked, err := restarted.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if stillLocked.Status != StateModified || !stillLocked.ManualAttention {
		t.Fatalf("removing only result evidence incorrectly released operation journal: %+v", stillLocked)
	}
	if err := os.Remove(workerRequestPath(layout.stateDir)); err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != StateCurrent || recovered.ManualAttention {
		t.Fatalf("manual result-evidence cleanup did not recover: %+v", recovered)
	}
}

func TestCoordinatorFailsClosedForInvalidManualAttentionMetadata(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "INVALID-MARKER")
	if err := os.WriteFile(manualAttentionPath(layout.stateDir), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator := NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
		Manager:  layout.manager(t, &recordingRunner{}),
		RulePath: layout.rulePath, StateDir: layout.stateDir,
		CapabilityProbe: supportedCapability,
		BootIDProvider:  func() (string, error) { return "boot-0001", nil },
	})

	status, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StateModified || !status.ManualAttention || status.CanInstall || status.CanUninstall {
		t.Fatalf("invalid manual-attention metadata did not fail closed: %+v", status)
	}
	if !strings.Contains(status.Reason, "元数据无法验证") {
		t.Fatalf("invalid marker reason = %q", status.Reason)
	}
}
func TestPersistWorkerResultRequiresTypedCleanupErrorForWarning(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	requestPath := workerRequestPath(layout.stateDir)
	request := WorkerRequest{
		Schema: workerSchema, ID: "0123456789abcdef0123456789abcdef",
		Action: ActionInstall, BootID: mustHostBootID(t),
	}
	committed := mmisolation.Result{
		State: mmisolation.StateManaged, Revision: "sha256:0123456789abcdef",
		Changed: true, Reloaded: true,
	}

	for _, test := range []struct {
		name        string
		err         error
		wantWarning bool
	}{
		{name: "typed cleanup", err: fmt.Errorf("%w: cleanup failed", mmisolation.ErrCleanupIncomplete), wantWarning: true},
		{name: "arbitrary committed error", err: errors.New(sensitiveCleanupError)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := persistWorkerResult(requestPath, request, committed, test.err); err == nil {
				t.Fatal("persistWorkerResult() unexpectedly returned nil")
			}
			result, err := loadWorkerResult(workerResultPath(layout.stateDir))
			if err != nil {
				t.Fatal(err)
			}
			if (result.WarningCode == workerWarningCleanupIncomplete) != test.wantWarning {
				t.Fatalf("warning code = %q, want warning=%v", result.WarningCode, test.wantWarning)
			}
		})
	}
}
