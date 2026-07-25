package device

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zanescope/vohive/pkg/logger"
)

const (
	startupStateSyncConcurrency = 2
	startupStateSyncTimeout     = 20 * time.Second
	startupStateSyncMaxDelay    = 60 * time.Second
)

func (w *Worker) qmiControlTasksReady() bool {
	if w == nil {
		return false
	}
	return w.QMICore == nil || w.qmiControlReady.Load()
}

func (w *Worker) markQMIControlUnavailable() {
	if w != nil && w.QMICore != nil {
		w.qmiControlReady.Store(false)
	}
}

func nextStartupStateSyncDelay(previous time.Duration) time.Duration {
	if previous <= 0 {
		return 2 * time.Second
	}
	next := previous * 2
	if next > startupStateSyncMaxDelay {
		return startupStateSyncMaxDelay
	}
	return next
}

func (p *Pool) startWorkerStartupSyncLoop(worker *Worker) {
	if p == nil || worker == nil || worker.QMICore == nil {
		return
	}
	go p.runWorkerStartupSyncLoop(worker)
}

func (p *Pool) runWorkerStartupSyncLoop(worker *Worker) {
	delay := time.Duration(0)
	attempt := 0
	for {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-p.ctx.Done():
				timer.Stop()
				return
			case <-worker.stop:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if !p.isCurrentWorker(worker) {
			return
		}
		if !worker.qmiControlTasksReady() {
			delay = nextStartupStateSyncDelay(delay)
			continue
		}

		select {
		case p.startupSyncSem <- struct{}{}:
		case <-p.ctx.Done():
			return
		case <-worker.stop:
			return
		}
		attempt++
		err := func() (err error) {
			defer func() {
				<-p.startupSyncSem
				if recovered := recover(); recovered != nil {
					err = fmt.Errorf("startup state sync panic: %v", recovered)
				}
			}()
			return p.syncWorkerStartupState(worker)
		}()
		if err == nil {
			logger.Info("device startup state synchronization completed", "device", worker.ID, "attempt", attempt)
			return
		}

		p.requestQMICoreRecoveryForTransportFailure(worker, "qmi_startup_sync_transport_down", err)
		logger.WarnRate("startup_state_sync:"+worker.ID, 30*time.Second,
			"device startup state has not converged; retrying",
			"device", worker.ID,
			"attempt", attempt,
			"next_retry_in", nextStartupStateSyncDelay(delay).String(),
			"err", err)
		delay = nextStartupStateSyncDelay(delay)
	}
}

func (p *Pool) syncWorkerStartupState(worker *Worker) error {
	if !p.isCurrentWorker(worker) {
		return fmt.Errorf("worker_not_current")
	}
	ctx, cancel := context.WithTimeout(p.ctx, startupStateSyncTimeout)
	defer cancel()

	runtimeErr := worker.RefreshRuntime(ctx, "startup_sync")
	identity, identityErr := worker.refreshIdentityLive(ctx, "startup_sync")
	if !p.isCurrentWorker(worker) {
		return fmt.Errorf("worker_not_current")
	}
	p.PersistRuntimeState(worker)
	p.PersistIdentityState(worker)

	if identityErr == nil && (identity.ICCID != "" || identity.IMSI != "") {
		if worker.CurrentICCID() != "" {
			p.resolveAndApplyPolicy(worker, "startup_sync")
		}
	}
	p.broadcastVoWiFiStateChange(worker.ID)

	if ctx.Err() != nil {
		return errors.Join(runtimeErr, identityErr, ctx.Err())
	}
	if runtimeErr != nil || identityErr != nil {
		return errors.Join(runtimeErr, identityErr)
	}
	if identity.ICCID == "" && identity.IMSI == "" {
		return fmt.Errorf("startup_identity_not_ready")
	}
	return nil
}
