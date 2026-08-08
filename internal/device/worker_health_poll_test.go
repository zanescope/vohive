package device

import (
	"errors"
	"testing"

	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/internal/config"
)

func TestNonQMIHealthFailureEventuallyBecomesInvalid(t *testing.T) {
	probeErr := errors.New("AT control port unavailable")
	backendStub := &workerStatusBackendStub{
		mode:      "custom-at-control",
		opModeErr: probeErr,
	}
	p := NewPool(&config.Config{})
	defer p.cancel()
	worker := &Worker{
		ID:      "at-dev",
		Config:  config.DeviceConfig{ID: "at-dev", DeviceBackend: backend.BackendAT},
		Backend: backendStub,
		Pool:    p,
	}
	p.workers[worker.ID] = worker

	for failure := 1; failure <= controlHealthFailureThreshold; failure++ {
		if needRescan := p.runWorkerHealthCheck(worker); !needRescan {
			t.Fatalf("failure %d did not request rescan", failure)
		}
		snapshot := worker.HealthSnapshot()
		wantState := HealthStateSuspect
		if failure == controlHealthFailureThreshold {
			wantState = HealthStateInvalid
		}
		if snapshot.Layer != HealthLayerAT || snapshot.State != wantState {
			t.Fatalf("failure %d snapshot = %+v, want layer=%s state=%s",
				failure, snapshot, HealthLayerAT, wantState)
		}
		if snapshot.ConsecutiveFailures != failure ||
			snapshot.Threshold != controlHealthFailureThreshold {
			t.Fatalf("failure %d counters = %d/%d, want %d/%d",
				failure,
				snapshot.ConsecutiveFailures,
				snapshot.Threshold,
				failure,
				controlHealthFailureThreshold)
		}
	}
	if !rescanHealthAllowsEviction(worker.HealthSnapshot().State) {
		t.Fatal("non-QMI worker never became eligible for offline eviction")
	}
}

func TestNonQMIHealthSuccessResetsFailureStreak(t *testing.T) {
	backendStub := &workerStatusBackendStub{
		mode:      "custom-at-control",
		opModeErr: errors.New("temporary failure"),
	}
	p := NewPool(&config.Config{})
	defer p.cancel()
	worker := &Worker{
		ID:      "at-dev",
		Config:  config.DeviceConfig{ID: "at-dev", DeviceBackend: backend.BackendAT},
		Backend: backendStub,
		Pool:    p,
	}
	p.workers[worker.ID] = worker

	p.runWorkerHealthCheck(worker)
	backendStub.opModeErr = nil
	p.runWorkerHealthCheck(worker)
	backendStub.opModeErr = errors.New("failed again")
	p.runWorkerHealthCheck(worker)

	snapshot := worker.HealthSnapshot()
	if snapshot.State != HealthStateSuspect || snapshot.ConsecutiveFailures != 1 {
		t.Fatalf("snapshot after success reset = %+v, want suspect with one failure", snapshot)
	}
}
