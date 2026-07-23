package updater

import (
	"path/filepath"
	"testing"
)

func validTestManifest() ReleaseManifest {
	return ReleaseManifest{
		Schema: 1, Product: "vohive", Repository: Repository,
		Version: "v1.6.0", Channel: ChannelStable,
		SourceRevision:    "0123456789abcdef0123456789abcdef01234567",
		MinUpdaterVersion: "v1.0.0", MinDirectUpgrade: "v1.4.0",
		ConfigSchema:   SchemaRange{Min: 0, Target: 1, Max: 1},
		DatabaseSchema: SchemaRange{Min: 0, Target: 1, Max: 1},
		Artifacts: []Artifact{{
			Name: "vohive_v1.6.0_linux_amd64.tar.gz", GOOS: "linux", GOARCH: "amd64",
			SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Size:   42, Format: "tar.gz",
		}},
	}
}

func TestReleaseManifestValidate(t *testing.T) {
	manifest := validTestManifest()
	if err := manifest.Validate("v1.6.0"); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	tests := []struct {
		name string
		edit func(*ReleaseManifest)
	}{
		{"wrong repository", func(m *ReleaseManifest) { m.Repository = "someone/else" }},
		{"short revision", func(m *ReleaseManifest) { m.SourceRevision = "abc" }},
		{"invalid updater", func(m *ReleaseManifest) { m.MinUpdaterVersion = "dev" }},
		{"reversed config range", func(m *ReleaseManifest) { m.ConfigSchema = SchemaRange{Min: 2, Target: 1, Max: 3} }},
		{"target above max", func(m *ReleaseManifest) { m.DatabaseSchema = SchemaRange{Min: 0, Target: 2, Max: 1} }},
		{"invalid artifact hash", func(m *ReleaseManifest) { m.Artifacts[0].SHA256 = "not-a-hash" }},
		{"unsafe artifact name", func(m *ReleaseManifest) { m.Artifacts[0].Name = "../vohive" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := validTestManifest()
			test.edit(&candidate)
			if err := candidate.Validate("v1.6.0"); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestSaveRequestKeepsSubscriptionChannelWithExactTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	want := UpdateRequest{Schema: 1, JobID: "job-20260722.1", Channel: ChannelStable, Version: "v1.6.0"}
	if err := SaveRequest(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRequest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel != ChannelStable {
		t.Fatalf("exact target changed subscription channel: got %q", got.Channel)
	}
	if got.JobID != want.JobID {
		t.Fatalf("job id did not round-trip: got %q", got.JobID)
	}
}

func TestSaveRequestRejectsUnsafeJobID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	err := SaveRequest(path, UpdateRequest{Schema: 1, JobID: "../../other", Channel: ChannelStable, Version: "v1.6.0"})
	if err == nil {
		t.Fatal("unsafe job id was accepted")
	}
}

func TestValidateProductionScope(t *testing.T) {
	deployment := DefaultDeployment()
	paths := PathsFor(DefaultDeploymentPath, deployment)
	if err := ValidateProductionScope(paths); err != nil {
		t.Fatalf("default deployment scope rejected: %v", err)
	}
	paths.DataDir = filepath.Join(t.TempDir(), "data")
	if err := ValidateProductionScope(paths); err == nil {
		t.Fatal("arbitrary data directory was accepted")
	}
}
