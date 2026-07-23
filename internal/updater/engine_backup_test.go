package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type backupTestService struct {
	startErr   error
	stopCalls  int
	startCalls int
}

func (s *backupTestService) Stop(context.Context) error {
	s.stopCalls++
	return nil
}

func (s *backupTestService) Start(context.Context) error {
	s.startCalls++
	return s.startErr
}

func (*backupTestService) Active(context.Context) (bool, error) {
	return false, nil
}

func TestBackupReportsCopyAndRestartFailures(t *testing.T) {
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
	if err := os.MkdirAll(deployment.ConfigPath, 0o700); err != nil {
		t.Fatal(err)
	}

	restartErr := errors.New("service restart failed")
	service := &backupTestService{startErr: restartErr}
	engine := &Engine{
		DeploymentFile: deploymentFile,
		Service:        service,
		Now:            func() time.Time { return time.Unix(6, 0) },
		validateScope:  func(RuntimePaths) error { return nil },
	}

	backup, err := engine.Backup(context.Background())
	if err == nil {
		t.Fatal("Backup succeeded despite copy and restart failures")
	}
	if backup != "" {
		t.Fatalf("Backup path = %q, want empty path for failed snapshot", backup)
	}
	if !errors.Is(err, restartErr) {
		t.Fatalf("Backup error %q does not retain the restart failure", err)
	}
	if service.stopCalls != 1 || service.startCalls != 1 {
		t.Fatalf("service calls: stop=%d start=%d, want one each", service.stopCalls, service.startCalls)
	}
	if !strings.Contains(err.Error(), "config is not a regular file") {
		t.Fatalf("Backup error %q does not retain the copy failure", err)
	}
	paths := PathsFor(deploymentFile, deployment)
	entries, readErr := os.ReadDir(paths.BackupsDir())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed backup left published or staging entries: %v", entries)
	}
}
