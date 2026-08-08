package hostconfig

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
)

type untrustedResultLauncher struct {
	requestPath string
	mutate      func()
	corrupt     bool
}

func (l untrustedResultLauncher) Launch(_ context.Context) error {
	if _, err := loadWorkerRequest(l.requestPath); err != nil {
		return err
	}
	if l.mutate != nil {
		l.mutate()
	}
	if l.corrupt {
		return os.WriteFile(workerResultPath(filepathDir(l.requestPath)), []byte("{not-json"), 0o600)
	}
	return nil
}

type blockingWorkerLauncher struct {
	requestPath string
	manager     *mmisolation.Manager
	entered     chan<- struct{}
	release     <-chan struct{}
}

func (l blockingWorkerLauncher) Launch(ctx context.Context) error {
	l.entered <- struct{}{}
	select {
	case <-l.release:
		return RunWorker(ctx, l.requestPath, l.manager)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCoordinatorJournalLocksUntrustedWorkerOutcomeAcrossRestart(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt bool
	}{
		{name: "missing result"},
		{name: "corrupt result", corrupt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := newHostConfigTestLayout(t)
			target := layout.addUSBDevice(t, "1-2", "2c7c", "JOURNAL-LOCK")
			manager := layout.manager(t, &recordingRunner{})
			bootID := "boot-journal-1"
			launcher := untrustedResultLauncher{
				requestPath: workerRequestPath(layout.stateDir),
				corrupt:     test.corrupt,
				mutate: func() {
					layout.writeManagedRule(t, []mmisolation.Entry{{
						TargetID: target.ID,
						Matcher: mmisolation.Matcher{
							Kind: mmisolation.MatcherSerial, VendorID: "2c7c", Serial: "JOURNAL-LOCK",
						},
					}})
				},
			}
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
			assertJournalLocked(t, status, layout)
			if _, err := os.Stat(workerRequestPath(layout.stateDir)); err != nil {
				t.Fatalf("operation journal was not retained: %v", err)
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
			assertJournalLocked(t, restartedStatus, layout)

			bootID = "boot-journal-2"
			recovered, err := restarted.Status(context.Background(), []Target{target})
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Status != StateCurrent || recovered.ManualAttention || !recovered.ManagedByVoHive {
				t.Fatalf("new boot did not clear stale journal: %+v", recovered)
			}
			for _, path := range []string{
				workerRequestPath(layout.stateDir), workerResultPath(layout.stateDir), manualAttentionPath(layout.stateDir),
			} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("stale evidence %q still exists: %v", path, err)
				}
			}
		})
	}
}

func TestCoordinatorNormalWorkerResultConsumesJournal(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "JOURNAL-NORMAL")
	manager := layout.manager(t, &recordingRunner{})
	probeCalls := 0
	sawJournalDuringFinalStatus := false
	probe := func() Capability {
		probeCalls++
		if probeCalls == 3 {
			if _, err := os.Stat(workerRequestPath(layout.stateDir)); err != nil {
				t.Fatalf("operation journal was released before final status verification: %v", err)
			}
			sawJournalDuringFinalStatus = true
		}
		return supportedCapability()
	}
	coordinator := newTestCoordinator(layout, manager, &synchronousWorkerLauncher{
		requestPath: workerRequestPath(layout.stateDir), manager: manager,
	}, probe)
	preview, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	status, err := coordinator.Apply(
		context.Background(), ActionInstall, []Target{target}, preview.Revision, preview.PlanRevision,
	)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StatePendingReplug || status.ManualAttention {
		t.Fatalf("normal operation status = %+v", status)
	}
	if !sawJournalDuringFinalStatus {
		t.Fatal("final status verification did not observe the operation journal")
	}
	for _, path := range []string{workerRequestPath(layout.stateDir), workerResultPath(layout.stateDir)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("normal operation retained metadata %q: %v", path, err)
		}
	}
	restarted := newTestCoordinator(layout, manager, nil, supportedCapability)
	repeated, err := restarted.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Status != StateCurrent || repeated.ManualAttention {
		t.Fatalf("normal operation journal caused a false lock: %+v", repeated)
	}
}

func TestCoordinatorFailsClosedForUnverifiableJournalBoot(t *testing.T) {
	for _, test := range []struct {
		name        string
		request     WorkerRequest
		bootFailure bool
	}{
		{
			name: "missing boot ID",
			request: WorkerRequest{
				Schema: workerSchema, ID: "0123456789abcdef0123456789abcdef", Action: ActionInstall,
				ExpectedRevision:     mmisolation.AbsentRevision,
				ExpectedPlanRevision: "sha256:" + strings.Repeat("0", 64),
			},
		},
		{
			name: "current boot unavailable",
			request: WorkerRequest{
				Schema: workerSchema, ID: "0123456789abcdef0123456789abcdef", Action: ActionInstall,
				BootID: "boot-journal-1", ExpectedRevision: mmisolation.AbsentRevision,
				ExpectedPlanRevision: "sha256:" + strings.Repeat("0", 64),
			},
			bootFailure: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := newHostConfigTestLayout(t)
			target := layout.addUSBDevice(t, "1-2", "2c7c", "JOURNAL-BOOT")
			if err := atomicWriteJSON(workerRequestPath(layout.stateDir), test.request, 0o600); err != nil {
				t.Fatal(err)
			}
			coordinator := NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
				Manager: layout.manager(t, &recordingRunner{}), RulePath: layout.rulePath, StateDir: layout.stateDir,
				CapabilityProbe: supportedCapability,
				BootIDProvider: func() (string, error) {
					if test.bootFailure {
						return "", errors.New("boot ID unavailable")
					}
					return "boot-journal-1", nil
				},
			})
			status, err := coordinator.Status(context.Background(), []Target{target})
			if err != nil {
				t.Fatal(err)
			}
			assertJournalLocked(t, status, layout)
		})
	}
}

func TestCoordinatorFailsClosedForUnverifiableResultBoot(t *testing.T) {
	for _, test := range []struct {
		name        string
		resultBoot  string
		bootFailure bool
	}{
		{name: "missing result boot ID"},
		{name: "current boot unavailable", resultBoot: "boot-result-1", bootFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := newHostConfigTestLayout(t)
			target := layout.addUSBDevice(t, "1-2", "2c7c", "RESULT-BOOT")
			result := WorkerResult{
				Schema: workerSchema,
				ID:     "0123456789abcdef0123456789abcdef",
				Action: ActionInstall,
				BootID: test.resultBoot,
			}
			if err := atomicWriteJSON(workerResultPath(layout.stateDir), result, 0o600); err != nil {
				t.Fatal(err)
			}
			coordinator := NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
				Manager: layout.manager(t, &recordingRunner{}), RulePath: layout.rulePath, StateDir: layout.stateDir,
				CapabilityProbe: supportedCapability,
				BootIDProvider: func() (string, error) {
					if test.bootFailure {
						return "", errors.New("boot ID unavailable")
					}
					return "boot-result-1", nil
				},
			})
			status, err := coordinator.Status(context.Background(), []Target{target})
			if err != nil {
				t.Fatal(err)
			}
			if status.Status != StateModified || !status.ManualAttention || status.CanInstall || status.CanUninstall ||
				status.RequiresReplug || status.Warning != "" {
				t.Fatalf("unverifiable result boot did not lock configuration: %+v", status)
			}
			if !strings.Contains(status.Reason, workerResultPath(layout.stateDir)) {
				t.Fatalf("result recovery guidance = %q", status.Reason)
			}
		})
	}
}

func TestCreateJSONNoReplacePreservesExistingOperationOwner(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	path := workerRequestPath(layout.stateDir)
	first := WorkerRequest{
		Schema: workerSchema, ID: "0123456789abcdef0123456789abcdef",
		Action: ActionUninstall, BootID: "boot-owner-1",
		ExpectedRevision:     mmisolation.AbsentRevision,
		ExpectedPlanRevision: "sha256:" + strings.Repeat("1", 64),
	}
	second := first
	second.ID = "fedcba9876543210fedcba9876543210"
	if err := createJSONNoReplace(path, first, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createJSONNoReplace(path, second, 0o600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second create error = %v, want os.ErrExist", err)
	}
	loaded, err := loadWorkerRequest(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != first.ID || loaded.BootID != first.BootID {
		t.Fatalf("operation owner was replaced: %+v", loaded)
	}
}

func TestCoordinatorsUseRequestJournalAsCrossProcessOwnershipLock(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "CROSS-PROCESS")
	manager := layout.manager(t, &recordingRunner{})
	bootID := mustHostBootID(t)
	previewer := NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
		Manager: manager, RulePath: layout.rulePath, StateDir: layout.stateDir,
		CapabilityProbe: supportedCapability,
		BootIDProvider:  func() (string, error) { return bootID, nil },
	})
	preview, err := previewer.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}

	readyAtOwnership := make(chan struct{}, 2)
	releaseOwnership := make(chan struct{})
	newBootProvider := func() func() (string, error) {
		calls := 0
		return func() (string, error) {
			calls++
			if calls == 2 {
				readyAtOwnership <- struct{}{}
				<-releaseOwnership
			}
			return bootID, nil
		}
	}
	launcherEntered := make(chan struct{}, 1)
	releaseWorker := make(chan struct{})
	launcher := blockingWorkerLauncher{
		requestPath: workerRequestPath(layout.stateDir), manager: manager,
		entered: launcherEntered, release: releaseWorker,
	}
	coordinators := []*LocalCoordinator{
		NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
			Manager: manager, RulePath: layout.rulePath, StateDir: layout.stateDir,
			CapabilityProbe: supportedCapability, Launcher: launcher, BootIDProvider: newBootProvider(),
		}),
		NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
			Manager: manager, RulePath: layout.rulePath, StateDir: layout.stateDir,
			CapabilityProbe: supportedCapability, Launcher: launcher, BootIDProvider: newBootProvider(),
		}),
	}
	type outcome struct {
		status Status
		err    error
	}
	outcomes := make(chan outcome, 2)
	for _, coordinator := range coordinators {
		go func(coordinator *LocalCoordinator) {
			status, applyErr := coordinator.Apply(
				context.Background(), ActionInstall, []Target{target},
				preview.Revision, preview.PlanRevision,
			)
			outcomes <- outcome{status: status, err: applyErr}
		}(coordinator)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-readyAtOwnership:
		case <-time.After(5 * time.Second):
			t.Fatal("coordinators did not both reach operation ownership acquisition")
		}
	}
	close(releaseOwnership)
	select {
	case <-launcherEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("winning coordinator did not launch its worker")
	}

	var busy outcome
	select {
	case busy = <-outcomes:
	case <-time.After(5 * time.Second):
		t.Fatal("losing coordinator did not fail while the owner worker was blocked")
	}
	if !errors.Is(busy.err, ErrOperationBusy) {
		t.Fatalf("losing coordinator error = %v, status=%+v; want ErrOperationBusy", busy.err, busy.status)
	}
	owned, err := loadWorkerRequest(workerRequestPath(layout.stateDir))
	if err != nil {
		t.Fatalf("winning operation journal was not preserved: %v", err)
	}
	if owned.BootID != bootID || owned.Action != ActionInstall {
		t.Fatalf("winning operation journal changed: %+v", owned)
	}
	if _, err := os.Lstat(workerResultPath(layout.stateDir)); !os.IsNotExist(err) {
		t.Fatalf("losing coordinator touched the owner's result path: %v", err)
	}

	close(releaseWorker)
	var winner outcome
	select {
	case winner = <-outcomes:
	case <-time.After(5 * time.Second):
		t.Fatal("winning coordinator did not finish")
	}
	if winner.err != nil || winner.status.Status != StatePendingReplug {
		t.Fatalf("winning coordinator outcome = status=%+v err=%v", winner.status, winner.err)
	}
	for _, path := range []string{workerRequestPath(layout.stateDir), workerResultPath(layout.stateDir)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("successful owner retained metadata %q: %v", path, err)
		}
	}
}

func TestCoordinatorLocksResidualCommittedUnexpectedError(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "COMMITTED-UNEXPECTED")
	manager := layout.manager(t, &recordingRunner{})
	committed, err := manager.Install(context.Background(), mmisolation.Request{
		Targets: []mmisolation.Target{{ID: target.ID, USBPath: target.USBPath}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bootID := mustHostBootID(t)
	result := WorkerResult{
		Schema: workerSchema, ID: "0123456789abcdef0123456789abcdef",
		Action: ActionInstall, BootID: bootID, Result: committed,
		Code: "worker_error", Error: sensitiveCleanupError,
	}
	if err := atomicWriteJSON(workerResultPath(layout.stateDir), result, 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator := NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
		Manager: manager, RulePath: layout.rulePath, StateDir: layout.stateDir,
		CapabilityProbe: supportedCapability,
		BootIDProvider:  func() (string, error) { return bootID, nil },
	})
	status, err := coordinator.Status(context.Background(), []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StateModified || !status.ManualAttention || status.CanInstall || status.CanUninstall {
		t.Fatalf("committed unexpected error did not remain locked: %+v", status)
	}
}

func assertJournalLocked(t *testing.T, status Status, layout hostConfigTestLayout) {
	t.Helper()
	if status.Status != StateModified || !status.ManualAttention || status.CanInstall || status.CanUninstall ||
		status.RequiresReplug || status.Warning != "" {
		t.Fatalf("journal did not lock configuration: %+v", status)
	}
	if !strings.Contains(status.Reason, workerRequestPath(layout.stateDir)) {
		t.Fatalf("journal recovery guidance = %q", status.Reason)
	}
}
