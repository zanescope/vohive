package hostfailover

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

var errProbeFailed = errors.New("probe failed")

type fakeCandidateSource struct {
	candidates []Candidate
}

func (s *fakeCandidateSource) Candidates([]string) []Candidate {
	return append([]Candidate(nil), s.candidates...)
}

type fakeRouteManager struct {
	reconciles  int
	activations []string
	deactivates []string
	activateErr error
}

func (m *fakeRouteManager) Reconcile() error {
	m.reconciles++
	return nil
}

func (m *fakeRouteManager) Activate(_, candidate string, _ int) (RouteLease, error) {
	if m.activateErr != nil {
		return RouteLease{}, m.activateErr
	}
	m.activations = append(m.activations, candidate)
	return RouteLease{CandidateInterface: candidate, platform: candidate}, nil
}

func (m *fakeRouteManager) Deactivate(lease RouteLease) error {
	m.deactivates = append(m.deactivates, lease.CandidateInterface)
	return nil
}

type probeSequence struct {
	results []error
	index   int
}

func (p *probeSequence) Probe(context.Context) error {
	if len(p.results) == 0 {
		return nil
	}
	index := p.index
	if index >= len(p.results) {
		index = len(p.results) - 1
	}
	p.index++
	return p.results[index]
}

func fixedProbe(err error) ProbeFunc {
	return func(context.Context) error { return err }
}

func testOptions(primary ProbeFunc, source CandidateSource, routes RouteManager, now func() time.Time) Options {
	return Options{
		PrimaryInterface:   "eth0",
		CandidateDeviceIDs: []string{"modem-a", "modem-b"},
		ProbeInterval:      time.Second,
		ProbeTimeout:       time.Second,
		FailureThreshold:   2,
		RecoveryThreshold:  2,
		MinimumBackupTime:  10 * time.Second,
		MaximumRouteMetric: 5,
		PrimaryProbe:       primary,
		CandidateSource:    source,
		RouteManager:       routes,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:                now,
	}
}

func TestNewRejectsCandidatesThatNormalizeToEmpty(t *testing.T) {
	now := time.Unix(1000, 0)
	options := testOptions(fixedProbe(nil), &fakeCandidateSource{}, &fakeRouteManager{}, func() time.Time { return now })
	options.CandidateDeviceIDs = []string{"  "}

	if _, err := New(options); err == nil {
		t.Fatal("empty normalized candidate list was accepted")
	}

	options = testOptions(fixedProbe(nil), &fakeCandidateSource{}, &fakeRouteManager{}, func() time.Time { return now })
	options.MinimumBackupTime = -time.Second
	if _, err := New(options); err == nil {
		t.Fatal("negative minimum backup time was accepted")
	}
}

func TestManagerActivatesAfterFailuresAndRestoresAfterStableRecovery(t *testing.T) {
	now := time.Unix(1000, 0)
	primary := &probeSequence{results: []error{errProbeFailed, errProbeFailed, nil, nil}}
	source := &fakeCandidateSource{candidates: []Candidate{{
		DeviceID: "modem-a", Interface: "wwan9", Connected: true, Probe: fixedProbe(nil),
	}}}
	routes := &fakeRouteManager{}
	manager, err := New(testOptions(primary.Probe, source, routes, func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(routes.activations) != 0 {
		t.Fatal("backup activated before failure threshold")
	}
	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := routes.activations; len(got) != 1 || got[0] != "wwan9" {
		t.Fatalf("activations = %v, want [wwan9]", got)
	}

	now = now.Add(5 * time.Second)
	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(routes.deactivates) != 0 {
		t.Fatal("backup removed before recovery threshold and hold-down")
	}
	now = now.Add(6 * time.Second)
	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := routes.deactivates; len(got) != 1 || got[0] != "wwan9" {
		t.Fatalf("deactivates = %v, want [wwan9]", got)
	}
	if snapshot := manager.Snapshot(); snapshot.State != "primary" {
		t.Fatalf("state = %q, want primary", snapshot.State)
	}
}

func TestManagerSelectsFirstCandidateThatActuallyReachesInternet(t *testing.T) {
	now := time.Unix(1000, 0)
	source := &fakeCandidateSource{candidates: []Candidate{
		{DeviceID: "modem-a", Interface: "wwan1", Connected: true, Probe: fixedProbe(errProbeFailed)},
		{DeviceID: "modem-b", Interface: "wwan2", Connected: true, Probe: fixedProbe(nil)},
	}}
	routes := &fakeRouteManager{}
	options := testOptions(fixedProbe(errProbeFailed), source, routes, func() time.Time { return now })
	options.FailureThreshold = 1
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := routes.activations; len(got) != 1 || got[0] != "wwan2" {
		t.Fatalf("activations = %v, want [wwan2]", got)
	}
}

func TestManagerSwitchesAwayFromFailedBackup(t *testing.T) {
	now := time.Unix(1000, 0)
	firstProbe := &probeSequence{results: []error{nil, errProbeFailed}}
	source := &fakeCandidateSource{candidates: []Candidate{
		{DeviceID: "modem-a", Interface: "wwan1", Connected: true, Probe: firstProbe.Probe},
		{DeviceID: "modem-b", Interface: "wwan2", Connected: true, Probe: fixedProbe(nil)},
	}}
	routes := &fakeRouteManager{}
	options := testOptions(fixedProbe(errProbeFailed), source, routes, func() time.Time { return now })
	options.FailureThreshold = 1
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := routes.activations; len(got) != 2 || got[0] != "wwan1" || got[1] != "wwan2" {
		t.Fatalf("activations = %v, want [wwan1 wwan2]", got)
	}
	if got := routes.deactivates; len(got) != 1 || got[0] != "wwan1" {
		t.Fatalf("deactivates = %v, want [wwan1]", got)
	}
}

func TestManagerReportsDegradedWithoutMutatingRoutes(t *testing.T) {
	now := time.Unix(1000, 0)
	source := &fakeCandidateSource{candidates: []Candidate{{
		DeviceID: "modem-a", Interface: "wwan1", Connected: false, Probe: fixedProbe(nil),
	}}}
	routes := &fakeRouteManager{}
	options := testOptions(fixedProbe(errProbeFailed), source, routes, func() time.Time { return now })
	options.FailureThreshold = 1
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot := manager.Snapshot(); snapshot.State != "degraded" {
		t.Fatalf("state = %q, want degraded", snapshot.State)
	}
	if len(routes.activations)+len(routes.deactivates) != 0 {
		t.Fatalf("unexpected route mutations: activate=%v deactivate=%v", routes.activations, routes.deactivates)
	}
}

func TestManagerRunReconcilesStaleRoutes(t *testing.T) {
	now := time.Unix(1000, 0)
	source := &fakeCandidateSource{}
	routes := &fakeRouteManager{}
	manager, err := New(testOptions(fixedProbe(nil), source, routes, func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if routes.reconciles != 1 {
		t.Fatalf("reconciles = %d, want 1", routes.reconciles)
	}
}
