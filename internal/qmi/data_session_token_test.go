package qmicore

import (
	"testing"
	"time"
)

func TestDataConnectedCallbackReceivesMonotonicSessionTokens(t *testing.T) {
	manager := &Manager{}
	tokens := make(chan uint64, 2)
	manager.SetOnConnect(func(sessionToken uint64) {
		tokens <- sessionToken
	})

	manager.dispatchDataConnected()
	manager.dispatchDataConnected()

	seen := make(map[uint64]bool, 2)
	for len(seen) < 2 {
		select {
		case token := <-tokens:
			seen[token] = true
		case <-time.After(time.Second):
			t.Fatal("data-connected callbacks did not complete")
		}
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("session tokens=%v, want 1 and 2", seen)
	}
}
