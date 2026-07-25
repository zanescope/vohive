package device

import (
	"fmt"
	"strings"
	"time"

	"github.com/zanescope/vohive/pkg/logger"
)

const qmiTransportFailureRecoveryReason = "qmi_transport_failed"

// qmiErrorIndicatesTransportDown 判断错误是否表示 QMI 控制面传输已断开
// （设备节点消失 / qmi-proxy socket 断裂 / 连接关闭等），即重连前不可能成功的状态。
func qmiErrorIndicatesTransportDown(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}
	for _, fragment := range []string{
		"broken pipe",
		"read failed: eof",
		"connection closed",
		"no such device",
		"no such file or directory",
		"write failed",
		"failed to open qmi device",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func (w *Worker) requestQMICoreRecovery(reason string) bool {
	if w == nil {
		return false
	}
	requester := w.qmiRecoveryRequester
	if requester == nil && w.QMICore != nil {
		requester = w.QMICore
	}
	if requester == nil {
		return false
	}
	return requester.RequestCoreRecovery(strings.TrimSpace(reason))
}

// requestQMICoreRecoveryForTransportFailure gives the live QMI core one bounded
// chance to discard its broken client/proxy connection and reopen it in place.
// The manager's exhausted callback remains responsible for escalating to a full
// Worker rebuild when this cheaper recovery cannot converge.
func (p *Pool) requestQMICoreRecoveryForTransportFailure(worker *Worker, reason string, err error) bool {
	if p == nil || worker == nil || err == nil || !p.isCurrentWorker(worker) {
		return false
	}
	if !qmiErrorIndicatesTransportDown(err.Error()) {
		return false
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "qmi_transport_down"
	}
	if !worker.requestQMICoreRecovery(reason) {
		return false
	}

	recoveryUntil := time.Now().Add(qmiHealthGraceAfterReset)
	worker.markHealthRecoveryWindow(qmiHealthGraceAfterReset)
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer:         HealthLayerQMI,
		State:         HealthStateRecovering,
		EventType:     reason,
		Reason:        reason,
		Err:           err,
		RecoveryUntil: recoveryUntil,
	})
	if p.lifecycle != nil {
		p.lifecycle.BeginRecovery(worker.ID, LifecyclePhaseRecovering, reason, qmiLifecycleRecoveryTTL)
	}
	logger.Info("QMI transport is down; requesting in-place core recovery first",
		"device", worker.ID,
		"reason", reason,
		"recovery_window", qmiHealthGraceAfterReset.String(),
		"err", err)
	return true
}

// handleTransportRecoveryExhausted 是唯一的传输层 exhausted 事件驱动重建入口：
// 仅当底层核心恢复彻底失败/设备节点消失时调用。
func (p *Pool) handleTransportRecoveryExhausted(worker *Worker, generation uint64, layer HealthLayer, reason string, err error) bool {
	if p == nil || worker == nil {
		return false
	}
	if current := p.GetWorker(worker.ID); current != worker {
		return false
	}
	if generation != 0 && worker.generation != 0 && generation != worker.generation {
		return false
	}
	if err == nil {
		err = fmt.Errorf("qmi recovery exhausted: %s", reason)
	}
	logger.Warn("传输核心恢复已彻底失败，调度 worker 重建",
		"device", worker.ID, "layer", layer, "reason", reason, "err", err)
	p.clearDesiredVoWiFiRecoverState(worker.ID)
	return p.scheduleWorkerRecoveryWithTransportEvent(worker.ID, qmiTransportFailureRecoveryReason, &TransportRecoveryEvent{
		DeviceID:         worker.ID,
		WorkerGeneration: worker.generation,
		Kind:             TransportRecoveryEventRecoveryExhausted,
		Source:           string(layer) + ":recovery_exhausted:" + reason,
		Err:              err,
	})
}

// maybeScheduleTransportRebuild applies the sliding-window guard before
// scheduling a worker rebuild. Over-cap devices are marked Failed instead of
// looping rebuilds.
func (p *Pool) maybeScheduleTransportRebuild(worker *Worker, layer HealthLayer, reason string, err error) bool {
	if p == nil || worker == nil || !p.acceptsWorkerCallback(worker, worker.generation) {
		return false
	}
	if p.transportRecovery != nil && !p.transportRecovery.AllowRebuild(worker.ID) {
		logger.Warn("传输恢复重建超过滑窗上限，置 Failed 等待人工/重枚举",
			"device", worker.ID, "layer", layer, "reason", reason, "err", err)
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:     layer,
			State:     HealthStateFailed,
			EventType: "transport_recovery_giveup",
			Reason:    reason,
			Err:       err,
		})
		return false
	}
	return p.handleTransportRecoveryExhausted(worker, worker.generation, layer, reason, err)
}
