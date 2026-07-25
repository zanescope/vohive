package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zanescope/vohive/internal/updater"
)

func TestGenerateVerifyAndUpdaterContract(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	policyPath := writeTestPolicy(t, filepath.Join(dir, "policy"))
	assetsDir := filepath.Join(dir, "assets")
	writeTestArchives(t, assetsDir, "v1.6.0")
	if err := os.WriteFile(filepath.Join(assetsDir, "vohive-install.sh"), []byte("installer"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "release-manifest.json")
	if err := run([]string{
		"generate", "--policy", policyPath, "--version", "v1.6.0", "--channel", "stable",
		"--revision", strings.Repeat("a", 40), "--assets-dir", assetsDir, "--output", manifestPath,
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := run([]string{"verify", "--policy", policyPath, "--manifest", manifestPath, "--assets-dir", assetsDir}); err != nil {
		t.Fatalf("verify: %v", err)
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var contract updater.ReleaseManifest
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatalf("updater manifest unmarshal: %v", err)
	}
	if err := contract.Validate("v1.6.0"); err != nil {
		t.Fatalf("updater manifest validation: %v", err)
	}
	for _, arch := range releaseArchitectures {
		artifact, err := contract.ArtifactFor("linux", arch)
		if err != nil {
			t.Fatalf("ArtifactFor(linux, %s): %v", arch, err)
		}
		if artifact.Name != archiveName("v1.6.0", arch) || artifact.Format != "tar.gz" || artifact.BinaryPath != "vohive" {
			t.Fatalf("unexpected updater artifact for %s: %+v", arch, artifact)
		}
	}
	if len(contract.Artifacts) != 3 {
		t.Fatalf("artifacts=%d, want 3 installable archives only", len(contract.Artifacts))
	}
}

func TestVerifyDetectsTamperedArtifact(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	policyPath := writeTestPolicy(t, filepath.Join(dir, "policy"))
	assetsDir := filepath.Join(dir, "assets")
	writeTestArchives(t, assetsDir, "v1.6.0")
	manifestPath := filepath.Join(dir, "release-manifest.json")
	if err := run([]string{
		"generate", "--policy", policyPath, "--version", "v1.6.0", "--channel", "stable",
		"--revision", strings.Repeat("b", 40), "--assets-dir", assetsDir, "--output", manifestPath,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetsDir, archiveName("v1.6.0", "arm64")), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "--policy", policyPath, "--manifest", manifestPath, "--assets-dir", assetsDir}); err == nil {
		t.Fatal("verify succeeded for a tampered artifact")
	}
}

func TestGenerateRejectsMissingArchitecture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	policyPath := writeTestPolicy(t, filepath.Join(dir, "policy"))
	assetsDir := filepath.Join(dir, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if err := os.WriteFile(filepath.Join(assetsDir, archiveName("v1.6.0", arch)), []byte(arch), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := run([]string{
		"generate", "--policy", policyPath, "--version", "v1.6.0", "--channel", "stable",
		"--revision", strings.Repeat("c", 40), "--assets-dir", assetsDir, "--output", filepath.Join(dir, "manifest.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "armv7") {
		t.Fatalf("generate error=%v, want missing armv7", err)
	}
}

func TestVersionChannelRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		channel string
		valid   bool
	}{
		{"v1.6.0", "stable", true},
		{"v1.6.0-beta.2", "beta", true},
		{"v1.6.0-beta.2", "stable", false},
		{"v1.6.0", "beta", false},
		{"v1.6.0-rc.1", "beta", false},
		{"dev-abcdef0", "dev", false},
	}
	for _, tt := range tests {
		err := validateVersionChannel(tt.version, tt.channel)
		if (err == nil) != tt.valid {
			t.Errorf("validateVersionChannel(%q, %q) error=%v, valid=%v", tt.version, tt.channel, err, tt.valid)
		}
	}
}

func TestMovingAliasDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		channel   string
		candidate string
		existing  []string
		advance   bool
		current   string
		wantErr   bool
	}{
		{name: "first stable", channel: "stable", candidate: "v1.6.0", advance: true},
		{name: "newer stable", channel: "stable", candidate: "v1.6.1", existing: []string{"v1.5.5", "latest", "v1.6.0"}, advance: true, current: "v1.6.0"},
		{name: "equal stable", channel: "stable", candidate: "v1.6.0", existing: []string{"v1.6.0"}, current: "v1.6.0"},
		{name: "older stable", channel: "stable", candidate: "v1.5.9", existing: []string{"v1.6.0"}, current: "v1.6.0"},
		{name: "non-v container tag", channel: "stable", candidate: "v1.6.1", existing: []string{"1.6.0", "edge-deadbeef"}, advance: true, current: "v1.6.0"},
		{name: "beta numeric ordering", channel: "beta", candidate: "v1.7.0-beta.10", existing: []string{"v1.7.0-beta.2", "beta", "v9.0.0"}, advance: true, current: "v1.7.0-beta.2"},
		{name: "older beta", channel: "beta", candidate: "v1.7.0-beta.2", existing: []string{"v1.7.0-beta.10"}, current: "v1.7.0-beta.10"},
		{name: "invalid candidate", channel: "stable", candidate: "v1.6.0-beta.1", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			advance, current, err := movingAliasDecision(tt.channel, tt.candidate, tt.existing)
			if (err != nil) != tt.wantErr {
				t.Fatalf("movingAliasDecision() error=%v, wantErr=%v", err, tt.wantErr)
			}
			if err == nil && (advance != tt.advance || current != tt.current) {
				t.Fatalf("movingAliasDecision()=(%v, %q), want (%v, %q)", advance, current, tt.advance, tt.current)
			}
		})
	}
}

func TestWorkflowMovingAliasContracts(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	tests := []struct {
		file      string
		required  []string
		forbidden []string
	}{
		{
			file: filepath.Join(repoRoot, ".github", "workflows", "binary-release.yml"),
			required: []string{
				"group: binary-release-publish",
				"go run ./scripts/release-manifest alias-gate",
				"ADVANCE_ALIAS: ${{ needs.bundle.outputs.advance_alias }}",
				"gh release create",
				"--json tagName,isDraft,isImmutable",
				"trap cleanup_key EXIT",
				"environment: release",
				`for placeholder in "${keys_placeholder}" "${hashes_placeholder}" "${bootstrap_placeholder}"`,
				`grep -qF "${placeholder}" dist/vohive-install.sh`,
			},
			forbidden: []string{
				`grep -q '@VOHIVE_'`,
				"softprops/action-gh-release",
			},
		},
		{
			file: filepath.Join(repoRoot, ".github", "workflows", "docker-publish.yml"),
			required: []string{
				"group: docker-publish",
				"go run ./scripts/release-manifest alias-gate",
				"if [[ \"${ADVANCE_ALIAS}\" == \"true\" ]]",
				"needs.validate.outputs.advance_alias",
				"go run ./cmd/vohive-verify",
				"release-manifest.json.minisig",
				`jq -r '.immutable'`,
				`grep -Fxq "${exact_tag}"`,
				"environment: release",
			},
			forbidden: []string{`tags="${CONTAINER_IMAGE}:${version}"$'\n'"${CONTAINER_IMAGE}:${version#v}"$'\n'"${CONTAINER_IMAGE}:latest"`},
		},
	}
	for _, tt := range tests {
		data, err := os.ReadFile(tt.file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, required := range tt.required {
			if !strings.Contains(text, required) {
				t.Errorf("%s is missing moving-alias contract %q", tt.file, required)
			}
		}
		for _, forbidden := range tt.forbidden {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains unconditional moving alias %q", tt.file, forbidden)
			}
		}
	}
}

func TestPolicyRejectsDifferentRepository(t *testing.T) {
	t.Parallel()
	policy := testPolicy()
	policy.Repository = "someone/vohive"
	if err := validatePolicy(policy); err == nil {
		t.Fatal("validatePolicy accepted a different repository")
	}
}

func TestPolicyRejectsSchemaDriftFromCompiledRuntime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*releasePolicy)
	}{
		{"config", func(p *releasePolicy) { p.ConfigSchema = schemaRange{Min: 0, Target: 2, Max: 2} }},
		{"database", func(p *releasePolicy) { p.DatabaseSchema = schemaRange{Min: 1, Target: 1, Max: 1} }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			policy := testPolicy()
			test.edit(&policy)
			if err := validatePolicy(policy); err == nil {
				t.Fatal("validatePolicy accepted a structurally valid schema range that differs from the compiled runtime")
			}
		})
	}
}

func writeTestPolicy(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "policy.json")
	policy := `{
  "schema": 1,
  "product": "vohive",
  "repository": "zanescope/vohive",
  "container_image": "ghcr.io/zanescope/vohive",
  "min_updater_version": "v1.6.0",
  "min_direct_upgrade": "v1.4.0",
  "upgrade_via": [],
  "config_schema": {"min": 0, "target": 1, "max": 1},
  "database_schema": {"min": 0, "target": 1, "max": 1}
}`
	if err := os.WriteFile(path, []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestArchives(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, arch := range releaseArchitectures {
		if err := os.WriteFile(filepath.Join(dir, archiveName(version, arch)), []byte("archive-"+arch), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func testPolicy() releasePolicy {
	return releasePolicy{
		Schema:            1,
		Product:           "vohive",
		Repository:        "zanescope/vohive",
		ContainerImage:    "ghcr.io/zanescope/vohive",
		MinUpdaterVersion: "v1.6.0",
		MinDirectUpgrade:  "v1.4.0",
		UpgradeVia:        []string{},
		ConfigSchema:      schemaRange{Min: 0, Target: 1, Max: 1},
		DatabaseSchema:    schemaRange{Min: 0, Target: 1, Max: 1},
	}
}
