package mbimcore

import "testing"

func TestDataConnectedCallbackKeepsTokenStableWithinSession(t *testing.T) {
	manager := &Manager{dataEpoch: 7}
	var tokens []uint64
	manager.OnDataConnected(func(sessionToken uint64) {
		tokens = append(tokens, sessionToken)
	})

	manager.dataMu.Lock()
	manager.queueDataConnectedCallbackLocked()
	manager.queueDataConnectedCallbackLocked()
	callbacks := manager.takePendingDataCallbacksLocked()
	manager.dataMu.Unlock()
	runDataCallbacks(callbacks)

	manager.mu.Lock()
	manager.dataEpoch++
	manager.mu.Unlock()
	manager.dataMu.Lock()
	manager.queueDataConnectedCallbackLocked()
	callbacks = manager.takePendingDataCallbacksLocked()
	manager.dataMu.Unlock()
	runDataCallbacks(callbacks)

	if len(tokens) != 3 || tokens[0] != 7 || tokens[1] != 7 || tokens[2] != 8 {
		t.Fatalf("session tokens=%v, want [7 7 8]", tokens)
	}
}
