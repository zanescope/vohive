package device

import (
	"context"
	"strings"
	"time"

	qmicore "github.com/zanescope/vohive/internal/qmi"
)

// bindQMICoreStatus consumes the durable latest-value lifecycle stream. QMI
// Event callbacks remain useful for data-session and diagnostic observations,
// but they must not decide whether the control plane is ready.
func (p *Pool) bindQMICoreStatus(worker *Worker) {
	if p == nil || worker == nil || worker.QMICore == nil {
		return
	}

	parent := p.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	statuses := worker.QMICore.SubscribeCoreStatus(ctx)
	generation := worker.generation

	go func() {
		defer cancel()
		for {
			select {
			case <-worker.stop:
				return
			case status, ok := <-statuses:
				if !ok {
					return
				}
				if !p.acceptsWorkerCallback(worker, generation) {
					return
				}
				p.applyQMICoreStatus(worker, status)
			}
		}
	}()
}

func qmiCoreStatusReady(status qmicore.CoreStatus) bool {
	return status.Phase == qmicore.CorePhaseReady &&
		status.ControlReady && status.CoreReady && !status.Terminal
}

func qmiCoreStatusReason(status qmicore.CoreStatus) string {
	for _, value := range []string{status.Reason, status.Stage, status.LastError, string(status.Phase)} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "qmi_core_status"
}

func (p *Pool) applyQMICoreStatus(worker *Worker, status qmicore.CoreStatus) bool {
	if p == nil || worker == nil || worker.retired.Load() {
		return false
	}

	// Keep worker admission valid through every device-scoped side effect. A
	// replacement needs p.mu exclusively, so an old callback cannot pass the
	// check and then update the replacement worker's lifecycle by device ID.
	p.mu.RLock()
	current := p.workers[worker.ID]
	currentGeneration := p.workerGenerations[worker.ID]
	if (current != nil && current != worker) ||
		(worker.generation != 0 && currentGeneration != 0 && worker.generation != currentGeneration) {
		p.mu.RUnlock()
		return false
	}
	defer p.mu.RUnlock()

	// Serialize admission and all readiness/lifecycle side effects. Otherwise
	// an older handler could pass admission, stall, and overwrite the effects
	// of a newer snapshot that completed first.
	worker.qmiCoreStatusMu.Lock()
	defer worker.qmiCoreStatusMu.Unlock()
	previous := worker.qmiCoreStatus
	hadPrevious := worker.qmiCoreStatusSeen
	if hadPrevious && (status.Generation < previous.Generation || status.Sequence <= previous.Sequence) {
		return false
	}
	worker.qmiCoreStatus = status
	worker.qmiCoreStatusSeen = true

	ready := qmiCoreStatusReady(status)
	wasReady := hadPrevious && qmiCoreStatusReady(previous)
	reason := qmiCoreStatusReason(status)
	phaseChanged := !hadPrevious || previous.Generation != status.Generation || previous.Phase != status.Phase
	readinessChanged := phaseChanged ||
		previous.ControlReady != status.ControlReady ||
		previous.CoreReady != status.CoreReady ||
		previous.Terminal != status.Terminal
	statusAt := status.UpdatedAt
	if statusAt.IsZero() {
		statusAt = time.Now()
	}

	if ready {
		if !wasReady || !worker.qmiControlReady.Load() {
			p.markQMIControlRecovered(worker, "qmi_core_status:"+reason)
		}
		return true
	}

	worker.markQMIControlUnavailableAt(statusAt)
	if !readinessChanged {
		return true
	}

	if status.Terminal {
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:     HealthLayerQMI,
			State:     HealthStateFailed,
			EventType: "qmi_core_terminal",
			Reason:    reason,
			At:        statusAt,
		})
		if p.lifecycle != nil {
			p.lifecycle.MarkOffline(worker.ID, "qmi_core_terminal:"+reason)
		}
		return true
	}

	switch status.Phase {
	case qmicore.CorePhaseStarting:
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:     HealthLayerQMI,
			State:     HealthStateRecovering,
			EventType: "qmi_core_starting",
			Reason:    reason,
			At:        statusAt,
		})
		if p.lifecycle != nil {
			p.lifecycle.BeginRecovery(worker.ID, LifecyclePhaseQMIStarting, reason, qmiLifecycleRecoveryTTL)
		}
	case qmicore.CorePhaseRecovering, qmicore.CorePhaseDegraded:
		recoveryUntil := time.Now().Add(qmiHealthGraceAfterReset)
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:         HealthLayerQMI,
			State:         HealthStateRecovering,
			EventType:     "qmi_core_" + string(status.Phase),
			Reason:        reason,
			RecoveryUntil: recoveryUntil,
			At:            statusAt,
		})
		worker.markHealthRecoveryWindow(qmiHealthGraceAfterReset)
		if p.lifecycle != nil {
			p.lifecycle.BeginRecovery(worker.ID, LifecyclePhaseRecovering, reason, qmiLifecycleRecoveryTTL)
		}
	case qmicore.CorePhaseTerminal, qmicore.CorePhaseStopping, qmicore.CorePhaseStopped:
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:     HealthLayerQMI,
			State:     HealthStateFailed,
			EventType: "qmi_core_" + string(status.Phase),
			Reason:    reason,
			At:        statusAt,
		})
		if p.lifecycle != nil {
			p.lifecycle.MarkOffline(worker.ID, "qmi_core_"+string(status.Phase)+":"+reason)
		}
	default:
		recoveryUntil := time.Now().Add(qmiHealthGraceAfterReset)
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:         HealthLayerQMI,
			State:         HealthStateRecovering,
			EventType:     "qmi_core_control_unready",
			Reason:        reason,
			RecoveryUntil: recoveryUntil,
			At:            statusAt,
		})
		worker.markHealthRecoveryWindow(qmiHealthGraceAfterReset)
		if p.lifecycle != nil {
			p.lifecycle.BeginRecovery(worker.ID, LifecyclePhaseRecovering, reason, qmiLifecycleRecoveryTTL)
		}
	}
	return true
}
