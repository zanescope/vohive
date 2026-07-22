package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBootRecoveryClearsNoStateOrphanLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("boot recovery process checks are Linux-only")
	}
	root := t.TempDir()
	deploymentFile := filepath.Join(root, "etc", "deployment.json")
	deployment := DefaultDeployment()
	deployment.InstallRoot = filepath.Join(root, "opt", "vohive")
	deployment.ConfigPath = filepath.Join(root, "etc", "config.yaml")
	deployment.DataPath = filepath.Join(root, "var", "data")
	deployment.StateRoot = filepath.Join(root, "var", "update")
	deployment.CurrentVersion = "v1.6.0"
	if err := SaveDeployment(deploymentFile, deployment); err != nil {
		t.Fatal(err)
	}
	paths := PathsFor(deploymentFile, deployment)
	if err := os.MkdirAll(paths.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.LockFile(), []byte("pid=2147483647\nstarted=2020-01-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{
		DeploymentFile: deploymentFile,
		Service:        manualGuardService{},
		BootRecovery:   true,
		validateScope:  func(RuntimePaths) error { return nil },
	}
	if _, err := engine.Recover(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.LockFile()); !os.IsNotExist(err) {
		t.Fatalf("orphan lock was not removed: %v", err)
	}
}

func TestBootRecoveryRefusesNoStateLiveWorkerLock(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("boot recovery process checks are Linux-only")
	}
	paths := testRuntimePaths(t)
	if err := os.MkdirAll(paths.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireUpdateLock(paths.LockFile())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	_, _, err = prepareRecoveryState(context.Background(), paths, manualGuardService{}, true, TransactionState{Phase: PhaseFailed})
	if !errors.Is(err, ErrUpdateLocked) {
		t.Fatalf("live worker error = %v, want ErrUpdateLocked", err)
	}
}
