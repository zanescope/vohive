package device

import (
	"reflect"
	"testing"

	"github.com/zanescope/vohive/internal/config"
)

type recordingPublicIPConfigurer struct {
	ipv4 []string
	ipv6 []string
}

func (r *recordingPublicIPConfigurer) SetPublicIPProbeURLs(ipv4URLs, ipv6URLs []string) {
	r.ipv4 = ipv4URLs
	r.ipv6 = ipv6URLs
}

func TestConfigurePublicIPProbeSourcesUsesIndependentDefaults(t *testing.T) {
	target := &recordingPublicIPConfigurer{}
	var pool *Pool
	pool.configurePublicIPProbeSources(target)

	defaults := config.DefaultPublicIPProbeConfig()
	if !reflect.DeepEqual(target.ipv4, defaults.IPv4URLs) || !reflect.DeepEqual(target.ipv6, defaults.IPv6URLs) {
		t.Fatalf("sources = (%v, %v), want defaults (%v, %v)", target.ipv4, target.ipv6, defaults.IPv4URLs, defaults.IPv6URLs)
	}
	target.ipv4[0] = "https://mutated.example"
	if got := config.DefaultPublicIPProbeConfig().IPv4URLs[0]; got == target.ipv4[0] {
		t.Fatal("target mutation changed package defaults")
	}
}

func TestConfigurePublicIPProbeSourcesCopiesCustomSlices(t *testing.T) {
	cfg := &config.Config{PublicIPProbe: config.PublicIPProbeConfig{
		IPv4URLs: []string{"https://v4.example"},
		IPv6URLs: []string{"https://v6.example"},
	}}
	target := &recordingPublicIPConfigurer{}
	(&Pool{cfg: cfg}).configurePublicIPProbeSources(target)

	if !reflect.DeepEqual(target.ipv4, cfg.PublicIPProbe.IPv4URLs) || !reflect.DeepEqual(target.ipv6, cfg.PublicIPProbe.IPv6URLs) {
		t.Fatalf("sources = (%v, %v), want custom (%v, %v)", target.ipv4, target.ipv6, cfg.PublicIPProbe.IPv4URLs, cfg.PublicIPProbe.IPv6URLs)
	}
	target.ipv4[0] = "https://mutated.example"
	target.ipv6[0] = "https://mutated-v6.example"
	if cfg.PublicIPProbe.IPv4URLs[0] != "https://v4.example" || cfg.PublicIPProbe.IPv6URLs[0] != "https://v6.example" {
		t.Fatalf("target mutation changed config: %+v", cfg.PublicIPProbe)
	}
}
