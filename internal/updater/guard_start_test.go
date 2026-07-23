package updater

import (
	"errors"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestGuardStartFailClosedAcrossTransactionStates(t *testing.T) {
	paths := testRuntimePaths(t)
	if err := os.MkdirAll(paths.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	state := TransactionState{Schema: 1, ID: "guard-job", Operation: "update", StartedAt: now, UpdatedAt: now}

	state.Phase = PhaseManualRecovery
	if err := atomicWriteJSON(paths.StateFile(), state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := guardStartPaths(paths); !errors.Is(err, ErrManualRecoveryRequired) {
		t.Fatalf("manual guard error = %v", err)
	}

	state.Phase = PhaseStarting
	if err := atomicWriteJSON(paths.StateFile(), state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := guardStartPaths(paths); !errors.Is(err, ErrInterruptedUpdate) {
		t.Fatalf("orphan transaction guard error = %v", err)
	}

	if runtime.GOOS != "linux" {
		t.Skip("live updater process verification is Linux-only")
	}
	lock, err := AcquireUpdateLock(paths.LockFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := guardStartPaths(paths); err != nil {
		t.Fatalf("live updater was blocked: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	state.Phase = PhaseCompleted
	if err := atomicWriteJSON(paths.StateFile(), state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := guardStartPaths(paths); err != nil {
		t.Fatalf("terminal transaction was blocked: %v", err)
	}
}

func TestGuardStartRejectsCorruptState(t *testing.T) {
	paths := testRuntimePaths(t)
	if err := os.MkdirAll(paths.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.StateFile(), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := guardStartPaths(paths); err == nil {
		t.Fatal("corrupt update state did not block service start")
	}
}
