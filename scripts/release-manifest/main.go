// Command release-manifest creates and verifies VoHive release manifests.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zanescope/vohive/internal/schemacontract"
	"golang.org/x/mod/semver"
)

const manifestSchema = 1

var (
	stableVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	betaVersionPattern   = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-beta\.(0|[1-9][0-9]*)$`)
	semverPattern        = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?$`)
	revisionPattern      = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)
	sha256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

var releaseArchitectures = []string{"amd64", "arm64", "armv7"}

type schemaRange struct {
	Min    int `json:"min"`
	Target int `json:"target"`
	Max    int `json:"max"`
}

type releasePolicy struct {
	Schema            int         `json:"schema"`
	Product           string      `json:"product"`
	Repository        string      `json:"repository"`
	ContainerImage    string      `json:"container_image"`
	MinUpdaterVersion string      `json:"min_updater_version"`
	MinDirectUpgrade  string      `json:"min_direct_upgrade"`
	UpgradeVia        []string    `json:"upgrade_via"`
	ConfigSchema      schemaRange `json:"config_schema"`
	DatabaseSchema    schemaRange `json:"database_schema"`
}

type releaseArtifact struct {
	Name       string `json:"name"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Format     string `json:"format"`
	BinaryPath string `json:"binary_path,omitempty"`
}

type releaseManifest struct {
	Schema            int               `json:"schema"`
	Product           string            `json:"product"`
	Repository        string            `json:"repository"`
	Version           string            `json:"version"`
	Channel           string            `json:"channel"`
	SourceRevision    string            `json:"source_revision"`
	MinUpdaterVersion string            `json:"min_updater_version"`
	MinDirectUpgrade  string            `json:"min_direct_upgrade,omitempty"`
	UpgradeVia        []string          `json:"upgrade_via,omitempty"`
	ConfigSchema      schemaRange       `json:"config_schema"`
	DatabaseSchema    schemaRange       `json:"database_schema"`
	Artifacts         []releaseArtifact `json:"artifacts"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "release-manifest: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usageText())
	}
	switch args[0] {
	case "generate":
		return runGenerate(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "alias-gate":
		return runAliasGate(args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usageText())
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usageText())
	}
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	policyPath := fs.String("policy", "packaging/release-policy.json", "release policy JSON")
	version := fs.String("version", "", "release version")
	channel := fs.String("channel", "", "stable or beta")
	revision := fs.String("revision", "", "full source revision")
	assetsDir := fs.String("assets-dir", "", "directory containing release archives")
	output := fs.String("output", "release-manifest.json", "manifest output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("generate: unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *version == "" || *channel == "" || *revision == "" || *assetsDir == "" {
		return errors.New("generate requires --version, --channel, --revision, and --assets-dir")
	}

	policy, err := readPolicy(*policyPath)
	if err != nil {
		return err
	}
	artifacts, err := collectArtifacts(*assetsDir, *version)
	if err != nil {
		return err
	}
	manifest := releaseManifest{
		Schema:            manifestSchema,
		Product:           policy.Product,
		Repository:        policy.Repository,
		Version:           *version,
		Channel:           *channel,
		SourceRevision:    strings.ToLower(*revision),
		MinUpdaterVersion: policy.MinUpdaterVersion,
		MinDirectUpgrade:  policy.MinDirectUpgrade,
		UpgradeVia:        append([]string(nil), policy.UpgradeVia...),
		ConfigSchema:      policy.ConfigSchema,
		DatabaseSchema:    policy.DatabaseSchema,
		Artifacts:         artifacts,
	}
	if err := validateManifest(manifest, policy); err != nil {
		return err
	}
	return writeJSONAtomic(*output, manifest)
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	policyPath := fs.String("policy", "packaging/release-policy.json", "release policy JSON")
	manifestPath := fs.String("manifest", "release-manifest.json", "manifest JSON")
	assetsDir := fs.String("assets-dir", "", "optional directory containing release archives")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("verify: unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	policy, err := readPolicy(*policyPath)
	if err != nil {
		return err
	}
	manifest, err := readManifest(*manifestPath)
	if err != nil {
		return err
	}
	if err := validateManifest(manifest, policy); err != nil {
		return err
	}
	if *assetsDir != "" {
		return verifyArtifacts(manifest.Artifacts, *assetsDir)
	}
	return nil
}

func runAliasGate(args []string) error {
	fs := flag.NewFlagSet("alias-gate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	channel := fs.String("channel", "", "stable or beta")
	candidate := fs.String("candidate", "", "candidate release version")
	existingFile := fs.String("existing-file", "", "newline-separated existing tags")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("alias-gate: unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *channel == "" || *candidate == "" || *existingFile == "" {
		return errors.New("alias-gate requires --channel, --candidate, and --existing-file")
	}
	tags, err := readTagLines(*existingFile)
	if err != nil {
		return err
	}
	advance, current, err := movingAliasDecision(*channel, *candidate, tags)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "advance_alias=%t\n", advance)
	fmt.Fprintf(os.Stdout, "current_alias_version=%s\n", current)
	return nil
}

func movingAliasDecision(channel, candidate string, existing []string) (bool, string, error) {
	if err := validateVersionChannel(candidate, channel); err != nil {
		return false, "", fmt.Errorf("invalid moving-alias candidate: %w", err)
	}
	current := ""
	for _, raw := range existing {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if err := validateVersionChannel(tag, channel); err != nil && !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
		if validateVersionChannel(tag, channel) != nil {
			continue
		}
		if current == "" || semver.Compare(tag, current) > 0 {
			current = tag
		}
	}
	if current == "" {
		return true, "", nil
	}
	return semver.Compare(candidate, current) > 0, current, nil
}

func readTagLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read existing tags: %w", err)
	}
	defer f.Close()
	var tags []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		tags = append(tags, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read existing tags: %w", err)
	}
	return tags, nil
}

func readPolicy(path string) (releasePolicy, error) {
	var policy releasePolicy
	if err := readStrictJSON(path, &policy); err != nil {
		return policy, fmt.Errorf("read policy: %w", err)
	}
	if err := validatePolicy(policy); err != nil {
		return policy, fmt.Errorf("invalid policy: %w", err)
	}
	return policy, nil
}

func readManifest(path string) (releaseManifest, error) {
	var manifest releaseManifest
	if err := readStrictJSON(path, &manifest); err != nil {
		return manifest, fmt.Errorf("read manifest: %w", err)
	}
	return manifest, nil
}

func readStrictJSON(path string, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validatePolicy(policy releasePolicy) error {
	if policy.Schema != manifestSchema {
		return fmt.Errorf("schema=%d, want %d", policy.Schema, manifestSchema)
	}
	if policy.Product != "vohive" || policy.Repository != "zanescope/vohive" {
		return errors.New("publishing identity must be vohive from zanescope/vohive")
	}
	if policy.ContainerImage != "ghcr.io/zanescope/vohive" {
		return fmt.Errorf("container_image=%q, want ghcr.io/zanescope/vohive", policy.ContainerImage)
	}
	if !semverPattern.MatchString(policy.MinUpdaterVersion) || !semverPattern.MatchString(policy.MinDirectUpgrade) {
		return errors.New("updater and direct-upgrade floors must be semantic versions")
	}
	for _, version := range policy.UpgradeVia {
		if !semverPattern.MatchString(version) {
			return fmt.Errorf("invalid upgrade_via version %q", version)
		}
	}
	expectedConfig := manifestSchemaRange(schemacontract.Config())
	if err := validateSchemaRange("config_schema", policy.ConfigSchema, expectedConfig); err != nil {
		return err
	}
	expectedDatabase := manifestSchemaRange(schemacontract.Database())
	return validateSchemaRange("database_schema", policy.DatabaseSchema, expectedDatabase)
}

func manifestSchemaRange(schema schemacontract.Range) schemaRange {
	return schemaRange{Min: schema.Min, Target: schema.Target, Max: schema.Max}
}

func validateSchemaRange(name string, schema, compiled schemaRange) error {
	if schema.Min < 0 || schema.Min > schema.Target || schema.Target > schema.Max {
		return fmt.Errorf("%s must satisfy 0 <= min <= target <= max", name)
	}
	if schema != compiled {
		return fmt.Errorf(
			"%s=%+v does not match compiled runtime capability %+v",
			name, schema, compiled,
		)
	}
	return nil
}

func validateManifest(manifest releaseManifest, policy releasePolicy) error {
	if manifest.Schema != manifestSchema {
		return fmt.Errorf("manifest schema=%d, want %d", manifest.Schema, manifestSchema)
	}
	if manifest.Product != policy.Product || manifest.Repository != policy.Repository {
		return errors.New("manifest publishing identity does not match policy")
	}
	if manifest.MinUpdaterVersion != policy.MinUpdaterVersion || manifest.MinDirectUpgrade != policy.MinDirectUpgrade {
		return errors.New("manifest upgrade floor does not match policy")
	}
	if !equalStrings(manifest.UpgradeVia, policy.UpgradeVia) {
		return errors.New("manifest upgrade_via does not match policy")
	}
	if manifest.ConfigSchema != policy.ConfigSchema || manifest.DatabaseSchema != policy.DatabaseSchema {
		return errors.New("manifest schema ranges do not match policy")
	}
	if err := validateVersionChannel(manifest.Version, manifest.Channel); err != nil {
		return err
	}
	if !revisionPattern.MatchString(manifest.SourceRevision) {
		return fmt.Errorf("source_revision %q must be a full lowercase hex revision", manifest.SourceRevision)
	}
	if len(manifest.Artifacts) != len(releaseArchitectures) {
		return fmt.Errorf("manifest must contain exactly %d installable archives", len(releaseArchitectures))
	}
	seen := make(map[string]bool, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		if artifact.GOOS != "linux" || !contains(releaseArchitectures, artifact.GOARCH) {
			return fmt.Errorf("unsupported artifact platform %s/%s", artifact.GOOS, artifact.GOARCH)
		}
		if seen[artifact.GOARCH] {
			return fmt.Errorf("duplicate artifact architecture %q", artifact.GOARCH)
		}
		seen[artifact.GOARCH] = true
		expectedName := archiveName(manifest.Version, artifact.GOARCH)
		if artifact.Name != expectedName || artifact.Format != "tar.gz" || artifact.BinaryPath != "vohive" {
			return fmt.Errorf("artifact metadata is invalid for %s", artifact.GOARCH)
		}
		if artifact.Size <= 0 || !sha256Pattern.MatchString(artifact.SHA256) {
			return fmt.Errorf("artifact integrity metadata is invalid for %s", artifact.GOARCH)
		}
	}
	for _, arch := range releaseArchitectures {
		if !seen[arch] {
			return fmt.Errorf("missing artifact architecture %q", arch)
		}
	}
	return nil
}

func validateVersionChannel(version, channel string) error {
	switch channel {
	case "stable":
		if stableVersionPattern.MatchString(version) {
			return nil
		}
	case "beta":
		if betaVersionPattern.MatchString(version) {
			return nil
		}
	default:
		return fmt.Errorf("unsupported channel %q", channel)
	}
	return fmt.Errorf("version %q is not valid for channel %q", version, channel)
}

func collectArtifacts(dir, version string) ([]releaseArtifact, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read assets directory: %w", err)
	}
	byArch := make(map[string]releaseArtifact, len(releaseArchitectures))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		arch, ok := archiveArchitecture(version, entry.Name())
		if !ok {
			continue
		}
		if _, exists := byArch[arch]; exists {
			return nil, fmt.Errorf("duplicate archive for architecture %q", arch)
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat artifact %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 {
			return nil, fmt.Errorf("artifact %q is not a non-empty regular file", entry.Name())
		}
		digest, err := fileSHA256(path)
		if err != nil {
			return nil, fmt.Errorf("hash artifact %q: %w", entry.Name(), err)
		}
		byArch[arch] = releaseArtifact{
			Name: entry.Name(), GOOS: "linux", GOARCH: arch, SHA256: digest,
			Size: info.Size(), Format: "tar.gz", BinaryPath: "vohive",
		}
	}
	artifacts := make([]releaseArtifact, 0, len(releaseArchitectures))
	for _, arch := range releaseArchitectures {
		artifact, ok := byArch[arch]
		if !ok {
			return nil, fmt.Errorf("missing release archive %q", archiveName(version, arch))
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

func archiveArchitecture(version, name string) (string, bool) {
	for _, arch := range releaseArchitectures {
		if name == archiveName(version, arch) {
			return arch, true
		}
	}
	return "", false
}

func archiveName(version, arch string) string {
	return fmt.Sprintf("vohive_%s_linux_%s.tar.gz", version, arch)
}

func verifyArtifacts(artifacts []releaseArtifact, dir string) error {
	for _, artifact := range artifacts {
		path := filepath.Join(dir, artifact.Name)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat artifact %q: %w", artifact.Name, err)
		}
		if !info.Mode().IsRegular() || info.Size() != artifact.Size {
			return fmt.Errorf("artifact %q size mismatch: got %d, want %d", artifact.Name, info.Size(), artifact.Size)
		}
		digest, err := fileSHA256(path)
		if err != nil {
			return fmt.Errorf("hash artifact %q: %w", artifact.Name, err)
		}
		if digest != artifact.SHA256 {
			return fmt.Errorf("artifact %q sha256 mismatch: got %s, want %s", artifact.Name, digest, artifact.SHA256)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".release-manifest-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func usageText() string {
	return `Usage:
  go run ./scripts/release-manifest generate --version VERSION --channel CHANNEL --revision REVISION --assets-dir DIR [--policy FILE] [--output FILE]
  go run ./scripts/release-manifest verify --manifest FILE [--assets-dir DIR] [--policy FILE]
  go run ./scripts/release-manifest alias-gate --channel CHANNEL --candidate VERSION --existing-file FILE
`
}
