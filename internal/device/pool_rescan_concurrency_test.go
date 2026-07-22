package device

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
)

func TestRescanAndReconnectSerializesHardwareDiscovery(t *testing.T) {
	origDiscover := discoverQMIDevicesFn
	t.Cleanup(func() { discoverQMIDevicesFn = origDiscover })

	var active atomic.Int32
	var maximum atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	discoverQMIDevicesFn = func() ([]QMIDevice, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		return nil, nil
	}

	pool := NewPool(&config.Config{})
	t.Cleanup(pool.cancel)

	firstDone := make(chan error, 1)
	go func() { firstDone <- pool.RescanAndReconnect() }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first rescan did not enter hardware discovery")
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- pool.RescanAndReconnect()
	}()
	<-secondStarted

	select {
	case <-entered:
		t.Fatal("second rescan entered hardware discovery before the first completed")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	for index, result := range []<-chan error{firstDone, secondDone} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("rescan %d failed: %v", index+1, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("rescan %d did not complete", index+1)
		}
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent discoveries = %d, want 1", got)
	}
}
