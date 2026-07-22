package updater

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstallRecoveryRestoresOriginallyAbsentManagedState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privilege is not guaranteed")
	}
	root := t.TempDir()
	paths := RuntimePaths{
		DeploymentFile: filepath.Join(root, "etc", "deployment.json"),
		InstallRoot:    filepath.Join(root, "opt", "vohive"),
		ConfigFile:     filepath.Join(root, "etc", "config.yaml"),
		DataDir:        filepath.Join(root, "var", "data"),
		StateRoot:      filepath.Join(root, "var", "update"),
	}
	backup := filepath.Join(paths.BackupsDir(), "install-backup")
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := BackupMetadata{
		Schema: 1, ConfigPath: paths.ConfigFile, DataPath: paths.DataDir,
		ConfigPresent: false, DataPresent: false,
		DeploymentPresent: false, ControlPresent: false,
	}
	if err := atomicWriteJSON(filepath.Join(backup, "metadata.json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}

	release := filepath.Join(paths.ReleasesDir(), "v2.0.0")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "vohive"), []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "v2.0.0"), paths.CurrentLink()); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		paths.ConfigFile:     "new config",
		paths.DeploymentFile: "new deployment",
		filepath.Join(paths.ControlDir(), "vohivectl"): "new control",
		filepath.Join(paths.DataDir, "vohive.db"):      "new data",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	engine := &Engine{installerRecovery: func(context.Context, RuntimePaths, string) error { return nil }}
	state := TransactionState{Operation: "install", BackupPath: backup, ControlTouched: true}
	if err := engine.restoreOriginalState(paths, state, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		paths.CurrentLink(), paths.LastGoodLink(), paths.ConfigFile,
		paths.DeploymentFile, filepath.Join(paths.ControlDir(), "vohivectl"),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("managed path %s was not restored to absence: %v", path, err)
		}
	}
	entries, err := os.ReadDir(paths.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("restored data directory contains %d entries", len(entries))
	}
}
