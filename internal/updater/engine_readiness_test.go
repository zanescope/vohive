package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareReadinessUsesConfigAndRotatesKey(t *testing.T) {
	t.Setenv("VOHIVE_SERVER_PORT", "")
	t.Setenv("PROXY_SERVER_PORT", "")

	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("server:\n  port: 9234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := RuntimePaths{StateRoot: filepath.Join(root, "state")}
	deployment := DefaultDeployment()
	deployment.ConfigPath = configPath

	expectation, err := (&Engine{}).prepareReadiness(paths, deployment, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if expectation.Endpoint != "http://127.0.0.1:9234/readyz" {
		t.Fatalf("Endpoint = %q", expectation.Endpoint)
	}
	if expectation.ExpectedVersion != "v1.2.3" {
		t.Fatalf("ExpectedVersion = %q", expectation.ExpectedVersion)
	}
	if expectation.KeyFile != paths.ReadinessKeyFile() {
		t.Fatalf("KeyFile = %q", expectation.KeyFile)
	}
	if _, err := loadReadinessKey(expectation.KeyFile); err != nil {
		t.Fatalf("loadReadinessKey() error = %v", err)
	}
}
