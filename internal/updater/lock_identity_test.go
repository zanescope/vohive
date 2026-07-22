package updater

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

func TestUpdateLockIdentityRejectsPIDReuseAcrossBoots(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process identity uses procfs")
	}
	paths := testRuntimePaths(t)
	if err := os.MkdirAll(paths.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	bootID, startTicks, err := currentUpdateProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	oldBootLock := fmt.Sprintf(
		"pid=%d\nstarted=2020-01-01T00:00:00Z\nboot_id=00000000-0000-0000-0000-000000000000\nprocess_start_ticks=%s\n",
		os.Getpid(), startTicks,
	)
	if err := os.WriteFile(paths.LockFile(), []byte(oldBootLock), 0o600); err != nil {
		t.Fatal(err)
	}
	if active, err := updateLockProcessActive(paths.LockFile()); err != nil || active {
		t.Fatalf("old-boot lock active=%v err=%v", active, err)
	}

	currentLock := fmt.Sprintf(
		"pid=%d\nstarted=2020-01-01T00:00:00Z\nboot_id=%s\nprocess_start_ticks=%s\n",
		os.Getpid(), bootID, startTicks,
	)
	if err := os.WriteFile(paths.LockFile(), []byte(currentLock), 0o600); err != nil {
		t.Fatal(err)
	}
	if active, err := updateLockProcessActive(paths.LockFile()); err != nil || !active {
		t.Fatalf("matching lock active=%v err=%v", active, err)
	}
}

func TestAcquireUpdateLockRecordsLinuxProcessIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process identity uses procfs")
	}
	paths := testRuntimePaths(t)
	lock, err := AcquireUpdateLock(paths.LockFile())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if active, err := updateLockProcessActive(paths.LockFile()); err != nil || !active {
		t.Fatalf("new lock active=%v err=%v", active, err)
	}
}
