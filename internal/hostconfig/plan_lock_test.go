package hostconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
	"github.com/zanescope/vohive/internal/updater"
)

func TestWorkerMapsTargetSnapshotConflictToPlanConflict(t *testing.T) {
	if got := workerErrorCode(mmisolation.ErrTargetSnapshotConflict); got != "plan_conflict" {
		t.Fatalf("workerErrorCode() = %q, want plan_conflict", got)
	}
}

func TestCoordinatorPlanCASRejectsChangedTargetSnapshotWithoutLaunching(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, hostConfigTestLayout, Target) []Target
	}{
		{
			name: "device removed",
			mutate: func(*testing.T, hostConfigTestLayout, Target) []Target {
				return nil
			},
		},
		{
			name: "device added",
			mutate: func(t *testing.T, layout hostConfigTestLayout, target Target) []Target {
				added := layout.addUSBDevice(t, "1-3", "2c7c", "PLAN-ADDED")
				return []Target{target, added}
			},
		},
		{
			name: "same device ID moved USB path",
			mutate: func(t *testing.T, layout hostConfigTestLayout, target Target) []Target {
				moved := layout.addUSBDevice(t, "2-4", "2c7c", "PLAN-MOVED")
				moved.ID = target.ID
				return []Target{moved}
			},
		},
		{
			name: "resolved matcher changed when serial became non-unique",
			mutate: func(t *testing.T, layout hostConfigTestLayout, target Target) []Target {
				layout.addUSBDevice(t, "3-5", "2c7c", "PLAN-ORIGINAL")
				return []Target{target}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := newHostConfigTestLayout(t)
			target := layout.addUSBDevice(t, "1-2", "2c7c", "PLAN-ORIGINAL")
			runner := &recordingRunner{}
			manager := layout.manager(t, runner)
			launcher := &synchronousWorkerLauncher{requestPath: workerRequestPath(layout.stateDir), manager: manager}
			coordinator := newTestCoordinator(layout, manager, launcher, supportedCapability)
			preview, err := coordinator.Status(context.Background(), []Target{target})
			if err != nil {
				t.Fatal(err)
			}
			changedTargets := test.mutate(t, layout, target)
			_, err = coordinator.Apply(
				context.Background(), ActionInstall, changedTargets,
				preview.Revision, preview.PlanRevision,
			)
			if !errors.Is(err, ErrPlanConflict) {
				t.Fatalf("Apply() error = %v, want ErrPlanConflict", err)
			}
			if launcher.callCount() != 0 {
				t.Fatal("plan conflict launched the privileged worker")
			}
			if calls := runner.snapshot(); len(calls) != 0 {
				t.Fatalf("plan conflict executed udev commands: %+v", calls)
			}
			if _, statErr := os.Lstat(layout.rulePath); !os.IsNotExist(statErr) {
				t.Fatalf("plan conflict wrote the rule: %v", statErr)
			}
		})
	}
}

func TestCoordinatorPlanCASRejectsMissingPlanWithoutLaunching(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "PLAN-MISSING")
	manager := layout.manager(t, &recordingRunner{})
	launcher := &synchronousWorkerLauncher{requestPath: workerRequestPath(layout.stateDir), manager: manager}
	coordinator := newTestCoordinator(layout, manager, launcher, supportedCapability)
	_, err := coordinator.Apply(
		context.Background(), ActionInstall, []Target{target}, mmisolation.AbsentRevision, "",
	)
	if !errors.Is(err, ErrPlanConflict) {
		t.Fatalf("Apply() error = %v, want ErrPlanConflict", err)
	}
	if launcher.callCount() != 0 {
		t.Fatal("missing plan revision launched the privileged worker")
	}
}

func TestWorkerRechecksPlanImmediatelyBeforeMutation(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "WORKER-PLAN")
	runner := &recordingRunner{}
	manager := layout.manager(t, runner)
	targets := []mmisolation.Target{{ID: target.ID, USBPath: target.USBPath}}
	preview, err := buildPlanSnapshot(manager, planInputsFromWorkerTargets(targets))
	if err != nil {
		t.Fatal(err)
	}
	layout.addUSBDevice(t, "1-3", "2c7c", "WORKER-PLAN")
	request := WorkerRequest{
		Schema: workerSchema, ID: "0123456789abcdef0123456789abcdef", Action: ActionInstall,
		BootID: mustHostBootID(t), Targets: targets,
		ExpectedRevision:     mmisolation.AbsentRevision,
		ExpectedPlanRevision: preview.Revision,
	}
	requestPath := workerRequestPath(layout.stateDir)
	if err := atomicWriteJSON(requestPath, request, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunWorker(context.Background(), requestPath, manager); !errors.Is(err, ErrPlanConflict) {
		t.Fatalf("RunWorker() error = %v, want ErrPlanConflict", err)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("worker plan conflict executed udev commands: %+v", calls)
	}
	if _, err := os.Lstat(layout.rulePath); !os.IsNotExist(err) {
		t.Fatalf("worker plan conflict wrote the rule: %v", err)
	}
	result, err := loadWorkerResult(workerResultPath(layout.stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if result.Code != "plan_conflict" {
		t.Fatalf("worker result code = %q, want plan_conflict", result.Code)
	}
}

func TestWorkerUpdateLockConflictDoesNotMutate(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "LOCKED-WORKER")
	runner := &recordingRunner{}
	manager := layout.manager(t, runner)
	targets := []mmisolation.Target{{ID: target.ID, USBPath: target.USBPath}}
	preview, err := buildPlanSnapshot(manager, planInputsFromWorkerTargets(targets))
	if err != nil {
		t.Fatal(err)
	}
	request := WorkerRequest{
		Schema: workerSchema, ID: "0123456789abcdef0123456789abcdef", Action: ActionInstall,
		BootID: mustHostBootID(t), Targets: targets,
		ExpectedRevision:     preview.RuleStatus.Revision,
		ExpectedPlanRevision: preview.Revision,
	}
	requestPath := workerRequestPath(layout.stateDir)
	if err := atomicWriteJSON(requestPath, request, 0o600); err != nil {
		t.Fatal(err)
	}
	lockPath, err := workerUpdateLockPath(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := updater.AcquireUpdateLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := RunWorker(context.Background(), requestPath, manager); !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("RunWorker() error = %v, want ErrOperationBusy", err)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("locked worker executed udev commands: %+v", calls)
	}
	if _, err := os.Lstat(layout.rulePath); !os.IsNotExist(err) {
		t.Fatalf("locked worker wrote the rule: %v", err)
	}
}

type blockingLockRunner struct {
	entered chan struct{}
	release <-chan struct{}
}

func (r *blockingLockRunner) Run(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	select {
	case <-r.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestWorkerHoldsSharedUpdateLockForEntireMutation(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "HOSTCONFIG-LOCK")
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	manager := layout.manager(t, &blockingLockRunner{entered: entered, release: release})
	targets := []mmisolation.Target{{ID: target.ID, USBPath: target.USBPath}}
	preview, err := buildPlanSnapshot(manager, planInputsFromWorkerTargets(targets))
	if err != nil {
		t.Fatal(err)
	}
	request := WorkerRequest{
		Schema: workerSchema, ID: "0123456789abcdef0123456789abcdef", Action: ActionInstall,
		BootID: mustHostBootID(t), Targets: targets,
		ExpectedRevision:     preview.RuleStatus.Revision,
		ExpectedPlanRevision: preview.Revision,
	}
	requestPath := workerRequestPath(layout.stateDir)
	if err := atomicWriteJSON(requestPath, request, 0o600); err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan error, 1)
	go func() { workerDone <- RunWorker(context.Background(), requestPath, manager) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach the udev reload")
	}
	lockPath, err := workerUpdateLockPath(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	if lock, err := updater.AcquireUpdateLock(lockPath); !errors.Is(err, updater.ErrUpdateLocked) {
		if lock != nil {
			lock.Release()
		}
		t.Fatalf("updater lock acquisition error = %v, want ErrUpdateLocked", err)
	}
	close(release)
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatalf("RunWorker() after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(layout.stateDir), "update", "update.lock")); !os.IsNotExist(err) {
		t.Fatalf("worker left the update lock behind: %v", err)
	}
}
