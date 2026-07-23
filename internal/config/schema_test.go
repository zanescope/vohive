package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultConfigDoesNotProvideFixedCredentials(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ConfigSchema != CurrentConfigSchema {
		t.Fatalf("ConfigSchema=%d want %d", cfg.ConfigSchema, CurrentConfigSchema)
	}
	if cfg.Web.Username != "" || cfg.Web.Password != "" {
		t.Fatal("DefaultConfig must not provide fixed credentials")
	}
}

func TestEnsureConfigFileCreatesRandomInitialCredentials(t *testing.T) {
	_ = ConsumeInitialAdminCredentials()
	dir := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(dir, "config.yaml")

	credentials, err := EnsureConfigFile(path)
	if err != nil {
		t.Fatalf("EnsureConfigFile() error=%v", err)
	}
	if credentials == nil {
		t.Fatal("EnsureConfigFile() did not return initial credentials")
	}
	if credentials.Username != InitialAdminUsername {
		t.Fatalf("username=%q want %q", credentials.Username, InitialAdminUsername)
	}
	if len(credentials.Password) < 32 || credentials.Password == "admin" || credentials.Password == "admin123" {
		t.Fatal("initial password is not high-entropy")
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("config dir mode=%v err=%v want 0700", infoMode(info), err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("config mode=%v err=%v want 0600", infoMode(info), err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error=%v", err)
	}
	if !strings.Contains(string(raw), credentials.Password) || strings.Contains(string(raw), "password: admin\n") {
		t.Fatal("fresh config does not contain only the generated password")
	}
	if err := ValidateYAML(raw); err != nil {
		t.Fatalf("ValidateYAML() error=%v", err)
	}

	consumed := ConsumeInitialAdminCredentials()
	if consumed == nil || consumed.Password != credentials.Password {
		t.Fatal("initial credentials were not consumable exactly once")
	}
	if second := ConsumeInitialAdminCredentials(); second != nil {
		t.Fatal("initial credentials were returned more than once")
	}
	if again, err := EnsureConfigFile(path); err != nil || again != nil {
		t.Fatalf("second EnsureConfigFile(): credentials_present=%t err=%v", again != nil, err)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}

func TestConfigSchema0To1MigrationPreservesUnknownFieldsAndComments(t *testing.T) {
	legacyKey := "disable_" + "network"
	input := []byte(`# keep root comment
server:
  port: ":7575"
future_root:
  mode: keep
web:
  username: alice
  password: secret
devices:
  - id: modem-1
    ` + legacyKey + `: true
    at_port: /dev/ttyUSB2
    future_driver_option: keep # keep device comment
`)

	plan, err := PlanMigration(input)
	if err != nil {
		t.Fatalf("PlanMigration() error=%v", err)
	}
	if plan.CurrentSchema != 0 || plan.TargetSchema != 1 || len(plan.Steps) != 1 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	out, applied, err := MigrateYAML(input)
	if err != nil {
		t.Fatalf("MigrateYAML() error=%v", err)
	}
	if !applied.NeedsMigration() {
		t.Fatal("migration plan unexpectedly empty")
	}
	text := string(out)
	for _, want := range []string{
		"config_schema: 1", "free_device_limit: 5", "network_enabled: false",
		"future_root:", "future_driver_option: keep", "# keep root comment",
		"# keep device comment", "username: alice", "password: secret", "port: 7575",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("migrated config missing %q", want)
		}
	}
	for _, forbidden := range []string{legacyKey + ":", "at_port:"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("migrated config retained %q", forbidden)
		}
	}
	if err := ValidateYAML(out); err != nil {
		t.Fatalf("ValidateYAML(migrated) error=%v", err)
	}

	again, secondPlan, err := MigrateYAML(out)
	if err != nil {
		t.Fatalf("second MigrateYAML() error=%v", err)
	}
	if secondPlan.NeedsMigration() || string(again) != string(out) {
		t.Fatal("migration is not idempotent")
	}
}

func TestConfigSchema0MissingCredentialsKeepsLegacyEffectiveLogin(t *testing.T) {
	out, _, err := MigrateYAML([]byte("server:\n  port: 7575\n"))
	if err != nil {
		t.Fatalf("MigrateYAML() error=%v", err)
	}
	text := string(out)
	if !strings.Contains(text, "username: admin") || !strings.Contains(text, "password: admin") {
		t.Fatal("legacy effective credentials were not persisted")
	}
}

func TestSameSchemaMigrationLeavesFileByteIdentical(t *testing.T) {
	input := []byte("config_schema: 1\nweb:\n  username: alice\n  password: secret\nunknown: keep\n")
	out, plan, err := MigrateYAML(input)
	if err != nil {
		t.Fatalf("MigrateYAML() error=%v", err)
	}
	if plan.NeedsMigration() || string(out) != string(input) {
		t.Fatalf("same-schema config was rewritten: plan=%+v", plan)
	}
}

func TestConfigSchemaTooNewIsRejectedWithoutRewrite(t *testing.T) {
	path := writeTempConfig(t, `
config_schema: 2
web:
  username: alice
  password: secret
future: keep
`)
	before, _ := os.ReadFile(path)
	_, err := Load(path)
	if !errors.Is(err, ErrConfigSchemaTooNew) {
		t.Fatalf("Load() error=%v want ErrConfigSchemaTooNew", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("too-new config was modified")
	}
}

func TestSchema1WithoutCredentialsIsRejected(t *testing.T) {
	input := []byte("config_schema: 1\nserver:\n  port: 7575\n")
	if err := ValidateYAML(input); err == nil {
		t.Fatal("ValidateYAML() accepted schema 1 without credentials")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, input, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() accepted schema 1 without credentials")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(input) {
		t.Fatal("invalid schema 1 config was rewritten")
	}
}

func TestInvalidExistingConfigIsNeverReplaced(t *testing.T) {
	input := []byte("server: [\n")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, input, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() accepted invalid YAML")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(input) {
		t.Fatal("invalid existing config was replaced")
	}
}

func TestVOHIVEEnvironmentOverridesLegacyPROXYPrefix(t *testing.T) {
	path := writeTempConfig(t, `
config_schema: 1
server:
  port: 7575
web:
  username: alice
  password: secret
`)
	t.Setenv("VOHIVE_SERVER_PORT", "9001")
	t.Setenv("PROXY_SERVER_PORT", "9002")
	t.Setenv("PROXY_WEBHOOK_RETRY_MAX", "9")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error=%v", err)
	}
	if cfg.Server.Port != ":9001" {
		t.Fatalf("Server.Port=%q want :9001", cfg.Server.Port)
	}
	if cfg.Webhook.RetryMax != 9 {
		t.Fatalf("Webhook.RetryMax=%d want legacy env fallback 9", cfg.Webhook.RetryMax)
	}
}

func TestFreshEnvironmentCredentialsSuppressGeneratedCredentialBanner(t *testing.T) {
	_ = ConsumeInitialAdminCredentials()
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("VOHIVE_WEB_USERNAME", "operator")
	t.Setenv("VOHIVE_WEB_PASSWORD", "environment-secret")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error=%v", err)
	}
	if cfg.Web.Username != "operator" || cfg.Web.Password != "environment-secret" {
		t.Fatal("environment credentials were not authoritative")
	}
	if credentials := ConsumeInitialAdminCredentials(); credentials != nil {
		t.Fatal("generated credential banner was not suppressed")
	}
}
