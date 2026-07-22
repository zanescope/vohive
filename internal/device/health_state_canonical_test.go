package device

import (
	"context"
	"testing"

	"github.com/zanescope/vohive/internal/backend"
)

func TestNonHealthyWatchdogStateClearsCachedHealthy(t *testing.T) {
	worker := &Worker{ID: "dev1"}
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer: HealthLayerQMI,
		State: HealthStateHealthy,
	})
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer: HealthLayerQMI,
		State: HealthStateSuspect,
	})
	if worker.state.Meta.Healthy {
		t.Fatal("Meta.Healthy=true while canonical watchdog state is Suspect")
	}
}

func TestRefreshRuntimePreservesCanonicalHealthState(t *testing.T) {
	worker := &Worker{
		ID: "dev1",
		Backend: &workerStatusBackendStub{
			mode:   backend.BackendQMI,
			opMode: backend.ModeOnline,
		},
	}
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer:  HealthLayerQMI,
		State:  HealthStateSuspect,
		Reason: "probe_failed",
	})

	if err := worker.RefreshRuntime(context.Background(), "test"); err != nil {
		t.Fatalf("RefreshRuntime() error = %v", err)
	}
	if got := worker.HealthSnapshot().State; got != HealthStateSuspect {
		t.Fatalf("health state = %s, want %s", got, HealthStateSuspect)
	}
	if worker.state.Meta.Healthy {
		t.Fatal("RefreshRuntime overwrote canonical Suspect state as healthy")
	}
}
