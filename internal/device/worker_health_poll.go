package device

import (
	"strings"
	"time"

	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/pkg/logger"
)

func healthLayerForWorker(worker *Worker) HealthLayer {
	if worker == nil {
		return HealthLayerPool
	}
	mode := ""
	if worker.Backend != nil {
		mode = strings.ToLower(strings.TrimSpace(worker.Backend.Mode()))
	}
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(worker.Config.DeviceBackend))
	}
	switch mode {
	case backend.BackendQMI:
		return HealthLayerQMI
	case backend.BackendMBIM:
		return HealthLayerMBIM
	default:
		return HealthLayerAT
	}
}

func (p *Pool) runWorkerHealthCheckSafely(worker *Worker) (needRescan bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("设备健康检查 panic，已隔离到单个 Worker",
				"device", func() string {
					if worker == nil {
						return ""
					}
					return worker.ID
				}(),
				"err", recovered)
			needRescan = false
		}
	}()
	return p.runWorkerHealthCheck(worker)
}

func (p *Pool) runWorkerHealthCheck(worker *Worker) bool {
	if !p.isCurrentWorker(worker) {
		return false
	}
	layer := healthLayerForWorker(worker)
	if layer == HealthLayerQMI && worker.QMICore != nil && !worker.qmiControlTasksReady() {
		logger.Debug("QMI control is not ready; skipping pool health probe", "device", worker.ID)
		return false
	}
	p.refreshIPs(worker, false)
	worker.cleanupFragmentCache(30 * time.Minute)

	if layer == HealthLayerMBIM && worker.MBIMCore != nil {
		// MBIM Core 自带 30 秒探活、单飞 reopen 和 exhausted 回调；池级重复探活会与它竞争控制端点。
		return false
	}

	healthy, healthErr := worker.ProbeDeviceHealth()
	if !p.isCurrentWorker(worker) {
		return false
	}
	if healthy {
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:     layer,
			State:     HealthStateHealthy,
			EventType: "control_health_check_ok",
			Reason:    "control_health_check_ok",
		})
		worker.resetHealthFailureStreak()
		return false
	}

	isQMI := layer == HealthLayerQMI
	if isQMI {
		if suppressed, reason := p.suppressQMIUnhealthyEviction(worker); suppressed {
			logger.Debug("QMI 节点当前处于恢复窗口，跳过本轮剥离判定", "device", worker.ID, "reason", reason)
			return false
		}
	}

	failures := 1
	if isQMI {
		failures = worker.recordHealthFailure()
	}
	// 传输确认已断开（broken pipe/EOF/connection closed 等）时，重连前不可能探活成功，
	// 没有必要再等满 3 次观察窗口——跳过等待，第一次失败就直接触发恢复。
	// Recovery is staged: reconnect QMI Core in place first; rebuild the Worker
	// only when the Core refuses recovery or later exhausts its recovery budget.
	transportDown := isQMI && healthErr != nil && qmiErrorIndicatesTransportDown(healthErr.Error())
	if transportDown && p.requestQMICoreRecoveryForTransportFailure(worker, "qmi_transport_down", healthErr) {
		return false
	}
	if isQMI && strings.TrimSpace(worker.Config.ControlDevice) != "" {
		if failures < qmiHealthFailureThreshold && !transportDown {
			logger.Warn("QMI 节点探活失败，进入连续失败观察窗口",
				"device", worker.ID,
				"failures", failures,
				"threshold", qmiHealthFailureThreshold)
			return false
		}
		reason := "qmi_health_threshold"
		if transportDown && failures < qmiHealthFailureThreshold {
			reason = "qmi_transport_down"
		}
		worker.RecordWatchdogEvent(WatchdogEvent{
			Layer:               HealthLayerQMI,
			State:               HealthStateInvalid,
			EventType:           reason,
			Reason:              reason,
			ConsecutiveFailures: failures,
			Threshold:           qmiHealthFailureThreshold,
		})
		if p.lifecycle != nil {
			p.lifecycle.BeginRecovery(worker.ID, LifecyclePhaseRecovering, reason, qmiLifecycleRecoveryTTL)
		}
		logger.Info("检测到免扫节点(QMI)探活超限，进入统一模组恢复流程", "device", worker.ID, "reason", reason)
		p.scheduleWorkerRecoveryWithTransportEvent(worker.ID, reason, &TransportRecoveryEvent{
			DeviceID:         worker.ID,
			WorkerGeneration: worker.generation,
			Kind:             TransportRecoveryEventHealthSuspect,
			Source:           reason,
			Err:              healthErr,
		})
		return false
	}

	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer:     layer,
		State:     HealthStateSuspect,
		EventType: "control_health_check_failed",
		Reason:    "control_health_check_failed",
		Err:       healthErr,
	})
	logger.Info("定时检查发现设备不健康，将触发重连扫描",
		"device", worker.ID,
		"backend", func() string {
			if worker.Backend != nil {
				return worker.Backend.Mode()
			}
			return "none"
		}())
	return true
}
