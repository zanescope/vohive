package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readPackagingScript(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}
