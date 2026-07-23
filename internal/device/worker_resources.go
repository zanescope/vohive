package device

import (
	"time"

	"github.com/zanescope/vohive/pkg/logger"
)

func (w *Worker) tryBeginHealthSync() bool {
	return w != nil && w.healthSyncInFlight.CompareAndSwap(false, true)
}

func (w *Worker) endHealthSync() {
	if w != nil {
		w.healthSyncInFlight.Store(false)
	}
}

func (p *Pool) startUdevWatcher() {
	if p == nil {
		return
	}
	select {
	case <-p.ctx.Done():
		return
	default:
	}

	p.udevWatcherMu.Lock()
	if p.udevWatcher != nil {
		p.udevWatcherMu.Unlock()
		return
	}
	watcher := NewUdevWatcher(p)
	p.udevWatcher = watcher
	p.udevWatcherMu.Unlock()
	watcher.Start()
}

func (p *Pool) stopUdevWatcher() {
	if p == nil {
		return
	}
	p.udevWatcherMu.Lock()
	watcher := p.udevWatcher
	p.udevWatcherMu.Unlock()
	if watcher != nil {
		watcher.Stop()
	}
}

func (p *Pool) cancelWorkerDeferredActions(deviceID string) {
	if p == nil || deviceID == "" {
		return
	}

	p.simEventMu.Lock()
	simTimer := p.simEventTimers[deviceID]
	delete(p.simEventTimers, deviceID)
	p.simEventMu.Unlock()
	if simTimer != nil {
		simTimer.Stop()
	}

	p.deviceEventWakeMu.Lock()
	wakeup := p.deviceEventWakeups[deviceID]
	delete(p.deviceEventWakeups, deviceID)
	p.deviceEventWakeMu.Unlock()
	if wakeup != nil && wakeup.timer != nil {
		wakeup.timer.Stop()
	}
}

func (p *Pool) stopWorkerResources(worker *Worker) {
	if worker == nil {
		return
	}
	worker.resourceStopOnce.Do(func() {
		worker.uimIndicationsReady.Store(false)
		worker.stopOnce.Do(func() {
			if worker.stop != nil {
				close(worker.stop)
			}
		})
		if p != nil {
			p.cancelWorkerDeferredActions(worker.ID)
			p.stopPublicIPState(worker)
		}

		worker.operatorScanMu.Lock()
		operatorScanCancel := worker.operatorScanCancel
		worker.operatorScanCancel = nil
		worker.operatorScanActive = false
		worker.operatorScanMu.Unlock()
		if operatorScanCancel != nil {
			operatorScanCancel()
		}

		if worker.Proxy != nil {
			worker.Proxy.Shutdown()
		}
		if worker.ESIMQMITransport != nil {
			_ = worker.ESIMQMITransport.Stop()
		}
		if worker.QMICore != nil {
			worker.QMICore.Stop()
		}
		if worker.MBIMCore != nil {
			_ = worker.MBIMCore.Close()
		}
		if worker.Backend != nil {
			_ = worker.Backend.Close()
		}
		if worker.Modem != nil && !worker.Modem.StopAndWait(2*time.Second) {
			logger.Warn("等待 AT 管理器退出超时", "device", worker.ID)
		}
	})
}
