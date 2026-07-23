package mbimcore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zanescope/vohive/pkg/mbim"
)

type fakeRoute struct {
	gateway string
	direct  bool
	ipv6    bool
}

type fakeNetcfg struct {
	iface          string
	v4addr         string
	v6addr         string
	v4gw           string
	v6gw           string
	v4prefix       int
	v6prefix       int
	mtu            int
	dns            []string
	up             bool
	flushed        bool
	addressFlushes int
	routeFlushes   int
	flushErr       error
	routeFlushErr  error
	routes         []fakeRoute
	operations     []string
}

func (f *fakeNetcfg) SetIPv4(iface, addr string, prefix int) error {
	f.iface, f.v4addr, f.v4prefix = iface, addr, prefix
	f.operations = append(f.operations, "set-v4")
	return nil
}

func (f *fakeNetcfg) SetIPv6(iface, addr string, prefix int) error {
	f.iface, f.v6addr, f.v6prefix = iface, addr, prefix
	f.operations = append(f.operations, "set-v6")
	return nil
}

func (f *fakeNetcfg) SetMTU(iface string, mtu int) error {
	f.mtu = mtu
	f.operations = append(f.operations, "set-mtu")
	return nil
}

func (f *fakeNetcfg) BringUp(iface string) error {
	f.up = true
	f.operations = append(f.operations, "up")
	return nil
}

func (f *fakeNetcfg) AddDefaultRoute(iface, gw string) error {
	route := fakeRoute{gateway: gw, ipv6: strings.Contains(gw, ":")}
	f.routes = append(f.routes, route)
	if route.ipv6 {
		f.v6gw = gw
	} else {
		f.v4gw = gw
	}
	f.operations = append(f.operations, "route-gateway")
	return nil
}

func (f *fakeNetcfg) AddDefaultRouteDirect(iface string, ipv6 bool) error {
	f.routes = append(f.routes, fakeRoute{direct: true, ipv6: ipv6})
	f.operations = append(f.operations, "route-direct")
	return nil
}

func (f *fakeNetcfg) SetDNS(dns []string) error {
	f.dns = append([]string(nil), dns...)
	f.operations = append(f.operations, "dns")
	return nil
}

func (f *fakeNetcfg) Flush(iface string) error {
	f.flushed = true
	f.addressFlushes++
	f.operations = append(f.operations, "flush-addresses")
	return f.flushErr
}

func (f *fakeNetcfg) FlushRoutes(iface string) error {
	f.routeFlushes++
	f.operations = append(f.operations, "flush-routes")
	return f.routeFlushErr
}

func TestCleanupActivatedDataSessionPreservesOriginalError(t *testing.T) {
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, service, cid, commandType, info, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectConnect &&
			commandType == uint32(mbim.CommandTypeSet) && len(info) >= 8 &&
			mbim.ReadU32ForTest(info[4:]) == mbim.ActivationCommandDeactivate {
			return mbim.BuildCommandDoneStatusForTest(h.TransactionID, service, cid, 1, nil), true
		}
		return mbim.TestAnswerConnectAndIPv4Config(written)
	})
	d := mbim.NewDevice(tr)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("open device: %v", err)
	}
	defer d.Close()

	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{
		flushErr:      errors.New("address cleanup failed"),
		routeFlushErr: errors.New("route cleanup failed"),
	}
	cause := errors.New("original IP configuration error")
	got := m.cleanupActivatedDataSessionLocked(context.Background(), d, fnc, "wwan0", cause)
	if got != cause {
		t.Fatalf("cleanup returned %v, want original cause %v", got, cause)
	}
	if !fnc.flushed || fnc.routeFlushes != 1 {
		t.Fatalf("best-effort cleanup was skipped: addresses=%v routes=%d", fnc.flushed, fnc.routeFlushes)
	}
}
func TestSetDataConfigStoresValues(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4v6"})
	if m.dataCfg.APN != "internet" || m.dataCfg.Interface != "wwan0" || m.dataCfg.IPVersion != "v4v6" {
		t.Fatalf("dataCfg not stored: %+v", m.dataCfg)
	}
}

func TestApplyIPConfigIPv6GatewayMTUAndDNS(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	ipc := mbim.IPConfiguration{
		IPv6Address:      "2001:db8::5",
		IPv6PrefixLength: 64,
		IPv6Gateway:      "2001:db8::1",
		IPv6DNS:          []string{"2001:4860:4860::8888"},
		IPv6MTU:          1420,
	}
	if err := m.applyIPConfig(fnc, "wwan0", ipc); err != nil {
		t.Fatalf("applyIPConfig: %v", err)
	}
	if fnc.v6addr != "2001:db8::5" || fnc.v6prefix != 64 || fnc.v6gw != "2001:db8::1" {
		t.Fatalf("IPv6 config not applied: %+v", fnc)
	}
	if fnc.mtu != 1420 || len(fnc.dns) != 1 || fnc.dns[0] != "2001:4860:4860::8888" {
		t.Fatalf("IPv6 MTU/DNS not applied: mtu=%d dns=%v", fnc.mtu, fnc.dns)
	}
	if fnc.routeFlushes != 1 || fnc.addressFlushes != 1 {
		t.Fatalf("stale network was not fully flushed: routes=%d addresses=%d", fnc.routeFlushes, fnc.addressFlushes)
	}
	if len(fnc.operations) < 2 || fnc.operations[0] != "flush-routes" || fnc.operations[1] != "flush-addresses" {
		t.Fatalf("flush must precede reconfiguration, operations=%v", fnc.operations)
	}
}

func TestApplyIPConfigIPv6WithoutGatewayUsesDirectRoute(t *testing.T) {
	for _, gateway := range []string{"", "::"} {
		t.Run("gateway="+gateway, func(t *testing.T) {
			m := New("/dev/cdc-wdm0", "auto")
			fnc := &fakeNetcfg{}
			ipc := mbim.IPConfiguration{
				IPv6Address:      "2001:db8::5",
				IPv6PrefixLength: 64,
				IPv6Gateway:      gateway,
				IPv6MTU:          1280,
			}
			if err := m.applyIPConfig(fnc, "wwan0", ipc); err != nil {
				t.Fatalf("applyIPConfig: %v", err)
			}
			if len(fnc.routes) != 1 || !fnc.routes[0].direct || !fnc.routes[0].ipv6 {
				t.Fatalf("IPv6 direct route not added: %+v", fnc.routes)
			}
		})
	}
}

func TestApplyIPConfigRejectsUnsafeIPv6MTU(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	err := m.applyIPConfig(fnc, "wwan0", mbim.IPConfiguration{
		IPv6Address:      "2001:db8::5",
		IPv6PrefixLength: 64,
		IPv6MTU:          1279,
	})
	if err == nil || !strings.Contains(err.Error(), "below 1280") {
		t.Fatalf("applyIPConfig error = %v, want IPv6 MTU validation", err)
	}
	if fnc.routeFlushes != 0 || fnc.addressFlushes != 0 {
		t.Fatalf("invalid snapshot must be rejected before flushing: %+v", fnc)
	}
}

func TestApplyIPConfigRejectsNonBearerAddressesBeforeFlush(t *testing.T) {
	tests := []struct {
		name string
		cfg  mbim.IPConfiguration
	}{
		{name: "IPv4 unspecified", cfg: mbim.IPConfiguration{IPv4Address: "0.0.0.0", IPv4PrefixLength: 24}},
		{name: "IPv4 loopback", cfg: mbim.IPConfiguration{IPv4Address: "127.0.0.1", IPv4PrefixLength: 8}},
		{name: "IPv4 link local", cfg: mbim.IPConfiguration{IPv4Address: "169.254.1.1", IPv4PrefixLength: 16}},
		{name: "IPv4 multicast", cfg: mbim.IPConfiguration{IPv4Address: "224.0.0.1", IPv4PrefixLength: 24}},
		{name: "IPv6 unspecified", cfg: mbim.IPConfiguration{IPv6Address: "::", IPv6PrefixLength: 64}},
		{name: "IPv6 loopback", cfg: mbim.IPConfiguration{IPv6Address: "::1", IPv6PrefixLength: 128}},
		{name: "IPv6 link local", cfg: mbim.IPConfiguration{IPv6Address: "fe80::1", IPv6PrefixLength: 64}},
		{name: "IPv6 multicast", cfg: mbim.IPConfiguration{IPv6Address: "ff02::1", IPv6PrefixLength: 64}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New("/dev/cdc-wdm0", "auto")
			fnc := &fakeNetcfg{}
			err := m.applyIPConfig(fnc, "wwan0", tt.cfg)
			if err == nil || !strings.Contains(err.Error(), "not global unicast") {
				t.Fatalf("applyIPConfig error = %v, want non-bearer address rejection", err)
			}
			if fnc.routeFlushes != 0 || fnc.addressFlushes != 0 {
				t.Fatalf("invalid address was flushed/applied: %+v", fnc)
			}
		})
	}
}

func TestValidateDataAddressAcceptsPrivateAndULA(t *testing.T) {
	for _, tt := range []struct {
		address string
		prefix  uint32
		ipv6    bool
	}{
		{address: "10.0.0.5", prefix: 24},
		{address: "fd00::5", prefix: 64, ipv6: true},
	} {
		if err := validateDataAddress(tt.address, tt.prefix, tt.ipv6); err != nil {
			t.Fatalf("validateDataAddress(%q) rejected valid bearer address: %v", tt.address, err)
		}
	}
}
func TestApplyIPConfigDualStackUsesTwoRoutesAndMinimumMTU(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	ipc := mbim.IPConfiguration{
		IPv4Address:      "10.0.0.5",
		IPv4PrefixLength: 24,
		IPv4Gateway:      "10.0.0.1",
		IPv4DNS:          []string{"8.8.8.8"},
		IPv4MTU:          1500,
		IPv6Address:      "2001:db8::5",
		IPv6PrefixLength: 64,
		IPv6Gateway:      "2001:db8::1",
		IPv6DNS:          []string{"2001:4860:4860::8888"},
		IPv6MTU:          1420,
	}
	if err := m.applyIPConfig(fnc, "wwan0", ipc); err != nil {
		t.Fatalf("applyIPConfig: %v", err)
	}
	if fnc.mtu != 1420 {
		t.Fatalf("MTU = %d, want minimum 1420", fnc.mtu)
	}
	if len(fnc.routes) != 2 || fnc.routes[0].ipv6 || !fnc.routes[1].ipv6 {
		t.Fatalf("dual-stack routes = %+v, want IPv4 then IPv6", fnc.routes)
	}
	if got := strings.Join(fnc.dns, ","); got != "8.8.8.8,2001:4860:4860::8888" {
		t.Fatalf("DNS = %s, want both family snapshots", got)
	}
}

func TestApplyIPConfigGatewayChangeFlushesOldRoute(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	first := mbim.IPConfiguration{IPv4Address: "10.0.0.5", IPv4PrefixLength: 24, IPv4Gateway: "10.0.0.1"}
	second := mbim.IPConfiguration{IPv4Address: "10.0.1.5", IPv4PrefixLength: 24, IPv4Gateway: "10.0.1.1"}
	if err := m.applyIPConfig(fnc, "wwan0", first); err != nil {
		t.Fatalf("first applyIPConfig: %v", err)
	}
	beforeSecond := len(fnc.operations)
	if err := m.applyIPConfig(fnc, "wwan0", second); err != nil {
		t.Fatalf("second applyIPConfig: %v", err)
	}
	if fnc.v4gw != "10.0.1.1" || fnc.routeFlushes != 2 {
		t.Fatalf("gateway refresh did not replace route: gateway=%s flushes=%d", fnc.v4gw, fnc.routeFlushes)
	}
	secondOps := fnc.operations[beforeSecond:]
	if len(secondOps) < 2 || secondOps[0] != "flush-routes" || secondOps[1] != "flush-addresses" {
		t.Fatalf("gateway refresh did not flush first: %v", secondOps)
	}
}

func TestConnectActivatesAndAppliesIPv4(t *testing.T) {
	tr := mbim.NewFakeTransport(mbim.TestAnswerConnectAndIPv4Config)
	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	m.netcfg = fnc
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()

	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !m.IsConnected() {
		t.Fatal("IsConnected = false after Connect")
	}
	if m.GetPrivateIP() != "10.0.0.5" {
		t.Fatalf("GetPrivateIP = %q, want 10.0.0.5", m.GetPrivateIP())
	}
	if fnc.iface != "wwan0" || fnc.v4addr != "10.0.0.5" || fnc.v4prefix != 24 || fnc.v4gw != "10.0.0.1" || !fnc.up {
		t.Fatalf("netcfg not applied correctly: %+v", fnc)
	}
}

func TestConnectTimeoutDefaultMatchesLibmbimConnectTimeout(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	if got := m.connectTimeoutOrDefault(); got != 120*time.Second {
		t.Fatalf("connectTimeoutOrDefault() = %v, want 120s", got)
	}
}

func TestConnectRecoversLeakedSessionOnMaxActivatedContexts(t *testing.T) {
	var activateAttempts int
	var deactivates int
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, err := mbim.DecodeHeaderForTest(written)
		if err != nil {
			return nil, false
		}
		if h.Type == mbim.MessageTypeOpen {
			return mbim.BuildOpenDoneForTest(h.TransactionID), true
		}
		if h.Type != mbim.MessageTypeCommand || len(written) < 48 {
			return nil, false
		}
		var svc mbim.UUID
		copy(svc[:], written[20:36])
		cid := mbim.ReadU32ForTest(written[36:])
		ct := mbim.ReadU32ForTest(written[40:])
		info := written[48:]
		switch {
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectDeviceServiceSubscribeList:
			return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, nil), true
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectRegisterState && ct == uint32(mbim.CommandTypeQuery):
			buf := make([]byte, 52)
			buf[4] = byte(registerStateHome)
			return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, buf), true
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectConnect && ct == uint32(mbim.CommandTypeSet):
			if len(info) < 8 {
				t.Fatalf("CONNECT info too short: %d", len(info))
			}
			switch mbim.ReadU32ForTest(info[4:]) {
			case mbim.ActivationCommandActivate:
				activateAttempts++
				if activateAttempts == 1 {
					return mbim.BuildCommandDoneStatusForTest(h.TransactionID, svc, cid, 0x0d, nil), true
				}
				resp := make([]byte, 36)
				resp[4] = byte(mbim.ActivationStateActivated)
				resp[12] = byte(mbim.ContextIPTypeIPv4)
				copy(resp[16:32], mbim.UUIDContextTypeInternet[:])
				return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, resp), true
			case mbim.ActivationCommandDeactivate:
				deactivates++
				resp := make([]byte, 36)
				resp[4] = byte(mbim.ActivationStateDeactivated)
				resp[12] = byte(mbim.ContextIPTypeDefault)
				copy(resp[16:32], mbim.UUIDContextTypeInternet[:])
				return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, resp), true
			default:
				t.Fatalf("unexpected CONNECT activation command: %d", mbim.ReadU32ForTest(info[4:]))
			}
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectIPConfiguration && ct == uint32(mbim.CommandTypeQuery):
			return mbim.TestAnswerConnectAndIPv4Config(written)
		}
		return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, nil), true
	})

	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()

	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if activateAttempts != 2 {
		t.Fatalf("activateAttempts = %d, want 2", activateAttempts)
	}
	if deactivates != 1 {
		t.Fatalf("deactivates = %d, want 1", deactivates)
	}
	if !m.IsConnected() {
		t.Fatal("should be connected after stale session cleanup and retry")
	}
}

func TestConnectRetriesBusyActivate(t *testing.T) {
	var activateAttempts int
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, err := mbim.DecodeHeaderForTest(written)
		if err != nil {
			return nil, false
		}
		if h.Type == mbim.MessageTypeOpen {
			return mbim.BuildOpenDoneForTest(h.TransactionID), true
		}
		if h.Type != mbim.MessageTypeCommand || len(written) < 48 {
			return nil, false
		}
		var svc mbim.UUID
		copy(svc[:], written[20:36])
		cid := mbim.ReadU32ForTest(written[36:])
		ct := mbim.ReadU32ForTest(written[40:])
		info := written[48:]
		switch {
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectDeviceServiceSubscribeList:
			return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, nil), true
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectRegisterState && ct == uint32(mbim.CommandTypeQuery):
			buf := make([]byte, 52)
			buf[4] = byte(registerStateHome)
			return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, buf), true
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectConnect && ct == uint32(mbim.CommandTypeSet):
			if len(info) < 8 || mbim.ReadU32ForTest(info[4:]) != mbim.ActivationCommandActivate {
				return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, nil), true
			}
			activateAttempts++
			if activateAttempts == 1 {
				return mbim.BuildCommandDoneStatusForTest(h.TransactionID, svc, cid, 0x01, nil), true
			}
			resp := make([]byte, 36)
			resp[4] = byte(mbim.ActivationStateActivated)
			resp[12] = byte(mbim.ContextIPTypeIPv4)
			copy(resp[16:32], mbim.UUIDContextTypeInternet[:])
			return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, resp), true
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectIPConfiguration && ct == uint32(mbim.CommandTypeQuery):
			return mbim.TestAnswerConnectAndIPv4Config(written)
		}
		return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, nil), true
	})

	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	m.activateRetryDelay = time.Millisecond
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()

	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if activateAttempts != 2 {
		t.Fatalf("activateAttempts = %d, want 2", activateAttempts)
	}
	if !m.IsConnected() {
		t.Fatal("should be connected after busy retry")
	}
}

func TestConnectReopensControlPlaneAfterActivateTimeout(t *testing.T) {
	first := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, err := mbim.DecodeHeaderForTest(written)
		if err != nil {
			return nil, false
		}
		if h.Type == mbim.MessageTypeOpen {
			return mbim.BuildOpenDoneForTest(h.TransactionID), true
		}
		if h.Type != mbim.MessageTypeCommand || len(written) < 48 {
			return nil, false
		}
		var svc mbim.UUID
		copy(svc[:], written[20:36])
		cid := mbim.ReadU32ForTest(written[36:])
		ct := mbim.ReadU32ForTest(written[40:])
		switch {
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectDeviceServiceSubscribeList:
			return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, nil), true
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectRegisterState && ct == uint32(mbim.CommandTypeQuery):
			buf := make([]byte, 52)
			buf[4] = byte(registerStateHome)
			return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, buf), true
		case svc.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectConnect && ct == uint32(mbim.CommandTypeSet):
			return nil, false
		}
		return mbim.BuildCommandDoneForTest(h.TransactionID, svc, cid, nil), true
	})
	second := mbim.NewFakeTransport(mbim.TestAnswerConnectAndIPv4Config)

	var dialCalls int
	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	m.connectTimeout = 10 * time.Millisecond
	m.dial = func(mode, path string) (mbim.Transport, error) {
		dialCalls++
		return second, nil
	}
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), first); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()

	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if dialCalls != 1 {
		t.Fatalf("dialCalls = %d, want 1", dialCalls)
	}
	if !m.IsConnected() {
		t.Fatal("should be connected after control-plane reopen")
	}
}

func TestDisconnectIdempotentWhenNotConnected(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	if err := m.Disconnect(); err != nil {
		t.Fatalf("Disconnect on fresh manager should be nil, got %v", err)
	}
}

func TestRotateIPDeactivatesThenReconnects(t *testing.T) {
	tr := mbim.NewFakeTransport(mbim.TestAnswerConnectAndIPv4Config)
	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()
	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := m.RotateIP(); err != nil {
		t.Fatalf("RotateIP: %v", err)
	}
	if !m.IsConnected() {
		t.Fatal("should be reconnected after rotate")
	}
}

func TestPublicIPProbeURLsAreCopiedAndContextCanCancel(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	m.SetDataConfig(DataConfig{Interface: "wwan0", IPVersion: "v4"})
	ipv4 := []string{"https://v4.example/ip"}
	ipv6 := []string{"https://v6.example/ip"}
	m.SetPublicIPProbeURLs(ipv4, ipv6)
	ipv4[0], ipv6[0] = "changed", "changed"

	m.mu.Lock()
	if m.publicIPv4URLs[0] != "https://v4.example/ip" || m.publicIPv6URLs[0] != "https://v6.example/ip" {
		t.Fatalf("probe URL setters retained caller slices: v4=%v v6=%v", m.publicIPv4URLs, m.publicIPv6URLs)
	}
	m.privateIPv4 = "10.0.0.5"
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	v4, v6 := m.GetPublicIPv4AndV6Context(ctx)
	if v4 != "" || v6 != "" {
		t.Fatalf("canceled probe = (%q, %q), want empty", v4, v6)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("canceled probe took %v, want < 1s", elapsed)
	}
}

func TestUnexpectedDeactivationTriggersReconnect(t *testing.T) {
	tr := mbim.NewFakeTransport(mbim.TestAnswerConnectAndIPv4Config)
	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()
	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	m.mu.Lock()
	m.connected = false
	m.mu.Unlock()

	m.handleConnectIndication(mbim.ConnectState{ActivationState: mbim.ActivationStateDeactivated})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.IsConnected() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("did not reconnect after unexpected deactivation")
}

func TestCloseFlushesNetworkWhenConnected(t *testing.T) {
	tr := mbim.NewFakeTransport(mbim.TestAnswerConnectAndIPv4Config)
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

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fnc.flushed {
		t.Fatal("Close should flush netcfg when a data session was connected")
	}
}

func TestCloseDoesNotFlushNeverOwnedInterface(t *testing.T) {
	tr := mbim.NewFakeTransport(mbim.TestAnswerConnectAndIPv4Config)
	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	m.netcfg = fnc
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fnc.flushed || fnc.routeFlushes != 0 {
		t.Fatalf("Close flushed a never-owned interface: addresses=%v routes=%d", fnc.flushed, fnc.routeFlushes)
	}
}

func TestConcurrentConnectCallsAreSerialized(t *testing.T) {
	tr := mbim.NewFakeTransport(mbim.TestAnswerConnectAndIPv4Config)
	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{}
	m.netcfg = fnc
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- m.Connect()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Connect failed: %v", err)
		}
	}
	if !m.IsConnected() {
		t.Fatal("should be connected")
	}
}

func TestConnectFailsWhenNotRegistered(t *testing.T) {
	tr := mbim.NewFakeTransport(mbim.TestAnswerRegistrationSearching)
	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	m.registrationTimeout = 100 * time.Millisecond
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()

	start := time.Now()
	err := m.Connect()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Connect should fail when not registered")
	}
	if !errors.Is(err, ErrNetworkNotRegistered) {
		t.Fatalf("err = %v, want ErrNetworkNotRegistered", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("Connect took %v, want < 1s with registrationTimeout=100ms", elapsed)
	}
}
