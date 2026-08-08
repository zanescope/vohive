package device

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/internal/modem"
	"github.com/zanescope/vohive/pkg/logger"
)

func (p *Pool) suppressQMIUnhealthyEviction(worker *Worker) (bool, string) {
	if worker == nil {
		return true, "worker_nil"
	}
	if p.IsESIMSwitching(worker.ID) {
		return true, "esim_switching"
	}
	if remain := worker.healthRecoveryRemaining(time.Now()); remain > 0 {
		return true, fmt.Sprintf("recovery_window(%s)", remain.Round(time.Second))
	}
	worker.qmiRegistrationMu.Lock()
	registrationInFlight := worker.qmiRegistrationInFlight
	worker.qmiRegistrationMu.Unlock()
	if registrationInFlight {
		return true, "registration_reconcile_in_flight"
	}
	if p != nil && p.lifecycle != nil {
		if canEvict, reason := p.lifecycle.CanEvict(worker.ID, time.Now()); !canEvict {
			return true, reason
		}
	}
	if worker.Backend == nil {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	mode, err := worker.Backend.GetOperatingMode(ctx)
	if err != nil {
		return false, ""
	}
	if mode == backend.ModeRFOff || mode == backend.ModeLowPower {
		return true, fmt.Sprintf("operating_mode=%d", int(mode))
	}
	return false, ""
}

func shouldFastStartMissingQMIWorker(cfg config.DeviceConfig, live QMIDevice, discoveryAvailable bool) bool {
	if !discoveryAvailable {
		// 发现失败时，检查配置的设备文件是否真的存在，避免反复尝试打开不存在的路径。
		// 模块 AT 重启后 USB 重新枚举前设备文件会消失，此时不应快速拉起。
		if ctrl := strings.TrimSpace(cfg.ControlDevice); ctrl != "" {
			if _, err := os.Stat(ctrl); err != nil {
				return false
			}
		}
		return true
	}
	if strings.TrimSpace(live.ControlPath) == "" && strings.TrimSpace(live.NetInterface) == "" && strings.TrimSpace(live.USBPath) == "" {
		return false
	}
	return !qmiManagedAttachmentChanged(cfg, live)
}

func (p *Pool) healthCheckWorkerSnapshot() []*Worker {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	workers := make([]*Worker, 0, len(p.workers))
	for _, w := range p.workers {
		if w != nil {
			workers = append(workers, w)
		}
	}
	return workers
}

func (p *Pool) runHealthCheckTick() bool {
	workers := p.healthCheckWorkerSnapshot()
	needRescan := false
	for _, w := range workers {
		if p.runWorkerHealthCheckSafely(w) {
			needRescan = true
		}
	}
	workerCount := len(workers)

	if !needRescan {
		needRescan = p.recoverMissingConfiguredWorkers(workerCount)
	}

	return needRescan
}

func (p *Pool) healthCheckLoop() {
	for _, w := range p.healthCheckWorkerSnapshot() {
		p.refreshWorkerIPsSafely(w)
	}

	healthTicker := time.NewTicker(healthCheckInterval)
	defer healthTicker.Stop()

	syncTimer := time.NewTimer(healthSyncOffset)
	defer syncTimer.Stop()

	sem := make(chan struct{}, healthSyncConcurrency)

	for {
		select {
		case <-p.ctx.Done():
			return

		case <-healthTicker.C:
			if p.runHealthCheckTickSafely() {
				p.scheduleRescan("health_check")
			}

		case <-syncTimer.C:
			for _, worker := range p.healthCheckWorkerSnapshot() {
				p.scheduleWorkerHealthSync(worker, sem)
			}
			syncTimer.Reset(healthSyncInterval)
		}
	}
}

func (w *Worker) GetCachedIP() string {
	if nc := w.NetworkController(); nc != nil && !nc.IsConnected() {
		return ""
	}
	w.cacheMu.RLock()
	defer w.cacheMu.RUnlock()
	return w.cachedIP
}

func (w *Worker) GetCachedIPv6() string {
	if nc := w.NetworkController(); nc != nil && !nc.IsConnected() {
		return ""
	}
	w.cacheMu.RLock()
	defer w.cacheMu.RUnlock()
	return w.cachedPublicIPv6
}

func (w *Worker) GetCachedDeviceStatus() modem.DeviceStatus {
	if w == nil {
		return modem.DeviceStatus{}
	}

	w.cacheMu.RLock()
	if w.state.Runtime.Ready || w.state.Identity.Ready {
		status := w.projectDeviceStatusLocked()
		w.cacheMu.RUnlock()
		return status
	}
	w.cacheMu.RUnlock()

	if w.Backend == nil || w.Backend.Mode() == "at" {
		if w.Modem != nil {
			return w.Modem.GetFullStatus()
		}
	}
	return modem.DeviceStatus{}
}

func (w *Worker) GetCachedHealthy() bool {
	if w == nil {
		return false
	}

	w.cacheMu.RLock()
	if w.state.Runtime.Ready || w.state.Identity.Ready {
		healthy := w.state.Meta.Healthy
		w.cacheMu.RUnlock()
		return healthy
	}
	w.cacheMu.RUnlock()

	if w.Backend == nil || w.Backend.Mode() == "at" {
		if w.Modem != nil {
			return w.Modem.IsHealthy()
		}
	}
	return false
}

func (w *Worker) GetCachedIMSI() string {
	if w == nil {
		return ""
	}
	w.cacheMu.RLock()
	imsi := strings.TrimSpace(w.state.Identity.IMSI)
	w.cacheMu.RUnlock()
	return imsi
}

func (w *Worker) markHealthRecoveryWindow(duration time.Duration) {
	if w == nil || duration <= 0 {
		return
	}
	deadline := time.Now().Add(duration)
	w.RecordWatchdogEvent(WatchdogEvent{
		Layer:         HealthLayerQMI,
		State:         HealthStateRecovering,
		EventType:     "recovery_window",
		Reason:        "recovery_window",
		RecoveryUntil: deadline,
	})
}

func (w *Worker) healthRecoveryRemaining(now time.Time) time.Duration {
	if w == nil {
		return 0
	}
	w.healthMu.Lock()
	defer w.healthMu.Unlock()
	if now.IsZero() {
		now = time.Now()
	}
	if now.After(w.healthGraceUntil) {
		return 0
	}
	return w.healthGraceUntil.Sub(now)
}

func (w *Worker) resetHealthFailureStreak() {
	if w == nil {
		return
	}
	w.healthMu.Lock()
	w.healthConsecutiveFailures = 0
	w.healthMu.Unlock()
}

func (w *Worker) recordControlHealthFailure(layer HealthLayer, cause error) int {
	if w == nil {
		return 0
	}
	if layer == "" {
		layer = HealthLayerPool
	}
	w.healthMu.Lock()
	w.healthConsecutiveFailures++
	failures := w.healthConsecutiveFailures
	w.healthMu.Unlock()

	state := HealthStateSuspect
	if failures >= controlHealthFailureThreshold {
		state = HealthStateInvalid
	}
	w.RecordWatchdogEvent(WatchdogEvent{
		Layer:               layer,
		State:               state,
		EventType:           "control_health_check_failed",
		Reason:              "control_health_check_failed",
		Err:                 cause,
		ConsecutiveFailures: failures,
		Threshold:           controlHealthFailureThreshold,
	})
	return failures
}

func (w *Worker) recordHealthFailure() int {
	return w.recordControlHealthFailure(HealthLayerQMI, nil)
}

func (w *Worker) InvalidateDynamicCache() {
	if w == nil {
		return
	}
	w.cacheMu.Lock()
	w.state.Runtime.Ready = false
	w.cacheMu.Unlock()
	logger.Info(fmt.Sprintf("[%s] 动态状态缓存已失效清空", w.ID))
}

func (w *Worker) PreWarmCache() {
	if w == nil {
		return
	}
	runtimeErr := w.RefreshRuntime(nil, "prewarm")
	identityErr := w.RefreshIdentityLive(nil, "prewarm")
	if w.Pool != nil {
		w.Pool.PersistRuntimeState(w)
		w.Pool.PersistIdentityState(w)
	}
	if runtimeErr != nil || identityErr != nil {
		logger.Warn("device cold-start prewarm did not converge", "device", w.ID, "runtime_err", runtimeErr, "identity_err", identityErr)
		return
	}
	logger.Info(fmt.Sprintf("[%s] 设备冷启动预热完毕", w.ID))
}

func (w *Worker) clearCachedIP() {
	if w == nil {
		return
	}
	if w.Pool != nil {
		w.Pool.invalidatePublicIPState(w, true)
		w.Pool.abortPublicIPRotation(w)
		return
	}
	w.cacheMu.Lock()
	w.cachedIP = ""
	w.cachedPublicIPv6 = ""
	w.cacheTime = time.Time{}
	w.cacheMu.Unlock()
}

func (w *Worker) NetworkConnected() bool {
	return w != nil && w.NetworkController() != nil && w.NetworkController().IsConnected()
}

// ipHealthy treats either IP family as a valid data-plane address.
func ipHealthy(v4, v6 string) bool {
	return strings.TrimSpace(v4) != "" || strings.TrimSpace(v6) != ""
}

// representativeIP 在仍需要单个字符串代表"当前公网 IP"的调用点（如 RotateWithNotify 的
// 新旧 IP 对比/返回值）使用：双栈下优先取 v4，仅有 v6 时退化为 v6。
func representativeIP(publicV4, publicV6 string) string {
	if publicV4 != "" {
		return publicV4
	}
	return publicV6
}
