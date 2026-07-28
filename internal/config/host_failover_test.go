package config

import (
	"strings"
	"testing"
)

func TestDefaultHostFailoverConfigIsDisabledAndValid(t *testing.T) {
	cfg := DefaultConfig().HostFailover
	if len(cfg.CandidateDeviceIDs) != 0 {
		t.Fatal("host failover must be disabled when no devices are selected")
	}
	if err := validateHostFailoverConfig(cfg); err != nil {
		t.Fatalf("default config is invalid: %v", err)
	}
}

func TestValidateHostFailoverAllowsAutoDiscoveredPrimary(t *testing.T) {
	cfg := DefaultConfig().HostFailover
	cfg.CandidateDeviceIDs = []string{"modem-1"}

	if err := validateHostFailoverConfig(cfg); err != nil {
		t.Fatalf("auto-discovered primary config rejected: %v", err)
	}
}

func TestValidateHostFailoverAcceptsOrderedCandidates(t *testing.T) {
	cfg := DefaultConfig().HostFailover
	cfg.PrimaryInterface = "eth0"
	cfg.CandidateDeviceIDs = []string{"wwan9", "wwan2"}

	if err := validateHostFailoverConfig(cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateHostFailoverRejectsDuplicateCandidates(t *testing.T) {
	cfg := DefaultConfig().HostFailover
	cfg.PrimaryInterface = "eth0"
	cfg.CandidateDeviceIDs = []string{"wwan9", " wwan9 "}

	err := validateHostFailoverConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v, want duplicate candidate error", err)
	}
}

func TestValidateHostFailoverRejectsUnsafeThresholdsAndMetric(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*HostFailoverConfig)
	}{
		{name: "zero failure threshold", mutate: func(cfg *HostFailoverConfig) { cfg.FailureThreshold = 0 }},
		{name: "zero recovery threshold", mutate: func(cfg *HostFailoverConfig) { cfg.RecoveryThreshold = 0 }},
		{name: "zero route metric", mutate: func(cfg *HostFailoverConfig) { cfg.MaximumRouteMetric = 0 }},
		{name: "short interval", mutate: func(cfg *HostFailoverConfig) { cfg.ProbeIntervalSeconds = 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig().HostFailover
			tt.mutate(&cfg)
			if err := validateHostFailoverConfig(cfg); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}
