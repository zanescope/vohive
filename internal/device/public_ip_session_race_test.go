package device

import (
	"testing"
	"time"
)

func TestPublicIPDataSessionTokensResetWithoutDisconnectAndDeduplicateCallbacks(t *testing.T) {
	tests := []struct {
		name   string
		source publicIPDataSessionSource
	}{
		{name: "QMI", source: publicIPDataSessionQMI},
		{name: "MBIM", source: publicIPDataSessionMBIM},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, worker, controller := newPublicIPStateHarness(t)
			usePublicIPCadenceForTest(t, worker, 1)
			_ = installFakePublicIPClock(t, worker)
			controller.setPrivate("10.0.0.2", "")
			controller.setPublic("", "")

			p.refreshIPs(worker, true)
			state := &worker.publicIP
			waitPublicIPTest(t, time.Second, func() bool {
				state.mu.Lock()
				defer state.mu.Unlock()
				return controller.probeCalls.Load() == 1 &&
					state.phase == publicIPProbePhaseDegraded &&
					state.periodicTimer != nil
			})

			state.mu.Lock()
			initialEpoch := state.epoch
			initialTimer := state.periodicTimer
			state.mu.Unlock()

			if !p.refreshIPsForDataSession(worker, test.source, 41) {
				t.Fatal("first data-session token was not accepted")
			}
			waitPublicIPTest(t, time.Second, func() bool {
				state.mu.Lock()
				defer state.mu.Unlock()
				return controller.probeCalls.Load() == 2 &&
					state.epoch == initialEpoch+1 &&
					state.phase == publicIPProbePhaseDegraded &&
					state.periodicTimer != nil
			})
			if timer, ok := initialTimer.(*fakePublicIPTimer); !ok || timer.active() {
				t.Fatal("new data session did not stop the previous degraded timer")
			}

			state.mu.Lock()
			firstSessionEpoch := state.epoch
			firstSessionTimer := state.periodicTimer
			firstSessionDue := state.nextProbeAt
			state.mu.Unlock()

			if p.refreshIPsForDataSession(worker, test.source, 41) {
				t.Fatal("duplicate data-session callback was accepted")
			}
			state.mu.Lock()
			if state.epoch != firstSessionEpoch ||
				state.periodicTimer != firstSessionTimer ||
				!state.nextProbeAt.Equal(firstSessionDue) {
				state.mu.Unlock()
				t.Fatalf("duplicate callback changed cadence: epoch=%d want=%d same_timer=%t due=%s want=%s",
					state.epoch, firstSessionEpoch, state.periodicTimer == firstSessionTimer,
					state.nextProbeAt, firstSessionDue)
			}
			state.mu.Unlock()
			if calls := controller.probeCalls.Load(); calls != 2 {
				t.Fatalf("duplicate callback launched a probe: calls=%d", calls)
			}

			// A new token is a real reconnect edge even when the private tuple
			// is unchanged and no disconnect callback was observed.
			if !p.refreshIPsForDataSession(worker, test.source, 42) {
				t.Fatal("new data-session token was not accepted")
			}
			waitPublicIPTest(t, time.Second, func() bool {
				state.mu.Lock()
				defer state.mu.Unlock()
				return controller.probeCalls.Load() == 3 &&
					state.epoch == firstSessionEpoch+1 &&
					state.phase == publicIPProbePhaseDegraded
			})
			if timer, ok := firstSessionTimer.(*fakePublicIPTimer); !ok || timer.active() {
				t.Fatal("same-tuple reconnect did not stop the previous degraded timer")
			}
		})
	}
}

func TestPublicIPTimerDispatchReservationBlocksUnchangedEvent(t *testing.T) {
	p, worker, controller := newPublicIPStateHarness(t)
	usePublicIPCadenceForTest(t, worker, 2)
	clock := installFakePublicIPClock(t, worker)
	controller.setPrivate("10.0.0.2", "")
	controller.setPublic("8.8.8.8", "")

	p.refreshIPs(worker, true)
	state := &worker.publicIP
	waitPublicIPTest(t, time.Second, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return controller.probeCalls.Load() == 1 &&
			state.phase == publicIPProbePhaseHealthy &&
			state.periodicTimer != nil
	})

	dispatchReserved := make(chan struct{})
	releaseDispatch := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseDispatch)
		}
	}()
	state.mu.Lock()
	epoch := state.epoch
	state.timerDispatchHook = func() {
		close(dispatchReserved)
		<-releaseDispatch
	}
	state.mu.Unlock()

	controller.setPublic("", "")
	timer := clock.latestActive(t)
	clock.mu.Lock()
	clock.now = clock.now.Add(timer.delay)
	clock.mu.Unlock()
	fireDone := make(chan bool, 1)
	go func() {
		fireDone <- timer.Fire()
	}()

	select {
	case <-dispatchReserved:
	case <-time.After(time.Second):
		t.Fatal("timer did not reserve dispatch")
	}

	// This is the deterministic gap that previously allowed Event to launch
	// before the timer reached refreshIPs, causing a pending second probe.
	p.refreshIPs(worker, true)
	state.mu.Lock()
	if state.epoch != epoch || state.inFlight || !state.timerDispatching || state.pending {
		state.mu.Unlock()
		t.Fatalf("event bypassed timer reservation: epoch=%d want=%d in_flight=%t dispatching=%t pending=%t",
			state.epoch, epoch, state.inFlight, state.timerDispatching, state.pending)
	}
	state.mu.Unlock()
	if calls := controller.probeCalls.Load(); calls != 1 {
		t.Fatalf("event launched during timer dispatch: calls=%d", calls)
	}

	close(releaseDispatch)
	released = true
	select {
	case fired := <-fireDone:
		if !fired {
			t.Fatal("reserved timer did not fire")
		}
	case <-time.After(time.Second):
		t.Fatal("reserved timer did not finish dispatch")
	}

	waitPublicIPTest(t, time.Second, func() bool {
		state.mu.Lock()
		defer state.mu.Unlock()
		return controller.probeCalls.Load() == 2 &&
			state.phase == publicIPProbePhaseFastRetry &&
			state.retryTimer != nil &&
			!state.timerDispatching &&
			!state.pending
	})
	time.Sleep(20 * time.Millisecond)
	if calls := controller.probeCalls.Load(); calls != 2 {
		t.Fatalf("timer/event interleaving launched %d probes, want two total", calls)
	}
}
