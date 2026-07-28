package device

import (
	"context"
	"testing"

	"github.com/zanescope/vohive/internal/config"
)

type hostFailoverNetworkStub struct {
	connected bool
	publicV4  string
}

func (s *hostFailoverNetworkStub) Connect() error    { s.connected = true; return nil }
func (s *hostFailoverNetworkStub) Disconnect() error { s.connected = false; return nil }
func (s *hostFailoverNetworkStub) IsConnected() bool { return s.connected }
func (s *hostFailoverNetworkStub) RotateIP() error   { return nil }
func (s *hostFailoverNetworkStub) GetPrivateIP() string {
	if s.connected {
		return "10.0.0.2"
	}
	return ""
}
func (s *hostFailoverNetworkStub) GetPrivateIPv6() string { return "" }
func (s *hostFailoverNetworkStub) GetPublicIPv4AndV6NoCache() (string, string) {
	return s.publicV4, ""
}
func (s *hostFailoverNetworkStub) GetPublicIPv4AndV6Context(context.Context) (string, string) {
	return s.publicV4, ""
}

func TestHostFailoverCandidatesPreserveConfiguredOrderAndProbeIPv4(t *testing.T) {
	pool := NewPool(&config.Config{})
	firstController := &hostFailoverNetworkStub{connected: true, publicV4: "198.51.100.1"}
	secondController := &hostFailoverNetworkStub{connected: true, publicV4: "198.51.100.2"}
	first := &Worker{
		ID: "first", Config: config.DeviceConfig{ID: "first", Interface: "wwan9"}, netOverride: firstController,
	}
	second := &Worker{
		ID: "second", Config: config.DeviceConfig{ID: "second", Interface: "wwan2"}, netOverride: secondController,
	}
	pool.workers[first.ID] = first
	pool.workers[second.ID] = second

	candidates := pool.Candidates([]string{"second", "first"})
	if len(candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(candidates))
	}
	if candidates[0].DeviceID != "second" || candidates[0].Interface != "wwan2" {
		t.Fatalf("first candidate = %+v, want second/wwan2", candidates[0])
	}
	if err := candidates[0].Probe(context.Background()); err != nil {
		t.Fatalf("candidate probe failed: %v", err)
	}
}

func TestHostFailoverCandidateProbeRejectsWorkerReplacement(t *testing.T) {
	pool := NewPool(&config.Config{})
	controller := &hostFailoverNetworkStub{connected: true, publicV4: "198.51.100.1"}
	worker := &Worker{
		ID: "modem", Config: config.DeviceConfig{ID: "modem", Interface: "wwan1"}, netOverride: controller,
	}
	pool.workers[worker.ID] = worker
	candidate := pool.Candidates([]string{"modem"})[0]
	pool.workers[worker.ID] = &Worker{ID: worker.ID}

	if err := candidate.Probe(context.Background()); err == nil {
		t.Fatal("stale candidate probe succeeded after worker replacement")
	}
}

func TestHostFailoverCandidateRequiresSuccessfulIPv4Probe(t *testing.T) {
	pool := NewPool(&config.Config{})
	controller := &hostFailoverNetworkStub{connected: true}
	worker := &Worker{
		ID: "modem", Config: config.DeviceConfig{ID: "modem", Interface: "wwan1"}, netOverride: controller,
	}
	pool.workers[worker.ID] = worker
	candidate := pool.Candidates([]string{"modem"})[0]

	if err := candidate.Probe(context.Background()); err == nil {
		t.Fatal("candidate without public IPv4 was accepted")
	}
}
