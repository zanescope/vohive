//go:build !windows

package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReadinessKeyRejectsGroupReadableFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "readiness.key")
	if err := RotateReadinessKey(keyFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyFile, 0o640); err != nil {
		t.Fatal(err)
	}

	_, err := loadReadinessKey(keyFile)
	if err == nil || !strings.Contains(err.Error(), "permissions allow group or other access") {
		t.Fatalf("loadReadinessKey() error = %v, want insecure-permissions rejection", err)
	}
}
