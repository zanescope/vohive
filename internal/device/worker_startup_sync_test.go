package device

import (
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/internal/config"
	qmicore "github.com/zanescope/vohive/internal/qmi"
)

func TestQMIControlTasksReadyRequiresReadyCore(t *testing.T) {
	worker := &Worker{QMICore: &qmicore.Manager{}}
	if worker.qmiControlTasksReady() {
		t.Fatal("QMI tasks reported ready before control readiness")
	}
	worker.qmiControlReady.Store(true)
	if !worker.qmiControlTasksReady() {
		t.Fatal("QMI tasks did not become ready after control readiness")
	}
	worker.markQMIControlUnavailable()
	if worker.qmiControlTasksReady() {
		t.Fatal("QMI tasks remained ready after control became unavailable")
	}
}

func TestStartupStateSyncDelayIsBounded(t *testing.T) {
	delay := time.Duration(0)
	for i := 0; i < 20; i++ {
		delay = nextStartupStateSyncDelay(delay)
	}
	if delay != startupStateSyncMaxDelay {
		t.Fatalf("startup sync delay = %s, want %s", delay, startupStateSyncMaxDelay)
	}
}

func TestNewPoolUsesConfiguredStartupStateSyncConcurrency(t *testing.T) {
	p := NewPool(&config.Config{Startup: config.StartupConfig{StateSyncConcurrency: 4}})
	defer p.cancel()
	if got := cap(p.startupSyncSem); got != 4 {
		t.Fatalf("startup sync semaphore capacity = %d, want 4", got)
	}
}

func TestHealthSyncWaitsForQMIControlReadiness(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	worker := &Worker{
		ID:      "dev-qmi-sync",
		Config:  config.DeviceConfig{ID: "dev-qmi-sync", DeviceBackend: backend.BackendQMI},
		QMICore: &qmicore.Manager{},
		Backend: &blockingHealthBackendStub{
			probeStarted: probeStarted,
			releaseProbe: releaseProbe,
		},
		Pool: p,
	}
	p.assignWorkerGeneration(worker)
	p.mu.Lock()
	p.workers[worker.ID] = worker
	p.mu.Unlock()
	sem := make(chan struct{}, 1)

	p.scheduleWorkerHealthSync(worker, sem)
	select {
	case <-probeStarted:
		t.Fatal("health sync started before QMI control readiness")
	case <-time.After(30 * time.Millisecond):
	}

	worker.qmiControlReady.Store(true)
	p.scheduleWorkerHealthSync(worker, sem)
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("health sync did not start after QMI control readiness")
	}

	worker.retired.Store(true)
	close(releaseProbe)
	deadline := time.Now().Add(time.Second)
	for worker.healthSyncInFlight.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if worker.healthSyncInFlight.Load() {
		t.Fatal("health sync guard was not released")
	}
}
