package mbimcore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/internal/netprobe"
	"github.com/zanescope/vohive/pkg/logger"
	"github.com/zanescope/vohive/pkg/mbim"
)

const (
	dataSessionID                    = 0
	defaultDataConnectCommandTimeout = 120 * time.Second
)

var (
	ErrNetworkNotRegistered = errors.New("mbimcore: network not registered")
	ErrDataConnectStopped   = errors.New("mbimcore: data connect stopped")
	publicIPFallbackDNSv4   = []string{
		"119.29.29.29",
		"223.5.5.5",
		"223.6.6.6",
		"1.1.1.1",
		"8.8.8.8",
		"9.9.9.9",
	}
	publicIPFallbackDNSv6 = []string{
		"2402:4e00::",
		"2402:4e00:1::",
		"2400:3200::1",
		"2400:3200:baba::1",
		"2606:4700:4700::1111",
		"2001:4860:4860::8888",
	}
)

func defaultPublicIPProbeURLs() ([]string, []string) {
	defaults := config.DefaultPublicIPProbeConfig()
	return append([]string(nil), defaults.IPv4URLs...), append([]string(nil), defaults.IPv6URLs...)
}

const (
	registerStateHome    uint32 = 3
	registerStateRoaming uint32 = 4
)

const (
	mbimStatusBusy                 uint32 = 0x01
	mbimStatusMaxActivatedContexts uint32 = 0x0d
)

type DataConfig struct {
	APN       string
	Interface string
	IPVersion string
	Username  string
	Password  string
}

func ipTypeFromVersion(v string) uint32 {
	enableV4, enableV6, err := config.ResolveIPFamily(v)
	switch {
	case err != nil, enableV4 && enableV6:
		return mbim.ContextIPTypeIPv4v6
	case enableV6:
		return mbim.ContextIPTypeIPv6
	default:
		return mbim.ContextIPTypeIPv4
	}
}

func cloneIPConfiguration(ipc mbim.IPConfiguration) mbim.IPConfiguration {
	cloned := ipc
	cloned.IPv4DNS = append([]string(nil), ipc.IPv4DNS...)
	cloned.IPv6DNS = append([]string(nil), ipc.IPv6DNS...)
	return cloned
}

func equalIPConfiguration(a, b mbim.IPConfiguration) bool {
	if a.IPv4Address != b.IPv4Address ||
		a.IPv4PrefixLength != b.IPv4PrefixLength ||
		a.IPv4Gateway != b.IPv4Gateway ||
		a.IPv4MTU != b.IPv4MTU ||
		a.IPv6Address != b.IPv6Address ||
		a.IPv6PrefixLength != b.IPv6PrefixLength ||
		a.IPv6Gateway != b.IPv6Gateway ||
		a.IPv6MTU != b.IPv6MTU ||
		len(a.IPv4DNS) != len(b.IPv4DNS) ||
		len(a.IPv6DNS) != len(b.IPv6DNS) {
		return false
	}
	for i := range a.IPv4DNS {
		if a.IPv4DNS[i] != b.IPv4DNS[i] {
			return false
		}
	}
	for i := range a.IPv6DNS {
		if a.IPv6DNS[i] != b.IPv6DNS[i] {
			return false
		}
	}
	return true
}

// clearAppliedIPConfigLocked invalidates the last successfully installed
// snapshot. Callers must hold m.mu.
func (m *Manager) clearAppliedIPConfigLocked() {
	m.appliedIPConfig = mbim.IPConfiguration{}
	m.hasAppliedIPConfig = false
}

func (m *Manager) Connect() error {
	return m.ConnectContext(context.Background())
}

// ConnectContext is the cancellable data-connect form used by lifecycle code.
// Connect preserves the NetworkController compatibility API.
func (m *Manager) ConnectContext(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	m.dataMu.Lock()
	err := m.runConnectLocked(parent)
	callbacks := m.takePendingDataCallbacksLocked()
	m.dataMu.Unlock()
	runDataCallbacks(callbacks)
	return err
}

func (m *Manager) runConnectLocked(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	m.mu.Lock()
	if m.dataStopRequested {
		m.mu.Unlock()
		cancel()
		return ErrDataConnectStopped
	}
	m.activeDataConnectCancel = cancel
	m.mu.Unlock()

	err := m.connectLocked(ctx)
	cancel()
	m.mu.Lock()
	m.activeDataConnectCancel = nil
	m.mu.Unlock()
	return err
}

func (m *Manager) beginDataStop() {
	m.mu.Lock()
	m.dataStopCount++
	m.dataStopRequested = m.dataStopCount > 0
	connectCancel := m.activeDataConnectCancel
	refreshCancel := m.activeIPConfigRefreshCancel
	m.mu.Unlock()
	if connectCancel != nil {
		connectCancel()
	}
	if refreshCancel != nil {
		refreshCancel()
	}
}

func (m *Manager) endDataStop() {
	m.mu.Lock()
	if m.dataStopCount > 0 {
		m.dataStopCount--
	}
	m.dataStopRequested = m.dataStopCount > 0
	m.mu.Unlock()
}

func (m *Manager) queueDataConnectedCallbackLocked() {
	m.mu.Lock()
	cb := m.dataConnectedCB
	m.mu.Unlock()
	if cb != nil {
		m.pendingDataCallbacks = append(m.pendingDataCallbacks, cb)
	}
}

func (m *Manager) queueDataDisconnectedCallbackLocked() {
	m.mu.Lock()
	cb := m.dataDisconnectedCB
	m.mu.Unlock()
	if cb != nil {
		m.pendingDataCallbacks = append(m.pendingDataCallbacks, cb)
	}
}

func (m *Manager) queueIPConfigChangedCallbackLocked() {
	m.mu.Lock()
	cb := m.ipConfigChangedCB
	m.mu.Unlock()
	if cb != nil {
		m.pendingDataCallbacks = append(m.pendingDataCallbacks, cb)
	}
}

func (m *Manager) takePendingDataCallbacksLocked() []func() {
	callbacks := m.pendingDataCallbacks
	m.pendingDataCallbacks = nil
	return callbacks
}

func runDataCallbacks(callbacks []func()) {
	for _, callback := range callbacks {
		if callback != nil {
			callback()
		}
	}
}

// connectLocked performs the CONNECT/IP_CONFIGURATION exchange. Callers must
// hold m.dataMu.
func (m *Manager) connectLocked(parent context.Context) error {
	d, err := m.device()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(parent, m.connectTimeoutOrDefault())
	defer cancel()
	if err := m.ensureRegistered(ctx, d); err != nil {
		return err
	}

	m.mu.Lock()
	cfg := m.dataCfg
	nc := m.netcfg
	m.desiredConnection = true
	m.dataEpoch++
	m.mu.Unlock()
	if nc == nil {
		nc = realNetConfigurator{}
	}

	st, err := m.activateDataSessionWithRetry(ctx, d, cfg)
	if errors.Is(err, context.DeadlineExceeded) {
		recoveryCtx, recoveryCancel := context.WithTimeout(parent, m.connectTimeoutOrDefault())
		defer recoveryCancel()
		if reopenErr := m.reopenControlPlaneForDataConnect(recoveryCtx); reopenErr != nil {
			return fmt.Errorf("mbimcore: CONNECT activate timeout recovery: %w", reopenErr)
		}
		ctx = recoveryCtx
		d, err = m.device()
		if err != nil {
			return err
		}
		if err := m.ensureRegistered(ctx, d); err != nil {
			return err
		}
		st, err = m.activateDataSessionWithRetry(ctx, d, cfg)
	}
	if err != nil {
		recovered, recoverErr := m.recoverStaleDataSession(ctx, d, err)
		if recoverErr != nil {
			return recoverErr
		}
		if recovered {
			st, err = m.activateDataSessionWithRetry(ctx, d, cfg)
		}
	}
	if err != nil {
		return fmt.Errorf("mbimcore: CONNECT activate: %w", err)
	}
	if st.ActivationState != mbim.ActivationStateActivated {
		return fmt.Errorf("mbimcore: CONNECT not activated (state=%d nwerror=%d)", st.ActivationState, st.NwError)
	}

	ipc, err := mbim.QueryIPConfiguration(ctx, d, dataSessionID)
	if err != nil {
		return m.cleanupActivatedDataSessionLocked(parent, d, nc, cfg.Interface, fmt.Errorf("mbimcore: IP_CONFIGURATION: %w", err))
	}
	if ipc.IPv4Address == "" && ipc.IPv6Address == "" {
		return m.cleanupActivatedDataSessionLocked(parent, d, nc, cfg.Interface, fmt.Errorf("mbimcore: no IP assigned"))
	}
	if err := m.applyIPConfig(nc, cfg.Interface, ipc); err != nil {
		return m.cleanupActivatedDataSessionLocked(parent, d, nc, cfg.Interface, fmt.Errorf("mbimcore: apply IP config: %w", err))
	}

	m.mu.Lock()
	m.privateIPv4 = ipc.IPv4Address
	m.privateIPv6 = ipc.IPv6Address
	m.ipv4DNS = append([]string(nil), ipc.IPv4DNS...)
	m.ipv6DNS = append([]string(nil), ipc.IPv6DNS...)
	m.appliedIPConfig = cloneIPConfiguration(ipc)
	m.hasAppliedIPConfig = true
	m.connected = true
	m.mu.Unlock()
	m.queueDataConnectedCallbackLocked()
	return nil
}

func (m *Manager) cleanupActivatedDataSessionLocked(parent context.Context, d *mbim.Device, nc netConfigurator, iface string, cause error) error {
	m.mu.Lock()
	m.connected = false
	m.privateIPv4, m.privateIPv6 = "", ""
	m.ipv4DNS, m.ipv6DNS = nil, nil
	m.clearAppliedIPConfigLocked()
	m.dataEpoch++
	m.markExpectedDeactivationLocked()
	m.mu.Unlock()
	m.queueDataDisconnectedCallbackLocked()

	if parent == nil {
		parent = context.Background()
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(parent, 20*time.Second)
	_, deactivateErr := mbim.Connect(cleanupCtx, d, dataSessionID, mbim.ActivationCommandDeactivate, "", "", "", mbim.AuthProtocolNone, mbim.ContextIPTypeDefault)
	cleanupCancel()
	if deactivateErr != nil {
		logger.Debug("[mbim] failed data-session deactivate cleanup failed",
			"control_device", m.controlDevice, "err", deactivateErr)
	}
	if err := flushNetwork(nc, iface); err != nil {
		logger.Debug("[mbim] failed data-session network cleanup failed",
			"control_device", m.controlDevice, "err", err)
	}
	return cause
}

func (m *Manager) connectTimeoutOrDefault() time.Duration {
	if m.connectTimeout > 0 {
		return m.connectTimeout
	}
	return defaultDataConnectCommandTimeout
}

func activateDataSession(ctx context.Context, d *mbim.Device, cfg DataConfig) (mbim.ConnectState, error) {
	return mbim.Connect(ctx, d, dataSessionID, mbim.ActivationCommandActivate, cfg.APN, cfg.Username, cfg.Password, mbim.AuthProtocolNone, ipTypeFromVersion(cfg.IPVersion))
}

func (m *Manager) activateDataSessionWithRetry(ctx context.Context, d *mbim.Device, cfg DataConfig) (mbim.ConnectState, error) {
	attempts := m.activateMaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	delay := m.activateRetryDelay
	if delay <= 0 {
		delay = time.Second
	}
	var st mbim.ConnectState
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		st, err = activateDataSession(ctx, d, cfg)
		if err == nil || !isConnectStatus(err, mbimStatusBusy) || attempt == attempts {
			return st, err
		}
		if sleepErr := sleepContext(ctx, delay); sleepErr != nil {
			return st, sleepErr
		}
		if delay < 5*time.Second {
			delay *= 2
		}
	}
	return st, err
}

func (m *Manager) recoverStaleDataSession(ctx context.Context, d *mbim.Device, activateErr error) (bool, error) {
	if !isConnectStatus(activateErr, mbimStatusMaxActivatedContexts) {
		return false, nil
	}
	m.mu.Lock()
	m.markExpectedDeactivationLocked()
	m.mu.Unlock()
	if _, err := mbim.Connect(ctx, d, dataSessionID, mbim.ActivationCommandDeactivate, "", "", "", mbim.AuthProtocolNone, mbim.ContextIPTypeDefault); err != nil {
		return true, fmt.Errorf("mbimcore: CONNECT recover stale session: deactivate after max activated contexts: %w", err)
	}
	return true, nil
}

func isConnectStatus(err error, status uint32) bool {
	var se *mbim.StatusError
	return errors.As(err, &se) && se.Op == "CONNECT" && se.Status == status
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *Manager) reopenControlPlaneForDataConnect(ctx context.Context) error {
	m.mu.Lock()
	dev, mon := m.dev, m.mon
	waiter := m.ussdWaiter
	healthDone := m.healthDone
	iface := m.dataCfg.Interface
	nc := m.netcfg
	hadData := m.connected || m.privateIPv4 != "" || m.privateIPv6 != "" || len(m.ipv4DNS) > 0 || len(m.ipv6DNS) > 0
	m.dev, m.mon, m.ussdWaiter = nil, nil, nil
	m.connected = false
	m.privateIPv4, m.privateIPv6 = "", ""
	m.ipv4DNS, m.ipv6DNS = nil, nil
	m.clearAppliedIPConfigLocked()
	m.dataEpoch++
	m.mu.Unlock()
	m.recoveryGate.Store(false)
	if hadData {
		m.queueDataDisconnectedCallbackLocked()
	}

	notifyUSSDWaiter(waiter, errUSSDClosed)
	if mon != nil {
		mon.Stop()
	}
	if healthDone != nil {
		m.healthOnce.Do(func() { close(healthDone) })
	}

	if nc != nil && iface != "" {
		if err := flushNetwork(nc, iface); err != nil {
			logger.Debug("[mbim] reopen old network cleanup failed", "control_device", m.controlDevice, "err", err)
		}
	}
	if dev != nil {
		if err := dev.Close(); err != nil {
			logger.Debug("[mbim] reopen old control close failed", "control_device", m.controlDevice, "err", err)
		}
	}
	return m.Open(ctx)
}

func (m *Manager) Disconnect() error {
	m.beginDataStop()
	m.dataMu.Lock()
	err := m.disconnectLocked()
	callbacks := m.takePendingDataCallbacksLocked()
	m.endDataStop()
	m.dataMu.Unlock()
	runDataCallbacks(callbacks)
	return err
}

// disconnectLocked performs the deactivate/flush sequence. Callers must hold
// m.dataMu.
func (m *Manager) disconnectLocked() error {
	m.mu.Lock()
	wasDesired := m.desiredConnection
	connected := m.connected
	hadData := connected || m.privateIPv4 != "" || m.privateIPv6 != "" || len(m.ipv4DNS) > 0 || len(m.ipv6DNS) > 0
	iface := m.dataCfg.Interface
	nc := m.netcfg
	reconnectCancel := m.reconnectCancel
	m.desiredConnection = false
	m.connected = false
	m.privateIPv4, m.privateIPv6 = "", ""
	m.ipv4DNS, m.ipv6DNS = nil, nil
	m.clearAppliedIPConfigLocked()
	m.dataEpoch++
	if connected || wasDesired {
		m.markExpectedDeactivationLocked()
	} else {
		m.expectedDeactivationEpoch = 0
		m.expectedDeactivationUntil = time.Time{}
	}
	m.reconnectCancel = nil
	m.mu.Unlock()
	if reconnectCancel != nil {
		reconnectCancel()
	}
	if hadData || wasDesired {
		m.queueDataDisconnectedCallbackLocked()
	}

	var result error
	if connected || wasDesired {
		d, err := m.device()
		if err != nil {
			result = errors.Join(result, err)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_, err = mbim.Connect(ctx, d, dataSessionID, mbim.ActivationCommandDeactivate, "", "", "", mbim.AuthProtocolNone, mbim.ContextIPTypeDefault)
			cancel()
			if err != nil {
				result = errors.Join(result, fmt.Errorf("mbimcore: CONNECT deactivate: %w", err))
			}
		}
	}
	if (hadData || wasDesired) && nc != nil && iface != "" {
		if err := flushNetwork(nc, iface); err != nil {
			logger.Debug("[mbim] disconnect network cleanup failed", "control_device", m.controlDevice, "err", err)
		}
	}
	return result
}

// markExpectedDeactivationLocked protects a freshly established bearer from
// delayed indications emitted by a proactive deactivate. The next connect
// advances dataEpoch once, so the grace token deliberately covers that epoch.
// Callers must hold m.mu.
func (m *Manager) markExpectedDeactivationLocked() {
	through := m.dataEpoch + 1
	if through > m.expectedDeactivationEpoch {
		m.expectedDeactivationEpoch = through
	}
	deadline := time.Now().Add(5 * time.Second)
	if deadline.After(m.expectedDeactivationUntil) {
		m.expectedDeactivationUntil = deadline
	}
}

func (m *Manager) RotateIP() error {
	m.dataMu.Lock()
	if !m.IsConnected() {
		m.dataMu.Unlock()
		return fmt.Errorf("mbimcore: network_not_connected")
	}
	err := m.disconnectLocked()
	if err != nil {
		err = fmt.Errorf("mbimcore: rotate disconnect: %w", err)
	} else {
		err = m.runConnectLocked(context.Background())
	}
	callbacks := m.takePendingDataCallbacksLocked()
	m.dataMu.Unlock()
	runDataCallbacks(callbacks)
	return err
}

func (m *Manager) IsConnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

func (m *Manager) GetPrivateIP() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.privateIPv4
}

func (m *Manager) GetPrivateIPv6() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.privateIPv6
}

// SetPublicIPProbeURLs replaces the ordered public-IP source lists. An empty
// family falls back to VoHive's validated built-in defaults.
func (m *Manager) SetPublicIPProbeURLs(ipv4, ipv6 []string) {
	defaultV4, defaultV6 := defaultPublicIPProbeURLs()
	if len(ipv4) == 0 {
		ipv4 = defaultV4
	}
	if len(ipv6) == 0 {
		ipv6 = defaultV6
	}
	m.mu.Lock()
	m.publicIPv4URLs = append([]string(nil), ipv4...)
	m.publicIPv6URLs = append([]string(nil), ipv6...)
	m.mu.Unlock()
}

func (m *Manager) GetPublicIPv4AndV6NoCache() (publicV4 string, publicV6 string) {
	return m.GetPublicIPv4AndV6Context(context.Background())
}

func bearerAddressMatchesFamily(value string, family netprobe.Family) bool {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil || !ip.IsGlobalUnicast() {
		return false
	}
	if family == netprobe.FamilyV4 {
		return ip.To4() != nil
	}
	if family == netprobe.FamilyV6 {
		return ip.To4() == nil
	}
	return false
}

// GetPublicIPv4AndV6Context probes only address families present on the actual
// bearer. Cancellation stops queued DNS and HTTP work during disconnect,
// worker replacement, or shutdown.
func (m *Manager) GetPublicIPv4AndV6Context(ctx context.Context) (publicV4 string, publicV6 string) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	cfg := m.dataCfg
	ipv4URLs := append([]string(nil), m.publicIPv4URLs...)
	ipv6URLs := append([]string(nil), m.publicIPv6URLs...)
	privateIPv4 := m.privateIPv4
	privateIPv6 := m.privateIPv6
	epoch := m.dataEpoch
	lookupCache := m.publicIPLookupCache
	if lookupCache == nil || m.publicIPLookupEpoch != epoch {
		lookupCache = netprobe.NewLookupCache(m.lookupPublicIPHost, 30*time.Second)
		m.publicIPLookupCache = lookupCache
		m.publicIPLookupEpoch = epoch
	}
	m.mu.Unlock()

	enableV4 := bearerAddressMatchesFamily(privateIPv4, netprobe.FamilyV4)
	enableV6 := bearerAddressMatchesFamily(privateIPv6, netprobe.FamilyV6)
	if !enableV4 && !enableV6 {
		return "", ""
	}

	prober := netprobe.New(netprobe.Config{
		Interface:    cfg.Interface,
		IPv4URLs:     ipv4URLs,
		IPv6URLs:     ipv6URLs,
		Timeout:      10 * time.Second,
		LookupFamily: lookupCache.Lookup,
	})
	var wg sync.WaitGroup
	if enableV4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := prober.ProbeResult(ctx, netprobe.FamilyV4)
			if err != nil {
				logger.Debug("[mbim] public IP probe failed",
					"control_device", m.controlDevice, "family", netprobe.FamilyV4, "err", err)
				return
			}
			publicV4 = result.IP
			logger.Debug("[mbim] public IP probe succeeded",
				"control_device", m.controlDevice, "family", netprobe.FamilyV4, "source", result.Source)
		}()
	}
	if enableV6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := prober.ProbeResult(ctx, netprobe.FamilyV6)
			if err != nil {
				logger.Debug("[mbim] public IP probe failed",
					"control_device", m.controlDevice, "family", netprobe.FamilyV6, "err", err)
				return
			}
			publicV6 = result.IP
			logger.Debug("[mbim] public IP probe succeeded",
				"control_device", m.controlDevice, "family", netprobe.FamilyV6, "source", result.Source)
		}()
	}
	wg.Wait()
	return publicV4, publicV6
}

func (m *Manager) lookupPublicIPHost(ctx context.Context, host string, family netprobe.Family) ([]string, error) {
	return m.lookupPublicIPHostWithQuery(ctx, host, family, queryDataDNS)
}

type dataDNSQueryFunc func(context.Context, string, netprobe.Family, []string, *net.Dialer) ([]string, error)

func (m *Manager) lookupPublicIPHostWithQuery(ctx context.Context, host string, family netprobe.Family, query dataDNSQueryFunc) ([]string, error) {
	host = strings.TrimSpace(host)
	if ip := net.ParseIP(host); ip != nil {
		return []string{ip.String()}, nil
	}
	if host == "" {
		return nil, fmt.Errorf("mbimcore: DNS host is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	iface := m.dataCfg.Interface
	ipv4DNS := append([]string(nil), m.ipv4DNS...)
	ipv6DNS := append([]string(nil), m.ipv6DNS...)
	hasIPv4Bearer := bearerAddressMatchesFamily(m.privateIPv4, netprobe.FamilyV4)
	hasIPv6Bearer := bearerAddressMatchesFamily(m.privateIPv6, netprobe.FamilyV6)
	m.mu.Unlock()

	if strings.TrimSpace(iface) == "" {
		return nil, netprobe.ErrInterfaceRequired
	}

	if family == netprobe.FamilyAny {
		v4, v4Err := m.lookupPublicIPHostWithQuery(ctx, host, netprobe.FamilyV4, query)
		v6, v6Err := m.lookupPublicIPHostWithQuery(ctx, host, netprobe.FamilyV6, query)
		if len(v4)+len(v6) > 0 {
			return append(v4, v6...), nil
		}
		return nil, errors.Join(v4Err, v6Err)
	}

	servers := orderedDataDNSServerEndpoints(
		family,
		ipv4DNS,
		ipv6DNS,
		hasIPv4Bearer,
		hasIPv6Bearer,
		iface,
	)
	if len(servers) == 0 {
		return nil, fmt.Errorf("mbimcore: no DNS server for %s", family)
	}
	return query(ctx, host, family, servers, dataBoundDialer(iface, 1200*time.Millisecond))
}

const maxCarrierDNSBeforeFallback = 2

func orderedDataDNSServerEndpoints(
	family netprobe.Family,
	ipv4DNS, ipv6DNS []string,
	hasIPv4Bearer, hasIPv6Bearer bool,
	iface string,
) []string {
	var carrierRaw, fallbackRaw []string
	switch family {
	case netprobe.FamilyV4:
		carrierRaw = append(carrierRaw, ipv4DNS...)
		carrierRaw = append(carrierRaw, ipv6DNS...)
		if hasIPv4Bearer {
			fallbackRaw = append(fallbackRaw, publicIPFallbackDNSv4...)
		}
		if hasIPv6Bearer {
			fallbackRaw = append(fallbackRaw, publicIPFallbackDNSv6...)
		}
	case netprobe.FamilyV6:
		carrierRaw = append(carrierRaw, ipv6DNS...)
		carrierRaw = append(carrierRaw, ipv4DNS...)
		if hasIPv6Bearer {
			fallbackRaw = append(fallbackRaw, publicIPFallbackDNSv6...)
		}
		if hasIPv4Bearer {
			fallbackRaw = append(fallbackRaw, publicIPFallbackDNSv4...)
		}
	default:
		return nil
	}

	carrierEndpoints := dnsServerEndpoints(carrierRaw, iface)
	if len(carrierEndpoints) > maxCarrierDNSBeforeFallback {
		carrierEndpoints = carrierEndpoints[:maxCarrierDNSBeforeFallback]
	}
	fallbackEndpoints := dnsServerEndpoints(fallbackRaw, iface)

	out := make([]string, 0, len(carrierEndpoints)+len(fallbackEndpoints))
	seen := make(map[string]struct{}, cap(out))
	appendUnique := func(values []string) {
		for _, endpoint := range values {
			if _, exists := seen[endpoint]; exists {
				continue
			}
			seen[endpoint] = struct{}{}
			out = append(out, endpoint)
		}
	}
	appendUnique(carrierEndpoints)
	appendUnique(fallbackEndpoints)
	return out
}
func dnsServerEndpoints(rawServers []string, iface string) []string {
	seen := make(map[string]struct{}, len(rawServers))
	out := make([]string, 0, len(rawServers))
	for _, raw := range rawServers {
		raw = strings.TrimSpace(raw)
		ip := net.ParseIP(raw)
		if ip == nil || (!ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast()) {
			continue
		}
		host := ip.String()
		if ip.IsLinkLocalUnicast() && ip.To4() == nil && strings.TrimSpace(iface) != "" {
			host += "%" + strings.TrimSpace(iface)
		}
		endpoint := net.JoinHostPort(host, "53")
		if _, exists := seen[endpoint]; exists {
			continue
		}
		seen[endpoint] = struct{}{}
		out = append(out, endpoint)
	}
	return out
}

const (
	dataDNSAttemptTimeout = 1200 * time.Millisecond
	dataDNSServerBudget   = 2 * dataDNSAttemptTimeout
)

type dataDNSExchangeFunc func(context.Context, *dns.Msg, string, string, *net.Dialer) (*dns.Msg, error)

func exchangeDataDNS(ctx context.Context, msg *dns.Msg, server, network string, dialer *net.Dialer) (*dns.Msg, error) {
	client := &dns.Client{Net: network, Timeout: dataDNSAttemptTimeout, Dialer: dialer}
	response, _, err := client.ExchangeContext(ctx, msg, server)
	return response, err
}

func queryDataDNS(ctx context.Context, host string, family netprobe.Family, servers []string, dialer *net.Dialer) ([]string, error) {
	return queryDataDNSWithExchange(ctx, host, family, servers, dialer, exchangeDataDNS)
}

func queryDataDNSWithExchange(ctx context.Context, host string, family netprobe.Family, servers []string, dialer *net.Dialer, exchange dataDNSExchangeFunc) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	qtype := uint16(dns.TypeA)
	if family == netprobe.FamilyV6 {
		qtype = dns.TypeAAAA
	}
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(host), qtype)

	var errs []error
	for _, server := range servers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		serverCtx, serverCancel := context.WithTimeout(ctx, dataDNSServerBudget)
		udpCtx, udpCancel := context.WithTimeout(serverCtx, dataDNSAttemptTimeout)
		response, err := exchange(udpCtx, msg, server, "udp", dialer)
		udpCancel()
		var udpFailure error
		needsTCP := false
		if err != nil {
			udpFailure = fmt.Errorf("%s/udp: %w", server, err)
			needsTCP = true
		}
		if err == nil && response == nil {
			udpFailure = fmt.Errorf("%s/udp: empty DNS response", server)
			needsTCP = true
		}
		if err == nil && response != nil && response.Truncated {
			udpFailure = fmt.Errorf("%s/udp: truncated DNS response", server)
			needsTCP = true
		}
		if needsTCP {
			if contextErr := ctx.Err(); contextErr != nil {
				serverCancel()
				return nil, contextErr
			}
			tcpResponse, tcpErr := exchange(serverCtx, msg, server, "tcp", dialer)
			if tcpErr != nil {
				serverCancel()
				errs = append(errs, errors.Join(udpFailure, fmt.Errorf("%s/tcp: %w", server, tcpErr)))
				continue
			}
			if tcpResponse == nil {
				serverCancel()
				errs = append(errs, errors.Join(udpFailure, fmt.Errorf("%s/tcp: empty DNS response", server)))
				continue
			}
			response = tcpResponse
		}
		serverCancel()
		if response.Rcode != dns.RcodeSuccess {
			errs = append(errs, fmt.Errorf("%s: DNS rcode=%s", server, dns.RcodeToString[response.Rcode]))
			continue
		}

		answers := make([]string, 0, len(response.Answer))
		for _, record := range response.Answer {
			switch value := record.(type) {
			case *dns.A:
				if family == netprobe.FamilyV4 && value != nil && value.A != nil {
					if addr, ok := netip.AddrFromSlice(value.A); ok {
						addr = addr.Unmap()
						if addr.Is4() && netprobe.IsPublicAddress(addr) {
							answers = append(answers, addr.String())
						}
					}
				}
			case *dns.AAAA:
				if family == netprobe.FamilyV6 && value != nil && value.AAAA != nil {
					if addr, ok := netip.AddrFromSlice(value.AAAA); ok &&
						addr.Is6() && netprobe.IsPublicAddress(addr) {
						answers = append(answers, addr.String())
					}
				}
			}
		}
		if len(answers) > 0 {
			return answers, nil
		}
		errs = append(errs, fmt.Errorf("%s: no %s answer", server, family))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.Join(errs...)
}

func (m *Manager) applyIPConfig(nc netConfigurator, iface string, ipc mbim.IPConfiguration) error {
	if nc == nil {
		return fmt.Errorf("mbimcore: network configurator is not set")
	}
	if strings.TrimSpace(iface) == "" {
		return fmt.Errorf("mbimcore: network interface is empty")
	}

	hasV4 := ipc.IPv4Address != ""
	hasV6 := ipc.IPv6Address != ""
	if !hasV4 && !hasV6 {
		return fmt.Errorf("mbimcore: IP configuration has no address")
	}
	if hasV4 {
		if err := validateDataAddress(ipc.IPv4Address, ipc.IPv4PrefixLength, false); err != nil {
			return err
		}
	}
	if hasV6 {
		if err := validateDataAddress(ipc.IPv6Address, ipc.IPv6PrefixLength, true); err != nil {
			return err
		}
	}

	v4Direct, err := defaultRouteIsDirect(ipc.IPv4Gateway, false)
	if err != nil && hasV4 {
		return err
	}
	v6Direct, err := defaultRouteIsDirect(ipc.IPv6Gateway, true)
	if err != nil && hasV6 {
		return err
	}
	mtu, err := selectDataMTU(ipc)
	if err != nil {
		return err
	}

	if err := flushNetwork(nc, iface); err != nil {
		return fmt.Errorf("mbimcore: flush stale network configuration: %w", err)
	}
	if mtu > 0 {
		if err := nc.SetMTU(iface, mtu); err != nil {
			return err
		}
	}
	if hasV4 {
		if err := nc.SetIPv4(iface, ipc.IPv4Address, int(ipc.IPv4PrefixLength)); err != nil {
			return err
		}
	}
	if hasV6 {
		if err := nc.SetIPv6(iface, ipc.IPv6Address, int(ipc.IPv6PrefixLength)); err != nil {
			return err
		}
	}
	if err := nc.BringUp(iface); err != nil {
		return err
	}
	if hasV4 {
		if v4Direct {
			err = nc.AddDefaultRouteDirect(iface, false)
		} else {
			err = nc.AddDefaultRoute(iface, ipc.IPv4Gateway)
		}
		if err != nil {
			return err
		}
	}
	if hasV6 {
		if v6Direct {
			err = nc.AddDefaultRouteDirect(iface, true)
		} else {
			err = nc.AddDefaultRoute(iface, ipc.IPv6Gateway)
		}
		if err != nil {
			return err
		}
	}

	dns := append(append([]string{}, ipc.IPv4DNS...), ipc.IPv6DNS...)
	if len(dns) > 0 {
		if err := nc.SetDNS(dns); err != nil {
			return err
		}
	}
	return nil
}

func validateDataAddress(raw string, prefix uint32, ipv6 bool) error {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return fmt.Errorf("mbimcore: invalid data address %q", raw)
	}
	if ipv6 {
		if ip.To4() != nil || prefix > 128 {
			return fmt.Errorf("mbimcore: invalid IPv6 address/prefix %q/%d", raw, prefix)
		}
	} else if ip.To4() == nil || prefix > 32 {
		return fmt.Errorf("mbimcore: invalid IPv4 address/prefix %q/%d", raw, prefix)
	}
	// RFC1918 and ULA bearer addresses are valid, but unspecified, loopback,
	// link-local, and multicast addresses cannot carry a public-IP probe.
	if !ip.IsGlobalUnicast() {
		return fmt.Errorf("mbimcore: data address is not global unicast %q", raw)
	}
	return nil
}

// defaultRouteIsDirect follows the point-to-point/raw-IP MBIM convention:
// an omitted or unspecified gateway means "default dev <interface>". An
// RA-only/SLAAC bearer needs a separate mode that preserves kernel link-local
// addresses and RA-learned routes instead of calling flushNetwork.
func defaultRouteIsDirect(raw string, ipv6 bool) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true, nil
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		return false, fmt.Errorf("mbimcore: invalid gateway %q", raw)
	}
	if ipv6 {
		if ip.To4() != nil {
			return false, fmt.Errorf("mbimcore: IPv6 gateway has wrong family %q", raw)
		}
	} else if ip.To4() == nil {
		return false, fmt.Errorf("mbimcore: IPv4 gateway has wrong family %q", raw)
	}
	return ip.IsUnspecified(), nil
}

func selectDataMTU(ipc mbim.IPConfiguration) (int, error) {
	hasV4 := ipc.IPv4Address != ""
	hasV6 := ipc.IPv6Address != ""
	if hasV6 && ipc.IPv6MTU > 0 && ipc.IPv6MTU < 1280 {
		return 0, fmt.Errorf("mbimcore: IPv6 MTU %d is below 1280", ipc.IPv6MTU)
	}

	mtu := uint32(0)
	if hasV4 && ipc.IPv4MTU > 0 {
		mtu = ipc.IPv4MTU
	}
	if hasV6 && ipc.IPv6MTU > 0 && (mtu == 0 || ipc.IPv6MTU < mtu) {
		mtu = ipc.IPv6MTU
	}
	if hasV6 && mtu == 0 {
		mtu = 1280
	}
	if hasV6 && mtu > 0 && mtu < 1280 {
		return 0, fmt.Errorf("mbimcore: dual-stack MTU %d is below IPv6 minimum 1280", mtu)
	}
	return int(mtu), nil
}

func flushNetwork(nc netConfigurator, iface string) error {
	if nc == nil || strings.TrimSpace(iface) == "" {
		return nil
	}
	return errors.Join(nc.FlushRoutes(iface), nc.Flush(iface))
}

func (m *Manager) handleConnectIndication(st mbim.ConnectState) {
	if st.SessionID != dataSessionID {
		return
	}
	if st.ActivationState != mbim.ActivationStateDeactivated && st.ActivationState != mbim.ActivationStateDeactivating {
		return
	}
	m.mu.Lock()
	desired := m.desiredConnection
	epoch := m.dataEpoch
	now := time.Now()
	expected := m.expectedDeactivationEpoch > 0 && epoch <= m.expectedDeactivationEpoch && now.Before(m.expectedDeactivationUntil)
	if !m.expectedDeactivationUntil.IsZero() && !now.Before(m.expectedDeactivationUntil) {
		m.expectedDeactivationEpoch = 0
		m.expectedDeactivationUntil = time.Time{}
	}
	m.mu.Unlock()
	if !desired {
		return
	}
	go m.handleUnexpectedDataDisconnect(epoch, expected, true)
}

func (m *Manager) handleUnexpectedDataDisconnect(epoch uint64, expected, allowRetry bool) {
	m.mu.Lock()
	d := m.dev
	connectInFlightAtQueryStart := m.activeDataConnectCancel != nil
	m.mu.Unlock()
	if d == nil {
		return
	}
	queryCtx, queryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	current, queryErr := mbim.QueryConnect(queryCtx, d, dataSessionID)
	queryCancel()
	if queryErr == nil && (current.ActivationState == mbim.ActivationStateActivated || current.ActivationState == mbim.ActivationStateActivating) {
		return
	}
	// A proactive deactivate can race its own activation retry; give the
	// authoritative state one bounded recheck before acting.
	if allowRetry && (expected || queryErr != nil) {
		m.scheduleExpectedDeactivationRecheck(epoch)
		return
	}

	// A serialized Connect owns convergence while it is in flight. A delayed
	// indication or failed query must not cancel that same operation.
	if connectInFlightAtQueryStart {
		return
	}
	m.mu.Lock()
	stillCurrent := m.dataEpoch == epoch && m.desiredConnection
	hadData := m.connected || m.privateIPv4 != "" || m.privateIPv6 != "" || len(m.ipv4DNS) > 0 || len(m.ipv6DNS) > 0
	m.mu.Unlock()
	if !stillCurrent || !hadData {
		return
	}
	m.dataMu.Lock()
	m.mu.Lock()
	hadData = m.connected || m.privateIPv4 != "" || m.privateIPv6 != "" || len(m.ipv4DNS) > 0 || len(m.ipv6DNS) > 0
	if m.dataEpoch != epoch || !m.desiredConnection || !hadData {
		m.mu.Unlock()
		callbacks := m.takePendingDataCallbacksLocked()
		m.dataMu.Unlock()
		runDataCallbacks(callbacks)
		return
	}
	iface := m.dataCfg.Interface
	nc := m.netcfg
	m.connected = false
	m.privateIPv4, m.privateIPv6 = "", ""
	m.ipv4DNS, m.ipv6DNS = nil, nil
	m.clearAppliedIPConfigLocked()
	m.dataEpoch++
	m.mu.Unlock()

	if err := flushNetwork(nc, iface); err != nil {
		logger.Warn("[mbim] unexpected disconnect network cleanup failed", "control_device", m.controlDevice, "err", err)
	}
	m.queueDataDisconnectedCallbackLocked()
	callbacks := m.takePendingDataCallbacksLocked()
	m.dataMu.Unlock()
	runDataCallbacks(callbacks)
	go m.reconnectWithBackoff()
}

func (m *Manager) scheduleExpectedDeactivationRecheck(epoch uint64) {
	m.mu.Lock()
	if m.dataEpoch != epoch || !m.desiredConnection {
		m.mu.Unlock()
		return
	}
	if m.deactivationRecheckPending && m.deactivationRecheckEpoch == epoch {
		m.mu.Unlock()
		return
	}
	m.deactivationRecheckPending = true
	m.deactivationRecheckEpoch = epoch
	m.mu.Unlock()

	go func() {
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		<-timer.C

		m.mu.Lock()
		valid := m.deactivationRecheckPending &&
			m.deactivationRecheckEpoch == epoch &&
			m.dataEpoch == epoch &&
			m.desiredConnection
		if m.deactivationRecheckEpoch == epoch {
			m.deactivationRecheckPending = false
		}
		m.mu.Unlock()
		if valid {
			m.handleUnexpectedDataDisconnect(epoch, false, false)
		}
	}()
}

func (m *Manager) handleIPConfigurationIndication() {
	m.mu.Lock()
	if !m.desiredConnection {
		m.mu.Unlock()
		return
	}
	if m.ipConfigRefreshRunning {
		m.ipConfigRefreshPending = true
		m.mu.Unlock()
		return
	}
	m.ipConfigRefreshRunning = true
	m.mu.Unlock()
	go m.runIPConfigurationRefreshes()
}

func (m *Manager) runIPConfigurationRefreshes() {
	retried := false
	for {
		retry := m.refreshIPConfigurationFromModem()
		m.mu.Lock()
		if m.ipConfigRefreshPending && m.desiredConnection {
			m.ipConfigRefreshPending = false
			retried = false
			m.mu.Unlock()
			continue
		}
		if retry && !retried && m.desiredConnection {
			retried = true
			m.mu.Unlock()
			timer := time.NewTimer(250 * time.Millisecond)
			<-timer.C
			continue
		}
		m.ipConfigRefreshRunning = false
		m.ipConfigRefreshPending = false
		m.mu.Unlock()
		return
	}
}

func (m *Manager) refreshIPConfigurationFromModem() bool {
	m.dataMu.Lock()
	m.mu.Lock()
	epoch := m.dataEpoch
	if !m.desiredConnection || !m.connected || m.dev == nil {
		m.mu.Unlock()
		callbacks := m.takePendingDataCallbacksLocked()
		m.dataMu.Unlock()
		runDataCallbacks(callbacks)
		return false
	}
	d := m.dev
	cfg := m.dataCfg
	nc := m.netcfg
	m.mu.Unlock()
	if nc == nil {
		nc = realNetConfigurator{}
	}

	refreshCtx, refreshCancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if m.dataStopRequested || m.dataEpoch != epoch || !m.desiredConnection {
		m.mu.Unlock()
		refreshCancel()
		callbacks := m.takePendingDataCallbacksLocked()
		m.dataMu.Unlock()
		runDataCallbacks(callbacks)
		return false
	}
	m.activeIPConfigRefreshCancel = refreshCancel
	m.mu.Unlock()
	defer func() {
		refreshCancel()
		m.mu.Lock()
		m.activeIPConfigRefreshCancel = nil
		m.mu.Unlock()
	}()

	queryCtx, queryCancel := context.WithTimeout(refreshCtx, 20*time.Second)
	ipc, err := mbim.QueryIPConfiguration(queryCtx, d, dataSessionID)
	queryCancel()
	m.mu.Lock()
	stillCurrent := m.dataEpoch == epoch && m.desiredConnection && !m.dataStopRequested
	m.mu.Unlock()
	if err != nil {
		callbacks := m.takePendingDataCallbacksLocked()
		m.dataMu.Unlock()
		runDataCallbacks(callbacks)
		if stillCurrent {
			logger.Warn("[mbim] authoritative IP configuration query failed", "control_device", m.controlDevice, "err", err)
		}
		return stillCurrent
	}
	if !stillCurrent {
		callbacks := m.takePendingDataCallbacksLocked()
		m.dataMu.Unlock()
		runDataCallbacks(callbacks)
		return false
	}
	m.mu.Lock()
	unchanged := m.hasAppliedIPConfig && equalIPConfiguration(m.appliedIPConfig, ipc)
	m.mu.Unlock()
	if unchanged {
		// The indication can still mean that the NAT-facing address changed,
		// so notify the upper layer without disrupting an identical bearer.
		m.queueIPConfigChangedCallbackLocked()
		callbacks := m.takePendingDataCallbacksLocked()
		m.dataMu.Unlock()
		runDataCallbacks(callbacks)
		return false
	}

	var applyErr error
	if ipc.IPv4Address == "" && ipc.IPv6Address == "" {
		applyErr = fmt.Errorf("mbimcore: no IP assigned")
	} else {
		applyErr = m.applyIPConfig(nc, cfg.Interface, ipc)
	}
	if applyErr != nil {
		applyErr = m.cleanupActivatedDataSessionLocked(refreshCtx, d, nc, cfg.Interface, fmt.Errorf("mbimcore: refresh IP configuration: %w", applyErr))
		callbacks := m.takePendingDataCallbacksLocked()
		m.dataMu.Unlock()
		runDataCallbacks(callbacks)
		logger.Warn("[mbim] IP configuration refresh failed", "control_device", m.controlDevice, "err", applyErr)
		go m.reconnectWithBackoff()
		return false
	}

	m.mu.Lock()
	if m.dataEpoch != epoch || !m.desiredConnection || m.dataStopRequested {
		m.mu.Unlock()
		callbacks := m.takePendingDataCallbacksLocked()
		m.dataMu.Unlock()
		runDataCallbacks(callbacks)
		return false
	}
	m.dataEpoch++
	if m.expectedDeactivationEpoch == epoch {
		m.expectedDeactivationEpoch = m.dataEpoch
	}
	m.privateIPv4 = ipc.IPv4Address
	m.privateIPv6 = ipc.IPv6Address
	m.ipv4DNS = append([]string(nil), ipc.IPv4DNS...)
	m.ipv6DNS = append([]string(nil), ipc.IPv6DNS...)
	m.appliedIPConfig = cloneIPConfiguration(ipc)
	m.hasAppliedIPConfig = true
	m.connected = true
	m.mu.Unlock()
	m.queueIPConfigChangedCallbackLocked()
	callbacks := m.takePendingDataCallbacksLocked()
	m.dataMu.Unlock()
	runDataCallbacks(callbacks)
	return false
}

func (m *Manager) reconnectWithBackoff() {
	if !m.reconnectGate.CompareAndSwap(false, true) {
		return
	}
	defer m.reconnectGate.Store(false)

	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if !m.desiredConnection {
		m.mu.Unlock()
		cancel()
		return
	}
	m.reconnectCancel = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		m.reconnectCancel = nil
		m.mu.Unlock()
	}()

	backoff := time.Second
	for attempt := 0; attempt < 6; attempt++ {
		m.mu.Lock()
		desired := m.desiredConnection
		m.mu.Unlock()
		if !desired {
			return
		}
		if err := m.ConnectContext(ctx); err == nil {
			return
		}
		if err := sleepContext(ctx, backoff); err != nil {
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (m *Manager) ensureRegistered(ctx context.Context, d *mbim.Device) error {
	m.mu.Lock()
	timeout := m.registrationTimeout
	m.mu.Unlock()
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		rs, err := mbim.QueryRegisterState(ctx, d)
		if err == nil && (rs.RegisterState == registerStateHome || rs.RegisterState == registerStateRoaming) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return ErrNetworkNotRegistered
		}
		if err := sleepContext(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}
