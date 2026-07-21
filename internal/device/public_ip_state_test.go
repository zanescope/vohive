package device

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vowifi-go/engine/swu"
	"github.com/zanescope/vowifi-go/runtimehost"
	"github.com/zanescope/vowifi-go/runtimehost/identity"
)

type publicIPStateTestController struct {
	mu sync.RWMutex

	connected   bool
	privateV4   string
	privateV6   string
	publicV4    string
	publicV6    string
	rotateErr   error
	rotateFn    func()
	connectHook func()
	probeFn     func(context.Context) (string, string)

	rotateCalls atomic.Int32
	probeCalls  atomic.Int32
}

func (c *publicIPStateTestController) Connect() error {
	c.mu.Lock()
	c.connected = true
	hook := c.connectHook
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (c *publicIPStateTestController) Disconnect() error {
	c.mu.Lock()
	c.connected = false
	c.mu.Unlock()
	return nil
}

func (c *publicIPStateTestController) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

func (c *publicIPStateTestController) RotateIP() error {
	c.rotateCalls.Add(1)
	c.mu.RLock()
	fn, err := c.rotateFn, c.rotateErr
	c.mu.RUnlock()
	if fn != nil {
		fn()
	}
	return err
}

func (c *publicIPStateTestController) GetPrivateIP() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.privateV4
}

func (c *publicIPStateTestController) GetPrivateIPv6() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.privateV6
}

func (c *publicIPStateTestController) GetPublicIPv4AndV6NoCache() (string, string) {
	return c.GetPublicIPv4AndV6Context(context.Background())
}

func (c *publicIPStateTestController) GetPublicIPv4AndV6Context(ctx context.Context) (string, string) {
	c.probeCalls.Add(1)
	c.mu.RLock()
	fn := c.probeFn
	v4, v6 := c.publicV4, c.publicV6
	c.mu.RUnlock()
	if fn != nil {
		return fn(ctx)
	}
	return v4, v6
}

func (c *publicIPStateTestController) setPrivate(v4, v6 string) {
	c.mu.Lock()
	c.privateV4, c.privateV6 = v4, v6
	c.mu.Unlock()
}

func (c *publicIPStateTestController) setPublic(v4, v6 string) {
	c.mu.Lock()
	c.publicV4, c.publicV6 = v4, v6
	c.mu.Unlock()
}

func (c *publicIPStateTestController) setProbe(fn func(context.Context) (string, string)) {
	c.mu.Lock()
	c.probeFn = fn
	c.mu.Unlock()
}
func (c *publicIPStateTestController) setConnectHook(fn func()) {
	c.mu.Lock()
	c.connectHook = fn
	c.mu.Unlock()
}

func newPublicIPStateHarness(t *testing.T) (*Pool, *Worker, *publicIPStateTestController) {
	t.Helper()
	p := NewPool(&config.Config{})
	controller := &publicIPStateTestController{connected: true}
	worker := &Worker{
		ID:          "public-ip-state-test",
		Pool:        p,
		netOverride: controller,
		stop:        make(chan struct{}),
	}
	if err := p.registerWorkerStarting(worker); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	t.Cleanup(func() {
		p.stopPublicIPState(worker)
		p.cancel()
	})
	return p, worker, controller
}

func seedPublicIPRuntime(worker *Worker, privateV4, privateV6, publicV4, publicV6 string) {
	state := &worker.publicIP
	state.mu.Lock()
	stopPublicIPTimersLocked(state)
	state.initialized = true
	state.connected = true
	state.rotating = false
	state.epoch++
	state.privateV4, state.privateV6 = privateV4, privateV6
	state.expectedV4, state.expectedV6 = expectedBearerFamilies(privateV4, privateV6)
	state.publishedV4, state.publishedV6 = publicV4, publicV6
	state.authoritativeV4, state.authoritativeV6 = publicV4, publicV6
	state.mobikeBaseV4, state.mobikeBaseV6 = "", ""
	state.retrying = false
	state.cycleResolvedV4 = !state.expectedV4 || publicV4 != ""
	state.cycleResolvedV6 = !state.expectedV6 || publicV6 != ""
	state.cycleMOBIKEReserved = false
	state.mobikeTransitionReserved = false
	state.pendingMOBIKE = nil
	state.mobikeInFlight = 0
	state.retryAttemptV4, state.retryAttemptV6 = 0, 0
	state.noAddressTries = 0
	state.revision++
	worker.cacheMu.Lock()
	worker.cachedIP, worker.cachedPublicIPv6 = publicV4, publicV6
	if publicV4 != "" || publicV6 != "" {
		worker.cacheTime = time.Now()
	} else {
		worker.cacheTime = time.Time{}
	}
	worker.cacheMu.Unlock()
	state.mu.Unlock()
}

func publicIPTestSnapshot(worker *Worker) publicIPSnapshot {
	state := &worker.publicIP
	state.mu.Lock()
	defer state.mu.Unlock()
	return publicIPSnapshot{
		epoch:      state.epoch,
		probeSeq:   state.probeSeq,
		generation: worker.generation,
		connected:  state.connected,
		privateV4:  state.privateV4,
		privateV6:  state.privateV6,
	}
}

func publicIPTestCached(worker *Worker) (string, string) {
	worker.cacheMu.RLock()
	defer worker.cacheMu.RUnlock()
	return worker.cachedIP, worker.cachedPublicIPv6
}

func waitPublicIPTest(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition was not satisfied before timeout")
	}
}

type publicIPTestTunnelManager struct {
	session *publicIPTestTunnelSession
}

func (m *publicIPTestTunnelManager) EstablishTunnel(context.Context, swu.TunnelConfig) (swu.TunnelSession, error) {
	return m.session, nil
}

type publicIPTestTunnelSession struct {
	mu      sync.Mutex
	request swu.MOBIKERequest
	calls   atomic.Int32
}

func (s *publicIPTestTunnelSession) Result() swu.TunnelResult {
	return swu.TunnelResult{Ready: true, IKEEstablished: true, IPsecEstablished: true}
}

func (s *publicIPTestTunnelSession) MOBIKE(_ context.Context, request swu.MOBIKERequest) (swu.MOBIKEResult, error) {
	s.mu.Lock()
	s.request = request
	s.mu.Unlock()
	s.calls.Add(1)
	return swu.MOBIKEResult{
		Rekeyed:          true,
		IKEEstablished:   true,
		IPsecEstablished: true,
		Reason:           "test mobike",
	}, nil
}

func (s *publicIPTestTunnelSession) Close(context.Context) error { return nil }

func (s *publicIPTestTunnelSession) lastRequest() swu.MOBIKERequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.request
}

func attachPublicIPTestRuntime(t *testing.T, pool *Pool, deviceID string) *publicIPTestTunnelSession {
	t.Helper()
	session := &publicIPTestTunnelSession{}
	instance, err := runtimehost.Start(context.Background(), runtimehost.StartRequest{
		DeviceID:      deviceID,
		Profile:       identity.Profile{IMSI: "310280233641503", MCC: "310", MNC: "280"},
		Dataplane:     runtimehost.DataplanePolicy{Mode: swu.DataplaneModeUserspace},
		TunnelManager: &publicIPTestTunnelManager{session: session},
	})
	if err != nil {
		t.Fatalf("start test runtime: %v", err)
	}
	pool.voWiFiHost().RuntimeStore().SetInstance(deviceID, instance)
	t.Cleanup(func() {
		pool.voWiFiHost().RuntimeStore().DeleteInstance(deviceID, instance)
		_ = instance.Stop(context.Background())
	})
	return session
}

type publicIPRotationNotice struct {
	deviceID string
	oldIP    string
	newIP    string
}

type publicIPRotationTestNotifier struct {
	calls   atomic.Int32
	notices chan publicIPRotationNotice
}

func (n *publicIPRotationTestNotifier) NotifySMS(string, string, string, time.Time) {}

func (n *publicIPRotationTestNotifier) NotifyIPRotated(deviceID, oldIP, newIP string, _ time.Duration) {
	n.calls.Add(1)
	select {
	case n.notices <- publicIPRotationNotice{deviceID: deviceID, oldIP: oldIP, newIP: newIP}:
	default:
	}
}

func (n *publicIPRotationTestNotifier) NotifyRaw(string) {}

func TestStartNetworkAndConnectCallbackLaunchSingleProbe(t *testing.T) {
	for _, withCallback := range []bool{false, true} {
		name := "without_callback"
		if withCallback {
			name = "with_synchronous_callback"
		}
		t.Run(name, func(t *testing.T) {
			p, worker, controller := newPublicIPStateHarness(t)
			if err := controller.Disconnect(); err != nil {
				t.Fatal(err)
			}
			controller.setPrivate("10.0.0.2", "")
			controller.setPublic("8.8.8.8", "")
			if withCallback {
				controller.setConnectHook(func() { p.refreshIPs(worker, false) })
			}

			if err := worker.StartNetwork(); err != nil {
				t.Fatalf("StartNetwork() error = %v", err)
			}
			// Startup post-apply and connect callbacks are ordinary observations of
			// the same epoch; they must not force a second external request.
			p.refreshIPs(worker, false)
			waitPublicIPTest(t, time.Second, func() bool { return controller.probeCalls.Load() >= 1 })
			time.Sleep(50 * time.Millisecond)
			if got := controller.probeCalls.Load(); got != 1 {
				t.Fatalf("public IP probe calls = %d, want 1", got)
			}
		})
	}
}
func TestPublicIPStateDropsStaleProbeAfterDisconnect(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("10.0.0.2", "")
	seedPublicIPRuntime(worker, "10.0.0.2", "", "", "")

	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	controller.setProbe(func(context.Context) (string, string) {
		close(probeStarted)
		<-probeRelease
		return "8.8.8.8", ""
	})

	state := &worker.publicIP
	state.mu.Lock()
	state.probeSeq++
	snapshot := publicIPSnapshot{
		epoch:      state.epoch,
		probeSeq:   state.probeSeq,
		generation: worker.generation,
		connected:  true,
		privateV4:  state.privateV4,
		privateV6:  state.privateV6,
	}
	probeCtx, cancel := context.WithCancel(p.ctx)
	state.cancel = cancel
	state.inFlight = true
	state.mu.Unlock()

	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		p.runPublicIPProbe(probeCtx, worker, controller, snapshot)
	}()
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}

	p.invalidatePublicIPState(worker, false)
	close(probeRelease)
	select {
	case <-probeDone:
	case <-time.After(time.Second):
		t.Fatal("stale probe did not return")
	}

	if v4, v6 := publicIPTestCached(worker); v4 != "" || v6 != "" {
		t.Fatalf("stale probe repopulated cache: (%q, %q)", v4, v6)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.connected || state.publishedV4 != "" || state.publishedV6 != "" {
		t.Fatalf("disconnect state was overwritten: connected=%t published=(%q,%q)", state.connected, state.publishedV4, state.publishedV6)
	}
}

func TestPublicIPProbeCycleDeadlineFinalizesAndSchedulesRetry(t *testing.T) {
	previousTimeout := publicIPProbeCycleTimeout
	publicIPProbeCycleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { publicIPProbeCycleTimeout = previousTimeout })

	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("8.8.8.8", "")
	deadlineErr := make(chan error, 1)
	controller.setProbe(func(ctx context.Context) (string, string) {
		<-ctx.Done()
		deadlineErr <- ctx.Err()
		return "", ""
	})

	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		v4, _ := publicIPTestCached(worker)
		return v4 == "8.8.8.8"
	})
	select {
	case err := <-deadlineErr:
		if err != context.DeadlineExceeded {
			t.Fatalf("probe context error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not observe the cycle deadline")
	}

	state := &worker.publicIP
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.inFlight || state.cancel != nil || !state.retrying || state.retryTimer == nil || state.retryAttemptV4 != 1 {
		t.Fatalf("deadline state: in_flight=%t cancel=%v retrying=%t timer=%v attempts=%d",
			state.inFlight, state.cancel != nil, state.retrying, state.retryTimer != nil, state.retryAttemptV4)
	}
	if state.authoritativeV4 != "" || state.publishedV4 != "8.8.8.8" {
		t.Fatalf("deadline publication: authoritative=%q published=%q", state.authoritativeV4, state.publishedV4)
	}
}
func TestPublicIPDisconnectClearsPublishedStateButRetainsMOBIKEBaseline(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("10.0.0.2", "")
	controller.setPublic("8.8.4.4", "")
	seedPublicIPRuntime(worker, "10.0.0.2", "", "8.8.8.8", "")

	p.invalidatePublicIPState(worker, false)
	if v4, v6 := publicIPTestCached(worker); v4 != "" || v6 != "" {
		t.Fatalf("disconnect did not clear cache: (%q, %q)", v4, v6)
	}
	state := &worker.publicIP
	state.mu.Lock()
	if state.connected || state.publishedV4 != "" || state.mobikeBaseV4 != "8.8.8.8" {
		state.mu.Unlock()
		t.Fatalf("disconnect state connected=%t published=%q baseline=%q", state.connected, state.publishedV4, state.mobikeBaseV4)
	}
	state.mu.Unlock()

	controller.setPrivate("10.0.0.3", "")
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		v4, _ := publicIPTestCached(worker)
		return v4 == "8.8.4.4"
	})
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.connected || state.mobikeBaseV4 != "" || state.publishedV4 != "8.8.4.4" {
		t.Fatalf("reconnect state connected=%t published=%q baseline=%q", state.connected, state.publishedV4, state.mobikeBaseV4)
	}
}

func TestPublicIPRotationDisconnectConnectProbeTriggersMOBIKEOnce(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("10.0.0.2", "")
	controller.setPublic("8.8.4.4", "")
	seedPublicIPRuntime(worker, "10.0.0.2", "", "8.8.8.8", "")
	session := attachPublicIPTestRuntime(t, p, worker.ID)

	baseline, ok := p.beginPublicIPRotation(worker, controller)
	if !ok || baseline.publicV4 != "8.8.8.8" {
		t.Fatalf("rotation baseline=%+v ok=%t", baseline, ok)
	}
	// MBIM queues the disconnected callback before the connected callback.
	p.invalidatePublicIPState(worker, false)
	controller.setPrivate("10.0.0.3", "")
	p.refreshIPs(worker, true)

	waitPublicIPTest(t, time.Second, func() bool { return session.calls.Load() == 1 })
	request := session.lastRequest()
	if request.OldIP != "8.8.8.8" || request.NewIP != "8.8.4.4" {
		t.Fatalf("MOBIKE request=%+v", request)
	}

	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool { return controller.probeCalls.Load() >= 2 })
	if got := session.calls.Load(); got != 1 {
		t.Fatalf("MOBIKE calls=%d, want exactly one", got)
	}
}

func TestRotateWithNotifySupportsIPv6OnlyAndNotifiesOnce(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	oldPublic := "2606:4700:4700::1111"
	newPublic := "2001:4860:4860::8888"
	controller.setPrivate("", "fd00::2")
	controller.setPublic("", oldPublic)
	seedPublicIPRuntime(worker, "", "fd00::2", "", oldPublic)

	controller.mu.Lock()
	controller.rotateFn = func() {
		controller.setPrivate("", "fd00::3")
		controller.setPublic("", newPublic)
	}
	controller.mu.Unlock()
	notifier := &publicIPRotationTestNotifier{notices: make(chan publicIPRotationNotice, 2)}
	p.SetNotifier(notifier)

	oldIP, newIP, err := worker.RotateWithNotify()
	if err != nil {
		t.Fatalf("RotateWithNotify: %v", err)
	}
	if oldIP != oldPublic || newIP != newPublic {
		t.Fatalf("RotateWithNotify returned (%q, %q)", oldIP, newIP)
	}
	if got := controller.rotateCalls.Load(); got != 1 {
		t.Fatalf("RotateIP calls=%d", got)
	}

	select {
	case notice := <-notifier.notices:
		if notice.deviceID != worker.ID || notice.oldIP != oldPublic || notice.newIP != newPublic {
			t.Fatalf("rotation notice=%+v", notice)
		}
	case <-time.After(time.Second):
		t.Fatal("rotation notification was not sent")
	}
	time.Sleep(20 * time.Millisecond)
	if got := notifier.calls.Load(); got != 1 {
		t.Fatalf("rotation notifications=%d, want exactly one", got)
	}
}

func TestRotateWithNotifyReturnsTheNotifiedDualStackChange(t *testing.T) {
	const (
		oldV4 = "8.8.8.8"
		newV4 = "9.9.9.9"
		oldV6 = "2606:4700:4700::1111"
		newV6 = "2001:4860:4860::8888"
	)
	tests := []struct {
		name                 string
		privateV4, privateV6 string
		publicV4, publicV6   string
		wantOld, wantNew     string
	}{
		{
			name:      "only IPv6 changes",
			privateV4: "10.0.0.3",
			privateV6: "fd00::3",
			publicV4:  oldV4,
			publicV6:  newV6,
			wantOld:   oldV6,
			wantNew:   newV6,
		},
		{
			name:      "IPv4 retires",
			privateV6: "fd00::3",
			publicV6:  oldV6,
			wantOld:   oldV4,
			wantNew:   oldV6,
		},
		{
			name:      "IPv6 retires",
			privateV4: "10.0.0.3",
			publicV4:  oldV4,
			wantOld:   oldV6,
			wantNew:   oldV4,
		},
		{
			name:      "surviving IPv4 changes",
			privateV4: "10.0.0.3",
			publicV4:  newV4,
			wantOld:   oldV4,
			wantNew:   newV4,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, worker, controller := newPublicIPStateHarness(t)
			controller.setPrivate("10.0.0.2", "fd00::2")
			controller.setPublic(oldV4, oldV6)
			seedPublicIPRuntime(worker, "10.0.0.2", "fd00::2", oldV4, oldV6)

			controller.mu.Lock()
			controller.rotateFn = func() {
				controller.setPrivate(test.privateV4, test.privateV6)
				controller.setPublic(test.publicV4, test.publicV6)
			}
			controller.mu.Unlock()
			notifier := &publicIPRotationTestNotifier{notices: make(chan publicIPRotationNotice, 2)}
			p.SetNotifier(notifier)

			oldIP, newIP, err := worker.RotateWithNotify()
			if err != nil {
				t.Fatalf("RotateWithNotify: %v", err)
			}
			if oldIP != test.wantOld || newIP != test.wantNew {
				t.Fatalf("RotateWithNotify returned (%q, %q), want (%q, %q)", oldIP, newIP, test.wantOld, test.wantNew)
			}

			select {
			case notice := <-notifier.notices:
				if notice.deviceID != worker.ID || notice.oldIP != oldIP || notice.newIP != newIP {
					t.Fatalf("rotation notice=%+v, return=(%q,%q)", notice, oldIP, newIP)
				}
			case <-time.After(time.Second):
				t.Fatal("rotation notification was not sent")
			}
			if got := notifier.calls.Load(); got != 1 {
				t.Fatalf("rotation notifications=%d, want exactly one", got)
			}
		})
	}
}
func TestPublicIPPartialDualStackResultsMergeAcrossRetries(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("10.0.0.2", "fd00::2")
	controller.setProbe(func(context.Context) (string, string) {
		if controller.probeCalls.Load() == 1 {
			return "8.8.8.8", ""
		}
		return "", "2606:4700:4700::1111"
	})

	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		v4, v6 := publicIPTestCached(worker)
		return v4 == "8.8.8.8" && v6 == ""
	})
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		v4, v6 := publicIPTestCached(worker)
		return v4 == "8.8.8.8" && v6 == "2606:4700:4700::1111"
	})

	state := &worker.publicIP
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.retryAttemptV4 != 0 || state.retryAttemptV6 != 0 || state.retryTimer != nil || state.periodicTimer == nil {
		t.Fatalf("retry state v4=%d v6=%d retry=%v periodic=%v", state.retryAttemptV4, state.retryAttemptV6, state.retryTimer != nil, state.periodicTimer != nil)
	}
}

func TestPublicIPExternalEchoOverridesPublicBearerAddress(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("8.8.8.8", "")
	controller.setPublic("9.9.9.9", "")

	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		v4, _ := publicIPTestCached(worker)
		return v4 == "9.9.9.9"
	})

	state := &worker.publicIP
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.publishedV4 != "9.9.9.9" || state.authoritativeV4 != "9.9.9.9" {
		t.Fatalf("published=%q authoritative=%q, want external echo", state.publishedV4, state.authoritativeV4)
	}
}

func TestPublicIPLocalBearerFallbackIsDisplayOnly(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("8.8.8.8", "")
	controller.setPublic("", "")
	session := attachPublicIPTestRuntime(t, p, worker.ID)

	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		v4, _ := publicIPTestCached(worker)
		return v4 == "8.8.8.8"
	})
	state := &worker.publicIP
	state.mu.Lock()
	if state.authoritativeV4 != "" || !state.retrying || state.pendingMOBIKE != nil {
		state.mu.Unlock()
		t.Fatalf("fallback became authoritative: authority=%q retrying=%t pending=%v",
			state.authoritativeV4, state.retrying, state.pendingMOBIKE != nil)
	}
	state.mu.Unlock()
	if got := session.calls.Load(); got != 0 {
		t.Fatalf("local fallback triggered %d MOBIKE calls", got)
	}

	controller.setPublic("9.9.9.9", "")
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.publishedV4 == "9.9.9.9" && state.authoritativeV4 == "9.9.9.9"
	})
	if got := session.calls.Load(); got != 0 {
		t.Fatalf("first external authority after fallback triggered %d MOBIKE calls", got)
	}
}

func TestPublicIPLocalFallbackDoesNotConsumeOldAuthoritativeBaseline(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("10.0.0.2", "")
	seedPublicIPRuntime(worker, "10.0.0.2", "", "1.1.1.1", "")
	session := attachPublicIPTestRuntime(t, p, worker.ID)

	p.invalidatePublicIPState(worker, false)
	controller.setPrivate("8.8.8.8", "")
	controller.setPublic("", "")
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		v4, _ := publicIPTestCached(worker)
		return v4 == "8.8.8.8"
	})
	state := &worker.publicIP
	state.mu.Lock()
	if state.mobikeBaseV4 != "1.1.1.1" || state.authoritativeV4 != "" {
		state.mu.Unlock()
		t.Fatalf("fallback consumed baseline: baseline=%q authority=%q", state.mobikeBaseV4, state.authoritativeV4)
	}
	state.mu.Unlock()

	controller.setPublic("9.9.9.9", "")
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool { return session.calls.Load() == 1 })
	request := session.lastRequest()
	if request.OldIP != "1.1.1.1" || request.NewIP != "9.9.9.9" {
		t.Fatalf("MOBIKE request=%+v", request)
	}
}

func TestPublicIPDualStackSplitRetryReservesOneMOBIKEPerCycle(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	oldV6 := "2606:4700:4700::1111"
	newV6 := "2001:4860:4860::8888"
	controller.setPrivate("10.0.0.2", "fd00::2")
	seedPublicIPRuntime(worker, "10.0.0.2", "fd00::2", "8.8.8.8", oldV6)
	session := attachPublicIPTestRuntime(t, p, worker.ID)

	p.invalidatePublicIPState(worker, false)
	controller.setPrivate("10.0.0.3", "fd00::3")
	controller.setProbe(func(context.Context) (string, string) {
		switch controller.probeCalls.Load() {
		case 1:
			return "9.9.9.9", ""
		case 2:
			return "", newV6
		default:
			return "1.1.1.1", newV6
		}
	})

	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool { return session.calls.Load() == 1 })
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		v4, v6 := publicIPTestCached(worker)
		return v4 == "9.9.9.9" && v6 == newV6
	})
	if got := session.calls.Load(); got != 1 {
		t.Fatalf("split retry triggered %d MOBIKE calls in one cycle", got)
	}

	controller.setPrivate("10.0.0.4", "fd00::4")
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool { return session.calls.Load() == 2 })
	request := session.lastRequest()
	if request.OldIP != "9.9.9.9" || request.NewIP != "1.1.1.1" {
		t.Fatalf("fresh-cycle MOBIKE request=%+v", request)
	}
}

func TestPublicIPDualStackUnchangedFirstFamilyDefersMOBIKEToChangedRetry(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	oldV6 := "2606:4700:4700::1111"
	newV6 := "2001:4860:4860::8888"
	controller.setPrivate("10.0.0.2", "fd00::2")
	seedPublicIPRuntime(worker, "10.0.0.2", "fd00::2", "8.8.8.8", oldV6)
	session := attachPublicIPTestRuntime(t, p, worker.ID)

	p.invalidatePublicIPState(worker, false)
	controller.setPrivate("10.0.0.3", "fd00::3")
	controller.setProbe(func(context.Context) (string, string) {
		if controller.probeCalls.Load() == 1 {
			return "8.8.8.8", ""
		}
		return "", newV6
	})

	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		v4, _ := publicIPTestCached(worker)
		return v4 == "8.8.8.8"
	})
	if got := session.calls.Load(); got != 0 {
		t.Fatalf("unchanged first family triggered %d MOBIKE calls", got)
	}
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool { return session.calls.Load() == 1 })
	request := session.lastRequest()
	if request.OldIP != oldV6 || request.NewIP != newV6 {
		t.Fatalf("retry MOBIKE request=%+v", request)
	}
}

func TestPublicIPSingleStackFamilyTransitionTriggersMOBIKE(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	newV6 := "2606:4700:4700::1111"
	controller.setPrivate("10.0.0.2", "")
	seedPublicIPRuntime(worker, "10.0.0.2", "", "8.8.8.8", "")
	session := attachPublicIPTestRuntime(t, p, worker.ID)

	p.invalidatePublicIPState(worker, false)
	controller.setPrivate("", "fd00::3")
	controller.setPublic("", newV6)
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool { return session.calls.Load() == 1 })
	request := session.lastRequest()
	if request.OldIP != "8.8.8.8" || request.NewIP != newV6 {
		t.Fatalf("cross-family MOBIKE request=%+v", request)
	}
}

func TestPublicIPDualToSingleStackTransitionsRetiredFamily(t *testing.T) {
	const oldV4 = "8.8.8.8"
	const oldV6 = "2606:4700:4700::1111"
	tests := []struct {
		name             string
		privateV4        string
		privateV6        string
		publicV4         string
		publicV6         string
		wantOld, wantNew string
	}{
		{
			name:      "v4 retired while v6 survives unchanged",
			privateV6: "fd00::3",
			publicV6:  oldV6,
			wantOld:   oldV4,
			wantNew:   oldV6,
		},
		{
			name:      "v6 retired while v4 survives unchanged",
			privateV4: "10.0.0.3",
			publicV4:  oldV4,
			wantOld:   oldV6,
			wantNew:   oldV4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, worker, controller := newPublicIPStateHarness(t)
			controller.setPrivate("10.0.0.2", "fd00::2")
			seedPublicIPRuntime(worker, "10.0.0.2", "fd00::2", oldV4, oldV6)
			session := attachPublicIPTestRuntime(t, p, worker.ID)

			p.invalidatePublicIPState(worker, false)
			controller.setPrivate(tt.privateV4, tt.privateV6)
			controller.setPublic(tt.publicV4, tt.publicV6)
			p.refreshIPs(worker, true)
			waitPublicIPTest(t, time.Second, func() bool { return session.calls.Load() == 1 })

			request := session.lastRequest()
			if request.OldIP != tt.wantOld || request.NewIP != tt.wantNew {
				t.Fatalf("cross-family MOBIKE request=%+v, want %s -> %s", request, tt.wantOld, tt.wantNew)
			}
			state := &worker.publicIP
			state.mu.Lock()
			baseV4, baseV6 := state.mobikeBaseV4, state.mobikeBaseV6
			state.mu.Unlock()
			if baseV4 != "" || baseV6 != "" {
				t.Fatalf("retired-family base leaked: v4=%q v6=%q", baseV4, baseV6)
			}
		})
	}
}
func TestPublicIPRotationChangePairRequiresUnambiguousCrossFamilyTransition(t *testing.T) {
	oldV6 := "2606:4700:4700::1111"
	newV6 := "2001:4860:4860::8888"
	tests := []struct {
		name        string
		baseline    publicIPRotationBaseline
		observation publicIPRotationObservation
		wantOld     string
		wantNew     string
		wantChanged bool
	}{
		{
			name:        "v4 only to v6 only",
			baseline:    publicIPRotationBaseline{publicV4: "8.8.8.8"},
			observation: publicIPRotationObservation{publicV6: newV6, expectedV6: true},
			wantOld:     "8.8.8.8", wantNew: newV6, wantChanged: true,
		},
		{
			name:        "v6 only to v4 only",
			baseline:    publicIPRotationBaseline{publicV6: oldV6},
			observation: publicIPRotationObservation{publicV4: "9.9.9.9", expectedV4: true},
			wantOld:     oldV6, wantNew: "9.9.9.9", wantChanged: true,
		},
		{
			name:        "dual stack to v6 only transitions retired v4",
			baseline:    publicIPRotationBaseline{publicV4: "8.8.8.8", publicV6: oldV6},
			observation: publicIPRotationObservation{publicV6: oldV6, expectedV6: true},
			wantOld:     "8.8.8.8", wantNew: oldV6, wantChanged: true,
		},
		{
			name:        "dual stack to v4 only transitions retired v6",
			baseline:    publicIPRotationBaseline{publicV4: "8.8.8.8", publicV6: oldV6},
			observation: publicIPRotationObservation{publicV4: "8.8.8.8", expectedV4: true},
			wantOld:     oldV6, wantNew: "8.8.8.8", wantChanged: true,
		},
		{
			name:        "surviving family change takes priority",
			baseline:    publicIPRotationBaseline{publicV4: "8.8.8.8", publicV6: oldV6},
			observation: publicIPRotationObservation{publicV6: newV6, expectedV6: true},
			wantOld:     oldV6, wantNew: newV6, wantChanged: true,
		},
		{
			name:        "dual stack must wait for matching family",
			baseline:    publicIPRotationBaseline{publicV4: "8.8.8.8"},
			observation: publicIPRotationObservation{publicV6: newV6, expectedV4: true, expectedV6: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldIP, newIP, changed := publicIPRotationChangePair(test.baseline, test.observation)
			if oldIP != test.wantOld || newIP != test.wantNew || changed != test.wantChanged {
				t.Fatalf("change pair = (%q, %q, %t), want (%q, %q, %t)",
					oldIP, newIP, changed, test.wantOld, test.wantNew, test.wantChanged)
			}
		})
	}
}

func TestPublicIPProbeSelfConvergesWhenPrivateAddressChangesMidFlight(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("10.0.0.2", "")
	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	controller.setProbe(func(context.Context) (string, string) {
		if controller.probeCalls.Load() == 1 {
			close(probeStarted)
			<-probeRelease
			return "8.8.8.8", ""
		}
		return "9.9.9.9", ""
	})

	p.refreshIPs(worker, true)
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("first probe did not start")
	}
	controller.setPrivate("10.0.0.3", "")
	close(probeRelease)

	waitPublicIPTest(t, time.Second, func() bool {
		v4, _ := publicIPTestCached(worker)
		return controller.probeCalls.Load() >= 2 && v4 == "9.9.9.9"
	})
	state := &worker.publicIP
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.privateV4 != "10.0.0.3" || state.inFlight {
		t.Fatalf("state did not converge: private=%q in_flight=%t", state.privateV4, state.inFlight)
	}
}

func TestRotateWithNotifyDoesNotRedialAfterObservationTimeout(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("10.0.0.2", "")
	seedPublicIPRuntime(worker, "10.0.0.2", "", "8.8.8.8", "")

	originalWait := publicIPLookupWait
	publicIPLookupWait = 30 * time.Millisecond
	t.Cleanup(func() { publicIPLookupWait = originalWait })

	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	controller.setProbe(func(context.Context) (string, string) {
		close(probeStarted)
		<-probeRelease
		return "9.9.9.9", ""
	})

	oldIP, newIP, err := worker.RotateWithNotify()
	if err != nil {
		t.Fatalf("RotateWithNotify() error = %v", err)
	}
	if oldIP != "8.8.8.8" || newIP != "Unknown" {
		t.Fatalf("RotateWithNotify() = (%q, %q), want old and pending result", oldIP, newIP)
	}
	if got := controller.rotateCalls.Load(); got != 1 {
		t.Fatalf("RotateIP calls = %d, want 1", got)
	}
	select {
	case <-probeStarted:
	default:
		t.Fatal("background probe did not start")
	}
	close(probeRelease)
	waitPublicIPTest(t, time.Second, func() bool {
		state := &worker.publicIP
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.publishedV4 == "9.9.9.9" && state.authoritativeV4 == "9.9.9.9"
	})
	if got := controller.rotateCalls.Load(); got != 1 {
		t.Fatalf("late result caused %d RotateIP calls", got)
	}
}
func TestPublicIPSameIDReplacementRejectsOldWorkerPublication(t *testing.T) {
	p, oldWorker, _ := newPublicIPStateHarness(t)
	seedPublicIPRuntime(oldWorker, "10.0.0.2", "", "8.8.8.8", "")
	snapshot := publicIPTestSnapshot(oldWorker)

	newWorker := &Worker{ID: oldWorker.ID, Pool: p, stop: make(chan struct{})}
	p.mu.Lock()
	newWorker.generation = p.workerGenerations[oldWorker.ID] + 1
	p.workerGenerations[oldWorker.ID] = newWorker.generation
	p.workers[oldWorker.ID] = newWorker
	p.mu.Unlock()
	t.Cleanup(func() { p.stopPublicIPState(newWorker) })

	result := p.publishPublicIPState(
		oldWorker, snapshot, "8.8.4.4", "",
		publicIPPublishOptions{updateV4: true, authoritativeV4: true},
	)
	if result.applied {
		t.Fatal("old worker publication was applied after same-ID replacement")
	}
	if v4, _ := publicIPTestCached(oldWorker); v4 != "8.8.8.8" {
		t.Fatalf("old worker cache changed to %q", v4)
	}
	if v4, v6 := publicIPTestCached(newWorker); v4 != "" || v6 != "" {
		t.Fatalf("replacement cache was polluted: (%q, %q)", v4, v6)
	}
}

func TestPublicIPPendingMOBIKECoalescesLatestSameFamilyBeforeDelivery(t *testing.T) {
	p, worker, _ := newPublicIPStateHarness(t)
	seedPublicIPRuntime(worker, "10.0.0.2", "", "8.8.8.8", "")
	session := attachPublicIPTestRuntime(t, p, worker.ID)
	snapshot := publicIPTestSnapshot(worker)

	first := p.publishPublicIPState(
		worker, snapshot, "8.8.4.4", "",
		publicIPPublishOptions{updateV4: true, authoritativeV4: true},
	)
	if !first.applied {
		t.Fatal("first publication was not applied")
	}

	state := &worker.publicIP
	state.mu.Lock()
	state.cycleMOBIKEReserved = false
	state.mu.Unlock()

	second := p.publishPublicIPState(
		worker, snapshot, "9.9.9.9", "",
		publicIPPublishOptions{updateV4: true, authoritativeV4: true},
	)
	if !second.applied {
		t.Fatal("second publication was not applied")
	}

	p.triggerPublicIPMOBIKE(worker)
	if got := session.calls.Load(); got != 1 {
		t.Fatalf("coalesced publication triggered %d MOBIKE calls", got)
	}
	request := session.lastRequest()
	if request.OldIP != "8.8.8.8" || request.NewIP != "9.9.9.9" {
		t.Fatalf("MOBIKE request=%+v", request)
	}
}
func TestPublicIPPendingMOBIKEDropsCoalescedRoundTrip(t *testing.T) {
	p, worker, _ := newPublicIPStateHarness(t)
	seedPublicIPRuntime(worker, "10.0.0.2", "", "8.8.8.8", "")
	session := attachPublicIPTestRuntime(t, p, worker.ID)
	snapshot := publicIPTestSnapshot(worker)

	first := p.publishPublicIPState(
		worker, snapshot, "8.8.4.4", "",
		publicIPPublishOptions{updateV4: true, authoritativeV4: true},
	)
	if !first.applied {
		t.Fatal("first publication was not applied")
	}

	state := &worker.publicIP
	state.mu.Lock()
	state.cycleMOBIKEReserved = false
	state.mu.Unlock()

	second := p.publishPublicIPState(
		worker, snapshot, "8.8.8.8", "",
		publicIPPublishOptions{updateV4: true, authoritativeV4: true},
	)
	if !second.applied {
		t.Fatal("round-trip publication was not applied")
	}

	p.triggerPublicIPMOBIKE(worker)
	if got := session.calls.Load(); got != 0 {
		t.Fatalf("coalesced A -> B -> A publication triggered %d MOBIKE calls", got)
	}
	state.mu.Lock()
	pending, inFlight := state.pendingMOBIKE, state.mobikeInFlight
	state.mu.Unlock()
	if pending != nil || inFlight != 0 {
		t.Fatalf("no-op event was not drained: pending=%v in_flight=%d", pending != nil, inFlight)
	}
}

func TestPublicIPNoAddressUsesBackoffWithoutLaunchingProbe(t *testing.T) {
	_, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("", "")

	worker.Pool.refreshIPs(worker, true)
	state := &worker.publicIP
	state.mu.Lock()
	firstTimer := state.retryTimer
	firstTries := state.noAddressTries
	state.mu.Unlock()
	if firstTries != 1 || firstTimer == nil || controller.probeCalls.Load() != 0 {
		t.Fatalf("first no-address state tries=%d timer=%v probes=%d", firstTries, firstTimer != nil, controller.probeCalls.Load())
	}

	worker.Pool.refreshIPs(worker, true)
	state.mu.Lock()
	secondTimer := state.retryTimer
	secondTries := state.noAddressTries
	state.mu.Unlock()
	if secondTries != 2 || secondTimer == nil || secondTimer == firstTimer || controller.probeCalls.Load() != 0 {
		t.Fatalf("second no-address state tries=%d replaced=%t probes=%d", secondTries, secondTimer != nil && secondTimer != firstTimer, controller.probeCalls.Load())
	}
	if retryDelay(2) <= retryDelay(1) {
		t.Fatalf("retry delay did not back off: first=%s second=%s", retryDelay(1), retryDelay(2))
	}
}

func TestPublicIPReconnectAddressLagRetainsMOBIKEBaseline(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("10.0.0.2", "")
	controller.setPublic("8.8.8.8", "")
	seedPublicIPRuntime(worker, "10.0.0.2", "", "8.8.8.8", "")
	session := attachPublicIPTestRuntime(t, p, worker.ID)

	p.invalidatePublicIPState(worker, false)
	controller.setPrivate("", "")
	controller.setPublic("8.8.4.4", "")
	p.refreshIPs(worker, true)

	state := &worker.publicIP
	state.mu.Lock()
	if !state.connected || state.privateV4 != "" || state.mobikeBaseV4 != "8.8.8.8" {
		state.mu.Unlock()
		t.Fatalf("address-lag state connected=%t private=%q baseline=%q", state.connected, state.privateV4, state.mobikeBaseV4)
	}
	state.mu.Unlock()

	controller.setPrivate("10.0.0.3", "")
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		v4, _ := publicIPTestCached(worker)
		return v4 == "8.8.4.4" && session.calls.Load() == 1
	})

	request := session.lastRequest()
	if request.OldIP != "8.8.8.8" || request.NewIP != "8.8.4.4" {
		t.Fatalf("MOBIKE request=%+v", request)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.mobikeBaseV4 != "" || state.publishedV4 != "8.8.4.4" {
		t.Fatalf("resolved state published=%q baseline=%q", state.publishedV4, state.mobikeBaseV4)
	}
}

func TestStopNetworkClearsMOBIKEBaseline(t *testing.T) {
	_, worker, controller := newPublicIPStateHarness(t)
	controller.setPrivate("10.0.0.2", "fd00::2")
	seedPublicIPRuntime(worker, "10.0.0.2", "fd00::2", "8.8.8.8", "2606:4700:4700::1111")

	state := &worker.publicIP
	state.mu.Lock()
	state.rotating = true
	state.mu.Unlock()

	if err := worker.StopNetwork(); err != nil {
		t.Fatalf("StopNetwork() error = %v", err)
	}
	if controller.IsConnected() {
		t.Fatal("controller remained connected")
	}
	if v4, v6 := publicIPTestCached(worker); v4 != "" || v6 != "" {
		t.Fatalf("explicit stop left cache populated: (%q, %q)", v4, v6)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.connected || state.rotating ||
		state.publishedV4 != "" || state.publishedV6 != "" ||
		state.mobikeBaseV4 != "" || state.mobikeBaseV6 != "" {
		t.Fatalf("explicit stop state connected=%t rotating=%t published=(%q,%q) baseline=(%q,%q)",
			state.connected, state.rotating,
			state.publishedV4, state.publishedV6,
			state.mobikeBaseV4, state.mobikeBaseV6)
	}
}
