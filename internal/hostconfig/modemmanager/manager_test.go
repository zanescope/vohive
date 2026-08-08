package modemmanager

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
)

type runnerCall struct {
	name string
	args []string
}

type fakeRunner struct {
	mu       sync.Mutex
	hooks    map[int]func()
	calls    []runnerCall
	failures map[int]error
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	callIndex := len(r.calls)
	r.calls = append(r.calls, runnerCall{name: name, args: append([]string(nil), args...)})
	if hook := r.hooks[callIndex]; hook != nil {
		hook()
	}
	if err := r.failures[callIndex]; err != nil {
		return []byte("synthetic reload failure"), err
	}
	return nil, nil
}

func (r *fakeRunner) snapshot() []runnerCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runnerCall(nil), r.calls...)
}

func TestInstallUsesCASAndFixedReloadCommand(t *testing.T) {
	layout := newTestLayout(t)
	firstPath := layout.addUSBDevice(t, "1-2", "2c7c", "0125", "FIRST")
	secondPath := layout.addUSBDevice(t, "1-3", "1199", "9071", "SECOND")
	runner := &fakeRunner{}
	manager := layout.manager(t, runner)

	first, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "first", USBPath: firstPath}},
	})
	if err != nil {
		t.Fatalf("Install(first): %v", err)
	}
	if !first.Changed || !first.Reloaded || first.State != StateManaged || first.Revision == "" {
		t.Fatalf("first result = %+v", first)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 1)

	_, err = manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "second", USBPath: secondPath}},
	})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("Install without managed revision error = %v, want ErrRevisionConflict", err)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 1)

	second, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "second", USBPath: secondPath}}, ExpectedRevision: first.Revision,
	})
	if err != nil {
		t.Fatalf("Install(second): %v", err)
	}
	if !second.Changed || second.Revision == first.Revision {
		t.Fatalf("second result = %+v", second)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 2)

	unchanged, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "second", USBPath: secondPath}}, ExpectedRevision: second.Revision,
	})
	if err != nil {
		t.Fatalf("Install(unchanged): %v", err)
	}
	if unchanged.Changed || unchanged.Reloaded || unchanged.Revision != second.Revision {
		t.Fatalf("unchanged result = %+v", unchanged)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 2)
}

func TestInstallRefusesForeignAndTamperedFiles(t *testing.T) {
	for _, test := range []struct {
		name  string
		state RuleState
		write func(*testing.T, testLayout)
		err   error
	}{
		{
			name: "foreign", state: StateForeign, err: ErrForeignRule,
			write: func(t *testing.T, layout testLayout) {
				writeTestFile(t, layout.rulePath, "# hand-written administrator rule\n")
			},
		},
		{
			name: "tampered", state: StateTampered, err: ErrTamperedRule,
			write: func(t *testing.T, layout testLayout) {
				data := mustRender(t, testEntries())
				data[len(data)-2] ^= 1
				if err := os.WriteFile(layout.rulePath, data, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := newTestLayout(t)
			targetPath := layout.addUSBDevice(t, "1-2", "2c7c", "0125", "FIRST")
			test.write(t, layout)
			before, err := os.ReadFile(layout.rulePath)
			if err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{}
			result, err := layout.manager(t, runner).Install(context.Background(), Request{
				Targets: []Target{{ID: "first", USBPath: targetPath}},
			})
			if !errors.Is(err, test.err) {
				t.Fatalf("Install() error = %v, want %v", err, test.err)
			}
			if result.State != test.state {
				t.Fatalf("result state = %s, want %s", result.State, test.state)
			}
			after, readErr := os.ReadFile(layout.rulePath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("refused file was modified")
			}
			if len(runner.snapshot()) != 0 {
				t.Fatal("udev was reloaded for a refused file")
			}
		})
	}
}

func TestInstallReloadFailureRollsBackCreation(t *testing.T) {
	layout := newTestLayout(t)
	targetPath := layout.addUSBDevice(t, "1-2", "2c7c", "0125", "FIRST")
	runner := &fakeRunner{failures: map[int]error{0: errors.New("reload failed")}}
	manager := layout.manager(t, runner)

	result, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "first", USBPath: targetPath}},
	})
	if err == nil || result.State != StateAbsent || result.Changed || result.ReloadIndeterminate {
		t.Fatalf("Install() result=%+v err=%v, want rolled-back absent state", result, err)
	}
	if _, statErr := os.Lstat(layout.rulePath); !os.IsNotExist(statErr) {
		t.Fatalf("rule remains after rollback: %v", statErr)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 2)
}

func TestInstallReloadFailureRestoresPreviousManagedRule(t *testing.T) {
	layout := newTestLayout(t)
	firstPath := layout.addUSBDevice(t, "1-2", "2c7c", "0125", "FIRST")
	secondPath := layout.addUSBDevice(t, "1-3", "1199", "9071", "SECOND")
	initialRunner := &fakeRunner{}
	manager := layout.manager(t, initialRunner)
	initial, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "first", USBPath: firstPath}},
	})
	if err != nil {
		t.Fatal(err)
	}

	failingRunner := &fakeRunner{failures: map[int]error{0: errors.New("reload failed")}}
	manager.runner = failingRunner
	result, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "second", USBPath: secondPath}}, ExpectedRevision: initial.Revision,
	})
	if err == nil || result.Revision != initial.Revision || result.Changed || result.ReloadIndeterminate {
		t.Fatalf("Install(update) result=%+v err=%v, want previous revision restored", result, err)
	}
	status, inspectErr := manager.Inspect()
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if status.Revision != initial.Revision || status.Entries[0].TargetID != "first" {
		t.Fatalf("restored status = %+v", status)
	}
	assertFixedReloadCalls(t, failingRunner.snapshot(), 2)
}

func TestUninstallRequiresRevisionAndRollsBackReloadFailure(t *testing.T) {
	layout := newTestLayout(t)
	targetPath := layout.addUSBDevice(t, "1-2", "2c7c", "0125", "FIRST")
	manager := layout.manager(t, &fakeRunner{})
	installed, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "first", USBPath: targetPath}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = manager.Uninstall(context.Background(), Request{})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("Uninstall without revision error = %v", err)
	}

	failingRunner := &fakeRunner{failures: map[int]error{0: errors.New("reload failed")}}
	manager.runner = failingRunner
	result, err := manager.Uninstall(context.Background(), Request{ExpectedRevision: installed.Revision})
	if err == nil || result.Revision != installed.Revision || result.Changed || result.ReloadIndeterminate {
		t.Fatalf("Uninstall(failing) result=%+v err=%v, want rollback", result, err)
	}
	status, inspectErr := manager.Inspect()
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if status.State != StateManaged || status.Revision != installed.Revision {
		t.Fatalf("status after uninstall rollback = %+v", status)
	}
	assertFixedReloadCalls(t, failingRunner.snapshot(), 2)

	successRunner := &fakeRunner{}
	manager.runner = successRunner
	result, err = manager.Uninstall(context.Background(), Request{ExpectedRevision: installed.Revision})
	if err != nil {
		t.Fatalf("Uninstall(success): %v", err)
	}
	if result.State != StateAbsent || result.Revision != AbsentRevision || !result.Changed || !result.Reloaded {
		t.Fatalf("successful uninstall result = %+v", result)
	}
	assertFixedReloadCalls(t, successRunner.snapshot(), 1)

	unchanged, err := manager.Uninstall(context.Background(), Request{ExpectedRevision: AbsentRevision})
	if err != nil {
		t.Fatalf("Uninstall(absent): %v", err)
	}
	if unchanged.Changed || unchanged.State != StateAbsent {
		t.Fatalf("absent uninstall result = %+v", unchanged)
	}
	assertFixedReloadCalls(t, successRunner.snapshot(), 1)
}

func assertFixedReloadCalls(t *testing.T, calls []runnerCall, want int) {
	t.Helper()
	if len(calls) != want {
		t.Fatalf("runner calls = %+v, want %d", calls, want)
	}
	for _, call := range calls {
		if call.name != "udevadm" || !reflect.DeepEqual(call.args, []string{"control", "--reload-rules"}) {
			t.Fatalf("unsafe runner call = %+v", call)
		}
	}
}
