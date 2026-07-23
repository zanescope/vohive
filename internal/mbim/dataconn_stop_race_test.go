package mbimcore

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zanescope/vohive/pkg/mbim"
)

func TestDelayedDeactivateDoesNotCancelConnectStartedDuringQuery(t *testing.T) {
	queryStarted := make(chan struct{})
	releaseQuery := make(chan struct{})
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, service, cid, commandType, _, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectConnect &&
			commandType == uint32(mbim.CommandTypeQuery) {
			close(queryStarted)
			<-releaseQuery
			info := make([]byte, 36)
			info[4] = byte(mbim.ActivationStateDeactivated)
			return mbim.BuildCommandDoneForTest(h.TransactionID, service, cid, info), true
		}
		return mbim.TestAnswerConnectAndIPv4Config(written)
	})

	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	var disconnected atomic.Int32
	m.OnDataDisconnected(func() { disconnected.Add(1) })
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()
	if err := m.Connect(); err != nil {
		t.Fatalf("initial Connect: %v", err)
	}

	m.handleConnectIndication(mbim.ConnectState{
		SessionID:       dataSessionID,
		ActivationState: mbim.ActivationStateDeactivated,
	})
	select {
	case <-queryStarted:
	case <-time.After(time.Second):
		t.Fatal("authoritative CONNECT query did not start")
	}

	connectDone := make(chan error, 1)
	go func() { connectDone <- m.Connect() }()
	waitForTestCondition(t, time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.activeDataConnectCancel != nil
	}, "new Connect did not install its cancellation context")
	close(releaseQuery)

	select {
	case err := <-connectDone:
		if err != nil {
			t.Fatalf("Connect started during stale query was canceled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connect started during stale query did not finish")
	}
	time.Sleep(50 * time.Millisecond)
	if !m.IsConnected() || disconnected.Load() != 0 {
		t.Fatalf("stale query disturbed new connect: connected=%v disconnects=%d",
			m.IsConnected(), disconnected.Load())
	}
}

func TestMaxContextProactiveDeactivateQueryFailuresDoNotCancelActivationRetry(t *testing.T) {
	var activateAttempts atomic.Int32
	var connectQueries atomic.Int32
	retryActivateStarted := make(chan struct{}, 1)

	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, service, cid, commandType, info, ok := dataTestCommand(written)
		if !ok || !service.Equal(mbim.UUIDBasicConnect) || cid != mbim.CIDBasicConnectConnect {
			return mbim.TestAnswerConnectAndIPv4Config(written)
		}
		switch commandType {
		case uint32(mbim.CommandTypeQuery):
			connectQueries.Add(1)
			return mbim.BuildCommandDoneForTest(h.TransactionID, service, cid, nil), true
		case uint32(mbim.CommandTypeSet):
			if len(info) < 8 {
				t.Fatalf("CONNECT info too short: %d", len(info))
			}
			switch mbim.ReadU32ForTest(info[4:]) {
			case mbim.ActivationCommandActivate:
				if activateAttempts.Add(1) == 1 {
					return mbim.BuildCommandDoneStatusForTest(h.TransactionID, service, cid, mbimStatusMaxActivatedContexts, nil), true
				}
				select {
				case retryActivateStarted <- struct{}{}:
				default:
				}
				// Leave the retry pending. The test later cancels it through
				// Disconnect after proving the stale indication did not.
				return nil, false
			case mbim.ActivationCommandDeactivate:
				response := make([]byte, 36)
				response[4] = byte(mbim.ActivationStateDeactivated)
				return mbim.BuildCommandDoneForTest(h.TransactionID, service, cid, response), true
			}
		}
		return mbim.TestAnswerConnectAndIPv4Config(written)
	})

	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}

	m.mu.Lock()
	m.connected = true
	m.privateIPv4 = "10.0.0.4"
	m.mu.Unlock()
	connectDone := make(chan error, 1)
	go func() { connectDone <- m.Connect() }()
	select {
	case <-retryActivateStarted:
	case <-time.After(time.Second):
		t.Fatal("activation retry did not start after max-context recovery")
	}

	m.handleConnectIndication(mbim.ConnectState{
		SessionID:       dataSessionID,
		ActivationState: mbim.ActivationStateDeactivated,
	})
	waitForTestCondition(t, 2*time.Second, func() bool {
		return connectQueries.Load() >= 2
	}, "proactive deactivate was not authoritatively rechecked")

	select {
	case err := <-connectDone:
		t.Fatalf("stale proactive-deactivate indication canceled activation retry: %v", err)
	default:
	}
	if err := m.Disconnect(); err != nil {
		if !m.IsConnected() || m.GetPrivateIP() != "10.0.0.4" {
			t.Fatalf("stale state was cleared while activation retry owned convergence: connected=%v private=%q", m.IsConnected(), m.GetPrivateIP())
		}
		t.Fatalf("Disconnect cleanup: %v", err)
	}
	select {
	case err := <-connectDone:
		if err == nil {
			t.Fatal("pending activation retry unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not cancel pending activation retry")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
func TestCloseCancelsInFlightIPConfigurationRefresh(t *testing.T) {
	var ipQueries atomic.Int32
	refreshStarted := make(chan struct{})
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		_, service, cid, commandType, _, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectIPConfiguration &&
			commandType == uint32(mbim.CommandTypeQuery) {
			if ipQueries.Add(1) == 2 {
				close(refreshStarted)
				return nil, false
			}
		}
		return mbim.TestAnswerConnectAndIPv4Config(written)
	})

	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	m.netcfg = fnc
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	flushes := fnc.routeFlushes

	m.handleIPConfigurationIndication()
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("IP configuration refresh did not start")
	}
	start := time.Now()
	closeDone := make(chan error, 1)
	go func() { closeDone <- m.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited for the full IP configuration query timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v with cancellable IP refresh", elapsed)
	}
	waitForTestCondition(t, time.Second, func() bool {
		m.mu.Lock()
		running := m.ipConfigRefreshRunning
		m.mu.Unlock()
		return !running
	}, "IP configuration worker did not stop after Close")
	if fnc.routeFlushes != flushes+1 {
		t.Fatalf("route flushes = %d, want one Close cleanup after %d", fnc.routeFlushes, flushes)
	}
}

func TestNestedDataStopRequestsKeepGateUntilAllFinish(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	m.beginDataStop()
	m.beginDataStop()
	m.endDataStop()

	m.mu.Lock()
	stillStopped := m.dataStopRequested && m.dataStopCount == 1
	m.mu.Unlock()
	if !stillStopped {
		t.Fatal("one stop completion cleared a concurrent stop gate")
	}

	m.endDataStop()
	m.mu.Lock()
	cleared := !m.dataStopRequested && m.dataStopCount == 0
	m.mu.Unlock()
	if !cleared {
		t.Fatal("stop gate was not cleared after all requests completed")
	}
}

func TestRotateDelayedDeactivateGetsBoundedAuthoritativeRecheck(t *testing.T) {
	var connectQueries atomic.Int32
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, service, cid, commandType, _, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) &&
			cid == mbim.CIDBasicConnectConnect &&
			commandType == uint32(mbim.CommandTypeQuery) {
			info := make([]byte, 36)
			if connectQueries.Add(1) == 1 {
				info[4] = byte(mbim.ActivationStateDeactivated)
			} else {
				info[4] = byte(mbim.ActivationStateActivated)
			}
			info[12] = byte(mbim.ContextIPTypeIPv4)
			copy(info[16:32], mbim.UUIDContextTypeInternet[:])
			return mbim.BuildCommandDoneForTest(h.TransactionID, service, cid, info), true
		}
		return mbim.TestAnswerConnectAndIPv4Config(written)
	})

	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	m.netcfg = fnc
	var disconnected atomic.Int32
	m.OnDataDisconnected(func() { disconnected.Add(1) })
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()
	if err := m.Connect(); err != nil {
		t.Fatalf("initial Connect: %v", err)
	}
	if err := m.RotateIP(); err != nil {
		t.Fatalf("RotateIP: %v", err)
	}
	m.reconnectGate.Store(true)
	defer m.reconnectGate.Store(false)
	m.mu.Lock()
	epoch := m.dataEpoch
	m.mu.Unlock()
	flushes := fnc.routeFlushes
	disconnects := disconnected.Load()

	m.handleConnectIndication(mbim.ConnectState{
		SessionID:       dataSessionID,
		ActivationState: mbim.ActivationStateDeactivated,
	})
	waitForTestCondition(t, 2*time.Second, func() bool {
		return connectQueries.Load() >= 2
	}, "delayed RotateIP deactivation did not receive its bounded recheck")
	time.Sleep(25 * time.Millisecond)

	m.mu.Lock()
	finalEpoch := m.dataEpoch
	privateIPv4 := m.privateIPv4
	m.mu.Unlock()
	if !m.IsConnected() || privateIPv4 != "10.0.0.5" {
		t.Fatalf("delayed old-bearer indication cleared rotated bearer: connected=%v private=%q",
			m.IsConnected(), privateIPv4)
	}
	if finalEpoch != epoch || fnc.routeFlushes != flushes || disconnected.Load() != disconnects {
		t.Fatalf("delayed RotateIP indication mutated new bearer: epoch %d->%d flushes %d->%d disconnects %d->%d",
			epoch, finalEpoch, flushes, fnc.routeFlushes, disconnects, disconnected.Load())
	}
}
