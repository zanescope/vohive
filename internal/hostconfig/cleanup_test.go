package hostconfig

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	mmisolation "github.com/zanescope/vohive/internal/hostconfig/modemmanager"
)

func TestCleanupManagedForUninstallAbsentIsNoOp(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	runner := &recordingRunner{}
	result, err := CleanupManagedForUninstall(
		context.Background(), layout.manager(t, runner),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != mmisolation.StateAbsent ||
		result.Revision != mmisolation.AbsentRevision || result.Changed || result.Reloaded {
		t.Fatalf("absent cleanup result = %+v", result)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("absent cleanup executed commands: %+v", calls)
	}
}

func TestCleanupManagedForUninstallRemovesOnlyCanonicalManagedRule(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	target := layout.addUSBDevice(t, "1-2", "2c7c", "CLEANUP-1")
	layout.writeManagedRule(t, []mmisolation.Entry{{
		TargetID: target.ID,
		Matcher: mmisolation.Matcher{
			Kind: mmisolation.MatcherSerial, VendorID: "2c7c", Serial: "CLEANUP-1",
		},
	}})
	manualPath := filepath.Join(filepath.Dir(layout.rulePath), "60-administrator.rules")
	manualData := []byte("# administrator-owned rule\n")
	if err := os.WriteFile(manualPath, manualData, 0o644); err != nil {
		t.Fatal(err)
	}

	before, err := layout.manager(t, nil).Inspect()
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	result, err := CleanupManagedForUninstall(
		context.Background(), layout.manager(t, runner),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != mmisolation.StateAbsent ||
		result.Revision != mmisolation.AbsentRevision || !result.Changed || !result.Reloaded ||
		before.Revision == "" || before.Revision == mmisolation.AbsentRevision {
		t.Fatalf("managed cleanup result = %+v, before = %+v", result, before)
	}
	if _, err := os.Lstat(layout.rulePath); !os.IsNotExist(err) {
		t.Fatalf("managed rule still exists: %v", err)
	}
	afterManual, err := os.ReadFile(manualPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterManual, manualData) {
		t.Fatalf("manual rule changed: %q", afterManual)
	}
	assertOnlyReloadCommands(t, runner.snapshot(), 1)
}

func TestCleanupManagedForUninstallPreservesUnownedOrModifiedRule(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, hostConfigTestLayout)
		want  error
	}{
		{
			name: "foreign",
			setup: func(t *testing.T, layout hostConfigTestLayout) {
				if err := os.WriteFile(
					layout.rulePath, []byte("# administrator-owned content\n"), 0o644,
				); err != nil {
					t.Fatal(err)
				}
			},
			want: mmisolation.ErrForeignRule,
		},
		{
			name: "tampered",
			setup: func(t *testing.T, layout hostConfigTestLayout) {
				layout.writeManagedRule(t, []mmisolation.Entry{{
					TargetID: "device-a",
					Matcher: mmisolation.Matcher{
						Kind: mmisolation.MatcherSerial, VendorID: "2c7c", Serial: "TAMPERED-1",
					},
				}})
				file, err := os.OpenFile(layout.rulePath, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("# modified after install\n"); err != nil {
					file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
			want: mmisolation.ErrTamperedRule,
		},
		{
			name: "symlink",
			setup: func(t *testing.T, layout hostConfigTestLayout) {
				target := layout.rulePath + ".administrator"
				if err := os.WriteFile(target, []byte("# administrator-owned target\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, layout.rulePath); err != nil {
					t.Skipf("symlink creation is unavailable: %v", err)
				}
			},
			want: mmisolation.ErrTamperedRule,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := newHostConfigTestLayout(t)
			test.setup(t, layout)
			beforeInfo, err := os.Lstat(layout.rulePath)
			if err != nil {
				t.Fatal(err)
			}
			var beforeData []byte
			if beforeInfo.Mode().IsRegular() {
				beforeData, err = os.ReadFile(layout.rulePath)
				if err != nil {
					t.Fatal(err)
				}
			}
			runner := &recordingRunner{}
			_, err = CleanupManagedForUninstall(
				context.Background(), layout.manager(t, runner),
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("cleanup error = %v, want %v", err, test.want)
			}
			afterInfo, statErr := os.Lstat(layout.rulePath)
			if statErr != nil {
				t.Fatalf("preserved path: %v", statErr)
			}
			if afterInfo.Mode() != beforeInfo.Mode() {
				t.Fatalf("preserved path mode = %v, want %v", afterInfo.Mode(), beforeInfo.Mode())
			}
			if beforeInfo.Mode().IsRegular() {
				afterData, readErr := os.ReadFile(layout.rulePath)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !bytes.Equal(afterData, beforeData) {
					t.Fatal("refused rule content changed")
				}
			}
			if calls := runner.snapshot(); len(calls) != 0 {
				t.Fatalf("refused cleanup executed commands: %+v", calls)
			}
		})
	}
}

type failOnceCleanupRunner struct {
	mu    sync.Mutex
	calls []recordedCommand
}

func (r *failOnceCleanupRunner) Run(
	_ context.Context,
	name string,
	args ...string,
) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := len(r.calls)
	r.calls = append(r.calls, recordedCommand{name: name, args: append([]string(nil), args...)})
	if index == 0 {
		return []byte("synthetic reload failure"), errors.New("reload failed")
	}
	return nil, nil
}

func (r *failOnceCleanupRunner) snapshot() []recordedCommand {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedCommand(nil), r.calls...)
}

func TestCleanupManagedForUninstallRollsBackReloadFailure(t *testing.T) {
	layout := newHostConfigTestLayout(t)
	layout.writeManagedRule(t, []mmisolation.Entry{{
		TargetID: "device-a",
		Matcher: mmisolation.Matcher{
			Kind: mmisolation.MatcherSerial, VendorID: "2c7c", Serial: "ROLLBACK-1",
		},
	}})
	before, err := os.ReadFile(layout.rulePath)
	if err != nil {
		t.Fatal(err)
	}
	runner := &failOnceCleanupRunner{}
	result, err := CleanupManagedForUninstall(
		context.Background(), layout.manager(t, runner),
	)
	if err == nil || result.Changed {
		t.Fatalf("cleanup result = %+v, error = %v; want rolled-back failure", result, err)
	}
	after, readErr := os.ReadFile(layout.rulePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("reload failure did not restore the exact managed rule")
	}
	assertOnlyReloadCommands(t, runner.snapshot(), 2)
}
