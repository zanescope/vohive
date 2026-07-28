package hostfailover

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type ProbeFunc func(context.Context) error

type PrimaryProbeFunc func(context.Context, string) error

type Candidate struct {
	DeviceID  string
	Interface string
	Connected bool
	Probe     ProbeFunc
}

type CandidateSource interface {
	Candidates(deviceIDs []string) []Candidate
}

type RouteLease struct {
	CandidateInterface string
	platform           any
}

type RouteManager interface {
	Reconcile() error
	ResolvePrimary(excludedInterfaces []string) (string, error)
	Activate(primaryInterface, candidateInterface string, maximumMetric int) (RouteLease, error)
	Deactivate(RouteLease) error
}

type Options struct {
	PrimaryInterface           string
	CandidateDeviceIDs         []string
	CandidateDeviceIDsProvider func() []string
	ProbeInterval              time.Duration
	ProbeTimeout               time.Duration
	FailureThreshold           int
	RecoveryThreshold          int
	MinimumBackupTime          time.Duration
	MaximumRouteMetric         int
	PrimaryProbe               PrimaryProbeFunc
	CandidateSource            CandidateSource
	RouteManager               RouteManager
	Logger                     *slog.Logger
	Now                        func() time.Time
}

type Snapshot struct {
	State             string
	ActiveDeviceID    string
	ActiveInterface   string
	PrimaryFailures   int
	PrimaryRecoveries int
}

type activeRoute struct {
	candidate Candidate
	lease     RouteLease
	since     time.Time
}

type Manager struct {
	options Options

	mu                  sync.Mutex
	active              *activeRoute
	primaryInterface    string
	primaryFailures     int
	primaryRecoveries   int
	noCandidateReported bool
}

func New(options Options) (*Manager, error) {
	options.PrimaryInterface = strings.TrimSpace(options.PrimaryInterface)
	options.CandidateDeviceIDs = normalizedDeviceIDs(options.CandidateDeviceIDs)
	if len(options.CandidateDeviceIDs) == 0 && options.CandidateDeviceIDsProvider == nil {
		return nil, fmt.Errorf("at least one candidate device or a candidate provider is required")
	}
	if options.ProbeInterval <= 0 || options.ProbeTimeout <= 0 {
		return nil, fmt.Errorf("probe interval and timeout must be positive")
	}
	if options.FailureThreshold < 1 || options.RecoveryThreshold < 1 {
		return nil, fmt.Errorf("failure and recovery thresholds must be positive")
	}
	if options.MaximumRouteMetric < 1 {
		return nil, fmt.Errorf("maximum route metric must be positive")
	}
	if options.MinimumBackupTime < 0 {
		return nil, fmt.Errorf("minimum backup time cannot be negative")
	}
	if options.PrimaryProbe == nil || options.CandidateSource == nil || options.RouteManager == nil {
		return nil, fmt.Errorf("primary probe, candidate source, and route manager are required")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Manager{options: options}, nil
}

func normalizedDeviceIDs(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (m *Manager) Run(ctx context.Context) (returnErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.options.RouteManager.Reconcile(); err != nil {
		return fmt.Errorf("remove stale VoHive failover routes: %w", err)
	}
	defer func() {
		m.mu.Lock()
		err := m.deactivateLocked("shutdown")
		m.mu.Unlock()
		if err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()

	m.options.Logger.Info("host network failover monitor started",
		"configured_primary_interface", m.options.PrimaryInterface,
		"candidate_devices", m.candidateDeviceIDs())

	if err := m.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		m.options.Logger.Warn("host network failover check failed", "err", err)
	}
	ticker := time.NewTicker(m.options.ProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := m.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				m.options.Logger.Warn("host network failover check failed", "err", err)
			}
		}
	}
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := Snapshot{
		State:             "primary",
		PrimaryFailures:   m.primaryFailures,
		PrimaryRecoveries: m.primaryRecoveries,
	}
	if m.active != nil {
		snapshot.State = "backup"
		snapshot.ActiveDeviceID = m.active.candidate.DeviceID
		snapshot.ActiveInterface = m.active.candidate.Interface
	} else if m.noCandidateReported {
		snapshot.State = "degraded"
	}
	return snapshot
}

func (m *Manager) tick(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	deviceIDs := m.candidateDeviceIDs()
	if len(deviceIDs) == 0 {
		if err := m.deactivateLocked("disabled"); err != nil {
			return err
		}
		m.primaryFailures = 0
		m.primaryRecoveries = 0
		m.noCandidateReported = false
		return nil
	}

	candidates := m.options.CandidateSource.Candidates(deviceIDs)
	primaryInterface, primaryErr := m.resolvePrimaryLocked(candidates)
	if primaryInterface != "" {
		primaryErr = m.probePrimary(ctx, primaryInterface)
	}
	primaryHealthy := primaryErr == nil
	if m.active == nil {
		return m.tickPrimaryLocked(ctx, deviceIDs, candidates, primaryHealthy, primaryErr)
	}
	return m.tickBackupLocked(ctx, candidates, primaryHealthy, primaryErr)
}

func (m *Manager) resolvePrimaryLocked(candidates []Candidate) (string, error) {
	if configured := strings.TrimSpace(m.options.PrimaryInterface); configured != "" {
		m.primaryInterface = configured
		return configured, nil
	}

	excluded := candidateInterfaces(candidates)
	if interfaceInList(m.primaryInterface, excluded) {
		m.primaryInterface = ""
	}
	resolved, err := m.options.RouteManager.ResolvePrimary(excluded)
	if err == nil && strings.TrimSpace(resolved) != "" {
		resolved = strings.TrimSpace(resolved)
		if resolved != m.primaryInterface {
			m.options.Logger.Info("primary host interface auto-discovered",
				"primary_interface", resolved)
		}
		m.primaryInterface = resolved
		return m.primaryInterface, nil
	}
	if m.primaryInterface != "" {
		return m.primaryInterface, nil
	}
	if err == nil {
		err = fmt.Errorf("no non-candidate IPv4 default route is available")
	}
	return "", fmt.Errorf("discover primary host interface: %w", err)
}

func candidateInterfaces(candidates []Candidate) []string {
	interfaces := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		iface := strings.TrimSpace(candidate.Interface)
		if iface == "" || interfaceInList(iface, interfaces) {
			continue
		}
		interfaces = append(interfaces, iface)
	}
	return interfaces
}

func interfaceInList(value string, interfaces []string) bool {
	value = strings.TrimSpace(value)
	for _, item := range interfaces {
		if strings.TrimSpace(item) == value {
			return true
		}
	}
	return false
}

func (m *Manager) tickPrimaryLocked(ctx context.Context, deviceIDs []string, candidates []Candidate, healthy bool, probeErr error) error {
	m.primaryRecoveries = 0
	if healthy {
		if m.primaryFailures > 0 || m.noCandidateReported {
			m.options.Logger.Info("primary host network is healthy",
				"primary_interface", m.primaryInterface)
		}
		m.primaryFailures = 0
		m.noCandidateReported = false
		return nil
	}

	if m.primaryFailures < m.options.FailureThreshold {
		m.primaryFailures++
	}
	if m.primaryFailures < m.options.FailureThreshold {
		m.options.Logger.Warn("primary host network probe failed",
			"primary_interface", m.primaryInterface,
			"failure_count", m.primaryFailures,
			"failure_threshold", m.options.FailureThreshold,
			"err", probeErr)
		return nil
	}

	candidate, ok := m.selectCandidateLocked(ctx, candidates, "")
	if !ok {
		if !m.noCandidateReported {
			m.options.Logger.Error("primary host network is down and no usable backup device is available",
				"primary_interface", m.primaryInterface,
				"candidate_devices", deviceIDs)
			m.noCandidateReported = true
		}
		return nil
	}
	if err := m.activateLocked(candidate); err != nil {
		m.primaryFailures = 0
		return err
	}
	return nil
}

func (m *Manager) tickBackupLocked(ctx context.Context, candidates []Candidate, primaryHealthy bool, primaryErr error) error {
	if primaryHealthy {
		if m.primaryRecoveries < m.options.RecoveryThreshold {
			m.primaryRecoveries++
		}
	} else {
		if m.primaryRecoveries > 0 {
			m.options.Logger.Warn("primary host network recovery was not stable",
				"primary_interface", m.primaryInterface,
				"err", primaryErr)
		}
		m.primaryRecoveries = 0
	}

	if primaryHealthy &&
		m.primaryRecoveries >= m.options.RecoveryThreshold &&
		m.options.Now().Sub(m.active.since) >= m.options.MinimumBackupTime {
		if err := m.deactivateLocked("primary_recovered"); err != nil {
			return err
		}
		m.primaryFailures = 0
		m.primaryRecoveries = 0
		m.noCandidateReported = false
		return nil
	}

	current := m.active.candidate
	if candidate, ok := m.findCurrentCandidateLocked(candidates, current); ok && m.probe(ctx, candidate.Probe) == nil {
		return nil
	}

	m.options.Logger.Warn("active host network backup is no longer usable",
		"device_id", current.DeviceID,
		"interface", current.Interface)
	replacement, replacementOK := m.selectCandidateLocked(ctx, candidates, candidateKey(current))
	if err := m.deactivateLocked("backup_unusable"); err != nil {
		return err
	}
	if !replacementOK {
		m.primaryFailures = m.options.FailureThreshold
		m.noCandidateReported = true
		return nil
	}
	if err := m.activateLocked(replacement); err != nil {
		m.primaryFailures = 0
		return err
	}
	return nil
}

func (m *Manager) findCurrentCandidateLocked(candidates []Candidate, current Candidate) (Candidate, bool) {
	for _, candidate := range candidates {
		if candidateKey(candidate) == candidateKey(current) && candidate.Connected && candidate.Probe != nil {
			return candidate, true
		}
	}
	return Candidate{}, false
}

func (m *Manager) selectCandidateLocked(ctx context.Context, candidates []Candidate, excludedKey string) (Candidate, bool) {
	for _, candidate := range candidates {
		candidate.DeviceID = strings.TrimSpace(candidate.DeviceID)
		candidate.Interface = strings.TrimSpace(candidate.Interface)
		if candidate.DeviceID == "" || candidate.Interface == "" || !candidate.Connected || candidate.Probe == nil {
			continue
		}
		if candidateKey(candidate) == excludedKey {
			continue
		}
		if err := m.probe(ctx, candidate.Probe); err != nil {
			m.options.Logger.Debug("host failover candidate probe failed",
				"device_id", candidate.DeviceID,
				"interface", candidate.Interface,
				"err", err)
			continue
		}
		return candidate, true
	}
	return Candidate{}, false
}

func (m *Manager) candidateDeviceIDs() []string {
	if m.options.CandidateDeviceIDsProvider != nil {
		return normalizedDeviceIDs(m.options.CandidateDeviceIDsProvider())
	}
	return append([]string(nil), m.options.CandidateDeviceIDs...)
}

func candidateKey(candidate Candidate) string {
	return strings.TrimSpace(candidate.DeviceID) + "\x00" + strings.TrimSpace(candidate.Interface)
}

func (m *Manager) probePrimary(ctx context.Context, interfaceName string) error {
	probeCtx, cancel := context.WithTimeout(ctx, m.options.ProbeTimeout)
	defer cancel()
	return m.options.PrimaryProbe(probeCtx, interfaceName)
}

func (m *Manager) probe(ctx context.Context, probe ProbeFunc) error {
	if probe == nil {
		return fmt.Errorf("probe is unavailable")
	}
	probeCtx, cancel := context.WithTimeout(ctx, m.options.ProbeTimeout)
	defer cancel()
	return probe(probeCtx)
}

func (m *Manager) activateLocked(candidate Candidate) error {
	lease, err := m.options.RouteManager.Activate(
		m.primaryInterface,
		candidate.Interface,
		m.options.MaximumRouteMetric,
	)
	if err != nil {
		return fmt.Errorf("activate host failover through %s/%s: %w", candidate.DeviceID, candidate.Interface, err)
	}
	m.active = &activeRoute{candidate: candidate, lease: lease, since: m.options.Now()}
	m.primaryRecoveries = 0
	m.noCandidateReported = false
	m.options.Logger.Warn("host network failover activated",
		"device_id", candidate.DeviceID,
		"interface", candidate.Interface,
		"primary_interface", m.primaryInterface)
	return nil
}

func (m *Manager) deactivateLocked(reason string) error {
	if m.active == nil {
		return nil
	}
	active := m.active
	if err := m.options.RouteManager.Deactivate(active.lease); err != nil {
		return fmt.Errorf("deactivate host failover route through %s: %w", active.candidate.Interface, err)
	}
	m.active = nil
	m.options.Logger.Info("host network failover deactivated",
		"reason", reason,
		"device_id", active.candidate.DeviceID,
		"interface", active.candidate.Interface)
	return nil
}
