package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerUsesManagedDynamicReadinessProbe(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"write_readiness_key()",
		`mv -f "$tmp_key" "$STATE_ROOT/readiness.key"`,
		`probe --expected-version "$VERSION"`,
		`probe --expected-version "$VERSION" --print-url`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("installer readiness flow is missing %q", required)
		}
	}
	if strings.Contains(text, "http://127.0.0.1:7575/readyz >/dev/null") {
		t.Fatal("installer still probes the fixed default port")
	}

	keyWrite := strings.LastIndex(text, "\nwrite_readiness_key\n")
	serviceStart := strings.LastIndex(text, "\nstart_service\n")
	if keyWrite < 0 || serviceStart < 0 || keyWrite > serviceStart {
		t.Fatalf("readiness key must be durable before service start: key=%d start=%d", keyWrite, serviceStart)
	}
}
