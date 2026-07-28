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
	reconciles          int
	activations         []string
	deactivates         []string
	activationPrimaries []string
	activateErr         error
	primaryInterface    string
	resolveErr          error
	excluded            [][]string
}

func (m *fakeRouteManager) Reconcile() error {
	m.reconciles++
	return nil
}

func (m *fakeRouteManager) ResolvePrimary(excludedInterfaces []string) (string, error) {
	m.excluded = append(m.excluded, append([]string(nil), excludedInterfaces...))
	if m.resolveErr != nil {
		return "", m.resolveErr
	}
	return m.primaryInterface, nil
}

func (m *fakeRouteManager) Activate(primary, candidate string, _ int) (RouteLease, error) {
	if m.activateErr != nil {
		return RouteLease{}, m.activateErr
	}
	m.activations = append(m.activations, candidate)
	m.activationPrimaries = append(m.activationPrimaries, primary)
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
		PrimaryProbe: func(ctx context.Context, _ string) error {
			return primary(ctx)
		},
		CandidateSource: source,
		RouteManager:    routes,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:             now,
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

func TestManagerDynamicallyEnablesAndDisablesFromCandidateProvider(t *testing.T) {
	now := time.Unix(1000, 0)
	deviceIDs := []string(nil)
	source := &fakeCandidateSource{candidates: []Candidate{{
		DeviceID: "modem-a", Interface: "wwan9", Connected: true, Probe: fixedProbe(nil),
	}}}
	routes := &fakeRouteManager{}
	options := testOptions(fixedProbe(errProbeFailed), source, routes, func() time.Time { return now })
	options.CandidateDeviceIDs = nil
	options.CandidateDeviceIDsProvider = func() []string {
		return append([]string(nil), deviceIDs...)
	}
	options.FailureThreshold = 1
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(routes.activations) != 0 {
		t.Fatal("disabled manager mutated routes")
	}

	deviceIDs = []string{"modem-a"}
	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := routes.activations; len(got) != 1 || got[0] != "wwan9" {
		t.Fatalf("activations = %v, want [wwan9]", got)
	}

	deviceIDs = nil
	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := routes.deactivates; len(got) != 1 || got[0] != "wwan9" {
		t.Fatalf("deactivates = %v, want [wwan9]", got)
	}
}

func TestManagerAutoDiscoversPrimaryAndExcludesCandidateInterfaces(t *testing.T) {
	now := time.Unix(1000, 0)
	source := &fakeCandidateSource{candidates: []Candidate{
		{DeviceID: "modem-a", Interface: "wwan9", Connected: true, Probe: fixedProbe(nil)},
		{DeviceID: "modem-b", Interface: "wwan10", Connected: true, Probe: fixedProbe(nil)},
	}}
	routes := &fakeRouteManager{primaryInterface: "eth0"}
	options := testOptions(fixedProbe(nil), source, routes, func() time.Time { return now })
	options.PrimaryInterface = ""
	probedInterface := ""
	options.PrimaryProbe = func(_ context.Context, interfaceName string) error {
		probedInterface = interfaceName
		return nil
	}
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if probedInterface != "eth0" {
		t.Fatalf("probed interface = %q, want eth0", probedInterface)
	}
	if len(routes.excluded) != 1 {
		t.Fatalf("resolve calls = %d, want 1", len(routes.excluded))
	}
	got := routes.excluded[0]
	if len(got) != 2 || got[0] != "wwan9" || got[1] != "wwan10" {
		t.Fatalf("excluded interfaces = %v, want [wwan9 wwan10]", got)
	}
}

func TestManagerCanActivateWhenHostStartsWithoutPrimaryRoute(t *testing.T) {
	now := time.Unix(1000, 0)
	source := &fakeCandidateSource{candidates: []Candidate{{
		DeviceID: "modem-a", Interface: "wwan9", Connected: true, Probe: fixedProbe(nil),
	}}}
	routes := &fakeRouteManager{resolveErr: errors.New("no primary route")}
	options := testOptions(fixedProbe(nil), source, routes, func() time.Time { return now })
	options.PrimaryInterface = ""
	options.FailureThreshold = 1
	primaryProbeCalls := 0
	options.PrimaryProbe = func(context.Context, string) error {
		primaryProbeCalls++
		return nil
	}
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if primaryProbeCalls != 0 {
		t.Fatalf("primary probe calls = %d, want 0 without an interface", primaryProbeCalls)
	}
	if got := routes.activations; len(got) != 1 || got[0] != "wwan9" {
		t.Fatalf("activations = %v, want [wwan9]", got)
	}
	if got := routes.activationPrimaries; len(got) != 1 || got[0] != "" {
		t.Fatalf("activation primaries = %v, want empty auto-discovery fallback", got)
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
