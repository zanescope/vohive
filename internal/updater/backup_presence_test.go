package updater

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestRestoreRemovesFilesThatWereAbsentFromBackup(t *testing.T) {
	paths := backupPresencePaths(t)
	backup, err := createBackup(paths, "v0.0.0-uninitialized", time.Unix(3, 0))
	if err != nil {
		t.Fatal(err)
	}

	deployment := DefaultDeployment()
	deployment.InstallRoot = paths.InstallRoot
	deployment.ConfigPath = paths.ConfigFile
	deployment.DataPath = paths.DataDir
	deployment.StateRoot = paths.StateRoot
	deployment.CurrentVersion = "v1.6.0"
	if err := SaveDeployment(paths.DeploymentFile, deployment); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ControlDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(paths.ControlDir(), "vohivectl")
	if err := os.WriteFile(control, []byte("new-control"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := restoreDeploymentFromBackup(paths, backup); err != nil {
		t.Fatal(err)
	}
	if err := restoreControlFromBackup(paths, backup); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.DeploymentFile, control} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists after presence-aware restore: %v", path, err)
		}
	}
}

func TestRestoreOptionalPointerCanRemoveANewCurrentPointer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privilege is not guaranteed")
	}
	paths := backupPresencePaths(t)
	release := filepath.Join(paths.ReleasesDir(), "v1.6.0")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "vohive"), []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := switchVersionPointer(paths, paths.CurrentLink(), release); err != nil {
		t.Fatal(err)
	}
	if err := restoreOptionalPointer(paths, paths.CurrentLink(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.CurrentLink()); !os.IsNotExist(err) {
		t.Fatalf("new current pointer still exists: %v", err)
	}
}

func backupPresencePaths(t *testing.T) RuntimePaths {
	t.Helper()
	root := t.TempDir()
	return RuntimePaths{
		DeploymentFile: filepath.Join(root, "etc", "deployment.json"),
		InstallRoot:    filepath.Join(root, "opt", "vohive"),
		ConfigFile:     filepath.Join(root, "etc", "config.yaml"),
		DataDir:        filepath.Join(root, "var", "data"),
		StateRoot:      filepath.Join(root, "var", "update"),
	}
}
