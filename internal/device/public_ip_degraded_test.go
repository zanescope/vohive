package device

import (
	"context"
	"testing"
	"time"
)

func usePublicIPCadenceForTest(t *testing.T, worker *Worker, retryLimit int) {
	t.Helper()
	if worker == nil {
		t.Fatal("worker is required")
	}
	state := &worker.publicIP
	state.mu.Lock()
	state.retryLimit = retryLimit
	state.mu.Unlock()
}

func TestPublicIPProbeTransitionsToTwoHourDegradedCadence(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	usePublicIPCadenceForTest(t, worker, 6)
	clock := installFakePublicIPClock(t, worker)
	controller.setPrivate("10.0.0.2", "")
	controller.setPublic("", "")

	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		state := &worker.publicIP
		state.mu.Lock()
		defer state.mu.Unlock()
		return controller.probeCalls.Load() == 1 &&
			state.phase == publicIPProbePhaseFastRetry &&
			state.retryAttemptV4 == 1
	})

	for wantAttempt := 2; wantAttempt <= publicIPRetryLimit; wantAttempt++ {
		clock.advanceAndFire(t, clock.latestActive(t))
		waitPublicIPTest(t, time.Second, func() bool {
			state := &worker.publicIP
			state.mu.Lock()
			defer state.mu.Unlock()
			return controller.probeCalls.Load() == int32(wantAttempt) &&
				state.retryAttemptV4 == wantAttempt
		})
	}

	state := &worker.publicIP
	state.mu.Lock()
	epoch := state.epoch
	degradedTimer := state.periodicTimer
	degradedDue := state.nextProbeAt
	if state.phase != publicIPProbePhaseDegraded || !state.degradedWarned ||
		state.retryAttemptV4 != publicIPRetryLimit || state.degradedSlowChecks != 0 ||
		state.timerKind != publicIPTimerDegraded || degradedTimer == nil {
		state.mu.Unlock()
		t.Fatalf("degraded state phase=%s warned=%t attempts=%d slow=%d kind=%d timer=%v",
			state.phase, state.degradedWarned, state.retryAttemptV4,
			state.degradedSlowChecks, state.timerKind, degradedTimer != nil)
	}
	state.mu.Unlock()

	wantDegradedDelay := stablePublicIPDelay(worker.ID, epoch, publicIPDegradedRecheckInterval)
	if timer := clock.latestActive(t); timer.delay != wantDegradedDelay {
		t.Fatalf("degraded delay=%s, want stable two-hour delay %s", timer.delay, wantDegradedDelay)
	}

	// Unchanged health observations and IP-change events cannot consume an
	// attempt, launch a probe, or replace/move the scheduled slow timer.
	p.refreshIPs(worker, false)
	p.refreshIPs(worker, true)
	time.Sleep(20 * time.Millisecond)
	state.mu.Lock()
	if controller.probeCalls.Load() != int32(publicIPRetryLimit) ||
		state.periodicTimer != degradedTimer || !state.nextProbeAt.Equal(degradedDue) {
		state.mu.Unlock()
		t.Fatalf("unchanged event moved degraded cadence: probes=%d same_timer=%t due=%s want=%s",
			controller.probeCalls.Load(), state.periodicTimer == degradedTimer,
			state.nextProbeAt, degradedDue)
	}
	state.mu.Unlock()

	clock.advanceAndFire(t, clock.latestActive(t))
	waitPublicIPTest(t, time.Second, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return controller.probeCalls.Load() == int32(publicIPRetryLimit+1) &&
			state.phase == publicIPProbePhaseDegraded &&
			state.retryAttemptV4 == publicIPRetryLimit &&
			state.degradedSlowChecks == 1
	})

	controller.setPublic("8.8.8.8", "")
	clock.advanceAndFire(t, clock.latestActive(t))
	waitPublicIPTest(t, time.Second, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return controller.probeCalls.Load() == int32(publicIPRetryLimit+2) &&
			state.phase == publicIPProbePhaseHealthy &&
			state.retryAttemptV4 == 0 &&
			state.timerKind == publicIPTimerHealthy &&
			state.periodicTimer != nil
	})

	healthyTimer := clock.latestActive(t)
	if want := stablePublicIPDelay(worker.ID, epoch, publicIPHealthyRecheckInterval); healthyTimer.delay != want {
		t.Fatalf("healthy delay=%s, want %s", healthyTimer.delay, want)
	}
	state.mu.Lock()
	healthyDue := state.nextProbeAt
	healthyTimerHandle := state.periodicTimer
	state.mu.Unlock()
	p.refreshIPs(worker, false)
	p.refreshIPs(worker, true)
	time.Sleep(20 * time.Millisecond)
	state.mu.Lock()
	defer state.mu.Unlock()
	if controller.probeCalls.Load() != int32(publicIPRetryLimit+2) ||
		state.periodicTimer != healthyTimerHandle || !state.nextProbeAt.Equal(healthyDue) {
		t.Fatalf("unchanged event moved healthy cadence: probes=%d same_timer=%t due=%s want=%s",
			controller.probeCalls.Load(), state.periodicTimer == healthyTimerHandle,
			state.nextProbeAt, healthyDue)
	}
}

func TestPublicIPNoAddressDegradedCounterSaturatesAndTupleChangeResets(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	const retryLimit = 2
	usePublicIPCadenceForTest(t, worker, retryLimit)
	clock := installFakePublicIPClock(t, worker)
	controller.setPrivate("", "")

	p.refreshIPs(worker, true)
	firstTimer := clock.latestActive(t)
	p.refreshIPs(worker, true)
	p.refreshIPs(worker, false)
	state := &worker.publicIP
	state.mu.Lock()
	if state.noAddressTries != 1 || state.retryTimer != firstTimer {
		state.mu.Unlock()
		t.Fatalf("unchanged no-address event moved cadence: tries=%d same_timer=%t",
			state.noAddressTries, state.retryTimer == firstTimer)
	}
	state.mu.Unlock()

	clock.advanceAndFire(t, firstTimer)
	state.mu.Lock()
	if state.phase != publicIPProbePhaseDegraded ||
		state.noAddressTries != retryLimit ||
		state.periodicTimer == nil {
		state.mu.Unlock()
		t.Fatalf("no-address degraded phase=%s tries=%d timer=%v",
			state.phase, state.noAddressTries, state.periodicTimer != nil)
	}
	state.mu.Unlock()

	clock.advanceAndFire(t, clock.latestActive(t))
	state.mu.Lock()
	if state.noAddressTries != retryLimit || state.degradedSlowChecks != 1 {
		state.mu.Unlock()
		t.Fatalf("no-address counter did not saturate: tries=%d slow=%d",
			state.noAddressTries, state.degradedSlowChecks)
	}
	slowTimer := state.periodicTimer
	oldEpoch := state.epoch
	state.mu.Unlock()

	controller.setPrivate("10.0.0.2", "")
	controller.setPublic("8.8.8.8", "")
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return controller.probeCalls.Load() == 1 &&
			state.epoch == oldEpoch+1 &&
			state.phase == publicIPProbePhaseHealthy &&
			state.noAddressTries == 0
	})
	if timer, ok := slowTimer.(*fakePublicIPTimer); !ok || timer.active() {
		t.Fatal("private tuple change did not stop the old degraded timer")
	}
}

func TestPublicIPReconnectAndESIMRotationResetSameTupleDegradedEpoch(t *testing.T) {
	tests := []struct {
		name    string
		trigger publicIPRefreshTrigger
	}{
		{name: "data reconnect", trigger: publicIPRefreshReconnect},
		{name: "eSIM rotation", trigger: publicIPRefreshRotation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, worker, controller := newPublicIPStateHarness(t)
			usePublicIPCadenceForTest(t, worker, 1)
			_ = installFakePublicIPClock(t, worker)
			controller.setPrivate("10.0.0.2", "")
			controller.setPublic("", "")

			p.refreshIPs(worker, true)
			waitPublicIPTest(t, time.Second, func() bool {
				state := &worker.publicIP
				state.mu.Lock()
				defer state.mu.Unlock()
				return state.phase == publicIPProbePhaseDegraded && state.periodicTimer != nil
			})
			state := &worker.publicIP
			state.mu.Lock()
			oldEpoch := state.epoch
			oldTimer := state.periodicTimer
			state.mu.Unlock()

			controller.setPublic("8.8.8.8", "")
			p.refreshIPsWithTrigger(worker, test.trigger)
			waitPublicIPTest(t, time.Second, func() bool {
				state.mu.Lock()
				defer state.mu.Unlock()
				return controller.probeCalls.Load() == 2 &&
					state.epoch == oldEpoch+1 &&
					state.phase == publicIPProbePhaseHealthy &&
					state.retryAttemptV4 == 0
			})
			if timer, ok := oldTimer.(*fakePublicIPTimer); !ok || timer.active() {
				t.Fatal("epoch reset did not stop the old degraded timer")
			}
		})
	}
}

func TestPublicIPFamilyChangeResetsDegradedEpoch(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	usePublicIPCadenceForTest(t, worker, 1)
	_ = installFakePublicIPClock(t, worker)
	controller.setPrivate("10.0.0.2", "")
	controller.setPublic("", "")

	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		state := &worker.publicIP
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.phase == publicIPProbePhaseDegraded
	})
	state := &worker.publicIP
	state.mu.Lock()
	oldEpoch := state.epoch
	state.mu.Unlock()

	controller.setPrivate("", "fd00::2")
	controller.setPublic("", "2606:4700:4700::1111")
	p.refreshIPs(worker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		_, v6 := publicIPTestCached(worker)
		return state.epoch == oldEpoch+1 &&
			state.phase == publicIPProbePhaseHealthy &&
			!state.expectedV4 && state.expectedV6 &&
			v6 == "2606:4700:4700::1111"
	})
}

func TestPublicIPUnchangedEventDuringProbeDoesNotQueueBypass(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	usePublicIPCadenceForTest(t, worker, 6)
	_ = installFakePublicIPClock(t, worker)
	controller.setPrivate("10.0.0.2", "")
	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	controller.setProbe(func(context.Context) (string, string) {
		close(probeStarted)
		<-probeRelease
		return "", ""
	})

	p.refreshIPs(worker, true)
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	p.refreshIPs(worker, true)
	p.refreshIPs(worker, false)
	state := &worker.publicIP
	state.mu.Lock()
	pending := state.pending
	state.mu.Unlock()
	if pending {
		t.Fatal("unchanged event queued an immediate follow-up probe")
	}

	close(probeRelease)
	waitPublicIPTest(t, time.Second, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return controller.probeCalls.Load() == 1 &&
			state.phase == publicIPProbePhaseFastRetry &&
			state.retryTimer != nil
	})
	time.Sleep(20 * time.Millisecond)
	if calls := controller.probeCalls.Load(); calls != 1 {
		t.Fatalf("unchanged event launched %d probes, want one", calls)
	}
}

func TestPublicIPStaleGenerationTimerCannotProbeReplacement(t *testing.T) {
	p, oldWorker, controller := newPublicIPStateHarness(t)
	usePublicIPCadenceForTest(t, oldWorker, 6)
	clock := installFakePublicIPClock(t, oldWorker)
	controller.setPrivate("10.0.0.2", "")
	controller.setPublic("", "")

	p.refreshIPs(oldWorker, true)
	waitPublicIPTest(t, time.Second, func() bool {
		state := &oldWorker.publicIP
		state.mu.Lock()
		defer state.mu.Unlock()
		return controller.probeCalls.Load() == 1 && state.retryTimer != nil
	})
	oldTimer := clock.latestActive(t)

	replacement := &Worker{
		ID:          oldWorker.ID,
		Pool:        p,
		netOverride: controller,
		stop:        make(chan struct{}),
	}
	p.mu.Lock()
	replacement.generation = p.workerGenerations[oldWorker.ID] + 1
	p.workerGenerations[oldWorker.ID] = replacement.generation
	p.workers[oldWorker.ID] = replacement
	p.mu.Unlock()
	t.Cleanup(func() { p.stopPublicIPState(replacement) })

	clock.advanceAndFire(t, oldTimer)
	time.Sleep(20 * time.Millisecond)
	if calls := controller.probeCalls.Load(); calls != 1 {
		t.Fatalf("stale generation timer launched a replacement probe: calls=%d", calls)
	}
	if v4, v6 := publicIPTestCached(replacement); v4 != "" || v6 != "" {
		t.Fatalf("stale timer populated replacement cache: (%q, %q)", v4, v6)
	}
}
