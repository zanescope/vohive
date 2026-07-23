package mbimcore

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zanescope/vohive/pkg/mbim"
)

func TestExpectedDeactivateQueryFailureGetsBoundedRecheck(t *testing.T) {
	var connectQueries atomic.Int32
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, service, cid, commandType, _, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectConnect &&
			commandType == uint32(mbim.CommandTypeQuery) {
			connectQueries.Add(1)
			return mbim.BuildCommandDoneForTest(h.TransactionID, service, cid, nil), true
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
		t.Fatalf("Connect: %v", err)
	}

	// Prevent this test from immediately reconnecting after the second failed
	// authoritative query; it is validating the stale-state cleanup itself.
	m.reconnectGate.Store(true)
	defer m.reconnectGate.Store(false)
	m.mu.Lock()
	m.markExpectedDeactivationLocked()
	m.mu.Unlock()
	flushes := fnc.routeFlushes

	m.handleConnectIndication(mbim.ConnectState{
		SessionID:       dataSessionID,
		ActivationState: mbim.ActivationStateDeactivated,
	})
	waitForTestCondition(t, 2*time.Second, func() bool {
		return connectQueries.Load() >= 2
	}, "expected deactivate was not rechecked after the first query failure")
	waitForTestCondition(t, time.Second, func() bool {
		return !m.IsConnected()
	}, "second failed authoritative query left stale connected state")
	if fnc.routeFlushes <= flushes || disconnected.Load() != 1 {
		t.Fatalf("recheck did not clean state: flushes %d->%d disconnects=%d",
			flushes, fnc.routeFlushes, disconnected.Load())
	}
}

func TestIPConfigurationQueryFailureRetriesOnce(t *testing.T) {
	var ipQueries atomic.Int32
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, service, cid, commandType, _, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectIPConfiguration &&
			commandType == uint32(mbim.CommandTypeQuery) {
			if ipQueries.Add(1) == 2 {
				return mbim.BuildCommandDoneForTest(h.TransactionID, service, cid, nil), true
			}
		}
		return mbim.TestAnswerConnectAndIPv4Config(written)
	})

	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	m.netcfg = fnc
	var changed atomic.Int32
	m.OnIPConfigChanged(func() { changed.Add(1) })
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()
	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	m.mu.Lock()
	initialEpoch := m.dataEpoch
	initialFlushes := fnc.routeFlushes
	m.mu.Unlock()
	m.handleIPConfigurationIndication()
	waitForTestCondition(t, 2*time.Second, func() bool {
		m.mu.Lock()
		running := m.ipConfigRefreshRunning
		m.mu.Unlock()
		return !running && ipQueries.Load() >= 3
	}, "IP configuration retry did not finish")
	if got := ipQueries.Load(); got != 3 {
		t.Fatalf("IP_CONFIGURATION queries = %d, want initial + failed refresh + one retry", got)
	}
	if got := changed.Load(); got != 1 {
		t.Fatalf("IP configuration callbacks = %d, want 1 after successful retry", got)
	}
	if !m.IsConnected() || m.GetPrivateIP() != "10.0.0.5" {
		t.Fatalf("successful retry did not preserve bearer: connected=%v private=%q",
			m.IsConnected(), m.GetPrivateIP())
	}
	m.mu.Lock()
	finalEpoch := m.dataEpoch
	m.mu.Unlock()
	if finalEpoch != initialEpoch || fnc.routeFlushes != initialFlushes {
		t.Fatalf("unchanged IP configuration disrupted bearer: epoch %d->%d flushes %d->%d",
			initialEpoch, finalEpoch, initialFlushes, fnc.routeFlushes)
	}
}

func testIPv4ConfigurationInfo(gateway, dnsServer [4]byte) []byte {
	const fixed = 60
	addrOff := fixed
	gatewayOff := addrOff + 8
	dnsOff := gatewayOff + 4
	info := make([]byte, dnsOff+4)
	info[4] = 0x0f
	info[12] = 1
	info[16] = byte(addrOff)
	info[28] = byte(gatewayOff)
	info[36] = 1
	info[40] = byte(dnsOff)
	info[52], info[53] = 0xdc, 0x05 // 1500, little endian
	info[addrOff] = 24
	copy(info[addrOff+4:], []byte{10, 0, 0, 5})
	copy(info[gatewayOff:], gateway[:])
	copy(info[dnsOff:], dnsServer[:])
	return info
}

func TestIPConfigurationChangeReappliesBearerSnapshot(t *testing.T) {
	var ipQueries atomic.Int32
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, service, cid, commandType, _, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) &&
			cid == mbim.CIDBasicConnectIPConfiguration &&
			commandType == uint32(mbim.CommandTypeQuery) {
			gateway := [4]byte{10, 0, 0, 1}
			dnsServer := [4]byte{8, 8, 8, 8}
			if ipQueries.Add(1) > 1 {
				gateway = [4]byte{10, 0, 0, 9}
				dnsServer = [4]byte{1, 1, 1, 1}
			}
			return mbim.BuildCommandDoneForTest(
				h.TransactionID,
				service,
				cid,
				testIPv4ConfigurationInfo(gateway, dnsServer),
			), true
		}
		return mbim.TestAnswerConnectAndIPv4Config(written)
	})

	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	m.netcfg = fnc
	var changed atomic.Int32
	m.OnIPConfigChanged(func() { changed.Add(1) })
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()
	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	m.mu.Lock()
	initialEpoch := m.dataEpoch
	m.mu.Unlock()
	initialRouteFlushes := fnc.routeFlushes
	initialAddressFlushes := fnc.addressFlushes

	m.handleIPConfigurationIndication()
	waitForTestCondition(t, 2*time.Second, func() bool {
		m.mu.Lock()
		running := m.ipConfigRefreshRunning
		m.mu.Unlock()
		return !running && ipQueries.Load() >= 2
	}, "changed IP configuration refresh did not finish")

	m.mu.Lock()
	finalEpoch := m.dataEpoch
	cached := cloneIPConfiguration(m.appliedIPConfig)
	hasCached := m.hasAppliedIPConfig
	m.mu.Unlock()
	if finalEpoch != initialEpoch+1 {
		t.Fatalf("changed IP configuration epoch = %d, want %d", finalEpoch, initialEpoch+1)
	}
	if fnc.routeFlushes != initialRouteFlushes+1 || fnc.addressFlushes != initialAddressFlushes+1 {
		t.Fatalf("changed snapshot flushes routes/addresses = %d/%d, want %d/%d",
			fnc.routeFlushes, fnc.addressFlushes, initialRouteFlushes+1, initialAddressFlushes+1)
	}
	if fnc.v4gw != "10.0.0.9" || len(fnc.dns) != 1 || fnc.dns[0] != "1.1.1.1" {
		t.Fatalf("changed gateway/DNS not applied: gateway=%q dns=%v", fnc.v4gw, fnc.dns)
	}
	if changed.Load() != 1 {
		t.Fatalf("IP configuration callbacks = %d, want 1", changed.Load())
	}
	wantCached := mbim.IPConfiguration{
		IPv4Address:      "10.0.0.5",
		IPv4PrefixLength: 24,
		IPv4Gateway:      "10.0.0.9",
		IPv4DNS:          []string{"1.1.1.1"},
		IPv4MTU:          1500,
	}
	if !hasCached || !equalIPConfiguration(cached, wantCached) {
		t.Fatalf("cached applied IP configuration = %+v present=%v, want %+v", cached, hasCached, wantCached)
	}
}
