package device

import (
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/backend"
)

func reserveRecoveryForWatchdogTest(t *testing.T, p *Pool, w *Worker) TransportRecoveryToken {
	t.Helper()
	token, status := p.transportRecovery.Begin(TransportRecoveryEvent{
		DeviceID:         w.ID,
		WorkerGeneration: w.generation,
		Kind:             TransportRecoveryEventRecoveryExhausted,
		Source:           "test_reservation",
	}, true)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("reserve recovery status=%s", status)
	}
	t.Cleanup(func() { p.transportRecovery.Finish(token) })
	return token
}

func TestQMIControlWatchdogRequestsCoreRecoveryOnce(t *testing.T) {
	p, w := newQMICoreStatusHarness(t)
	requester := &qmiCoreRecoveryRequesterStub{accepted: true}
	w.qmiRecoveryRequester = requester
	now := time.Now()
	w.markQMIControlUnavailableAt(now.Add(-qmiControlRecoveryRequestDelay - time.Second))

	if p.handleQMIControlNotReady(w, now) {
		t.Fatal("non-terminal watchdog request unexpectedly requested a live rescan")
	}
	if len(requester.calls) != 1 || requester.calls[0] != "qmi_control_unready_watchdog" {
		t.Fatalf("core recovery calls=%v", requester.calls)
	}
	if got := w.HealthSnapshot().State; got != HealthStateRecovering {
		t.Fatalf("health=%s want=%s", got, HealthStateRecovering)
	}

	p.handleQMIControlNotReady(w, now.Add(time.Second))
	if len(requester.calls) != 1 {
		t.Fatalf("watchdog enqueued duplicate recovery calls=%v", requester.calls)
	}
}

func TestQMIControlWatchdogRetriesRejectedCoreRecovery(t *testing.T) {
	p, w := newQMICoreStatusHarness(t)
	requester := &qmiCoreRecoveryRequesterStub{accepted: false}
	w.qmiRecoveryRequester = requester
	now := time.Now()
	w.markQMIControlUnavailableAt(now.Add(-qmiControlRecoveryRequestDelay - time.Second))

	p.handleQMIControlNotReady(w, now)
	p.handleQMIControlNotReady(w, now.Add(time.Minute))
	if len(requester.calls) != 2 {
		t.Fatalf("rejected core recovery was not retried: calls=%v", requester.calls)
	}
}

func TestQMIControlWatchdogExhaustionLatchesFailed(t *testing.T) {
	p, w := newQMICoreStatusHarness(t)
	w.Config.DeviceBackend = backend.BackendQMI
	w.Config.ControlDevice = "/dev/cdc-wdm-test"
	reserveRecoveryForWatchdogTest(t, p, w)
	now := time.Now()
	w.markQMIControlUnavailableAt(now.Add(-qmiControlRecoveryExhaustDelay - time.Second))

	if p.handleQMIControlNotReady(w, now) {
		t.Fatal("first exhaustion pass should use the transport rebuild entrypoint directly")
	}
	if got := w.HealthSnapshot().State; got != HealthStateFailed {
		t.Fatalf("health=%s want=%s", got, HealthStateFailed)
	}
	if terminal, reason := qmiTerminalWorkerNeedsLiveReprobe(w); !terminal || reason != "qmi_control_unready_exhausted" {
		t.Fatalf("terminal=%v reason=%q", terminal, reason)
	}
	if !p.handleQMIControlNotReady(w, now.Add(time.Second)) {
		t.Fatal("latched failed worker did not request a later live rescan")
	}
}

func TestExpiredLifecycleActivelyFailsWorkerOnce(t *testing.T) {
	p, w := newQMICoreStatusHarness(t)
	reserveRecoveryForWatchdogTest(t, p, w)
	now := time.Now()
	p.lifecycle.BeginRecoveryAt(w.ID, LifecyclePhaseRecovering, "stuck_recovery", now.Add(-time.Minute), time.Second)

	if !p.recoverExpiredWorkerLifecycle(w, now) {
		t.Fatal("expired lifecycle was not consumed by active watchdog")
	}
	if got := w.HealthSnapshot().State; got != HealthStateFailed {
		t.Fatalf("health=%s want=%s", got, HealthStateFailed)
	}
	if p.recoverExpiredWorkerLifecycle(w, now.Add(time.Second)) {
		t.Fatal("expired lifecycle was emitted more than once")
	}
}
