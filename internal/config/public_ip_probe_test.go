package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoadPublicIPProbeDefaultsPerFamily(t *testing.T) {
	path := writeTempConfig(t, `
server:
  port: 7575
public_ip_probe:
  ipv4_urls: []
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := DefaultPublicIPProbeConfig()
	if !reflect.DeepEqual(cfg.PublicIPProbe, want) {
		t.Fatalf("PublicIPProbe = %#v, want %#v", cfg.PublicIPProbe, want)
	}

	cfg.PublicIPProbe.IPv4URLs[0] = "changed"
	if DefaultPublicIPProbeConfig().IPv4URLs[0] == "changed" {
		t.Fatal("default URL slice was exposed for mutation")
	}
}

func TestLoadPublicIPProbeExplicitListReplacesDefaultsAndDeduplicates(t *testing.T) {
	path := writeTempConfig(t, `
public_ip_probe:
  ipv4_urls:
    - "  https://Probe.Example/v4?region=cn  "
    - "https://probe.example/v4?region=cn"
  ipv6_urls:
    - https://v6.example/ip
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := cfg.PublicIPProbe.IPv4URLs, []string{"https://Probe.Example/v4?region=cn"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IPv4URLs = %v, want %v", got, want)
	}
	if got, want := cfg.PublicIPProbe.IPv6URLs, []string{"https://v6.example/ip"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IPv6URLs = %v, want %v", got, want)
	}
}

func TestPublicIPProbeValidation(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: "   "},
		{name: "http", value: "http://example.com/ip"},
		{name: "relative", value: "/ip"},
		{name: "missing-host", value: "https:///ip"},
		{name: "userinfo", value: "https://user:password@example.com/ip"},
		{name: "fragment", value: "https://example.com/ip#secret"},
		{name: "control", value: "https://example.com/ip\nheader"},
		{name: "bad-port", value: "https://example.com:bad/ip"},
		{name: "private-literal", value: "https://10.0.0.1/ip"},
		{name: "wrong-family-literal", value: "https://[2606:4700:4700::1111]/ip"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NormalizePublicIPProbeConfig(PublicIPProbeConfig{IPv4URLs: []string{test.value}})
			if err == nil {
				t.Fatal("NormalizePublicIPProbeConfig() unexpectedly succeeded")
			}
			if !strings.Contains(err.Error(), "ipv4_urls[0]") {
				t.Fatalf("error = %q, want indexed field", err)
			}
		})
	}

	_, err := NormalizePublicIPProbeConfig(PublicIPProbeConfig{IPv4URLs: []string{
		"https://one.example", "https://two.example", "https://three.example",
		"https://four.example", "https://five.example",
	}})
	if err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("too-many error = %v", err)
	}
}

func TestPublicIPProbeAcceptsPublicFamilyMatchingLiterals(t *testing.T) {
	got, err := NormalizePublicIPProbeConfig(PublicIPProbeConfig{
		IPv4URLs: []string{"https://8.8.8.8/ip"},
		IPv6URLs: []string{"https://[2606:4700:4700::1111]/ip"},
	})
	if err != nil {
		t.Fatalf("NormalizePublicIPProbeConfig() error = %v", err)
	}
	if len(got.IPv4URLs) != 1 || len(got.IPv6URLs) != 1 {
		t.Fatalf("normalized config = %#v", got)
	}
}

func TestLoadPublicIPProbeRejectsUnknownSectionField(t *testing.T) {
	path := writeTempConfig(t, `
public_ip_probe:
  ipv4_url:
    - https://cn-v4.example/ip
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() unexpectedly accepted misspelled ipv4_url")
	}
	if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), "ipv4_url") {
		t.Fatalf("Load() error = %q, want unknown ipv4_url", err)
	}
}

func TestLoadPublicIPProbeDoesNotRejectUnrelatedRootFields(t *testing.T) {
	path := writeTempConfig(t, `
future_root_field: true
public_ip_probe:
  ipv4_urls:
    - https://cn-v4.example/ip
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.PublicIPProbe.IPv4URLs; !reflect.DeepEqual(got, []string{"https://cn-v4.example/ip"}) {
		t.Fatalf("IPv4URLs = %v", got)
	}
}
func TestPublicIPProbeIsYAMLOnly(t *testing.T) {
	t.Setenv("PROXY_PUBLIC_IP_PROBE_IPV4_URLS", "https://env.example/ip")
	path := writeTempConfig(t, `
server:
  port: 7575
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(cfg.PublicIPProbe.IPv4URLs, defaultPublicIPv4ProbeURLs) {
		t.Fatalf("IPv4URLs = %v, environment unexpectedly overrode YAML-only config", cfg.PublicIPProbe.IPv4URLs)
	}
}

func TestRootPersistenceKeepsPublicIPProbe(t *testing.T) {
	path := writeTempConfig(t, `
public_ip_probe:
  ipv4_urls:
    - https://cn-v4.example/ip
  ipv6_urls:
    - https://cn-v6.example/ip
devices: []
proxy:
  instances: []
`)
	if err := AddDeviceInFile(path, DeviceConfig{ID: "dev1", DeviceBackend: "mbim"}); err != nil {
		t.Fatalf("AddDeviceInFile() error = %v", err)
	}
	if err := AddProxyInstanceInFile(path, ProxyInstance{ID: "proxy1", DeviceID: "dev1", Mode: "socks5", ListenPort: 1080}); err != nil {
		t.Fatalf("AddProxyInstanceInFile() error = %v", err)
	}
	if err := UpdateNotificationInFile(path, TelegramConfig{}, FeishuConfig{}, QQConfig{}, WebhookConfig{}, BarkConfig{}, EmailConfig{}, PushplusConfig{}); err != nil {
		t.Fatalf("UpdateNotificationInFile() error = %v", err)
	}
	if err := UpdateWebCredentialsInFile(path, "admin", "hash"); err != nil {
		t.Fatalf("UpdateWebCredentialsInFile() error = %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after persistence error = %v", err)
	}
	want := PublicIPProbeConfig{
		IPv4URLs: []string{"https://cn-v4.example/ip"},
		IPv6URLs: []string{"https://cn-v6.example/ip"},
	}
	if !reflect.DeepEqual(cfg.PublicIPProbe, want) {
		t.Fatalf("PublicIPProbe after persistence = %#v, want %#v", cfg.PublicIPProbe, want)
	}
}
