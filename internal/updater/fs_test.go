package updater

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDetectLayoutAt(t *testing.T) {
	root := t.TempDir()
	if got, err := DetectLayoutAt(root); err != nil || got != LayoutV2 {
		t.Fatalf("empty root: got %q, err %v", got, err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "vohive"), []byte("legacy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := DetectLayoutAt(root); err != nil || got != LayoutV1 {
		t.Fatalf("legacy root: got %q, err %v", got, err)
	}
}

func TestUpdateLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update.lock")
	first, err := AcquireUpdateLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireUpdateLock(path); err != ErrUpdateLocked {
		t.Fatalf("second lock: got %v, want %v", err, ErrUpdateLocked)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireUpdateLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	paths := RuntimePaths{
		DeploymentFile: filepath.Join(root, "etc", "deployment.json"),
		InstallRoot:    filepath.Join(root, "opt", "vohive"),
		ConfigFile:     filepath.Join(root, "etc", "config.yaml"),
		DataDir:        filepath.Join(root, "var", "data"),
		StateRoot:      filepath.Join(root, "var", "update"),
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("old-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.DataDir, "db"), []byte("old-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	backup, err := createBackup(paths, "v1.5.0", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("new-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.DataDir, "db"), []byte("new-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreBackup(paths, backup); err != nil {
		t.Fatal(err)
	}
	config, _ := os.ReadFile(paths.ConfigFile)
	data, _ := os.ReadFile(filepath.Join(paths.DataDir, "db"))
	if string(config) != "old-config" || string(data) != "old-data" {
		t.Fatalf("restore mismatch: config=%q data=%q", config, data)
	}
}
