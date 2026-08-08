package hostconfig

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
)

type hostConfigTestLayout struct {
	mount    string
	usbRoot  string
	rulePath string
	stateDir string
}

func newHostConfigTestLayout(t *testing.T) hostConfigTestLayout {
	t.Helper()
	root := t.TempDir()
	layout := hostConfigTestLayout{
		mount:    filepath.Join(root, "sys"),
		rulePath: filepath.Join(root, "etc", "udev", "rules.d", "78-mm-vohive-managed.rules"),
		stateDir: filepath.Join(root, "var", "lib", "vohive", "host-config"),
	}
	layout.usbRoot = filepath.Join(layout.mount, "bus", "usb", "devices")
	for _, dir := range []string{layout.usbRoot, filepath.Dir(layout.rulePath), layout.stateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	return layout
}

func (l hostConfigTestLayout) addUSBDevice(t *testing.T, kernelPath, vendorID, serial string) Target {
	t.Helper()
	path := filepath.Join(l.usbRoot, kernelPath)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
	for name, value := range map[string]string{
		"idVendor":  vendorID + "\n",
		"idProduct": "0125\n",
		"serial":    serial + "\n",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0o600); err != nil {
			t.Fatalf("WriteFile(%q): %v", name, err)
		}
	}
	return Target{
		ID:            "device-" + kernelPath,
		Name:          "Modem " + kernelPath,
		USBPath:       path,
		ControlDevice: filepath.Join(l.mount, "class", "usbmisc", "cdc-wdm0"),
	}
}

func (l hostConfigTestLayout) manager(t *testing.T, runner mmisolation.Runner) *mmisolation.Manager {
	t.Helper()
	manager, err := mmisolation.New(mmisolation.Options{
		RulePath:   l.rulePath,
		SysfsRoot:  l.usbRoot,
		SysfsMount: l.mount,
		Runner:     runner,
	})
	if err != nil {
		t.Fatalf("New manager: %v", err)
	}
	return manager
}

func (l hostConfigTestLayout) writeManagedRule(t *testing.T, entries []mmisolation.Entry) {
	t.Helper()
	data, err := mmisolation.Render(entries)
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}
	if err := os.WriteFile(l.rulePath, data, 0o644); err != nil {
		t.Fatalf("WriteFile(rule): %v", err)
	}
}

func supportedCapability() Capability {
	return Capability{Supported: true}
}

func mustHostBootID(t *testing.T) string {
	t.Helper()
	bootID, err := readHostBootID()
	if err != nil {
		t.Fatalf("readHostBootID(): %v", err)
	}
	return bootID
}

func newTestCoordinator(
	layout hostConfigTestLayout,
	manager *mmisolation.Manager,
	launcher Launcher,
	probe func() Capability,
) *LocalCoordinator {
	return NewLocalCoordinatorWithOptions(LocalCoordinatorOptions{
		Manager: manager, RulePath: layout.rulePath, StateDir: layout.stateDir,
		CapabilityProbe: probe, Launcher: launcher,
	})
}

func TestCoordinatorStatusClassifications(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		layout := newHostConfigTestLayout(t)
		target := layout.addUSBDevice(t, "1-2", "2c7c", "ABSENT-1")
		status, err := newTestCoordinator(
			layout, layout.manager(t, nil), nil, supportedCapability,
		).Status(context.Background(), []Target{target})
		if err != nil {
			t.Fatal(err)
		}
		if status.Status != StateAbsent || !status.CanInstall || status.CanUninstall ||
			status.TotalDevices != 1 || status.CoveredDevices != 0 ||
			status.Revision != mmisolation.AbsentRevision {
			t.Fatalf("absent status = %+v", status)
		}
	})

	t.Run("current", func(t *testing.T) {
		layout := newHostConfigTestLayout(t)
		target := layout.addUSBDevice(t, "1-2", "2c7c", "CURRENT-1")
		layout.writeManagedRule(t, []mmisolation.Entry{{
			TargetID: target.ID,
			Matcher: mmisolation.Matcher{
				Kind: mmisolation.MatcherSerial, VendorID: "2c7c", Serial: "CURRENT-1",
			},
		}})
		status, err := newTestCoordinator(
			layout, layout.manager(t, nil), nil, supportedCapability,
		).Status(context.Background(), []Target{target})
		if err != nil {
			t.Fatal(err)
		}
		if status.Status != StateCurrent || status.CanInstall || !status.CanUninstall ||
			!status.ManagedByVoHive || status.CoveredDevices != 1 ||
			len(status.Devices) != 1 || !status.Devices[0].Covered {
			t.Fatalf("current status = %+v", status)
		}
	})

	t.Run("stale", func(t *testing.T) {
		layout := newHostConfigTestLayout(t)
		target := layout.addUSBDevice(t, "1-2", "2c7c", "NEW-1")
		layout.writeManagedRule(t, []mmisolation.Entry{{
			TargetID: "removed-device",
			Matcher: mmisolation.Matcher{
				Kind: mmisolation.MatcherSerial, VendorID: "2c7c", Serial: "OLD-1",
			},
		}})
		status, err := newTestCoordinator(
			layout, layout.manager(t, nil), nil, supportedCapability,
		).Status(context.Background(), []Target{target})
		if err != nil {
			t.Fatal(err)
		}
		if status.Status != StateStale || !status.CanInstall || !status.CanUninstall ||
			status.CoveredDevices != 0 {
			t.Fatalf("stale status = %+v", status)
		}
	})

	t.Run("partial", func(t *testing.T) {
		layout := newHostConfigTestLayout(t)
		valid := layout.addUSBDevice(t, "1-2", "2c7c", "VALID-1")
		invalid := Target{ID: "missing", USBPath: filepath.Join(layout.mount, "missing")}
		status, err := newTestCoordinator(
			layout, layout.manager(t, nil), nil, supportedCapability,
		).Status(context.Background(), []Target{valid, invalid})
		if err != nil {
			t.Fatal(err)
		}
		if status.Status != StatePartial || status.CanInstall || status.CanUninstall ||
			status.TotalDevices != 2 || status.Reason == "" || status.Devices[1].Reason == "" {
			t.Fatalf("partial status = %+v", status)
		}
	})

	t.Run("foreign", func(t *testing.T) {
		layout := newHostConfigTestLayout(t)
		target := layout.addUSBDevice(t, "1-2", "2c7c", "FOREIGN-1")
		if err := os.WriteFile(layout.rulePath, []byte("# administrator rule\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		status, err := newTestCoordinator(
			layout, layout.manager(t, nil), nil, supportedCapability,
		).Status(context.Background(), []Target{target})
		if err != nil {
			t.Fatal(err)
		}
		if status.Status != StateForeign || status.ManagedByVoHive ||
			status.CanInstall || status.CanUninstall || status.Reason == "" {
			t.Fatalf("foreign status = %+v", status)
		}
	})

	t.Run("modified", func(t *testing.T) {
		layout := newHostConfigTestLayout(t)
		target := layout.addUSBDevice(t, "1-2", "2c7c", "MODIFIED-1")
		layout.writeManagedRule(t, []mmisolation.Entry{{
			TargetID: target.ID,
			Matcher: mmisolation.Matcher{
				Kind: mmisolation.MatcherSerial, VendorID: "2c7c", Serial: "MODIFIED-1",
			},
		}})
		file, err := os.OpenFile(layout.rulePath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("# locally changed\n"); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		status, err := newTestCoordinator(
			layout, layout.manager(t, nil), nil, supportedCapability,
		).Status(context.Background(), []Target{target})
		if err != nil {
			t.Fatal(err)
		}
		if status.Status != StateModified || status.ManagedByVoHive ||
			status.CanInstall || status.CanUninstall || status.Reason == "" {
			t.Fatalf("modified status = %+v", status)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		layout := newHostConfigTestLayout(t)
		target := layout.addUSBDevice(t, "1-2", "2c7c", "UNSUPPORTED-1")
		const reason = "test host is not supported"
		status, err := newTestCoordinator(
			layout,
			layout.manager(t, nil),
			nil,
			func() Capability { return Capability{Reason: reason} },
		).Status(context.Background(), []Target{target})
		if err != nil {
			t.Fatal(err)
		}
		if status.Status != StateUnsupported || status.Reason != reason ||
			status.CanInstall || status.CanUninstall || len(status.Devices) != 1 ||
			status.Devices[0].Reason != reason {
			t.Fatalf("unsupported status = %+v", status)
		}
	})
}

type recordedCommand struct {
	name string
	args []string
}

type recordingRunner struct {
	mu    sync.Mutex
	calls []recordedCommand
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedCommand{name: name, args: append([]string(nil), args...)})
	return nil, nil
}

func (r *recordingRunner) snapshot() []recordedCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedCommand(nil), r.calls...)
}

type synchronousWorkerLauncher struct {
	requestPath string
	manager     *mmisolation.Manager
	mu          sync.Mutex
	calls       int
	entered     chan struct{}
	release     <-chan struct{}
}

func (l *synchronousWorkerLauncher) Launch(ctx context.Context) error {
	l.mu.Lock()
	l.calls++
	entered := l.entered
	release := l.release
	l.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return RunWorker(ctx, l.requestPath, l.manager)
}

func (l *synchronousWorkerLauncher) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func TestCoordinatorApplyRoundTripCASRequiresReplugAndBusy(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "ROUNDTRIP-1")
	runner := &recordingRunner{}
	manager := layout.manager(t, runner)
	launcher := &synchronousWorkerLauncher{
		requestPath: workerRequestPath(layout.stateDir),
		manager:     manager,
	}
	coordinator := newTestCoordinator(layout, manager, launcher, supportedCapability)
	preview, previewErr := coordinator.Status(context.Background(), []Target{target})
	if previewErr != nil {
		t.Fatal(previewErr)
	}

	_, err := coordinator.Apply(
		context.Background(), ActionInstall, []Target{target}, "", preview.PlanRevision,
	)
	if !errors.Is(err, mmisolation.ErrRevisionConflict) {
		t.Fatalf("Apply(missing revision) error = %v, want revision conflict", err)
	}
	if launcher.callCount() != 0 {
		t.Fatal("missing revision launched the privileged worker")
	}

	_, err = coordinator.Apply(
		context.Background(), ActionInstall, []Target{target}, "sha256:stale", preview.PlanRevision,
	)
	if !errors.Is(err, mmisolation.ErrRevisionConflict) {
		t.Fatalf("Apply(stale install) error = %v, want revision conflict", err)
	}
	if launcher.callCount() != 0 {
		t.Fatal("CAS conflict launched the privileged worker")
	}

	installed, err := coordinator.Apply(
		context.Background(), ActionInstall, []Target{target}, mmisolation.AbsentRevision, preview.PlanRevision,
	)
	if err != nil {
		t.Fatalf("Apply(install): %v", err)
	}
	if installed.Status != StatePendingReplug || !installed.RequiresReplug ||
		installed.Revision == "" || installed.Revision == mmisolation.AbsentRevision ||
		!installed.ManagedByVoHive || !installed.CanUninstall {
		t.Fatalf("installed status = %+v", installed)
	}

	removed, err := coordinator.Apply(
		context.Background(), ActionUninstall, []Target{target}, installed.Revision, installed.PlanRevision,
	)
	if err != nil {
		t.Fatalf("Apply(uninstall): %v", err)
	}
	if removed.Status != StatePendingReplug || !removed.RequiresReplug ||
		removed.Revision != mmisolation.AbsentRevision || removed.ManagedByVoHive ||
		!removed.CanInstall || removed.CanUninstall {
		t.Fatalf("removed status = %+v", removed)
	}
	if _, err := os.Lstat(layout.rulePath); !os.IsNotExist(err) {
		t.Fatalf("managed rule still exists after uninstall: %v", err)
	}
	assertOnlyReloadCommands(t, runner.snapshot(), 2)

	blockingLayout := newHostConfigTestLayout(t)
	blockingTarget := blockingLayout.addUSBDevice(t, "2-1", "2c7c", "BUSY-1")
	blockingRunner := &recordingRunner{}
	blockingManager := blockingLayout.manager(t, blockingRunner)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	blockingLauncher := &synchronousWorkerLauncher{
		requestPath: workerRequestPath(blockingLayout.stateDir),
		manager:     blockingManager,
		entered:     entered,
		release:     release,
	}
	blockingCoordinator := newTestCoordinator(
		blockingLayout, blockingManager, blockingLauncher, supportedCapability,
	)
	blockingPreview, previewErr := blockingCoordinator.Status(context.Background(), []Target{blockingTarget})
	if previewErr != nil {
		t.Fatal(previewErr)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, applyErr := blockingCoordinator.Apply(
			context.Background(), ActionInstall, []Target{blockingTarget}, mmisolation.AbsentRevision, blockingPreview.PlanRevision,
		)
		firstDone <- applyErr
	}()
	<-entered
	_, err = blockingCoordinator.Apply(
		context.Background(), ActionInstall, []Target{blockingTarget}, mmisolation.AbsentRevision, blockingPreview.PlanRevision,
	)
	if !errors.Is(err, ErrOperationBusy) {
		t.Fatalf("concurrent Apply error = %v, want ErrOperationBusy", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Apply after release: %v", err)
	}
}

func TestWorkerRejectsInvalidInputAndUsesFixedActions(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "WORKER-1")
	runner := &recordingRunner{}
	manager := layout.manager(t, runner)
	requestPath := workerRequestPath(layout.stateDir)

	if err := RunWorker(context.Background(), "request.json", manager); !errors.Is(err, mmisolation.ErrInvalidRequest) {
		t.Fatalf("RunWorker(relative path) error = %v", err)
	}
	wrongPath := filepath.Join(layout.stateDir, "other.json")
	if err := RunWorker(context.Background(), wrongPath, manager); !errors.Is(err, mmisolation.ErrInvalidRequest) {
		t.Fatalf("RunWorker(wrong basename) error = %v", err)
	}

	unknownField := `{"schema":1,"id":"0123456789abcdef0123456789abcdef","action":"install","surprise":true}`
	if err := os.WriteFile(requestPath, []byte(unknownField), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunWorker(context.Background(), requestPath, manager); !errors.Is(err, mmisolation.ErrInvalidRequest) {
		t.Fatalf("RunWorker(unknown field) error = %v", err)
	}

	unknownAction := `{"schema":1,"id":"0123456789abcdef0123456789abcdef","action":"restart"}`
	if err := os.WriteFile(requestPath, []byte(unknownAction), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunWorker(context.Background(), requestPath, manager); !errors.Is(err, mmisolation.ErrInvalidRequest) {
		t.Fatalf("RunWorker(unknown action) error = %v", err)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("invalid worker input executed commands: %+v", calls)
	}

	workerTargets := []mmisolation.Target{{ID: target.ID, USBPath: target.USBPath}}
	installPlan, err := buildPlanSnapshot(manager, planInputsFromWorkerTargets(workerTargets))
	if err != nil {
		t.Fatal(err)
	}
	installRequest := WorkerRequest{
		Schema:               workerSchema,
		ID:                   "0123456789abcdef0123456789abcdef",
		Action:               ActionInstall,
		BootID:               mustHostBootID(t),
		Targets:              workerTargets,
		ExpectedRevision:     mmisolation.AbsentRevision,
		ExpectedPlanRevision: installPlan.Revision,
	}
	if err := atomicWriteJSON(requestPath, installRequest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunWorker(context.Background(), requestPath, manager); err != nil {
		t.Fatalf("RunWorker(install): %v", err)
	}
	installResult, err := loadWorkerResult(workerResultPath(layout.stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if installResult.Action != ActionInstall || !installResult.Result.Changed ||
		!installResult.Result.Reloaded || installResult.Result.Revision == "" {
		t.Fatalf("install result = %+v", installResult)
	}

	uninstallPlan, err := buildPlanSnapshot(manager, planInputsFromWorkerTargets(workerTargets))
	if err != nil {
		t.Fatal(err)
	}
	uninstallRequest := WorkerRequest{
		Schema:               workerSchema,
		ID:                   "fedcba9876543210fedcba9876543210",
		Action:               ActionUninstall,
		BootID:               mustHostBootID(t),
		Targets:              workerTargets,
		ExpectedRevision:     installResult.Result.Revision,
		ExpectedPlanRevision: uninstallPlan.Revision,
	}
	if err := atomicWriteJSON(requestPath, uninstallRequest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunWorker(context.Background(), requestPath, manager); err != nil {
		t.Fatalf("RunWorker(uninstall): %v", err)
	}
	uninstallResult, err := loadWorkerResult(workerResultPath(layout.stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if uninstallResult.Action != ActionUninstall || !uninstallResult.Result.Changed ||
		!uninstallResult.Result.Reloaded ||
		uninstallResult.Result.Revision != mmisolation.AbsentRevision {
		t.Fatalf("uninstall result = %+v", uninstallResult)
	}
	assertOnlyReloadCommands(t, runner.snapshot(), 2)
}

func assertOnlyReloadCommands(t *testing.T, calls []recordedCommand, want int) {
	t.Helper()
	if len(calls) != want {
		t.Fatalf("command calls = %+v, want %d reloads", calls, want)
	}
	wantArgs := []string{"control", "--reload-rules"}
	for _, call := range calls {
		if call.name != "udevadm" || !reflect.DeepEqual(call.args, wantArgs) {
			t.Fatalf("unexpected privileged command: %+v", call)
		}
		for _, forbidden := range []string{"trigger", "restart"} {
			if call.name == forbidden {
				t.Fatalf("worker executed forbidden command %q", forbidden)
			}
			for _, arg := range call.args {
				if arg == forbidden {
					t.Fatalf("worker executed forbidden action %q: %+v", forbidden, call)
				}
			}
		}
	}
}

func TestWorkerRequestRejectsTrailingJSONValue(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	path := workerRequestPath(layout.stateDir)
	request := WorkerRequest{
		Schema: workerSchema,
		ID:     "0123456789abcdef0123456789abcdef",
		Action: ActionUninstall,
		BootID: mustHostBootID(t),
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\n{}\n")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkerRequest(path); !errors.Is(err, mmisolation.ErrInvalidRequest) {
		t.Fatalf("loadWorkerRequest(trailing value) error = %v", err)
	}
}

func TestOpenBoundedRegularFileRejectsGroupOrWorldPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 == 0 {
		t.Skip("platform does not expose group/world mode bits")
	}
	file, err := openBoundedRegularFile(path, maxWorkerFileBytes)
	if file != nil {
		file.Close()
	}
	if err == nil {
		t.Fatal("openBoundedRegularFile accepted group-readable metadata")
	}
}
