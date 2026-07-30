package device

import (
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zanescope/vohive/internal/db"
	"github.com/zanescope/vohive/internal/netprobe"
	"github.com/zanescope/vohive/pkg/logger"
)

const (
	publicIPRetryBase               = 2 * time.Second
	publicIPRetryMax                = 5 * time.Minute
	publicIPRetryLimit              = 6
	publicIPHealthyRecheckInterval  = 10 * time.Minute
	publicIPDegradedRecheckInterval = 2 * time.Hour
)

var publicIPProbeCycleTimeout = 15 * time.Second

type publicIPTimer interface {
	Stop() bool
}

type publicIPTimerFactory func(time.Duration, func()) publicIPTimer

type publicIPProbePhase uint8

const (
	publicIPProbePhaseIdle publicIPProbePhase = iota
	publicIPProbePhaseHealthy
	publicIPProbePhaseFastRetry
	publicIPProbePhaseDegraded
)

func (phase publicIPProbePhase) String() string {
	switch phase {
	case publicIPProbePhaseHealthy:
		return "healthy"
	case publicIPProbePhaseFastRetry:
		return "fast_retry"
	case publicIPProbePhaseDegraded:
		return "degraded"
	default:
		return "idle"
	}
}

type publicIPRefreshTrigger uint8

const (
	publicIPRefreshObservation publicIPRefreshTrigger = iota
	publicIPRefreshEvent
	publicIPRefreshTimer
	publicIPRefreshReconnect
	publicIPRefreshRotation
)

type publicIPTimerKind uint8

const (
	publicIPTimerNone publicIPTimerKind = iota
	publicIPTimerFastRetry
	publicIPTimerHealthy
	publicIPTimerDegraded
)

type publicIPRuntime struct {
	mu        sync.Mutex
	persistMu sync.Mutex
	publishMu sync.Mutex
	mobikeMu  sync.Mutex

	initialized bool
	connected   bool
	epoch       uint64
	privateV4   string
	privateV6   string
	expectedV4  bool
	expectedV6  bool
	rotating    bool

	publishedV4     string
	publishedV6     string
	authoritativeV4 string
	authoritativeV6 string
	mobikeBaseV4    string
	mobikeBaseV6    string

	inFlight            bool
	pending             bool
	cancel              context.CancelFunc
	probeSeq            uint64
	revision            uint64
	retrying            bool
	cycleResolvedV4     bool
	cycleResolvedV6     bool
	cycleMOBIKEReserved bool
	phase               publicIPProbePhase
	degradedSince       time.Time
	degradedSlowChecks  int
	degradedWarned      bool
	lastFailureClass    string
	nextProbeAt         time.Time
	timerKind           publicIPTimerKind
	retryLimit          int
	now                 func() time.Time
	scheduleTimer       publicIPTimerFactory

	mobikeSeq                uint64
	mobikeInFlight           uint64
	mobikeTransitionReserved bool
	pendingMOBIKE            *publicIPMOBIKEEvent

	retryAttemptV4 int
	retryAttemptV6 int
	noAddressTries int
	retryTimer     publicIPTimer
	periodicTimer  publicIPTimer
}
type publicIPProbeWithContext interface {
	GetPublicIPv4AndV6Context(context.Context) (publicV4 string, publicV6 string)
}

type publicIPSnapshot struct {
	epoch      uint64
	probeSeq   uint64
	generation uint64
	connected  bool
	privateV4  string
	privateV6  string
}

type publicIPMOBIKEEvent struct {
	id         uint64
	epoch      uint64
	family     netprobe.Family
	oldIP      string
	newIP      string
	transition bool
}

type publicIPPublishOptions struct {
	updateV4          bool
	updateV6          bool
	replace           bool
	authoritativeV4   bool
	authoritativeV6   bool
	persist           bool
	forceBroadcast    bool
	allowUnregistered bool
}

type publicIPPublishResult struct {
	applied      bool
	revision     uint64
	changed      bool
	oldV4, oldV6 string
	newV4, newV6 string
}

func (p *Pool) isCurrentPublicIPWorker(worker *Worker) bool {
	if p == nil || worker == nil {
		return false
	}
	p.mu.RLock()
	current := p.workers[worker.ID]
	generation := p.workerGenerations[worker.ID]
	p.mu.RUnlock()
	if current != worker {
		return false
	}
	return generation == 0 || worker.generation == 0 || generation == worker.generation
}

func normalizeBearerIP(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	host := value
	if parsedHost, _, err := net.SplitHostPort(value); err == nil {
		host = parsedHost
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if !addr.IsGlobalUnicast() {
		return ""
	}
	return addr.String()
}

func isUsablePublicIP(value string, wantV4 bool) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	return addr.Is4() == wantV4 && netprobe.IsPublicAddress(addr)
}

func expectedBearerFamilies(privateV4, privateV6 string) (bool, bool) {
	v4 := normalizeBearerIP(privateV4)
	v6 := normalizeBearerIP(privateV6)
	return v4 != "" && netip.MustParseAddr(v4).Is4(), v6 != "" && netip.MustParseAddr(v6).Is6()
}

func stablePublicIPDelay(workerID string, epoch uint64, base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(workerID))
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(strconv.FormatUint(epoch, 10)))
	factor := int64(h.Sum32()%21) - 10
	return base + time.Duration(int64(base)*factor/100)
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 7 {
		shift = 7
	}
	delay := publicIPRetryBase * time.Duration(1<<shift)
	if delay > publicIPRetryMax {
		delay = publicIPRetryMax
	}
	return delay
}

func publicIPRetryLimitFor(state *publicIPRuntime) int {
	if state != nil && state.retryLimit > 0 {
		return state.retryLimit
	}
	return publicIPRetryLimit
}

func publicIPNowFor(state *publicIPRuntime) time.Time {
	if state != nil && state.now != nil {
		return state.now()
	}
	return time.Now()
}

func publicIPTimerFactoryFor(state *publicIPRuntime) publicIPTimerFactory {
	if state != nil && state.scheduleTimer != nil {
		return state.scheduleTimer
	}
	return func(delay time.Duration, fn func()) publicIPTimer {
		return time.AfterFunc(delay, fn)
	}
}

func saturatingPublicIPIncrement(value, limit int) int {
	if value < 0 {
		value = 0
	}
	if limit > 0 && value >= limit {
		return limit
	}
	maxInt := int(^uint(0) >> 1)
	if value >= maxInt {
		return maxInt
	}
	return value + 1
}

func resetPublicIPProbeCadenceLocked(state *publicIPRuntime, phase publicIPProbePhase) {
	state.phase = phase
	state.retrying = false
	state.retryAttemptV4 = 0
	state.retryAttemptV6 = 0
	state.noAddressTries = 0
	state.degradedSince = time.Time{}
	state.degradedSlowChecks = 0
	state.degradedWarned = false
	state.lastFailureClass = ""
}

func enterPublicIPDegradedLocked(state *publicIPRuntime, failureClass string) bool {
	if state.phase != publicIPProbePhaseDegraded {
		state.phase = publicIPProbePhaseDegraded
		state.degradedSince = publicIPNowFor(state)
		state.degradedSlowChecks = 0
	} else {
		state.degradedSlowChecks = saturatingPublicIPIncrement(state.degradedSlowChecks, 0)
	}
	state.retrying = false
	state.lastFailureClass = failureClass
	if state.degradedWarned {
		return false
	}
	state.degradedWarned = true
	return true
}

func stopPublicIPTimersLocked(state *publicIPRuntime) {
	if state.cancel != nil {
		state.cancel()
		state.cancel = nil
	}
	if state.retryTimer != nil {
		state.retryTimer.Stop()
		state.retryTimer = nil
	}
	if state.periodicTimer != nil {
		state.periodicTimer.Stop()
		state.periodicTimer = nil
	}
	state.inFlight = false
	state.pending = false
	state.nextProbeAt = time.Time{}
	state.timerKind = publicIPTimerNone
}

func (p *Pool) schedulePublicIPTimerLocked(worker *Worker, state *publicIPRuntime, delay time.Duration, kind publicIPTimerKind) {
	epoch := state.epoch
	phase := state.phase
	now := publicIPNowFor(state)
	scheduleTimer := publicIPTimerFactoryFor(state)
	delay = stablePublicIPDelay(worker.ID, epoch, delay)
	periodic := kind != publicIPTimerFastRetry
	var timer publicIPTimer
	fire := func() {
		state.mu.Lock()
		if state.epoch != epoch || !state.connected || state.phase != phase || state.timerKind != kind {
			state.mu.Unlock()
			return
		}
		if periodic {
			if state.periodicTimer != timer {
				state.mu.Unlock()
				return
			}
			state.periodicTimer = nil
		} else {
			if state.retryTimer != timer {
				state.mu.Unlock()
				return
			}
			state.retryTimer = nil
		}
		state.nextProbeAt = time.Time{}
		state.timerKind = publicIPTimerNone
		state.mu.Unlock()
		p.refreshIPsWithTrigger(worker, publicIPRefreshTimer)
	}
	if periodic {
		if state.periodicTimer != nil {
			state.periodicTimer.Stop()
		}
		timer = scheduleTimer(delay, fire)
		state.periodicTimer = timer
	} else {
		if state.retryTimer != nil {
			state.retryTimer.Stop()
		}
		timer = scheduleTimer(delay, fire)
		state.retryTimer = timer
	}
	state.nextProbeAt = now.Add(delay)
	state.timerKind = kind
}

func publicIPStateMatchesLocked(worker *Worker, state *publicIPRuntime, snapshot publicIPSnapshot) bool {
	if worker.generation != snapshot.generation || state.epoch != snapshot.epoch ||
		state.connected != snapshot.connected {
		return false
	}
	if snapshot.probeSeq != 0 && state.probeSeq != snapshot.probeSeq {
		return false
	}
	if snapshot.connected {
		return state.privateV4 == snapshot.privateV4 && state.privateV6 == snapshot.privateV6
	}
	return true
}

func queuePublicIPMOBIKELocked(state *publicIPRuntime, family netprobe.Family, oldIP, newIP string, transition bool) {
	oldIP = strings.TrimSpace(oldIP)
	newIP = strings.TrimSpace(newIP)
	if oldIP == "" || newIP == "" || oldIP == newIP {
		return
	}
	if state.pendingMOBIKE != nil {
		state.cycleMOBIKEReserved = true
		if transition {
			state.mobikeTransitionReserved = true
		}
		if state.pendingMOBIKE.family == family {
			state.pendingMOBIKE.newIP = newIP
			state.pendingMOBIKE.transition = state.pendingMOBIKE.transition || transition
		}
		return
	}
	if state.cycleMOBIKEReserved {
		return
	}
	if transition && state.mobikeTransitionReserved {
		return
	}
	state.cycleMOBIKEReserved = true
	if transition {
		state.mobikeTransitionReserved = true
	}
	state.mobikeSeq++
	state.pendingMOBIKE = &publicIPMOBIKEEvent{
		id:         state.mobikeSeq,
		epoch:      state.epoch,
		family:     family,
		oldIP:      oldIP,
		newIP:      newIP,
		transition: transition,
	}
}

func observeAuthoritativePublicIPLocked(state *publicIPRuntime, family netprobe.Family, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	current := &state.authoritativeV4
	base := &state.mobikeBaseV4
	otherBase := &state.mobikeBaseV6
	expected := state.expectedV4
	otherExpected := state.expectedV6
	if family == netprobe.FamilyV6 {
		current = &state.authoritativeV6
		base = &state.mobikeBaseV6
		otherBase = &state.mobikeBaseV4
		expected = state.expectedV6
		otherExpected = state.expectedV4
	}

	previous := *current
	if previous != "" {
		*current = value
		queuePublicIPMOBIKELocked(state, family, previous, value, false)
		return
	}

	baseline := *base
	singleFamily := expected && !otherExpected
	if singleFamily && *otherBase != "" && (baseline == "" || baseline == value) {
		// If the surviving family is unchanged, the retired family's address
		// may still be the tunnel's active path. Transition it to the sole
		// remaining address instead of silently treating the epoch as a no-op.
		baseline = *otherBase
	}
	*current = value
	*base = ""
	if singleFamily {
		// A base for a family that is no longer present must not leak into a
		// later epoch and become the old side of an unrelated transition.
		*otherBase = ""
	}
	queuePublicIPMOBIKELocked(state, family, baseline, value, true)
}

func (p *Pool) publishPublicIPState(
	worker *Worker,
	snapshot publicIPSnapshot,
	publicV4, publicV6 string,
	options publicIPPublishOptions,
) publicIPPublishResult {
	result := publicIPPublishResult{}
	if p == nil || worker == nil {
		return result
	}
	state := &worker.publicIP
	state.publishMu.Lock()
	defer state.publishMu.Unlock()

	if !options.allowUnregistered && !p.isCurrentPublicIPWorker(worker) {
		return result
	}
	state.mu.Lock()
	if !publicIPStateMatchesLocked(worker, state, snapshot) {
		state.mu.Unlock()
		return result
	}

	worker.cacheMu.Lock()
	result.oldV4, result.oldV6 = worker.cachedIP, worker.cachedPublicIPv6
	result.newV4, result.newV6 = result.oldV4, result.oldV6
	if options.replace {
		result.newV4, result.newV6 = publicV4, publicV6
	} else {
		if options.updateV4 {
			result.newV4 = publicV4
		}
		if options.updateV6 {
			result.newV6 = publicV6
		}
	}
	worker.cachedIP, worker.cachedPublicIPv6 = result.newV4, result.newV6
	if result.newV4 == "" && result.newV6 == "" {
		worker.cacheTime = time.Time{}
	} else {
		worker.cacheTime = time.Now()
	}
	worker.cacheMu.Unlock()

	result.changed = result.oldV4 != result.newV4 || result.oldV6 != result.newV6
	state.publishedV4, state.publishedV6 = result.newV4, result.newV6
	if options.authoritativeV4 && options.updateV4 && publicV4 != "" {
		observeAuthoritativePublicIPLocked(state, netprobe.FamilyV4, publicV4)
	}
	if options.authoritativeV6 && options.updateV6 && publicV6 != "" {
		observeAuthoritativePublicIPLocked(state, netprobe.FamilyV6, publicV6)
	}
	state.revision++
	result.revision = state.revision
	state.mu.Unlock()

	result.applied = true
	if options.persist {
		if options.allowUnregistered {
			state.persistMu.Lock()
			p.persistPublicIPSnapshot(worker, result.newV4, result.newV6, snapshot.privateV4, snapshot.privateV6)
			state.persistMu.Unlock()
		} else {
			p.persistPublicIPSnapshotAsync(worker, snapshot, result.revision, result.newV4, result.newV6)
		}
	}
	if options.forceBroadcast || result.changed {
		p.broadcastVoWiFiStateChange(worker.ID)
	}
	return result
}
func (p *Pool) invalidatePublicIPState(worker *Worker, persist bool) {
	p.invalidatePublicIPStateWithMode(worker, persist, false)
}

func (p *Pool) invalidateRemovedPublicIPState(worker *Worker) {
	p.invalidatePublicIPStateWithMode(worker, true, true)
}

func (p *Pool) invalidatePublicIPStateWithMode(worker *Worker, persist, allowUnregistered bool) {
	if p == nil || worker == nil {
		return
	}
	if !allowUnregistered && !p.isCurrentPublicIPWorker(worker) {
		return
	}
	state := &worker.publicIP
	state.mu.Lock()
	wasConnected := state.connected || !state.initialized
	stopPublicIPTimersLocked(state)
	state.initialized = true
	if allowUnregistered {
		state.rotating = false
		state.mobikeBaseV4 = ""
		state.mobikeBaseV6 = ""
	} else {
		if state.mobikeBaseV4 == "" {
			state.mobikeBaseV4 = state.authoritativeV4
		}
		if state.mobikeBaseV6 == "" {
			state.mobikeBaseV6 = state.authoritativeV6
		}
	}
	state.connected = false
	state.epoch++
	state.privateV4 = ""
	state.privateV6 = ""
	state.expectedV4 = false
	state.expectedV6 = false
	state.authoritativeV4 = ""
	state.authoritativeV6 = ""
	resetPublicIPProbeCadenceLocked(state, publicIPProbePhaseIdle)
	state.cycleResolvedV4 = false
	state.cycleResolvedV6 = false
	state.cycleMOBIKEReserved = false
	state.mobikeTransitionReserved = false
	state.pendingMOBIKE = nil
	state.mobikeInFlight = 0
	snapshot := publicIPSnapshot{epoch: state.epoch, generation: worker.generation}
	state.mu.Unlock()

	p.publishPublicIPState(worker, snapshot, "", "", publicIPPublishOptions{
		updateV4:          true,
		updateV6:          true,
		replace:           true,
		persist:           persist,
		forceBroadcast:    wasConnected,
		allowUnregistered: allowUnregistered,
	})
}

func (p *Pool) abortPublicIPRotation(worker *Worker) {
	if worker == nil {
		return
	}
	state := &worker.publicIP
	state.mu.Lock()
	state.rotating = false
	state.mobikeBaseV4 = ""
	state.mobikeBaseV6 = ""
	state.mobikeTransitionReserved = false
	state.pendingMOBIKE = nil
	state.mobikeInFlight = 0
	state.mu.Unlock()
}

func (p *Pool) stopPublicIPState(worker *Worker) {
	if worker == nil {
		return
	}
	state := &worker.publicIP
	state.mu.Lock()
	stopPublicIPTimersLocked(state)
	state.rotating = false
	state.mobikeBaseV4 = ""
	state.mobikeBaseV6 = ""
	state.mobikeTransitionReserved = false
	state.pendingMOBIKE = nil
	state.mobikeInFlight = 0
	state.epoch++
	resetPublicIPProbeCadenceLocked(state, publicIPProbePhaseIdle)
	state.mu.Unlock()
}
func cachedWorkerIMEI(worker *Worker) string {
	if worker == nil {
		return ""
	}
	worker.cacheMu.RLock()
	imei := strings.TrimSpace(worker.state.Identity.IMEI)
	worker.cacheMu.RUnlock()
	if imei == "" {
		imei = strings.TrimSpace(worker.Config.ModemIMEI)
	}
	return imei
}

func (p *Pool) persistPublicIPSnapshot(worker *Worker, publicV4, publicV6, privateV4, privateV6 string) {
	if worker == nil || db.DB == nil {
		return
	}
	imei := cachedWorkerIMEI(worker)
	if imei == "" {
		return
	}
	if err := db.ReplaceDeviceIPsV6(imei, publicV4, publicV6, privateV4, privateV6); err != nil {
		logger.Warn(fmt.Sprintf("[%s] 更新数据承载 IP 快照失败", worker.ID), "err", err)
	}
}

func (p *Pool) persistPublicIPSnapshotAsync(worker *Worker, snapshot publicIPSnapshot, revision uint64, publicV4, publicV6 string) {
	if p == nil || worker == nil || db.DB == nil || cachedWorkerIMEI(worker) == "" {
		return
	}
	state := &worker.publicIP
	go func() {
		state.persistMu.Lock()
		defer state.persistMu.Unlock()
		if !p.isCurrentPublicIPWorker(worker) {
			return
		}
		state.mu.Lock()
		valid := publicIPStateMatchesLocked(worker, state, snapshot) && state.revision == revision
		state.mu.Unlock()
		if !valid {
			return
		}
		p.persistPublicIPSnapshot(worker, publicV4, publicV6, snapshot.privateV4, snapshot.privateV6)
	}()
}

type publicIPRotationBaseline struct {
	publicV4  string
	publicV6  string
	privateV4 string
	privateV6 string
}

type publicIPRotationObservation struct {
	publicV4       string
	publicV6       string
	privateV4      string
	privateV6      string
	expectedV4     bool
	expectedV6     bool
	privateChanged bool
}

func publicIPRotationCancellation(p *Pool, worker *Worker) error {
	if p != nil && p.ctx != nil {
		if err := p.ctx.Err(); err != nil {
			return err
		}
	}
	if worker != nil && worker.stop != nil {
		select {
		case <-worker.stop:
			return fmt.Errorf("worker_stopped")
		default:
		}
	}
	return nil
}

func (p *Pool) beginPublicIPRotation(worker *Worker, nc NetworkController) (publicIPRotationBaseline, bool) {
	baseline := publicIPRotationBaseline{}
	if p == nil || worker == nil || nc == nil || !p.isCurrentPublicIPWorker(worker) {
		return baseline, false
	}
	baseline.privateV4 = normalizeBearerIP(nc.GetPrivateIP())
	baseline.privateV6 = normalizeBearerIP(nc.GetPrivateIPv6())

	state := &worker.publicIP
	state.publishMu.Lock()
	defer state.publishMu.Unlock()
	if !p.isCurrentPublicIPWorker(worker) {
		return publicIPRotationBaseline{}, false
	}

	state.mu.Lock()
	baseline.publicV4 = state.authoritativeV4
	baseline.publicV6 = state.authoritativeV6
	if state.mobikeBaseV4 == "" {
		state.mobikeBaseV4 = state.authoritativeV4
	}
	if state.mobikeBaseV6 == "" {
		state.mobikeBaseV6 = state.authoritativeV6
	}

	stopPublicIPTimersLocked(state)
	state.initialized = true
	state.connected = false
	state.rotating = true
	state.epoch++
	state.privateV4, state.privateV6 = "", ""
	state.expectedV4, state.expectedV6 = false, false
	state.publishedV4, state.publishedV6 = "", ""
	state.authoritativeV4, state.authoritativeV6 = "", ""
	resetPublicIPProbeCadenceLocked(state, publicIPProbePhaseIdle)
	state.cycleResolvedV4, state.cycleResolvedV6 = false, false
	state.cycleMOBIKEReserved = false
	state.mobikeTransitionReserved = false
	state.pendingMOBIKE = nil
	state.mobikeInFlight = 0
	state.revision++
	revision := state.revision
	snapshot := publicIPSnapshot{epoch: state.epoch, generation: worker.generation}

	worker.cacheMu.Lock()
	worker.cachedIP, worker.cachedPublicIPv6 = "", ""
	worker.cacheTime = time.Time{}
	worker.cacheMu.Unlock()
	state.mu.Unlock()

	p.persistPublicIPSnapshotAsync(worker, snapshot, revision, "", "")
	p.broadcastVoWiFiStateChange(worker.ID)
	return baseline, true
}

func observePublicIPRotation(worker *Worker, nc NetworkController, baseline publicIPRotationBaseline) publicIPRotationObservation {
	observation := publicIPRotationObservation{}
	if worker == nil || nc == nil {
		return observation
	}
	state := &worker.publicIP
	state.mu.Lock()
	observation.publicV4 = state.authoritativeV4
	observation.publicV6 = state.authoritativeV6
	observation.expectedV4 = state.expectedV4
	observation.expectedV6 = state.expectedV6
	state.mu.Unlock()
	observation.privateV4 = normalizeBearerIP(nc.GetPrivateIP())
	observation.privateV6 = normalizeBearerIP(nc.GetPrivateIPv6())
	hasPrivate := observation.privateV4 != "" || observation.privateV6 != ""
	observation.privateChanged = hasPrivate &&
		(observation.privateV4 != baseline.privateV4 || observation.privateV6 != baseline.privateV6)
	return observation
}
func publicIPRotationChangePair(baseline publicIPRotationBaseline, observation publicIPRotationObservation) (oldIP, newIP string, changed bool) {
	if baseline.publicV4 != "" && observation.publicV4 != "" && observation.publicV4 != baseline.publicV4 {
		return baseline.publicV4, observation.publicV4, true
	}
	if baseline.publicV6 != "" && observation.publicV6 != "" && observation.publicV6 != baseline.publicV6 {
		return baseline.publicV6, observation.publicV6, true
	}
	if observation.expectedV4 && !observation.expectedV6 && baseline.publicV6 != "" &&
		observation.publicV4 != "" &&
		(baseline.publicV4 == "" || observation.publicV4 == baseline.publicV4) &&
		observation.publicV4 != baseline.publicV6 {
		return baseline.publicV6, observation.publicV4, true
	}
	if observation.expectedV6 && !observation.expectedV4 && baseline.publicV4 != "" &&
		observation.publicV6 != "" &&
		(baseline.publicV6 == "" || observation.publicV6 == baseline.publicV6) &&
		observation.publicV6 != baseline.publicV4 {
		return baseline.publicV4, observation.publicV6, true
	}
	return "", "", false
}

func (p *Pool) waitForPublicIPRotation(
	ctx context.Context,
	worker *Worker,
	nc NetworkController,
	baseline publicIPRotationBaseline,
) (publicIPRotationObservation, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var latest publicIPRotationObservation
	for {
		if !p.isCurrentPublicIPWorker(worker) {
			return latest, false, fmt.Errorf("stale_worker")
		}
		latest = observePublicIPRotation(worker, nc, baseline)
		if _, _, changed := publicIPRotationChangePair(baseline, latest); changed {
			return latest, true, nil
		}
		if baseline.publicV4 == "" && baseline.publicV6 == "" && latest.privateChanged && (latest.publicV4 != "" || latest.publicV6 != "") {
			return latest, false, nil
		}
		select {
		case <-ctx.Done():
			if latest.privateChanged {
				return latest, false, nil
			}
			return latest, false, ctx.Err()
		case <-worker.stop:
			return latest, false, fmt.Errorf("worker_stopped")
		case <-ticker.C:
		}
	}
}

func (p *Pool) refreshIPs(worker *Worker, checkPublic bool) {
	trigger := publicIPRefreshObservation
	if checkPublic {
		trigger = publicIPRefreshEvent
	}
	p.refreshIPsWithTrigger(worker, trigger)
}

func (p *Pool) refreshIPsWithTrigger(worker *Worker, trigger publicIPRefreshTrigger) {
	if p == nil || worker == nil || !p.isCurrentPublicIPWorker(worker) {
		return
	}
	if p.ctx != nil && p.ctx.Err() != nil {
		return
	}
	nc := worker.NetworkController()
	if nc == nil || !nc.IsConnected() {
		p.invalidatePublicIPState(worker, true)
		return
	}
	privateV4 := normalizeBearerIP(nc.GetPrivateIP())
	privateV6 := normalizeBearerIP(nc.GetPrivateIPv6())
	expectedV4, expectedV6 := expectedBearerFamilies(privateV4, privateV6)

	state := &worker.publicIP
	state.mu.Lock()
	forceEpoch := trigger == publicIPRefreshReconnect || trigger == publicIPRefreshRotation
	newEpoch := forceEpoch || !state.initialized || !state.connected ||
		state.privateV4 != privateV4 || state.privateV6 != privateV6
	if newEpoch {
		if state.connected {
			if state.mobikeBaseV4 == "" && state.authoritativeV4 != "" {
				state.mobikeBaseV4 = state.authoritativeV4
			}
			if state.mobikeBaseV6 == "" && state.authoritativeV6 != "" {
				state.mobikeBaseV6 = state.authoritativeV6
			}
		}
		stopPublicIPTimersLocked(state)
		state.initialized = true
		state.connected = true
		state.rotating = false
		state.epoch++
		state.privateV4 = privateV4
		state.privateV6 = privateV6
		state.expectedV4 = expectedV4
		state.expectedV6 = expectedV6
		state.authoritativeV4 = ""
		state.authoritativeV6 = ""
		resetPublicIPProbeCadenceLocked(state, publicIPProbePhaseHealthy)
		state.cycleResolvedV4 = false
		state.cycleResolvedV6 = false
		state.cycleMOBIKEReserved = false
		state.mobikeTransitionReserved = false
		state.pendingMOBIKE = nil
		state.mobikeInFlight = 0
	} else {
		switch trigger {
		case publicIPRefreshObservation:
			state.mu.Unlock()
			return
		case publicIPRefreshEvent:
			// An unchanged indication is not a new connectivity epoch. In
			// particular, it must not bypass an active fast/degraded cadence
			// or move the already scheduled deadline.
			if state.inFlight || state.retryTimer != nil || state.periodicTimer != nil ||
				state.phase == publicIPProbePhaseFastRetry || state.phase == publicIPProbePhaseDegraded {
				state.mu.Unlock()
				return
			}
		}
	}
	snapshot := publicIPSnapshot{
		epoch:      state.epoch,
		generation: worker.generation,
		connected:  true,
		privateV4:  privateV4,
		privateV6:  privateV6,
	}

	launchProbe := false
	var (
		failureLog       string
		failureClass     string
		failureNextProbe time.Time
		failureAttempt   int
		failurePhase     publicIPProbePhase
	)
	retryLimit := publicIPRetryLimitFor(state)
	if !expectedV4 && !expectedV6 {
		state.noAddressTries = saturatingPublicIPIncrement(state.noAddressTries, retryLimit)
		failureClass = "no_bearer_address"
		failureAttempt = state.noAddressTries
		if state.noAddressTries < retryLimit {
			state.phase = publicIPProbePhaseFastRetry
			state.retrying = true
			p.schedulePublicIPTimerLocked(worker, state, retryDelay(state.noAddressTries), publicIPTimerFastRetry)
			failureLog = "fast"
		} else {
			if state.retryTimer != nil {
				state.retryTimer.Stop()
				state.retryTimer = nil
			}
			if enterPublicIPDegradedLocked(state, failureClass) {
				failureLog = "entered_degraded"
			} else {
				failureLog = "degraded"
			}
			p.schedulePublicIPTimerLocked(worker, state, publicIPDegradedRecheckInterval, publicIPTimerDegraded)
		}
		failureNextProbe = state.nextProbeAt
		failurePhase = state.phase
	} else if newEpoch || trigger != publicIPRefreshObservation {
		if state.inFlight {
			if trigger == publicIPRefreshTimer {
				state.pending = true
			}
		} else {
			if state.retryTimer != nil {
				state.retryTimer.Stop()
				state.retryTimer = nil
			}
			if state.periodicTimer != nil {
				state.periodicTimer.Stop()
				state.periodicTimer = nil
			}
			state.nextProbeAt = time.Time{}
			state.timerKind = publicIPTimerNone
			if state.phase == publicIPProbePhaseHealthy {
				state.cycleResolvedV4 = false
				state.cycleResolvedV6 = false
				state.cycleMOBIKEReserved = false
			}
			state.probeSeq++
			snapshot.probeSeq = state.probeSeq
			state.inFlight = true
			launchProbe = true
		}
	}
	var probeCtx context.Context
	if launchProbe {
		var cancel context.CancelFunc
		parent := p.ctx
		if parent == nil {
			parent = context.Background()
		}
		probeCtx, cancel = context.WithCancel(parent)
		state.cancel = cancel
	}
	state.mu.Unlock()

	switch failureLog {
	case "entered_degraded":
		logger.Warn("public IP probing entered degraded cadence",
			"device", worker.ID,
			"failure_class", failureClass,
			"attempt", failureAttempt,
			"retry_limit", retryLimit,
			"next_probe_at", failureNextProbe)
	case "fast":
		logger.Debug("public IP probe deferred while bearer address is unavailable",
			"device", worker.ID,
			"phase", failurePhase.String(),
			"attempt", failureAttempt,
			"next_probe_at", failureNextProbe)
	case "degraded":
		logger.Debug("public IP degraded check still has no bearer address",
			"device", worker.ID,
			"phase", failurePhase.String(),
			"attempt", failureAttempt,
			"next_probe_at", failureNextProbe)
	}

	published := p.publishPublicIPState(worker, snapshot, "", "", publicIPPublishOptions{
		replace:        newEpoch,
		persist:        true,
		forceBroadcast: newEpoch,
	})
	if !published.applied {
		if launchProbe {
			state.mu.Lock()
			if publicIPStateMatchesLocked(worker, state, snapshot) {
				stopPublicIPTimersLocked(state)
			}
			state.mu.Unlock()
		}
		return
	}
	if launchProbe {
		go p.runPublicIPProbe(probeCtx, worker, nc, snapshot)
	}
}

func (p *Pool) runPublicIPProbe(ctx context.Context, worker *Worker, nc NetworkController, snapshot publicIPSnapshot) {
	if ctx == nil {
		ctx = context.Background()
	}
	cycleCtx := ctx
	cancelCycle := func() {}
	if publicIPProbeCycleTimeout > 0 {
		cycleCtx, cancelCycle = context.WithTimeout(ctx, publicIPProbeCycleTimeout)
	}
	defer cancelCycle()

	var publicV4, publicV6 string
	if withContext, ok := nc.(publicIPProbeWithContext); ok {
		publicV4, publicV6 = withContext.GetPublicIPv4AndV6Context(cycleCtx)
	} else {
		publicV4, publicV6 = nc.GetPublicIPv4AndV6NoCache()
	}
	poolStopped := p.ctx != nil && p.ctx.Err() != nil
	if ctx.Err() != nil || poolStopped || !p.isCurrentPublicIPWorker(worker) {
		return
	}
	if !nc.IsConnected() {
		p.invalidatePublicIPState(worker, true)
		return
	}
	privateV4 := normalizeBearerIP(nc.GetPrivateIP())
	privateV6 := normalizeBearerIP(nc.GetPrivateIPv6())

	state := &worker.publicIP
	state.mu.Lock()
	if !publicIPStateMatchesLocked(worker, state, snapshot) {
		state.mu.Unlock()
		return
	}
	if privateV4 != snapshot.privateV4 || privateV6 != snapshot.privateV6 {
		state.inFlight = false
		state.pending = false
		if state.cancel != nil {
			state.cancel()
			state.cancel = nil
		}
		state.mu.Unlock()
		p.refreshIPsWithTrigger(worker, publicIPRefreshEvent)
		return
	}
	state.inFlight = false
	if state.cancel != nil {
		state.cancel()
		state.cancel = nil
	}
	pending := state.pending
	state.pending = false

	expectedV4, expectedV6 := state.expectedV4, state.expectedV6
	localPublicV4, localPublicV6 := "", ""
	if expectedV4 && isUsablePublicIP(snapshot.privateV4, true) {
		localPublicV4 = snapshot.privateV4
	}
	if expectedV6 && isUsablePublicIP(snapshot.privateV6, false) {
		localPublicV6 = snapshot.privateV6
	}
	if !expectedV4 || !isUsablePublicIP(publicV4, true) {
		publicV4 = ""
	}
	if !expectedV6 || !isUsablePublicIP(publicV6, false) {
		publicV6 = ""
	}
	if publicV4 != "" {
		state.cycleResolvedV4 = true
	}
	if publicV6 != "" {
		state.cycleResolvedV6 = true
	}

	wasDegraded := state.phase == publicIPProbePhaseDegraded
	retryLimit := publicIPRetryLimitFor(state)
	v4Resolved := !expectedV4 || state.cycleResolvedV4
	v6Resolved := !expectedV6 || state.cycleResolvedV6
	if v4Resolved {
		state.retryAttemptV4 = 0
	} else {
		state.retryAttemptV4 = saturatingPublicIPIncrement(state.retryAttemptV4, retryLimit)
	}
	if v6Resolved {
		state.retryAttemptV6 = 0
	} else {
		state.retryAttemptV6 = saturatingPublicIPIncrement(state.retryAttemptV6, retryLimit)
	}
	var (
		failureLog            string
		failureNextProbe      time.Time
		failureAttempt        int
		recoveredFromDegraded bool
		recoveredDegradedFor  time.Duration
		recoveredSlowChecks   int
		recoveredFailureClass string
	)
	if v4Resolved && v6Resolved {
		if wasDegraded {
			recoveredFromDegraded = true
			recoveredDegradedFor = publicIPNowFor(state).Sub(state.degradedSince)
			if recoveredDegradedFor < 0 {
				recoveredDegradedFor = 0
			}
			recoveredSlowChecks = state.degradedSlowChecks
			recoveredFailureClass = state.lastFailureClass
		}
		resetPublicIPProbeCadenceLocked(state, publicIPProbePhaseHealthy)
		p.schedulePublicIPTimerLocked(worker, state, publicIPHealthyRecheckInterval, publicIPTimerHealthy)
	} else {
		attempt := state.retryAttemptV4
		if attempt == 0 || (state.retryAttemptV6 > 0 && state.retryAttemptV6 < attempt) {
			attempt = state.retryAttemptV6
		}
		failureAttempt = attempt
		if attempt < retryLimit {
			state.phase = publicIPProbePhaseFastRetry
			state.retrying = true
			failureLog = "fast"
			p.schedulePublicIPTimerLocked(worker, state, retryDelay(attempt), publicIPTimerFastRetry)
		} else {
			if state.retryTimer != nil {
				state.retryTimer.Stop()
				state.retryTimer = nil
			}
			if enterPublicIPDegradedLocked(state, "public_ip_probe") {
				failureLog = "entered_degraded"
			} else {
				failureLog = "degraded"
			}
			p.schedulePublicIPTimerLocked(worker, state, publicIPDegradedRecheckInterval, publicIPTimerDegraded)
		}
		failureNextProbe = state.nextProbeAt
	}

	displayV4, displayV6 := publicV4, publicV6
	updateV4, updateV6 := publicV4 != "", publicV6 != ""
	if !updateV4 && localPublicV4 != "" && state.authoritativeV4 == "" && state.publishedV4 == "" {
		displayV4 = localPublicV4
		updateV4 = true
	}
	if !updateV6 && localPublicV6 != "" && state.authoritativeV6 == "" && state.publishedV6 == "" {
		displayV6 = localPublicV6
		updateV6 = true
	}
	state.mu.Unlock()

	published := p.publishPublicIPState(worker, snapshot, displayV4, displayV6, publicIPPublishOptions{
		updateV4:        updateV4,
		updateV6:        updateV6,
		authoritativeV4: publicV4 != "",
		authoritativeV6: publicV6 != "",
		persist:         updateV4 || updateV6,
	})
	if published.applied {
		if published.changed {
			logger.Info(fmt.Sprintf("[%s] 公网 IP 已更新", worker.ID),
				"old_ip", published.oldV4, "new_ip", published.newV4,
				"old_ipv6", published.oldV6, "new_ipv6", published.newV6)
		}
		if recoveredFromDegraded {
			logger.Info("public IP probing recovered from degraded cadence",
				"device", worker.ID,
				"failure_class", recoveredFailureClass,
				"degraded_for", recoveredDegradedFor,
				"slow_checks", recoveredSlowChecks)
		}
		p.triggerPublicIPMOBIKE(worker)
	}
	switch failureLog {
	case "entered_degraded":
		logger.Warn("public IP probing entered degraded cadence",
			"device", worker.ID,
			"failure_class", "public_ip_probe",
			"ipv4_resolved", v4Resolved,
			"ipv6_resolved", v6Resolved,
			"attempt", failureAttempt,
			"retry_limit", retryLimit,
			"next_probe_at", failureNextProbe)
	case "fast":
		logger.Debug("public IP probe incomplete; fast retry scheduled",
			"device", worker.ID,
			"ipv4_resolved", v4Resolved,
			"ipv6_resolved", v6Resolved,
			"attempt", failureAttempt,
			"next_probe_at", failureNextProbe)
	case "degraded":
		logger.Debug("public IP degraded probe remains incomplete",
			"device", worker.ID,
			"ipv4_resolved", v4Resolved,
			"ipv6_resolved", v6Resolved,
			"attempt", failureAttempt,
			"next_probe_at", failureNextProbe)
	}
	if pending {
		p.refreshIPsWithTrigger(worker, publicIPRefreshTimer)
	}
}

func (p *Pool) triggerPublicIPMOBIKE(worker *Worker) {
	if p == nil || worker == nil {
		return
	}
	state := &worker.publicIP
	state.mobikeMu.Lock()
	defer state.mobikeMu.Unlock()

	for {
		if !p.isCurrentPublicIPWorker(worker) {
			return
		}
		state.mu.Lock()
		event := state.pendingMOBIKE
		if event == nil {
			state.mu.Unlock()
			return
		}
		if event.epoch != state.epoch {
			state.pendingMOBIKE = nil
			state.mu.Unlock()
			continue
		}
		// Coalescing can fold A -> B -> A back into a no-op while another
		// MOBIKE call is still running. Keep the reservation semantics, but do
		// not issue a meaningless A -> A trigger when the queue is drained.
		if strings.TrimSpace(event.oldIP) == strings.TrimSpace(event.newIP) {
			state.pendingMOBIKE = nil
			state.mu.Unlock()
			continue
		}
		state.pendingMOBIKE = nil
		state.mobikeInFlight = event.id
		state.mu.Unlock()

		if !p.isCurrentPublicIPWorker(worker) {
			return
		}
		state.mu.Lock()
		current := state.epoch == event.epoch && state.mobikeInFlight == event.id
		state.mu.Unlock()
		if !current {
			continue
		}

		if app := p.voWiFiHost().Instance(worker.ID); app != nil {
			if err := app.TriggerMOBIKE(event.oldIP, event.newIP); err != nil {
				logger.Warn(fmt.Sprintf("[%s] MOBIKE 漫游触发失败", worker.ID), "err", err)
			}
		}

		state.mu.Lock()
		if state.mobikeInFlight == event.id {
			state.mobikeInFlight = 0
		}
		state.mu.Unlock()
	}
}
