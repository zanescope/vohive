package device

import (
	"fmt"
	"strings"
	"time"

	"github.com/zanescope/vohive/pkg/logger"
)

var (
	qmiControlRecoveryRequestDelay = 30 * time.Second
	qmiControlRecoveryExhaustDelay = qmiLifecycleRecoveryTTL
)

type qmiControlWatchdogSnapshot struct {
	unreadySince time.Time
}

func (w *Worker) markQMIControlUnavailableAt(at time.Time) {
	if w == nil || w.QMICore == nil {
		return
	}
	if at.IsZero() || at.After(time.Now().Add(time.Minute)) {
		at = time.Now()
	}
	w.qmiControlReady.Store(false)
	w.qmiControlWatchdogMu.Lock()
	if w.qmiControlUnreadySince.IsZero() || at.Before(w.qmiControlUnreadySince) {
		w.qmiControlUnreadySince = at
	}
	w.qmiControlWatchdogMu.Unlock()
}

func (w *Worker) resetQMIControlWatchdog() {
	if w == nil {
		return
	}
	w.qmiControlWatchdogMu.Lock()
	w.qmiControlUnreadySince = time.Time{}
	w.qmiControlRecoveryAt = time.Time{}
	w.qmiControlExhausted = false
	w.qmiControlWatchdogMu.Unlock()
}

func (w *Worker) qmiControlWatchdogSnapshot(now time.Time) qmiControlWatchdogSnapshot {
	if w == nil {
		return qmiControlWatchdogSnapshot{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	w.qmiControlWatchdogMu.Lock()
	if w.QMICore != nil && !w.qmiControlReady.Load() && w.qmiControlUnreadySince.IsZero() {
		w.qmiControlUnreadySince = now
	}
	snapshot := qmiControlWatchdogSnapshot{
		unreadySince: w.qmiControlUnreadySince,
	}
	w.qmiControlWatchdogMu.Unlock()
	return snapshot
}

func (w *Worker) claimQMIControlRecovery(now time.Time) bool {
	if w == nil {
		return false
	}
	w.qmiControlWatchdogMu.Lock()
	defer w.qmiControlWatchdogMu.Unlock()
	if w.qmiControlExhausted || !w.qmiControlRecoveryAt.IsZero() {
		return false
	}
	w.qmiControlRecoveryAt = now
	return true
}

func (w *Worker) releaseQMIControlRecoveryClaim(now time.Time) {
	if w == nil {
		return
	}
	w.qmiControlWatchdogMu.Lock()
	if w.qmiControlRecoveryAt.Equal(now) {
		w.qmiControlRecoveryAt = time.Time{}
	}
	w.qmiControlWatchdogMu.Unlock()
}

func (w *Worker) claimQMIControlExhaustion() bool {
	if w == nil {
		return false
	}
	w.qmiControlWatchdogMu.Lock()
	defer w.qmiControlWatchdogMu.Unlock()
	if w.qmiControlExhausted {
		return false
	}
	w.qmiControlExhausted = true
	return true
}

func (p *Pool) handleQMIControlNotReady(worker *Worker, now time.Time) bool {
	if p == nil || worker == nil || worker.QMICore == nil || worker.qmiControlTasksReady() {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	health := worker.HealthSnapshot()
	if health.State == HealthStateFailed {
		return true
	}
	watch := worker.qmiControlWatchdogSnapshot(now)
	if watch.unreadySince.IsZero() || now.Before(watch.unreadySince) {
		return false
	}
	unreadyFor := now.Sub(watch.unreadySince)
	if unreadyFor >= qmiControlRecoveryExhaustDelay {
		if !worker.claimQMIControlExhaustion() {
			return false
		}
		reason := strings.TrimSpace(health.Reason)
		if reason == "" {
			reason = "qmi_control_not_ready"
		}
		err := fmt.Errorf("QMI control remained unavailable for %s: %s", unreadyFor.Round(time.Second), reason)
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:     HealthLayerQMI,
			State:     HealthStateFailed,
			EventType: "qmi_control_unready_exhausted",
			Reason:    reason,
			Err:       err,
			At:        now,
		})
		if p.lifecycle != nil {
			p.lifecycle.MarkOffline(worker.ID, "qmi_control_unready_exhausted")
		}
		p.handleTransportRecoveryExhausted(worker, worker.generation, HealthLayerQMI, "qmi_control_unready_exhausted", err)
		return false
	}
	if unreadyFor < qmiControlRecoveryRequestDelay || !worker.claimQMIControlRecovery(now) {
		return false
	}
	err := fmt.Errorf("QMI control unavailable for %s", unreadyFor.Round(time.Second))
	if !p.requestQMICoreRecovery(worker, "qmi_control_unready_watchdog", err) {
		worker.releaseQMIControlRecoveryClaim(now)
		logger.WarnRate("qmi_control_unready_recovery_rejected:"+worker.ID, time.Minute,
			"QMI control readiness watchdog could not enqueue core recovery; hard deadline remains armed",
			"device", worker.ID,
			"unready_for", unreadyFor.Round(time.Second).String())
	}
	return false
}

func (p *Pool) recoverExpiredWorkerLifecycle(worker *Worker, now time.Time) bool {
	if p == nil || worker == nil || p.lifecycle == nil {
		return false
	}
	expired, ok := p.lifecycle.TakeExpiredRecovery(worker.ID, now)
	if !ok {
		return false
	}
	reason := fmt.Sprintf("lifecycle_deadline_expired(%s:%s)", expired.Phase, expired.Reason)
	err := fmt.Errorf("device recovery lifecycle exceeded deadline: phase=%s reason=%s", expired.Phase, expired.Reason)
	layer := healthLayerForWorker(worker)
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer:     layer,
		State:     HealthStateFailed,
		EventType: "lifecycle_recovery_exhausted",
		Reason:    reason,
		Err:       err,
		At:        now,
	})
	p.handleTransportRecoveryExhausted(worker, worker.generation, layer, reason, err)
	return true
}
