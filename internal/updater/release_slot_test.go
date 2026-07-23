package updater

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFailedReleaseSlotCleanupAllowsSameVersionRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX executable mode bits")
	}
	paths := testRuntimePaths(t)
	staging := filepath.Join(paths.ReleasesDir(), ".staging-first")
	release := filepath.Join(paths.ReleasesDir(), "v1.7.0")
	writeTestReleaseDir(t, staging, "new")

	reused, err := prepareReleasePromotion(paths, staging, release, "job-first", "", "")
	if err != nil || reused {
		t.Fatalf("prepare first promotion = (%v, %v), want (false, nil)", reused, err)
	}
	if err := installStagedRelease(paths, staging, release); err != nil {
		t.Fatal(err)
	}
	state := TransactionState{Schema: 1, ID: "job-first", InstalledReleasePath: release}
	if err := cleanupInstalledRelease(paths, state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(release); !os.IsNotExist(err) {
		t.Fatalf("failed release slot still exists: %v", err)
	}

	retryStaging := filepath.Join(paths.ReleasesDir(), ".staging-retry")
	writeTestReleaseDir(t, retryStaging, "new")
	if reused, err := prepareReleasePromotion(paths, retryStaging, release, "job-retry", "", ""); err != nil || reused {
		t.Fatalf("prepare retry = (%v, %v), want (false, nil)", reused, err)
	}
}

func TestPromotionReusesMatchingLastGoodRelease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX executable mode bits")
	}
	paths := testRuntimePaths(t)
	release := filepath.Join(paths.ReleasesDir(), "v1.7.0")
	staging := filepath.Join(paths.ReleasesDir(), ".staging-reuse")
	writeTestReleaseDir(t, release, "signed")
	writeTestReleaseDir(t, staging, "signed")

	reused, err := prepareReleasePromotion(paths, staging, release, "job-reuse", filepath.Join(paths.ReleasesDir(), "v1.6.0"), release)
	if err != nil || !reused {
		t.Fatalf("prepare reusable last-good = (%v, %v), want (true, nil)", reused, err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("verified staging directory was not removed: %v", err)
	}
}

func TestReleaseCleanupRefusesReferencedTarget(t *testing.T) {
	paths := testRuntimePaths(t)
	release := filepath.Join(paths.ReleasesDir(), "v1.7.0")
	writeTestReleaseDir(t, release, "new")
	if err := os.WriteFile(filepath.Join(release, releaseTransactionMarker), []byte("job-ref\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.InstallRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "v1.7.0"), paths.CurrentLink()); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := cleanupInstalledRelease(paths, TransactionState{Schema: 1, ID: "job-ref", InstalledReleasePath: release})
	if err == nil {
		t.Fatal("cleanup removed a release still referenced by current")
	}
	if _, statErr := os.Stat(release); statErr != nil {
		t.Fatalf("referenced release was removed: %v", statErr)
	}
}

func testRuntimePaths(t *testing.T) RuntimePaths {
	t.Helper()
	root := t.TempDir()
	paths := RuntimePaths{
		DeploymentFile: filepath.Join(root, "etc", "deployment.json"),
		InstallRoot:    filepath.Join(root, "opt", "vohive"),
		ConfigFile:     filepath.Join(root, "etc", "config.yaml"),
		DataDir:        filepath.Join(root, "var", "data"),
		StateRoot:      filepath.Join(root, "var", "update"),
	}
	if err := os.MkdirAll(paths.ReleasesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeTestReleaseDir(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vohive", "vohivectl"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(content+name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}
