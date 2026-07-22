package updater

import "testing"

func TestReleaseVersionsRejectBuildMetadata(t *testing.T) {
	for _, version := range []string{"v1.6.0+local", "1.6.0-beta.1+build.7"} {
		if validVersion(version) {
			t.Fatalf("version with build metadata was accepted: %s", version)
		}
	}
	for _, version := range []string{"v1.6.0", "v1.6.0-beta.1"} {
		if !validVersion(version) {
			t.Fatalf("release version was rejected: %s", version)
		}
	}
}
