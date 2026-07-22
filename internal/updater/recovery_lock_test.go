package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type terminalRecoveryService struct {
	active      bool
	activeCalls int
}

func (s *terminalRecoveryService) Stop(context.Context) error  { return nil }
func (s *terminalRecoveryService) Start(context.Context) error { return nil }
func (s *terminalRecoveryService) Active(context.Context) (bool, error) {
	s.activeCalls++
	return s.active, nil
}

func TestRecoverAuditsOrphanLockBeforeReturningTerminalState(t *testing.T) {
	paths, state := writeTerminalRecoveryFixture(t)
	service := &terminalRecoveryService{}

	inactive, terminal, err := prepareRecoveryState(context.Background(), paths, service, true, state)
	if err != nil {
		t.Fatal(err)
	}
	if !inactive || !terminal {
		t.Fatalf("inactive, terminal = %v, %v; want true, true", inactive, terminal)
	}
	if service.activeCalls != 1 {
		t.Fatalf("Active calls = %d, want 1", service.activeCalls)
	}
	if _, err := os.Stat(paths.LockFile()); !os.IsNotExist(err) {
		t.Fatalf("orphan lock still exists: %v", err)
	}
}

func TestNormalRecoverDoesNotRemoveTerminalTransactionLock(t *testing.T) {
	paths, state := writeTerminalRecoveryFixture(t)
	service := &terminalRecoveryService{}

	_, _, err := prepareRecoveryState(context.Background(), paths, service, false, state)
	if !errors.Is(err, ErrUpdateLocked) {
		t.Fatalf("Recover error = %v, want %v", err, ErrUpdateLocked)
	}
	if service.activeCalls != 0 {
		t.Fatalf("Active calls = %d, want 0", service.activeCalls)
	}
	if _, err := os.Stat(paths.LockFile()); err != nil {
		t.Fatalf("transaction lock was removed: %v", err)
	}
}

func TestBootRecoverKeepsTerminalLockWhileServiceIsActive(t *testing.T) {
	paths, state := writeTerminalRecoveryFixture(t)
	service := &terminalRecoveryService{active: true}

	if _, _, err := prepareRecoveryState(context.Background(), paths, service, true, state); err == nil {
		t.Fatal("Recover succeeded while service was active")
	}
	if _, err := os.Stat(paths.LockFile()); err != nil {
		t.Fatalf("transaction lock was removed: %v", err)
	}
}

func writeTerminalRecoveryFixture(t *testing.T) (RuntimePaths, TransactionState) {
	t.Helper()
	root := t.TempDir()
	paths := RuntimePaths{
		DeploymentFile: filepath.Join(root, "etc", "deployment.json"),
		InstallRoot:    filepath.Join(root, "opt", "vohive"),
		ConfigFile:     filepath.Join(root, "etc", "config.yaml"),
		DataDir:        filepath.Join(root, "var", "data"),
		StateRoot:      filepath.Join(root, "var", "update"),
	}
	if err := os.MkdirAll(paths.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	state := TransactionState{
		Schema: 1, ID: "completed-install", Operation: "install", Phase: PhaseCompleted,
		CurrentVersion: "v1.6.0", TargetVersion: "v1.6.0", StartedAt: now, UpdatedAt: now,
	}
	if err := os.WriteFile(paths.LockFile(), []byte("pid=123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return paths, state
}
