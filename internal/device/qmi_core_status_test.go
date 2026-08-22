package device

import (
	"sync"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
	qmicore "github.com/zanescope/vohive/internal/qmi"
)

func newQMICoreStatusHarness(t *testing.T) (*Pool, *Worker) {
	t.Helper()
	p := NewPool(&config.Config{})
	w := &Worker{
		ID:      "qmi-core-status-test",
		Pool:    p,
		QMICore: &qmicore.Manager{},
		stop:    make(chan struct{}),
	}
	if err := p.registerWorkerStarting(w); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	t.Cleanup(p.cancel)
	return p, w
}

func readyQMICoreStatus(sequence, generation uint64) qmicore.CoreStatus {
	return qmicore.CoreStatus{
		Sequence:     sequence,
		Generation:   generation,
		Phase:        qmicore.CorePhaseReady,
		ControlReady: true,
		CoreReady:    true,
		Stage:        "ready",
		UpdatedAt:    time.Now(),
	}
}

func recoveringQMICoreStatus(sequence, generation uint64) qmicore.CoreStatus {
	return qmicore.CoreStatus{
		Sequence:   sequence,
		Generation: generation,
		Phase:      qmicore.CorePhaseRecovering,
		Recovering: true,
		Stage:      "recovering",
		Reason:     "test_recovery",
		UpdatedAt:  time.Now(),
	}
}

func TestQMICoreStatusIsAuthoritativeAndRejectsStaleSnapshots(t *testing.T) {
	p, w := newQMICoreStatusHarness(t)

	starting := qmicore.CoreStatus{
		Sequence:   1,
		Generation: 1,
		Phase:      qmicore.CorePhaseStarting,
		Stage:      "allocate_services",
		UpdatedAt:  time.Now(),
	}
	if !p.applyQMICoreStatus(w, starting) {
		t.Fatal("starting status was rejected")
	}
	if w.qmiControlTasksReady() {
		t.Fatal("starting core was marked ready")
	}
	if got := p.LifecycleSnapshot(w.ID).Phase; got != LifecyclePhaseQMIStarting {
		t.Fatalf("lifecycle phase=%s want=%s", got, LifecyclePhaseQMIStarting)
	}

	if !p.applyQMICoreStatus(w, readyQMICoreStatus(2, 1)) {
		t.Fatal("ready status was rejected")
	}
	if !w.qmiControlTasksReady() {
		t.Fatal("ready core was not published to worker readiness")
	}
	if got := p.LifecycleSnapshot(w.ID).Phase; got != LifecyclePhaseOnline {
		t.Fatalf("lifecycle phase=%s want=%s", got, LifecyclePhaseOnline)
	}

	if p.applyQMICoreStatus(w, recoveringQMICoreStatus(1, 1)) {
		t.Fatal("stale sequence was accepted")
	}
	if !w.qmiControlTasksReady() {
		t.Fatal("stale recovery snapshot cleared current readiness")
	}

	if !p.applyQMICoreStatus(w, recoveringQMICoreStatus(3, 1)) {
		t.Fatal("new recovery status was rejected")
	}
	if w.qmiControlTasksReady() {
		t.Fatal("recovering core remained ready")
	}
	if got := p.LifecycleSnapshot(w.ID).Phase; got != LifecyclePhaseRecovering {
		t.Fatalf("lifecycle phase=%s want=%s", got, LifecyclePhaseRecovering)
	}

	if !p.applyQMICoreStatus(w, readyQMICoreStatus(4, 2)) {
		t.Fatal("new generation ready status was rejected")
	}
	if p.applyQMICoreStatus(w, recoveringQMICoreStatus(5, 1)) {
		t.Fatal("older generation was accepted despite a higher sequence")
	}
	if !w.qmiControlTasksReady() {
		t.Fatal("older generation cleared replacement generation readiness")
	}
}

func TestQMICoreStatusReadyFieldDropArmsRecoveryAndTerminalLatchesFailed(t *testing.T) {
	p, w := newQMICoreStatusHarness(t)
	if !p.applyQMICoreStatus(w, readyQMICoreStatus(1, 1)) {
		t.Fatal("initial ready status was rejected")
	}

	unready := readyQMICoreStatus(2, 1)
	unready.ControlReady = false
	unready.CoreReady = false
	unready.Stage = "client_cleanup"
	unready.Reason = "client_missing"
	if !p.applyQMICoreStatus(w, unready) {
		t.Fatal("same-phase readiness drop was rejected")
	}
	if w.qmiControlTasksReady() {
		t.Fatal("same-phase readiness drop left control tasks enabled")
	}
	if got := w.HealthSnapshot().State; got != HealthStateRecovering {
		t.Fatalf("health after readiness drop=%s want=%s", got, HealthStateRecovering)
	}
	if got := p.LifecycleSnapshot(w.ID).Phase; got != LifecyclePhaseRecovering {
		t.Fatalf("lifecycle after readiness drop=%s want=%s", got, LifecyclePhaseRecovering)
	}

	terminal := unready
	terminal.Sequence = 3
	terminal.Phase = qmicore.CorePhaseTerminal
	terminal.Terminal = true
	terminal.UpdatedAt = time.Now()
	if !p.applyQMICoreStatus(w, terminal) {
		t.Fatal("terminal status was rejected")
	}
	if got := w.HealthSnapshot().State; got != HealthStateFailed {
		t.Fatalf("health after terminal=%s want=%s", got, HealthStateFailed)
	}

	w.RecordWatchdogEvent(WatchdogEvent{
		Layer:     HealthLayerQMI,
		State:     HealthStateSuspect,
		EventType: "late_suspect",
		Reason:    "late_suspect",
	})
	if got := w.HealthSnapshot().State; got != HealthStateFailed {
		t.Fatalf("late suspect downgraded terminal health to %s", got)
	}

	if !p.applyQMICoreStatus(w, readyQMICoreStatus(4, 2)) {
		t.Fatal("authoritative ready status was rejected")
	}
	if got := w.HealthSnapshot().State; got != HealthStateHealthy {
		t.Fatalf("authoritative ready did not clear terminal failure: %s", got)
	}
}

func TestConcurrentQMICoreStatusesCannotApplyOldEffectsAfterNewerStatus(t *testing.T) {
	p, w := newQMICoreStatusHarness(t)

	const total = 64
	var wg sync.WaitGroup
	for sequence := uint64(1); sequence <= total; sequence++ {
		status := recoveringQMICoreStatus(sequence, 1)
		if sequence%2 == 0 {
			status = readyQMICoreStatus(sequence, 1)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.applyQMICoreStatus(w, status)
		}()
	}
	wg.Wait()

	w.qmiCoreStatusMu.Lock()
	last := w.qmiCoreStatus
	w.qmiCoreStatusMu.Unlock()
	if last.Sequence != total || last.Phase != qmicore.CorePhaseReady {
		t.Fatalf("last status=%+v want sequence=%d ready", last, total)
	}
	if !w.qmiControlTasksReady() {
		t.Fatal("an older concurrent recovery effect won after the newest ready status")
	}
}

func TestQMICoreStatusFromReplacedWorkerCannotChangeDeviceLifecycle(t *testing.T) {
	p, oldWorker := newQMICoreStatusHarness(t)
	p.lifecycle.FinishOnline(oldWorker.ID)

	replacement := &Worker{
		ID:         oldWorker.ID,
		Pool:       p,
		generation: oldWorker.generation + 1,
		QMICore:    &qmicore.Manager{},
		stop:       make(chan struct{}),
	}
	p.mu.Lock()
	p.workers[replacement.ID] = replacement
	p.workerGenerations[replacement.ID] = replacement.generation
	p.mu.Unlock()

	if p.applyQMICoreStatus(oldWorker, recoveringQMICoreStatus(1, 1)) {
		t.Fatal("replaced worker status was accepted")
	}
	if got := p.LifecycleSnapshot(replacement.ID).Phase; got != LifecyclePhaseOnline {
		t.Fatalf("stale worker changed replacement lifecycle to %s", got)
	}
	if replacement.qmiControlReady.Load() {
		t.Fatal("stale worker status changed replacement readiness")
	}
}

func TestQMICoreLifecycleDoesNotResetDegradedPublicIPCadence(t *testing.T) {
	p, w, controller := newPublicIPStateHarness(t)
	usePublicIPCadenceForTest(t, w, 1)
	_ = installFakePublicIPClock(t, w)
	controller.setPrivate("10.0.0.2", "")
	controller.setPublic("", "")

	p.refreshIPs(w, true)
	waitPublicIPTest(t, time.Second, func() bool {
		state := &w.publicIP
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.phase == publicIPProbePhaseDegraded && state.periodicTimer != nil
	})

	state := &w.publicIP
	state.mu.Lock()
	epoch := state.epoch
	timer := state.periodicTimer
	nextProbeAt := state.nextProbeAt
	state.mu.Unlock()

	p.applyQMICoreStatus(w, readyQMICoreStatus(1, 1))
	p.applyQMICoreStatus(w, recoveringQMICoreStatus(2, 1))
	p.applyQMICoreStatus(w, readyQMICoreStatus(3, 2))

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.epoch != epoch || state.phase != publicIPProbePhaseDegraded ||
		state.periodicTimer != timer || !state.nextProbeAt.Equal(nextProbeAt) {
		t.Fatalf("core status changed public-IP cadence: epoch=%d/%d phase=%s same_timer=%t next=%s/%s",
			state.epoch, epoch, state.phase, state.periodicTimer == timer,
			state.nextProbeAt, nextProbeAt)
	}
	if got := controller.probeCalls.Load(); got != 1 {
		t.Fatalf("core status launched public-IP probes=%d want=1", got)
	}
}
