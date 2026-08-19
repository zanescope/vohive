package device

import (
	"errors"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/internal/config"
	mbimcore "github.com/zanescope/vohive/internal/mbim"
	qmicore "github.com/zanescope/vohive/internal/qmi"
)

func TestHealthLayerForWorkerDistinguishesMBIM(t *testing.T) {
	tests := []struct {
		name   string
		worker *Worker
		want   HealthLayer
	}{
		{
			name:   "backend_mbim",
			worker: &Worker{Backend: &workerStatusBackendStub{mode: backend.BackendMBIM}},
			want:   HealthLayerMBIM,
		},
		{
			name:   "config_mbim",
			worker: &Worker{Config: config.DeviceConfig{DeviceBackend: backend.BackendMBIM}},
			want:   HealthLayerMBIM,
		},
		{
			name:   "qmi",
			worker: &Worker{Backend: &workerStatusBackendStub{mode: backend.BackendQMI}},
			want:   HealthLayerQMI,
		},
		{
			name:   "at_default",
			worker: &Worker{},
			want:   HealthLayerAT,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := healthLayerForWorker(tt.worker); got != tt.want {
				t.Fatalf("healthLayerForWorker() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPoolHealthCheckDelegatesMBIMProbeToCore(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	worker := &Worker{
		ID:       "mbim-dev",
		Config:   config.DeviceConfig{ID: "mbim-dev", DeviceBackend: backend.BackendMBIM},
		Backend:  &workerStatusBackendStub{mode: backend.BackendMBIM, opModeErr: errors.New("duplicate probe")},
		MBIMCore: &mbimcore.Manager{},
	}
	p.mu.Lock()
	p.workers[worker.ID] = worker
	p.mu.Unlock()

	if needRescan := p.runWorkerHealthCheck(worker); needRescan {
		t.Fatal("MBIM pool health check requested a generic rescan")
	}
	snapshot := worker.HealthSnapshot()
	if snapshot.Layer != HealthLayerPool || snapshot.EventType != "" {
		t.Fatalf("pool-level probe overwrote MBIM core health state: %+v", snapshot)
	}
}

func TestPoolHealthCheckRescansTerminalQMIWorkerWithUnreadyControl(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	worker := &Worker{
		ID:      "qmi-terminal",
		Config:  config.DeviceConfig{ID: "qmi-terminal", DeviceBackend: backend.BackendQMI},
		QMICore: &qmicore.Manager{},
	}
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer:     HealthLayerQMI,
		State:     HealthStateFailed,
		EventType: "transport_recovery_giveup",
	})
	p.mu.Lock()
	p.workers[worker.ID] = worker
	p.mu.Unlock()

	if needRescan := p.runWorkerHealthCheck(worker); !needRescan {
		t.Fatal("terminal QMI worker with unready control did not request a rescan")
	}
}

func TestRunHealthTaskSafelyRecoversPanic(t *testing.T) {
	result, panicValue := runHealthTaskSafely(func() bool {
		panic("health boom")
	})
	if result {
		t.Fatal("result = true after panic")
	}
	if panicValue != "health boom" {
		t.Fatalf("panicValue = %v, want health boom", panicValue)
	}
}

func TestWorkerCallbackRejectsStaleGeneration(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	stale := &Worker{ID: "dev1", generation: 1}
	current := &Worker{ID: "dev1", generation: 2}
	p.mu.Lock()
	p.workers[current.ID] = current
	p.workerGenerations[current.ID] = current.generation
	p.mu.Unlock()

	if p.acceptsWorkerCallback(stale, stale.generation) {
		t.Fatal("stale generation callback was accepted")
	}
	if !p.acceptsWorkerCallback(current, current.generation) {
		t.Fatal("current generation callback was rejected")
	}
	current.retired.Store(true)
	if p.acceptsWorkerCallback(current, current.generation) {
		t.Fatal("retired worker callback was accepted")
	}
}

func TestRefreshIPsNilWorkerIsNoop(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	p.refreshIPs(nil, false)
}

func TestHealthSyncKeepsOneInFlightTaskPerWorker(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	worker := &Worker{
		ID:     "dev-sync",
		Config: config.DeviceConfig{ID: "dev-sync", DeviceBackend: backend.BackendQMI},
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
	sem := make(chan struct{}, 2)

	p.scheduleWorkerHealthSync(worker, sem)
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("health sync did not enter backend probe")
	}
	p.scheduleWorkerHealthSync(worker, sem)
	if got := len(sem); got != 1 {
		t.Fatalf("in-flight health sync tasks = %d, want 1", got)
	}

	worker.retired.Store(true)
	close(releaseProbe)
	deadline := time.Now().Add(time.Second)
	for worker.healthSyncInFlight.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if worker.healthSyncInFlight.Load() {
		t.Fatal("health sync single-flight flag was not released")
	}
	if got := len(sem); got != 0 {
		t.Fatalf("health sync semaphore slots still held = %d", got)
	}
}
