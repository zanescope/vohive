package device

import (
	"strings"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/internal/config"
)

func TestSuppressQMIUnhealthyEvictionDuringLifecycleRecovery(t *testing.T) {
	pool := NewPool(&config.Config{})
	worker := &Worker{
		ID: "dev1",
		Config: config.DeviceConfig{
			ID:            "dev1",
			DeviceBackend: backend.BackendQMI,
			ControlDevice: "/dev/cdc-wdm0",
		},
		Backend: &workerStatusBackendStub{mode: backend.BackendQMI, opModeErr: errBackendUnavailable{}},
	}
	pool.workers["dev1"] = worker
	pool.lifecycle.BeginRecovery("dev1", LifecyclePhaseQMIStarting, "modem_reboot", qmiLifecycleRecoveryTTL)

	suppressed, reason := pool.suppressQMIUnhealthyEviction(worker)
	if !suppressed {
		t.Fatal("expected lifecycle recovery to suppress eviction")
	}
	if !strings.Contains(reason, "lifecycle_qmi_starting") {
		t.Fatalf("reason=%q want contains lifecycle_qmi_starting", reason)
	}
}

func TestSuppressQMIUnhealthyEvictionAfterLifecycleDeadline(t *testing.T) {
	pool := NewPool(&config.Config{})
	worker := &Worker{
		ID: "dev1",
		Config: config.DeviceConfig{
			ID:            "dev1",
			DeviceBackend: backend.BackendQMI,
			ControlDevice: "/dev/cdc-wdm0",
		},
		Backend: &workerStatusBackendStub{mode: backend.BackendQMI, opModeErr: errBackendUnavailable{}},
	}
	now := time.Now().Add(-2 * qmiLifecycleRecoveryTTL)
	pool.lifecycle.BeginRecoveryAt("dev1", LifecyclePhaseRecovering, "modem_reboot", now, time.Second)

	suppressed, reason := pool.suppressQMIUnhealthyEviction(worker)
	if suppressed {
		t.Fatalf("suppressed=true want false reason=%q", reason)
	}
}

func TestLifecycleReadDoesNotConsumeActiveExpirySignal(t *testing.T) {
	lifecycle := newLifecycleCoordinator()
	now := time.Now()
	lifecycle.BeginRecoveryAt("dev1", LifecyclePhaseRecovering, "stuck", now.Add(-time.Minute), time.Second)

	if got := lifecycle.getSnapshotAt("dev1", now).Phase; got != LifecyclePhaseOffline {
		t.Fatalf("projected phase=%s want=%s", got, LifecyclePhaseOffline)
	}
	if expired, ok := lifecycle.TakeExpiredRecovery("dev1", now); !ok || expired.Phase != LifecyclePhaseRecovering {
		t.Fatalf("expiry was consumed by read: ok=%v snapshot=%+v", ok, expired)
	}
	if _, ok := lifecycle.TakeExpiredRecovery("dev1", now.Add(time.Second)); ok {
		t.Fatal("expiry signal was consumed more than once")
	}
}

type errBackendUnavailable struct{}

func (errBackendUnavailable) Error() string { return "backend unavailable" }
