package device

import (
	"errors"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
)

// Over-cap MBIM exhausted events must NOT schedule a rebuild; instead the
// worker is marked Failed.
func TestMBIMRecoveryExhaustedRespectsRebuildGuard(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	worker := &Worker{ID: "mbim-dev", generation: 1, stop: make(chan struct{})}
	p.workers[worker.ID] = worker
	p.transportRecovery.SetWorkerGeneration(worker.ID, worker.generation)

	now := time.Now()
	event := testTransportRecoveryEvent(worker.ID, worker.generation)
	for i := 0; i < rebuildMaxInWindow; i++ {
		commitBudgetedRebuildAt(t, p.transportRecovery, event, now)
	}

	scheduled := p.maybeScheduleTransportRebuild(worker, HealthLayerMBIM, "still_hung", errors.New("hung"))
	if scheduled {
		t.Fatal("rebuild should be refused once the window cap is hit")
	}
	if got := worker.HealthSnapshot().State; got != HealthStateFailed {
		t.Fatalf("worker state = %v, want Failed after guard refusal", got)
	}
	if got := rebuildAttemptCount(p.transportRecovery, worker.ID); got != rebuildMaxInWindow {
		t.Fatalf("rate-limited rebuild changed count to %d, want %d", got, rebuildMaxInWindow)
	}
}
