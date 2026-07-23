package updater

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestControlBinaryRestoresFromThisTransactionBackup(t *testing.T) {
	root := t.TempDir()
	paths := RuntimePaths{
		DeploymentFile: filepath.Join(root, "etc", "deployment.json"),
		InstallRoot:    filepath.Join(root, "opt", "vohive"),
		ConfigFile:     filepath.Join(root, "etc", "config.yaml"),
		DataDir:        filepath.Join(root, "var", "data"),
		StateRoot:      filepath.Join(root, "var", "update"),
	}
	if err := os.MkdirAll(paths.ControlDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(paths.ControlDir(), "vohivectl")
	if err := os.WriteFile(control, []byte("transaction-old"), 0o755); err != nil {
		t.Fatal(err)
	}
	backup, err := createBackup(paths, "v1.5.0", time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.ControlDir(), "vohivectl.previous"), []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := restoreControlFromBackup(paths, backup); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(control)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "transaction-old" {
		t.Fatalf("control = %q, want exact transaction backup", data)
	}
	if _, err := os.Stat(filepath.Join(paths.ControlDir(), "vohivectl.previous")); !os.IsNotExist(err) {
		t.Fatalf("stale previous control binary remains: %v", err)
	}
}
