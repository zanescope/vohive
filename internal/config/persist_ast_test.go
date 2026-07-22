package config

import (
	"os"
	"strings"
	"testing"
)

func TestNotificationAndCredentialPatchesPreserveUnknownYAML(t *testing.T) {
	path := writeTempConfig(t, `
# keep root comment
config_schema: 1
web:
  username: alice
  password: old-secret
  future_session_option: keep
email:
  enabled: false
  future_tls_option: keep # keep email comment
future_root: keep
`)
	err := UpdateNotificationInFile(path,
		TelegramConfig{}, FeishuConfig{}, QQConfig{}, WebhookConfig{}, BarkConfig{},
		EmailConfig{
			Enabled:     true,
			UseSSL:      true,
			SMTPHost:    "smtp.example.com",
			SMTPPort:    465,
			Username:    "mailer",
			Password:    "mail-secret",
			FromAddress: "from@example.com",
			ToAddresses: []string{"to@example.com"},
		},
		PushplusConfig{},
	)
	if err != nil {
		t.Fatalf("UpdateNotificationInFile() error=%v", err)
	}
	if err := UpdateWebCredentialsInFile(path, "alice", "new-secret"); err != nil {
		t.Fatalf("UpdateWebCredentialsInFile() error=%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		"use_ssl: true", "future_tls_option: keep", "# keep email comment",
		"future_session_option: keep", "future_root: keep", "# keep root comment",
		"password: new-secret",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("patched config missing %q", want)
		}
	}
}

func TestDevicePatchPreservesUnknownDeviceFields(t *testing.T) {
	path := writeTempConfig(t, `
server:
  port: 7575
web:
  username: alice
  password: secret
devices:
  - id: modem-1
    name: old
    future_driver_option: keep
`)
	if err := UpdateDeviceInFile(path, "modem-1", DeviceConfig{ID: "modem-1", Name: "new"}); err != nil {
		t.Fatalf("UpdateDeviceInFile() error=%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "future_driver_option: keep") || !strings.Contains(text, "name: new") {
		t.Fatal("device patch lost fields")
	}
}
