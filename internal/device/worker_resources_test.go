package device

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
)

func TestWorkerHealthSyncSingleFlight(t *testing.T) {
	worker := &Worker{}
	if !worker.tryBeginHealthSync() {
		t.Fatal("first health sync did not acquire the single-flight guard")
	}
	if worker.tryBeginHealthSync() {
		t.Fatal("second health sync acquired the guard while the first was active")
	}
	worker.endHealthSync()
	if !worker.tryBeginHealthSync() {
		t.Fatal("health sync guard was not released")
	}
	worker.endHealthSync()
}

func TestStopWorkerResourcesIsIdempotent(t *testing.T) {
	pool := NewPool(&config.Config{})
	t.Cleanup(pool.cancel)

	var operatorCancels atomic.Int32
	_, cancel := context.WithCancel(context.Background())
	worker := &Worker{
		ID:                 "dev1",
		stop:               make(chan struct{}),
		operatorScanActive: true,
		operatorScanCancel: func() {
			operatorCancels.Add(1)
			cancel()
		},
	}
	worker.publicIP.retryTimer = time.AfterFunc(time.Hour, func() {})
	worker.publicIP.periodicTimer = time.AfterFunc(time.Hour, func() {})
	pool.simEventTimers[worker.ID] = time.AfterFunc(time.Hour, func() {})
	pool.deviceEventWakeups[worker.ID] = &deviceEventRecoverWakeup{
		timer: time.AfterFunc(time.Hour, func() {}),
	}

	pool.stopWorkerResources(worker)
	pool.stopWorkerResources(worker)

	select {
	case <-worker.stop:
	default:
		t.Fatal("worker stop channel is still open")
	}
	if got := operatorCancels.Load(); got != 1 {
		t.Fatalf("operator scan cancellation count = %d, want 1", got)
	}
	if worker.operatorScanActive || worker.operatorScanCancel != nil {
		t.Fatal("operator scan state was not cleared")
	}
	worker.publicIP.mu.Lock()
	retryTimer := worker.publicIP.retryTimer
	periodicTimer := worker.publicIP.periodicTimer
	worker.publicIP.mu.Unlock()
	if retryTimer != nil || periodicTimer != nil {
		t.Fatal("public IP timers were not cleared")
	}
	if _, ok := pool.simEventTimers[worker.ID]; ok {
		t.Fatal("SIM event timer was not removed")
	}
	if _, ok := pool.deviceEventWakeups[worker.ID]; ok {
		t.Fatal("device event wakeup was not removed")
	}
}

func TestUdevWatcherStopCancelsPendingRescan(t *testing.T) {
	watcher := NewUdevWatcher(nil)
	watcher.debounce = time.Hour
	watcher.scheduleRescan()
	watcher.Stop()

	watcher.pendingMu.Lock()
	timer := watcher.timer
	pending := watcher.pending
	watcher.pendingMu.Unlock()
	if timer != nil || pending {
		t.Fatalf("stopped watcher retained pending rescan: timer=%v pending=%v", timer != nil, pending)
	}

	watcher.scheduleRescan()
	watcher.pendingMu.Lock()
	defer watcher.pendingMu.Unlock()
	if watcher.timer != nil || watcher.pending {
		t.Fatal("stopped watcher scheduled another rescan")
	}
}

func TestPoolShutdownStopsWatcherAndWorkers(t *testing.T) {
	pool := NewPool(&config.Config{})
	watcher := NewUdevWatcher(pool)
	pool.udevWatcher = watcher
	worker := &Worker{ID: "dev1", stop: make(chan struct{})}
	pool.workers[worker.ID] = worker

	if err := pool.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case <-watcher.stop:
	default:
		t.Fatal("udev watcher stop channel is still open")
	}
	select {
	case <-worker.stop:
	default:
		t.Fatal("worker stop channel is still open after shutdown")
	}
	if err := pool.Shutdown(); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
}
