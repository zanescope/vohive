package updater

import (
	"errors"
	"os"
	"testing"
)

func TestGuardStartRejectsLockWithoutState(t *testing.T) {
	paths := testRuntimePaths(t)
	if err := os.MkdirAll(paths.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.LockFile(), []byte("pid=2147483647\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := guardStartPaths(paths); !errors.Is(err, ErrInterruptedUpdate) {
		t.Fatalf("guard error = %v, want ErrInterruptedUpdate", err)
	}
}
