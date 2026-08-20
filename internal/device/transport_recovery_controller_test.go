package device

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
)

func testTransportRecoveryEvent(deviceID string, generation uint64) TransportRecoveryEvent {
	return TransportRecoveryEvent{
		DeviceID:         deviceID,
		WorkerGeneration: generation,
		Kind:             TransportRecoveryEventRecoveryExhausted,
		Err:              errors.New("QMI: read failed: EOF"),
	}
}

func rebuildAttemptCount(c *TransportRecoveryController, deviceID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.rebuildTimes[deviceID])
}

func commitBudgetedRebuildAt(t *testing.T, c *TransportRecoveryController, event TransportRecoveryEvent, now time.Time) {
	t.Helper()
	token, status := c.beginAt(event, true, now)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("Begin() status=%v, want accepted", status)
	}
	if !c.commitAt(token, now) {
		t.Fatal("Commit() = false, want true")
	}
	if !c.Finish(token) {
		t.Fatal("Finish() = false, want true")
	}
}

func TestTransportRecoveryControllerSerializesPerDevice(t *testing.T) {
	controller := NewTransportRecoveryController(nil)
	event := testTransportRecoveryEvent("dev1", 0)

	first, status := controller.Begin(event, false)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("first Begin() status=%v, want accepted", status)
	}
	if _, status := controller.Begin(event, false); status != TransportRecoveryBeginDuplicate {
		t.Fatalf("second Begin() status=%v, want duplicate", status)
	}
	if !controller.Commit(first) {
		t.Fatal("first Commit() = false, want true")
	}
	if !controller.Finish(first) {
		t.Fatal("first Finish() = false, want true")
	}

	second, status := controller.Begin(event, false)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("Begin() after Finish status=%v, want accepted", status)
	}
	if !controller.Finish(second) {
		t.Fatal("second Finish() = false, want true")
	}
}

func TestTransportRecoveryControllerAllowsDifferentDevices(t *testing.T) {
	controller := NewTransportRecoveryController(nil)

	first, status := controller.Begin(testTransportRecoveryEvent("dev1", 0), false)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("dev1 Begin() status=%v, want accepted", status)
	}
	second, status := controller.Begin(testTransportRecoveryEvent("dev2", 0), false)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("dev2 Begin() status=%v, want accepted", status)
	}
	controller.Finish(first)
	controller.Finish(second)
}

func TestTransportRecoveryControllerIgnoresStaleWorkerGeneration(t *testing.T) {
	controller := NewTransportRecoveryController(nil)
	controller.SetWorkerGenerationForTest("dev1", 3)

	if _, status := controller.Begin(testTransportRecoveryEvent("dev1", 2), true); status != TransportRecoveryBeginStaleGeneration {
		t.Fatalf("stale generation Begin() status=%v, want stale generation", status)
	}
	if got := rebuildAttemptCount(controller, "dev1"); got != 0 {
		t.Fatalf("stale generation recorded %d rebuild attempts, want 0", got)
	}
	token, status := controller.Begin(testTransportRecoveryEvent("dev1", 3), true)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("current generation Begin() status=%v, want accepted", status)
	}
	controller.Finish(token)
}

func TestTransportRecoveryControllerAcceptsStructuredRecoveryEvents(t *testing.T) {
	controller := NewTransportRecoveryController(nil)

	first, status := controller.Begin(TransportRecoveryEvent{
		DeviceID: "dev1",
		Kind:     TransportRecoveryEventRecoveryExhausted,
		Err:      errors.New("write failed: write unix @->@qmi-proxy: write: broken pipe"),
	}, false)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("recovery exhausted Begin() status=%v, want accepted", status)
	}
	controller.Finish(first)

	second, status := controller.Begin(TransportRecoveryEvent{
		DeviceID: "dev1",
		Kind:     TransportRecoveryEventHealthSuspect,
		Err:      errors.New("QMI service operation timeout: NAS GetServingSystem: context deadline exceeded"),
	}, false)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("health threshold Begin() status=%v, want accepted", status)
	}
	controller.Finish(second)
}

func TestTransportRecoveryControllerDuplicateDoesNotConsumeBudget(t *testing.T) {
	controller := NewTransportRecoveryController(nil)
	now := time.Now()
	controller.SetWorkerGeneration("dev1", 1)
	event := testTransportRecoveryEvent("dev1", 1)

	token, status := controller.beginAt(event, true, now)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("first Begin() status=%v, want accepted", status)
	}
	if _, status := controller.beginAt(event, true, now); status != TransportRecoveryBeginDuplicate {
		t.Fatalf("duplicate Begin() status=%v, want duplicate", status)
	}
	if got := rebuildAttemptCount(controller, "dev1"); got != 0 {
		t.Fatalf("reservation/duplicate recorded %d rebuild attempts, want 0", got)
	}
	if !controller.commitAt(token, now) {
		t.Fatal("Commit() = false, want true")
	}
	if got := rebuildAttemptCount(controller, "dev1"); got != 1 {
		t.Fatalf("committed rebuild count=%d, want 1", got)
	}
	controller.Finish(token)
}

func TestTransportRecoveryControllerConcurrentDuplicateUsesOneBudgetSlot(t *testing.T) {
	controller := NewTransportRecoveryController(nil)
	now := time.Now()
	controller.SetWorkerGeneration("dev1", 1)
	event := testTransportRecoveryEvent("dev1", 1)

	type result struct {
		token  TransportRecoveryToken
		status TransportRecoveryBeginStatus
	}
	const callers = 64
	start := make(chan struct{})
	results := make(chan result, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			<-start
			token, status := controller.beginAt(event, true, now)
			results <- result{token: token, status: status}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var accepted TransportRecoveryToken
	duplicates := 0
	for result := range results {
		switch result.status {
		case TransportRecoveryBeginAccepted:
			if accepted.valid() {
				t.Fatal("more than one concurrent Begin() was accepted")
			}
			accepted = result.token
		case TransportRecoveryBeginDuplicate:
			duplicates++
		default:
			t.Fatalf("concurrent Begin() status=%v, want accepted or duplicate", result.status)
		}
	}
	if !accepted.valid() || duplicates != callers-1 {
		t.Fatalf("accepted=%v duplicates=%d, want one accepted and %d duplicates", accepted.valid(), duplicates, callers-1)
	}
	if got := rebuildAttemptCount(controller, "dev1"); got != 0 {
		t.Fatalf("concurrent reservations recorded %d attempts before Commit, want 0", got)
	}
	if !controller.commitAt(accepted, now) {
		t.Fatal("winning Commit() = false, want true")
	}
	if got := rebuildAttemptCount(controller, "dev1"); got != 1 {
		t.Fatalf("committed rebuild count=%d, want 1", got)
	}
	controller.Finish(accepted)
}

func TestTransportRecoveryControllerCommitRejectsGenerationChangedAfterBegin(t *testing.T) {
	controller := NewTransportRecoveryController(nil)
	now := time.Now()
	controller.SetWorkerGeneration("dev1", 1)
	token, status := controller.beginAt(testTransportRecoveryEvent("dev1", 1), true, now)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("Begin() status=%v, want accepted", status)
	}

	controller.SetWorkerGeneration("dev1", 2)
	if controller.commitAt(token, now) {
		t.Fatal("stale reservation Commit() = true, want false")
	}
	if got := rebuildAttemptCount(controller, "dev1"); got != 0 {
		t.Fatalf("stale Commit recorded %d rebuild attempts, want 0", got)
	}
	newToken, status := controller.beginAt(testTransportRecoveryEvent("dev1", 2), true, now)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("new generation Begin() status=%v, want accepted", status)
	}
	controller.Finish(newToken)
}

func TestTransportRecoveryControllerOldFinishCannotClearNewOperation(t *testing.T) {
	controller := NewTransportRecoveryController(nil)
	event := testTransportRecoveryEvent("dev1", 0)

	oldToken, status := controller.Begin(event, false)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("old Begin() status=%v, want accepted", status)
	}
	if !controller.Finish(oldToken) {
		t.Fatal("old Finish() = false, want true")
	}
	newToken, status := controller.Begin(event, false)
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("new Begin() status=%v, want accepted", status)
	}
	if controller.Finish(oldToken) {
		t.Fatal("stale Finish() cleared a newer operation")
	}
	if _, status := controller.Begin(event, false); status != TransportRecoveryBeginDuplicate {
		t.Fatalf("Begin() after stale Finish status=%v, want duplicate", status)
	}
	controller.Finish(newToken)
}

func TestTransportRecoveryControllerSlidingWindow(t *testing.T) {
	controller := NewTransportRecoveryController(nil)
	now := time.Now()
	event := testTransportRecoveryEvent("dev1", 1)
	controller.SetWorkerGeneration("dev1", 1)

	for i := 0; i < rebuildMaxInWindow; i++ {
		commitBudgetedRebuildAt(t, controller, event, now)
	}
	if _, status := controller.beginAt(event, true, now.Add(time.Minute)); status != TransportRecoveryBeginRateLimited {
		t.Fatalf("over-cap Begin() status=%v, want rate limited", status)
	}
	if got := rebuildAttemptCount(controller, "dev1"); got != rebuildMaxInWindow {
		t.Fatalf("over-cap attempt changed rebuild count to %d, want %d", got, rebuildMaxInWindow)
	}

	token, status := controller.beginAt(event, true, now.Add(rebuildWindow))
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("Begin() at window boundary status=%v, want accepted", status)
	}
	if !controller.commitAt(token, now.Add(rebuildWindow)) {
		t.Fatal("Commit() at window boundary = false, want true")
	}
	controller.Finish(token)
}

func TestTransportRecoveryControllerBudgetPersistsAcrossWorkerGenerations(t *testing.T) {
	controller := NewTransportRecoveryController(nil)
	now := time.Now()
	deviceID := "dev1"

	for i := 0; i < rebuildMaxInWindow; i++ {
		generation := uint64(i + 1)
		controller.SetWorkerGeneration(deviceID, generation)
		commitBudgetedRebuildAt(t, controller, testTransportRecoveryEvent(deviceID, generation), now)
	}
	controller.SetWorkerGeneration(deviceID, 42)
	if _, status := controller.beginAt(testTransportRecoveryEvent(deviceID, 42), true, now); status != TransportRecoveryBeginRateLimited {
		t.Fatalf("Begin() after generation changes status=%v, want rate limited", status)
	}
}

func TestRemoveWorkerRegistrationIfCurrentKeepsNewWorker(t *testing.T) {
	pool := NewPool(&config.Config{})
	defer pool.cancel()

	oldWorker := &Worker{ID: "dev1", stop: make(chan struct{})}
	newWorker := &Worker{ID: "dev1", stop: make(chan struct{})}

	if err := pool.registerWorkerStarting(oldWorker); err != nil {
		t.Fatalf("register old worker: %v", err)
	}
	pool.mu.Lock()
	pool.workers["dev1"] = newWorker
	pool.mu.Unlock()

	pool.removeWorkerRegistrationIfCurrent(oldWorker)

	if got := pool.GetWorker("dev1"); got != newWorker {
		t.Fatalf("GetWorker() = %#v, want new worker", got)
	}
}

func TestTransportRecoverySchedulingConflictDoesNotConsumeBudget(t *testing.T) {
	pool := NewPool(&config.Config{})
	defer pool.cancel()
	worker := &Worker{ID: "dev1", stop: make(chan struct{}), generation: 1}
	pool.workers[worker.ID] = worker
	pool.transportRecovery.SetWorkerGeneration(worker.ID, worker.generation)

	if !pool.beginModemRebootRecovery(worker.ID) {
		t.Fatal("failed to occupy modem recovery slot")
	}
	defer pool.finishModemRebootRecovery(worker.ID)

	status := pool.scheduleTransportRebuildWithEvent(worker.ID, qmiTransportFailureRecoveryReason, &TransportRecoveryEvent{
		DeviceID:         worker.ID,
		WorkerGeneration: worker.generation,
		Kind:             TransportRecoveryEventRecoveryExhausted,
		Source:           "recovery_exhausted:test",
		Err:              errors.New("qmi recovery exhausted"),
	})
	if status != TransportRecoveryBeginAccepted {
		t.Fatalf("schedule status=%v, want reservation accepted", status)
	}

	deadline := time.Now().Add(time.Second)
	for {
		pool.transportRecovery.mu.Lock()
		_, active := pool.transportRecovery.active[worker.ID]
		pool.transportRecovery.mu.Unlock()
		if !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed scheduling reservation was not released")
		}
		time.Sleep(time.Millisecond)
	}
	if got := rebuildAttemptCount(pool.transportRecovery, worker.ID); got != 0 {
		t.Fatalf("failed scheduling recorded %d rebuild attempts, want 0", got)
	}
}

func TestTerminalWorkerReprobeIsImmediateThenCooledDown(t *testing.T) {
	c := NewTransportRecoveryController(nil)
	now := time.Now()
	dev := "dev-terminal"

	if allowed, retryAfter := c.allowTerminalWorkerReprobeAt(dev, now); !allowed || retryAfter != 0 {
		t.Fatalf("first terminal reprobe = (%v, %s), want (true, 0)", allowed, retryAfter)
	}
	c.SetWorkerGeneration(dev, 2)
	if allowed, retryAfter := c.allowTerminalWorkerReprobeAt(dev, now.Add(time.Minute)); allowed || retryAfter != 4*time.Minute {
		t.Fatalf("cooled terminal reprobe after generation change = (%v, %s), want (false, 4m)", allowed, retryAfter)
	}
	if allowed, retryAfter := c.allowTerminalWorkerReprobeAt(dev, now.Add(terminalWorkerReprobeCooldown)); !allowed || retryAfter != 0 {
		t.Fatalf("terminal reprobe at cooldown boundary = (%v, %s), want (true, 0)", allowed, retryAfter)
	}
}

func TestTerminalWorkerUdevAddGrantsOneImmediateReprobe(t *testing.T) {
	c := NewTransportRecoveryController(nil)
	now := time.Now()
	dev := "dev-terminal"

	if allowed, _ := c.allowTerminalWorkerReprobeAt(dev, now); !allowed {
		t.Fatal("initial terminal reprobe was not allowed")
	}
	if allowed, _ := c.allowTerminalWorkerReprobeAt(dev, now.Add(time.Second)); allowed {
		t.Fatal("terminal reprobe unexpectedly bypassed cooldown before udev add")
	}

	c.noteTerminalWorkerUdevAddAt(dev, now.Add(2*time.Second))
	if allowed, retryAfter := c.allowTerminalWorkerReprobeAt(dev, now.Add(3*time.Second)); !allowed || retryAfter != 0 {
		t.Fatalf("terminal reprobe after udev add = (%v, %s), want (true, 0)", allowed, retryAfter)
	}
	if allowed, _ := c.allowTerminalWorkerReprobeAt(dev, now.Add(4*time.Second)); allowed {
		t.Fatal("single udev add granted more than one terminal reprobe")
	}
}

func TestTerminalWorkerUdevAddPermitExpires(t *testing.T) {
	c := NewTransportRecoveryController(nil)
	now := time.Now()
	dev := "dev-terminal"

	if allowed, _ := c.allowTerminalWorkerReprobeAt(dev, now); !allowed {
		t.Fatal("initial terminal reprobe was not allowed")
	}
	c.noteTerminalWorkerUdevAddAt(dev, now.Add(time.Second))
	if allowed, _ := c.allowTerminalWorkerReprobeAt(dev, now.Add(time.Second+terminalWorkerUdevAddTTL+time.Nanosecond)); allowed {
		t.Fatal("expired udev add permit bypassed terminal reprobe cooldown")
	}
}

func TestTerminalWorkerUdevAddPermitFromFutureIsRejected(t *testing.T) {
	c := NewTransportRecoveryController(nil)
	now := time.Now()
	dev := "dev-terminal"

	if allowed, _ := c.allowTerminalWorkerReprobeAt(dev, now); !allowed {
		t.Fatal("initial terminal reprobe was not allowed")
	}
	c.noteTerminalWorkerUdevAddAt(dev, now.Add(time.Second))
	if allowed, _ := c.allowTerminalWorkerReprobeAt(dev, now); allowed {
		t.Fatal("future udev add permit bypassed terminal reprobe cooldown")
	}
}

func TestTerminalWorkerUdevAddPermitDoesNotCrossWorkerGeneration(t *testing.T) {
	c := NewTransportRecoveryController(nil)
	now := time.Now()
	dev := "dev-terminal"

	c.SetWorkerGeneration(dev, 1)
	if allowed, _ := c.allowTerminalWorkerReprobeAt(dev, now); !allowed {
		t.Fatal("initial terminal reprobe was not allowed")
	}
	c.noteTerminalWorkerUdevAddAt(dev, now.Add(time.Second))
	c.SetWorkerGeneration(dev, 2)
	if allowed, _ := c.allowTerminalWorkerReprobeAt(dev, now.Add(2*time.Second)); allowed {
		t.Fatal("stale udev add permit crossed into a replacement Worker generation")
	}
}
