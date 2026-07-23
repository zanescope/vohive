package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type manualGuardResolver struct {
	called bool
}

func (r *manualGuardResolver) Check(context.Context, CheckRequest) (Candidate, error) {
	r.called = true
	return Candidate{HasUpdate: true, CurrentVer: "v1.6.0", LatestVer: "v1.7.0"}, nil
}

type manualGuardService struct{}

func (manualGuardService) Stop(context.Context) error           { return nil }
func (manualGuardService) Start(context.Context) error          { return nil }
func (manualGuardService) Active(context.Context) (bool, error) { return false, nil }

type manualGuardReady struct{}

func (manualGuardReady) Ready(context.Context, string) error { return nil }

type manualGuardLauncher struct {
	called bool
}

func (l *manualGuardLauncher) Launch(context.Context, InstallType) error {
	l.called = true
	return nil
}

func TestEngineDoesNotOverwriteManualRecoveryState(t *testing.T) {
	deploymentFile, paths := writeManualRecoveryFixture(t)
	resolver := &manualGuardResolver{}
	engine := &Engine{
		DeploymentFile: deploymentFile,
		Resolver:       resolver,
		Service:        manualGuardService{},
		Ready:          manualGuardReady{},
		validateScope:  func(RuntimePaths) error { return nil },
	}

	_, err := engine.Update(context.Background(), UpdateRequest{Schema: 1, Channel: ChannelStable, Version: "v1.7.0"})
	if !errors.Is(err, ErrManualRecoveryRequired) {
		t.Fatalf("Update error = %v, want %v", err, ErrManualRecoveryRequired)
	}
	if resolver.called {
		t.Fatal("resolver was called after manual recovery became required")
	}
	assertManualRecoveryStatePreserved(t, paths)
}

func TestCoordinatorDoesNotOverwriteManualRecoveryState(t *testing.T) {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		t.Skip("container capability policy disables native host updates")
	}
	deploymentFile, paths := writeManualRecoveryFixture(t)
	resolver := &manualGuardResolver{}
	launcher := &manualGuardLauncher{}
	coordinator := &LocalCoordinator{
		DeploymentFile: deploymentFile,
		Resolver:       resolver,
		Launcher:       launcher,
		Now:            time.Now,
		validateScope:  func(RuntimePaths) error { return nil },
	}

	_, err := coordinator.Start(context.Background(), UpdateRequest{Schema: 1, Channel: ChannelStable, Version: "v1.7.0"})
	if !errors.Is(err, ErrManualRecoveryRequired) {
		t.Fatalf("Start error = %v, want %v", err, ErrManualRecoveryRequired)
	}
	if !resolver.called {
		t.Fatal("expected exact target revalidation before acquiring the transaction lock")
	}
	if launcher.called {
		t.Fatal("update worker launched while manual recovery was required")
	}
	assertManualRecoveryStatePreserved(t, paths)
}

func writeManualRecoveryFixture(t *testing.T) (string, RuntimePaths) {
	t.Helper()
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
	now := time.Now().UTC()
	state := TransactionState{
		Schema: 1, ID: "manual-recovery", Operation: "update", Phase: PhaseManualRecovery,
		CurrentVersion: "v1.6.0", TargetVersion: "v1.7.0", BackupPath: filepath.Join(paths.BackupsDir(), "preserve-me"),
		StartedAt: now, UpdatedAt: now,
	}
	if err := atomicWriteJSON(paths.StateFile(), state, 0o600); err != nil {
		t.Fatal(err)
	}
	return deploymentFile, paths
}

func assertManualRecoveryStatePreserved(t *testing.T, paths RuntimePaths) {
	t.Helper()
	state, err := LoadState(paths.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != PhaseManualRecovery || state.BackupPath != filepath.Join(paths.BackupsDir(), "preserve-me") {
		t.Fatalf("manual recovery state was overwritten: %#v", state)
	}
	if _, err := os.Stat(paths.LockFile()); !os.IsNotExist(err) {
		t.Fatalf("transaction lock was not released: %v", err)
	}
}
