//go:build linux

package modemmanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func prepareManagedUpdate(
	t *testing.T,
) (testLayout, *Manager, Status, string) {
	t.Helper()
	layout := newTestLayout(t)
	firstPath := layout.addUSBDevice(t, "1-2", "2c7c", "0125", "FIRST")
	secondPath := layout.addUSBDevice(t, "1-3", "1199", "9071", "SECOND")
	manager := layout.manager(t, &fakeRunner{})
	first, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "first", USBPath: firstPath}},
	})
	if err != nil {
		t.Fatalf("Install(first): %v", err)
	}
	return layout, manager, Status{
		State: StateManaged, Revision: first.Revision, Entries: first.Entries,
	}, secondPath
}

func updateManagedRule(
	t *testing.T,
	manager *Manager,
	before Status,
	secondPath string,
) (Result, error) {
	t.Helper()
	return manager.Install(context.Background(), Request{
		Targets:          []Target{{ID: "second", USBPath: secondPath}},
		ExpectedRevision: before.Revision,
	})
}

func TestManagedUpdateUsesSingleExchangeWithoutPathGap(t *testing.T) {
	layout, manager, before, secondPath := prepareManagedUpdate(t)
	runner := &fakeRunner{}
	manager.runner = runner
	realExchange := manager.fileOps.exchange
	exchangeCalls := 0
	manager.fileOps.exchange = func(first, second string) error {
		exchangeCalls++
		if _, err := os.Lstat(first); err != nil {
			t.Fatalf("temporary path is missing before exchange: %v", err)
		}
		if _, err := os.Lstat(second); err != nil {
			t.Fatalf("canonical path is missing before exchange: %v", err)
		}
		if err := realExchange(first, second); err != nil {
			return err
		}
		if _, err := os.Lstat(first); err != nil {
			t.Fatalf("backup path is missing after exchange: %v", err)
		}
		if _, err := os.Lstat(second); err != nil {
			t.Fatalf("canonical path is missing after exchange: %v", err)
		}
		return nil
	}

	result, err := updateManagedRule(t, manager, before, secondPath)
	if err != nil {
		t.Fatalf("Install(update): %v", err)
	}
	if exchangeCalls != 1 {
		t.Fatalf("exchange calls = %d, want exactly one", exchangeCalls)
	}
	if !result.Changed || !result.Reloaded || result.Revision == before.Revision {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Lstat(layout.rulePath); err != nil {
		t.Fatalf("canonical path disappeared: %v", err)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 1)
}

func TestManagedUpdateValidationFailureRollsBackByExchange(t *testing.T) {
	_, manager, before, secondPath := prepareManagedUpdate(t)
	runner := &fakeRunner{}
	manager.runner = runner
	realExchange := manager.fileOps.exchange
	exchangeCalls := 0
	manager.fileOps.exchange = func(first, second string) error {
		exchangeCalls++
		return realExchange(first, second)
	}
	manager.fileOps.afterExchange = func() error {
		return errors.New("synthetic post-exchange validation failure")
	}

	result, err := updateManagedRule(t, manager, before, secondPath)
	if err == nil || result.Changed || result.Revision != before.Revision {
		t.Fatalf("Install(update) result=%+v err=%v, want restored previous rule", result, err)
	}
	if exchangeCalls != 2 {
		t.Fatalf("exchange calls = %d, want install plus rollback", exchangeCalls)
	}
	status, inspectErr := manager.Inspect()
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if status.State != StateManaged || status.Revision != before.Revision {
		t.Fatalf("status after validation rollback = %+v", status)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("validation failure reloaded udev before a rule was accepted")
	}
}

func TestManagedUpdateConcurrentModificationFailsClosed(t *testing.T) {
	layout, manager, before, secondPath := prepareManagedUpdate(t)
	runner := &fakeRunner{}
	manager.runner = runner
	realExchange := manager.fileOps.exchange
	exchangeCalls := 0
	manager.fileOps.exchange = func(first, second string) error {
		exchangeCalls++
		return realExchange(first, second)
	}
	concurrent := []byte("# concurrent administrator change\n")
	manager.fileOps.afterExchange = func() error {
		if err := os.WriteFile(layout.rulePath, concurrent, 0o644); err != nil {
			return err
		}
		return errors.New("force validation rollback")
	}

	result, err := updateManagedRule(t, manager, before, secondPath)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("Install(update) error = %v, want ErrRevisionConflict", err)
	}
	if exchangeCalls != 1 {
		t.Fatalf("exchange calls = %d, rollback overwrote a concurrent change", exchangeCalls)
	}
	if !result.Changed || result.State != StateForeign || result.Reloaded {
		t.Fatalf("result = %+v, want visible concurrent state", result)
	}
	data, readErr := os.ReadFile(layout.rulePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != string(concurrent) {
		t.Fatalf("concurrent canonical change was overwritten: %q", data)
	}
	backups, globErr := filepath.Glob(
		filepath.Join(
			filepath.Dir(layout.rulePath),
			"."+filepath.Base(layout.rulePath)+".tmp-*",
		),
	)
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(backups) != 1 {
		t.Fatalf("recovery backups = %v, want expected previous rule retained", backups)
	}
	backupInfo, statErr := os.Lstat(backups[0])
	if statErr != nil {
		t.Fatal(statErr)
	}
	if err := verifyExactRegularFile(
		backups[0], backupInfo, before.Revision, true,
	); err != nil {
		t.Fatalf("retained recovery backup: %v", err)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("concurrent mutation failure reloaded udev")
	}
}

func TestManagedUpdateBackupModificationFailsClosed(t *testing.T) {
	layout, manager, before, secondPath := prepareManagedUpdate(t)
	runner := &fakeRunner{}
	manager.runner = runner
	realExchange := manager.fileOps.exchange
	exchangeCalls := 0
	manager.fileOps.exchange = func(first, second string) error {
		exchangeCalls++
		return realExchange(first, second)
	}
	concurrent := []byte("# concurrent backup change\n")
	manager.fileOps.afterExchange = func() error {
		backups, err := filepath.Glob(filepath.Join(
			filepath.Dir(layout.rulePath),
			"."+filepath.Base(layout.rulePath)+".tmp-*",
		))
		if err != nil {
			return err
		}
		if len(backups) != 1 {
			return fmt.Errorf("recovery backups = %v, want one", backups)
		}
		if err := os.WriteFile(backups[0], concurrent, 0o644); err != nil {
			return err
		}
		return errors.New("force exchange rollback")
	}

	result, err := updateManagedRule(t, manager, before, secondPath)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("Install(update) error = %v, want ErrRevisionConflict", err)
	}
	if exchangeCalls != 1 {
		t.Fatalf("exchange calls = %d, modified backup entered canonical path", exchangeCalls)
	}
	if !result.Changed || result.State != StateManaged || result.Reloaded ||
		result.Revision == before.Revision {
		t.Fatalf("result = %+v, want prepared replacement retained", result)
	}
	status, inspectErr := manager.Inspect()
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if status.State != StateManaged || status.Revision != result.Revision {
		t.Fatalf("canonical status = %+v, result = %+v", status, result)
	}
	canonical, readErr := os.ReadFile(layout.rulePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(canonical) == string(concurrent) {
		t.Fatal("modified backup was exchanged into the canonical rule path")
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("backup mutation failure reloaded udev")
	}
}

func TestManagedUpdateDirectorySyncFailureRollsBackAndReloadsOldRule(t *testing.T) {
	_, manager, before, secondPath := prepareManagedUpdate(t)
	runner := &fakeRunner{}
	manager.runner = runner
	realExchange := manager.fileOps.exchange
	exchangeCalls := 0
	manager.fileOps.exchange = func(first, second string) error {
		exchangeCalls++
		return realExchange(first, second)
	}
	realSync := manager.fileOps.syncDirectory
	syncCalls := 0
	manager.fileOps.syncDirectory = func(path string) error {
		syncCalls++
		if syncCalls == 1 {
			return errors.New("synthetic directory sync failure")
		}
		return realSync(path)
	}

	result, err := updateManagedRule(t, manager, before, secondPath)
	if err == nil || result.Changed || result.Revision != before.Revision {
		t.Fatalf("Install(update) result=%+v err=%v, want restored previous rule", result, err)
	}
	if exchangeCalls != 2 {
		t.Fatalf("exchange calls = %d, want install plus rollback", exchangeCalls)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 1)
}

func TestManagedUpdateReloadFailureUsesExchangeRollback(t *testing.T) {
	_, manager, before, secondPath := prepareManagedUpdate(t)
	runner := &fakeRunner{failures: map[int]error{0: errors.New("reload failed")}}
	manager.runner = runner
	realExchange := manager.fileOps.exchange
	exchangeCalls := 0
	manager.fileOps.exchange = func(first, second string) error {
		exchangeCalls++
		return realExchange(first, second)
	}

	result, err := updateManagedRule(t, manager, before, secondPath)
	if err == nil || result.Changed || result.Revision != before.Revision || result.ReloadIndeterminate {
		t.Fatalf("Install(update) result=%+v err=%v, want restored previous rule", result, err)
	}
	if exchangeCalls != 2 {
		t.Fatalf("exchange calls = %d, want install plus rollback", exchangeCalls)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 2)
}

func TestManagedUpdateDoubleReloadFailureReportsIndeterminate(t *testing.T) {
	layout, manager, before, secondPath := prepareManagedUpdate(t)
	runner := &fakeRunner{failures: map[int]error{
		0: errors.New("initial reload failed"),
		1: errors.New("rollback reload failed"),
	}}
	manager.runner = runner

	result, err := updateManagedRule(t, manager, before, secondPath)
	if err == nil || result.Changed || result.Revision != before.Revision {
		t.Fatalf("Install(update) result=%+v err=%v, want restored previous rule", result, err)
	}
	if !result.ReloadIndeterminate {
		t.Fatalf("result = %+v, want indeterminate udev reload state", result)
	}
	status, inspectErr := manager.Inspect()
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if status.Revision != before.Revision {
		t.Fatalf("disk revision = %s, want restored %s", status.Revision, before.Revision)
	}
	if _, statErr := os.Lstat(layout.rulePath); statErr != nil {
		t.Fatalf("restored rule missing: %v", statErr)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 2)
}

func TestManagedUpdateExchangeUnavailableFailsClosed(t *testing.T) {
	for _, exchangeErr := range []error{syscall.ENOSYS, syscall.EINVAL} {
		t.Run(exchangeErr.Error(), func(t *testing.T) {
			_, manager, before, secondPath := prepareManagedUpdate(t)
			runner := &fakeRunner{}
			manager.runner = runner
			manager.fileOps.exchange = func(string, string) error {
				return exchangeErr
			}

			result, err := updateManagedRule(t, manager, before, secondPath)
			if !errors.Is(err, exchangeErr) {
				t.Fatalf("Install(update) error = %v, want %v", err, exchangeErr)
			}
			if errors.Is(err, ErrCleanupIncomplete) {
				t.Fatalf("pre-commit exchange error was marked as cleanup incomplete: %v", err)
			}
			if result.Changed || result.Revision != before.Revision {
				t.Fatalf("result = %+v, expected canonical rule was changed", result)
			}
			if len(runner.snapshot()) != 0 {
				t.Fatal("unavailable atomic exchange reloaded udev")
			}
		})
	}
}

func TestManagedUpdateCleanupFailureReportsCommittedChange(t *testing.T) {
	for _, test := range []struct {
		name   string
		inject func(*Manager)
	}{
		{
			name: "remove backup",
			inject: func(manager *Manager) {
				manager.fileOps.remove = func(string) error {
					return errors.New("synthetic backup removal failure")
				}
			},
		},
		{
			name: "sync cleanup",
			inject: func(manager *Manager) {
				realSync := manager.fileOps.syncDirectory
				syncCalls := 0
				manager.fileOps.syncDirectory = func(path string) error {
					syncCalls++
					if syncCalls == 2 {
						return errors.New("synthetic cleanup sync failure")
					}
					return realSync(path)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, manager, before, secondPath := prepareManagedUpdate(t)
			runner := &fakeRunner{}
			manager.runner = runner
			test.inject(manager)

			result, err := updateManagedRule(t, manager, before, secondPath)
			if err == nil || !strings.Contains(err.Error(), "backup cleanup failed") {
				t.Fatalf("Install(update) error = %v, want committed cleanup error", err)
			}
			if !errors.Is(err, ErrCleanupIncomplete) {
				t.Fatalf("Install(update) error = %v, want ErrCleanupIncomplete", err)
			}
			if !result.Changed || !result.Reloaded || result.Revision == before.Revision {
				t.Fatalf("result = %+v, committed change was reported as unchanged", result)
			}
			status, inspectErr := manager.Inspect()
			if inspectErr != nil {
				t.Fatal(inspectErr)
			}
			if status.State != StateManaged || status.Revision != result.Revision {
				t.Fatalf("committed status = %+v, result = %+v", status, result)
			}
			assertFixedReloadCalls(t, runner.snapshot(), 1)
		})
	}
}

func atomicallyReplaceTestFile(t *testing.T, path string, data []byte) os.FileInfo {
	t.Helper()
	temp, err := os.CreateTemp(filepath.Dir(path), ".concurrent-*")
	if err != nil {
		t.Fatal(err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		t.Fatal(err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		t.Fatal(err)
	}
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(tempPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		t.Fatal(err)
	}
	return info
}

func singleTransactionFile(t *testing.T, rulePath, suffix string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(
		filepath.Dir(rulePath), "."+filepath.Base(rulePath)+suffix,
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("transaction files = %v, want one", paths)
	}
	return paths[0]
}

func TestManagedUpdatePreExchangeReplacementRestoresCapturedFile(t *testing.T) {
	layout, manager, before, secondPath := prepareManagedUpdate(t)
	runner := &fakeRunner{}
	manager.runner = runner
	realExchange := manager.fileOps.exchange
	exchangeCalls := 0
	concurrent := []byte("# concurrent administrator replacement\n")
	var concurrentInfo os.FileInfo
	manager.fileOps.exchange = func(first, second string) error {
		if exchangeCalls == 0 {
			concurrentInfo = atomicallyReplaceTestFile(t, second, concurrent)
		}
		exchangeCalls++
		return realExchange(first, second)
	}

	result, err := updateManagedRule(t, manager, before, secondPath)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("Install(update) error = %v, want ErrRevisionConflict", err)
	}
	if exchangeCalls != 2 {
		t.Fatalf("exchange calls = %d, want capture plus safe rollback", exchangeCalls)
	}
	if !result.Changed || result.State != StateForeign || result.Reloaded {
		t.Fatalf("result = %+v, want restored concurrent state", result)
	}
	data, readErr := os.ReadFile(layout.rulePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != string(concurrent) {
		t.Fatalf("restored concurrent content = %q", data)
	}
	canonicalInfo, statErr := os.Lstat(layout.rulePath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if concurrentInfo == nil || !os.SameFile(concurrentInfo, canonicalInfo) {
		t.Fatal("rollback did not restore the exact file captured by the exchange")
	}
	if paths, globErr := filepath.Glob(filepath.Join(
		filepath.Dir(layout.rulePath), "."+filepath.Base(layout.rulePath)+".tmp-*",
	)); globErr != nil || len(paths) != 0 {
		t.Fatalf("prepared replacement was not cleaned up: paths=%v err=%v", paths, globErr)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("pre-exchange conflict reloaded udev")
	}
}

func TestUninstallModifiedStagedRuleFailsClosed(t *testing.T) {
	layout := newTestLayout(t)
	targetPath := layout.addUSBDevice(t, "1-2", "2c7c", "0125", "FIRST")
	manager := layout.manager(t, &fakeRunner{})
	installed, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "first", USBPath: targetPath}},
	})
	if err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("# modified staged uninstall rule\n")
	var stagedPath string
	runner := &fakeRunner{
		failures: map[int]error{0: errors.New("reload failed")},
		hooks: map[int]func(){0: func() {
			stagedPath = singleTransactionFile(t, layout.rulePath, ".removed-*")
			atomicallyReplaceTestFile(t, stagedPath, concurrent)
		}},
	}
	manager.runner = runner

	result, err := manager.Uninstall(context.Background(), Request{ExpectedRevision: installed.Revision})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("Uninstall() error = %v, want ErrRevisionConflict", err)
	}
	if result.State != StateAbsent || !result.Changed || result.Reloaded {
		t.Fatalf("result = %+v, want absent fail-closed state", result)
	}
	if _, statErr := os.Lstat(layout.rulePath); !os.IsNotExist(statErr) {
		t.Fatalf("modified staged rule entered canonical path: %v", statErr)
	}
	data, readErr := os.ReadFile(stagedPath)
	if readErr != nil || string(data) != string(concurrent) {
		t.Fatalf("staged evidence = %q, err=%v", data, readErr)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 1)
}

func TestCreateRollbackModifiedStagedRuleFailsClosed(t *testing.T) {
	layout := newTestLayout(t)
	targetPath := layout.addUSBDevice(t, "1-2", "2c7c", "0125", "FIRST")
	concurrent := []byte("# modified staged create rollback\n")
	var stagedPath string
	runner := &fakeRunner{
		failures: map[int]error{
			0: errors.New("initial reload failed"),
			1: errors.New("rollback reload failed"),
		},
		hooks: map[int]func(){1: func() {
			stagedPath = singleTransactionFile(t, layout.rulePath, ".removed-*")
			atomicallyReplaceTestFile(t, stagedPath, concurrent)
		}},
	}
	manager := layout.manager(t, runner)

	result, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "first", USBPath: targetPath}},
	})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("Install() error = %v, want ErrRevisionConflict", err)
	}
	if result.State != StateAbsent || result.Changed || result.Reloaded || !result.ReloadIndeterminate {
		t.Fatalf("result = %+v, want absent fail-closed state", result)
	}
	if _, statErr := os.Lstat(layout.rulePath); !os.IsNotExist(statErr) {
		t.Fatalf("modified staged rule entered canonical path: %v", statErr)
	}
	data, readErr := os.ReadFile(stagedPath)
	if readErr != nil || string(data) != string(concurrent) {
		t.Fatalf("staged evidence = %q, err=%v", data, readErr)
	}
	assertFixedReloadCalls(t, runner.snapshot(), 2)
}
