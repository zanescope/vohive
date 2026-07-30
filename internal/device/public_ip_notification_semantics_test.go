package device

import (
	"testing"
	"time"
)

func TestDuplicateDataSessionCallbackStillNotifiesSubscribers(t *testing.T) {
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

			notifications := make(chan string, 2)
			p.OnDataConnected(func(deviceID string) {
				notifications <- deviceID
			})

			p.handlePublicIPDataSessionConnected(worker, test.source, 51)
			state := &worker.publicIP
			state.mu.Lock()
			firstEpoch := state.epoch
			state.mu.Unlock()

			// The public-IP state machine deduplicates this token, but existing
			// data-connected subscribers must still observe the callback.
			p.handlePublicIPDataSessionConnected(worker, test.source, 51)

			for count := 0; count < 2; count++ {
				select {
				case deviceID := <-notifications:
					if deviceID != worker.ID {
						t.Fatalf("notification device=%q, want %q", deviceID, worker.ID)
					}
				case <-time.After(time.Second):
					t.Fatalf("received %d data-connected notifications, want two", count)
				}
			}

			waitPublicIPTest(t, time.Second, func() bool {
				state.mu.Lock()
				defer state.mu.Unlock()
				return controller.probeCalls.Load() == 1 &&
					state.epoch == firstEpoch &&
					state.phase == publicIPProbePhaseDegraded &&
					state.periodicTimer != nil
			})
			select {
			case deviceID := <-notifications:
				t.Fatalf("unexpected extra data-connected notification for %q", deviceID)
			default:
			}
		})
	}
}
