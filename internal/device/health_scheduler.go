package device

import (
	"sync"
	"time"

	"github.com/zanescope/vohive/pkg/logger"
)

const (
	healthCheckInterval = time.Minute
	healthSyncOffset    = 30 * time.Second
	healthSyncInterval  = time.Minute
	healthSyncTimeout   = 20 * time.Second
)

func runHealthTaskSafely(task func() bool) (result bool, panicValue any) {
	defer func() {
		panicValue = recover()
	}()
	if task != nil {
		result = task()
	}
	return result, nil
}

func (p *Pool) runHealthCheckTickSafely() bool {
	needRescan, panicValue := runHealthTaskSafely(p.runHealthCheckTick)
	if panicValue != nil {
		logger.Error("health check tick panic recovered; scheduler will continue", "err", panicValue)
		return false
	}
	return needRescan
}

func (p *Pool) refreshWorkerIPsSafely(worker *Worker) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			logger.Error("initial IP refresh panic recovered",
				"device", func() string {
					if worker == nil {
						return ""
					}
					return worker.ID
				}(),
				"err", panicValue)
		}
	}()
	if p.isCurrentWorker(worker) {
		p.refreshIPs(worker, false)
	}
}

func (p *Pool) scheduleWorkerHealthSync(worker *Worker, sem chan struct{}) {
	if !p.isCurrentWorker(worker) || !worker.healthSyncInFlight.CompareAndSwap(false, true) {
		return
	}
	select {
	case sem <- struct{}{}:
	case <-p.ctx.Done():
		worker.healthSyncInFlight.Store(false)
		return
	default:
		worker.healthSyncInFlight.Store(false)
		logger.Debug("设备状态同步并发已满，跳过本轮", "device", worker.ID)
		return
	}

	go func() {
		var releaseOnce sync.Once
		releaseSlot := func() {
			releaseOnce.Do(func() { <-sem })
		}
		defer func() {
			releaseSlot()
			worker.healthSyncInFlight.Store(false)
			if panicValue := recover(); panicValue != nil {
				logger.Error("设备状态同步 panic recovered", "device", worker.ID, "err", panicValue)
			}
		}()

		timeout := time.AfterFunc(healthSyncTimeout, func() {
			releaseSlot()
			if p.isCurrentWorker(worker) {
				logger.WarnRate("health_sync_timeout:"+worker.ID, 5*time.Minute,
					"设备状态同步超时；释放全局槽位并保留设备单飞标记", "device", worker.ID)
			}
		})
		defer timeout.Stop()

		if !p.isCurrentWorker(worker) {
			return
		}
		isATMode := worker.Backend == nil || worker.Backend.Mode() == "at"
		if isATMode && worker.Modem != nil {
			worker.Modem.RefreshStatus(
				func(msg string) {
					if p.isCurrentWorker(worker) {
						if notifier := p.getNotifier(); notifier != nil {
							notifier.NotifyRaw(msg)
						}
					}
				},
				func(msg string) {
					if p.isCurrentWorker(worker) {
						if notifier := p.getNotifier(); notifier != nil {
							notifier.NotifyRaw(msg)
						}
					}
				},
			)
		}
		if !p.isCurrentWorker(worker) {
			return
		}
		_ = worker.RefreshRuntime(nil, "health_sync")
		if p.isCurrentWorker(worker) {
			p.PersistRuntimeState(worker)
		}
	}()
}
