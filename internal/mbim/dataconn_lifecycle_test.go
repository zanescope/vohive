package mbimcore

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/zanescope/vohive/internal/netprobe"
	"github.com/zanescope/vohive/pkg/mbim"
)

func dataTestCommand(written []byte) (mbim.TestHeader, mbim.UUID, uint32, uint32, []byte, bool) {
	h, err := mbim.DecodeHeaderForTest(written)
	if err != nil || h.Type != mbim.MessageTypeCommand || len(written) < 48 {
		return mbim.TestHeader{}, mbim.UUID{}, 0, 0, nil, false
	}
	var service mbim.UUID
	copy(service[:], written[20:36])
	return h, service, mbim.ReadU32ForTest(written[36:]), mbim.ReadU32ForTest(written[40:]), written[48:], true
}

func waitForTestCondition(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(message)
}

func TestCloseGateRejectsConnectAlreadyWaitingForDataLock(t *testing.T) {
	var activates atomic.Int32
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		_, service, cid, commandType, info, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectConnect &&
			commandType == uint32(mbim.CommandTypeSet) && len(info) >= 8 &&
			mbim.ReadU32ForTest(info[4:]) == mbim.ActivationCommandActivate {
			activates.Add(1)
		}
		return mbim.TestAnswerConnectAndIPv4Config(written)
	})

	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}

	m.dataMu.Lock()
	connectStarted := make(chan struct{})
	connectDone := make(chan error, 1)
	go func() {
		close(connectStarted)
		connectDone <- m.Connect()
	}()
	<-connectStarted

	closeDone := make(chan error, 1)
	go func() { closeDone <- m.Close() }()
	waitForTestCondition(t, time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.dataStopRequested
	}, "Close did not install its connect-stop gate")
	m.dataMu.Unlock()

	if err := <-connectDone; err == nil {
		t.Fatal("Connect waiting at Close start unexpectedly succeeded")
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := activates.Load(); got != 0 {
		t.Fatalf("activate commands = %d, want 0 after Close gate", got)
	}
}

func TestDisconnectCancelsEnsureRegisteredSleep(t *testing.T) {
	var registerQueries atomic.Int32
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		_, service, cid, commandType, _, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectRegisterState &&
			commandType == uint32(mbim.CommandTypeQuery) {
			registerQueries.Add(1)
		}
		return mbim.TestAnswerRegistrationSearching(written)
	})

	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	m.registrationTimeout = 10 * time.Second
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()

	connectDone := make(chan error, 1)
	go func() { connectDone <- m.Connect() }()
	waitForTestCondition(t, time.Second, func() bool {
		return registerQueries.Load() > 0
	}, "Connect never entered registration polling")

	start := time.Now()
	if err := m.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Disconnect took %v while registration poll slept", elapsed)
	}
	if err := <-connectDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect error = %v, want context cancellation", err)
	}
}

func TestDelayedExpectedDeactivateDoesNotClearRotatedBearerOnQueryError(t *testing.T) {
	var connectQueries atomic.Int32
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		h, service, cid, commandType, _, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectConnect &&
			commandType == uint32(mbim.CommandTypeQuery) {
			connectQueries.Add(1)
			// A transient malformed/empty response models a lost authoritative
			// query while a delayed indication from the old bearer arrives.
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
	if err := m.RotateIP(); err != nil {
		t.Fatalf("RotateIP: %v", err)
	}

	m.mu.Lock()
	epoch := m.dataEpoch
	m.mu.Unlock()
	flushes := fnc.routeFlushes
	disconnects := disconnected.Load()

	m.handleConnectIndication(mbim.ConnectState{
		SessionID:       dataSessionID,
		ActivationState: mbim.ActivationStateDeactivated,
	})
	waitForTestCondition(t, time.Second, func() bool {
		return connectQueries.Load() == 1
	}, "delayed deactivate did not trigger authoritative CONNECT query")
	time.Sleep(50 * time.Millisecond)

	m.mu.Lock()
	gotEpoch := m.dataEpoch
	privateIPv4 := m.privateIPv4
	m.mu.Unlock()
	if !m.IsConnected() || privateIPv4 != "10.0.0.5" {
		t.Fatalf("rotated bearer was cleared: connected=%v privateIPv4=%q", m.IsConnected(), privateIPv4)
	}
	if gotEpoch != epoch || fnc.routeFlushes != flushes || disconnected.Load() != disconnects {
		t.Fatalf("delayed event mutated bearer: epoch %d->%d flushes %d->%d disconnects %d->%d",
			epoch, gotEpoch, flushes, fnc.routeFlushes, disconnects, disconnected.Load())
	}
}

func TestIPConfigurationBurstCoalescesToOnePendingRefresh(t *testing.T) {
	var ipQueries atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	tr := mbim.NewFakeTransport(func(written []byte) ([]byte, bool) {
		_, service, cid, commandType, _, ok := dataTestCommand(written)
		if ok && service.Equal(mbim.UUIDBasicConnect) && cid == mbim.CIDBasicConnectIPConfiguration &&
			commandType == uint32(mbim.CommandTypeQuery) {
			if ipQueries.Add(1) == 2 {
				close(refreshStarted)
				<-releaseRefresh
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
	defer m.Close()
	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	m.handleIPConfigurationIndication()
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("first authoritative refresh did not start")
	}
	for i := 0; i < 50; i++ {
		m.handleIPConfigurationIndication()
	}
	close(releaseRefresh)

	waitForTestCondition(t, 2*time.Second, func() bool {
		m.mu.Lock()
		running := m.ipConfigRefreshRunning
		m.mu.Unlock()
		return !running && ipQueries.Load() >= 3
	}, "coalesced IP configuration refreshes did not finish")
	if got := ipQueries.Load(); got != 3 {
		t.Fatalf("IP_CONFIGURATION queries = %d, want initial connect + 2 coalesced refreshes", got)
	}
}

func TestCloseRunsDataCallbackOutsideManagerLocks(t *testing.T) {
	tr := mbim.NewFakeTransport(mbim.TestAnswerConnectAndIPv4Config)
	m := New("/dev/cdc-wdm0", "auto")
	m.netcfg = &fakeNetcfg{}
	m.SetDataConfig(DataConfig{APN: "internet", Interface: "wwan0", IPVersion: "v4"})
	if err := m.openWithTransport(context.Background(), tr); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := m.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	callbackDone := make(chan error, 1)
	m.OnDataDisconnected(func() {
		// Disconnect takes both lifecycle locks. This would deadlock if Close
		// invoked the callback while holding either of them.
		callbackDone <- m.Disconnect()
	})
	closeDone := make(chan error, 1)
	go func() { closeDone <- m.Close() }()

	select {
	case err := <-callbackDone:
		if err != nil {
			t.Fatalf("callback reentrant Disconnect: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close data callback deadlocked")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after callback")
	}
}

func TestLookupPublicIPHostCrossesCarrierDNSTransportFamily(t *testing.T) {
	tests := []struct {
		name        string
		family      netprobe.Family
		privateIPv4 string
		privateIPv6 string
		ipv4DNS     []string
		ipv6DNS     []string
		wantFirst   string
		wantSecond  string
		forbidden   string
		answer      string
	}{
		{
			name:        "AAAA over IPv4 carrier DNS",
			family:      netprobe.FamilyV6,
			privateIPv4: "10.0.0.5",
			ipv4DNS:     []string{"10.0.0.53"},
			wantFirst:   "10.0.0.53:53",
			wantSecond:  "119.29.29.29:53",
			forbidden:   "2402:4e00",
			answer:      "2606:4700:4700::1001",
		},
		{
			name:        "A over IPv6 carrier DNS",
			family:      netprobe.FamilyV4,
			privateIPv6: "2001:db8::5",
			ipv6DNS:     []string{"2001:db8::53"},
			wantFirst:   "[2001:db8::53]:53",
			wantSecond:  "[2402:4e00::]:53",
			forbidden:   "119.29.29.29",
			answer:      "8.8.4.4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New("/dev/cdc-wdm0", "auto")
			m.SetDataConfig(DataConfig{Interface: "wwan0"})
			m.mu.Lock()
			m.privateIPv4 = tt.privateIPv4
			m.privateIPv6 = tt.privateIPv6
			m.ipv4DNS = append([]string(nil), tt.ipv4DNS...)
			m.ipv6DNS = append([]string(nil), tt.ipv6DNS...)
			m.mu.Unlock()

			var queriedServers []string
			query := func(_ context.Context, _ string, family netprobe.Family, servers []string, _ *net.Dialer) ([]string, error) {
				if family != tt.family {
					t.Fatalf("query family = %s, want %s", family, tt.family)
				}
				queriedServers = append([]string(nil), servers...)
				return []string{tt.answer}, nil
			}
			got, err := m.lookupPublicIPHostWithQuery(context.Background(), "probe.example", tt.family, query)
			if err != nil {
				t.Fatalf("lookupPublicIPHostWithQuery: %v", err)
			}
			if len(got) != 1 || got[0] != tt.answer {
				t.Fatalf("answer = %v, want %s", got, tt.answer)
			}
			if len(queriedServers) < 2 || queriedServers[0] != tt.wantFirst || queriedServers[1] != tt.wantSecond {
				t.Fatalf("DNS order = %v, want first %q then %q", queriedServers, tt.wantFirst, tt.wantSecond)
			}
			if strings.Contains(strings.Join(queriedServers, ","), tt.forbidden) {
				t.Fatalf("DNS list %v includes fallback for unreachable bearer family %q", queriedServers, tt.forbidden)
			}
		})
	}
}

func TestOrderedDataDNSServerEndpointsCapsCarrierBudgetBeforeFallback(t *testing.T) {
	got := orderedDataDNSServerEndpoints(
		netprobe.FamilyV4,
		[]string{"10.0.0.53", "10.0.0.54"},
		[]string{"2001:db8::53", "2001:db8::54"},
		true,
		true,
		"wwan0",
	)
	wantPrefix := []string{
		"10.0.0.53:53",
		"10.0.0.54:53",
		"119.29.29.29:53",
		"223.5.5.5:53",
	}
	if len(got) < len(wantPrefix) {
		t.Fatalf("DNS order = %v, want prefix %v", got, wantPrefix)
	}
	for index, want := range wantPrefix {
		if got[index] != want {
			t.Fatalf("DNS order[%d] = %q, want %q; full order=%v", index, got[index], want, got)
		}
	}
	for _, droppedCarrier := range []string{"[2001:db8::53]:53", "[2001:db8::54]:53"} {
		for _, endpoint := range got {
			if endpoint == droppedCarrier {
				t.Fatalf("carrier DNS %q escaped the total pre-fallback limit: %v", droppedCarrier, got)
			}
		}
	}
}
func TestQueryDataDNSBoundsUDPAndTCPFallbackWithinPerServerBudget(t *testing.T) {
	blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen blackhole UDP: %v", err)
	}
	t.Cleanup(func() { _ = blackhole.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			if _, _, err := blackhole.ReadFrom(buf); err != nil {
				return
			}
		}
	}()

	host, port, err := net.SplitHostPort(blackhole.LocalAddr().String())
	if err != nil {
		t.Fatalf("split blackhole address: %v", err)
	}
	tcpListener, err := net.Listen("tcp4", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("listen blackhole TCP tracker: %v", err)
	}
	t.Cleanup(func() { _ = tcpListener.Close() })
	var tcpAttempts atomic.Int32
	go func() {
		for {
			conn, err := tcpListener.Accept()
			if err != nil {
				return
			}
			tcpAttempts.Add(1)
			_ = conn.Close()
		}
	}()

	goodPacket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen good DNS: %v", err)
	}
	server := &dns.Server{
		PacketConn: goodPacket,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
			response := new(dns.Msg)
			response.SetReply(request)
			if len(request.Question) > 0 {
				response.Answer = []dns.RR{&dns.A{
					Hdr: dns.RR_Header{
						Name:   request.Question[0].Name,
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    30,
					},
					A: net.ParseIP("8.8.4.4").To4(),
				}}
			}
			_ = w.WriteMsg(response)
		}),
	}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	got, err := queryDataDNS(ctx, "probe.example", netprobe.FamilyV4,
		[]string{blackhole.LocalAddr().String(), goodPacket.LocalAddr().String()},
		&net.Dialer{Timeout: 1200 * time.Millisecond})
	if err != nil {
		t.Fatalf("queryDataDNS: %v", err)
	}
	if len(got) != 1 || got[0] != "8.8.4.4" {
		t.Fatalf("queryDataDNS = %v, want 8.8.4.4", got)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("fallback DNS took %v, want bounded per-server timeout", elapsed)
	}
	if got := tcpAttempts.Load(); got != 1 {
		t.Fatalf("TCP fallback connections after UDP timeout = %d, want 1", got)
	}
}

func TestQueryDataDNSFallsBackToTCPAfterUDPBlackhole(t *testing.T) {
	blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen UDP blackhole: %v", err)
	}
	t.Cleanup(func() { _ = blackhole.Close() })
	go func() {
		buffer := make([]byte, 512)
		for {
			if _, _, err := blackhole.ReadFrom(buffer); err != nil {
				return
			}
		}
	}()

	host, port, err := net.SplitHostPort(blackhole.LocalAddr().String())
	if err != nil {
		t.Fatalf("split UDP blackhole address: %v", err)
	}
	tcpListener, err := net.Listen("tcp4", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("listen TCP DNS fallback: %v", err)
	}
	var tcpQueries atomic.Int32
	tcpServer := &dns.Server{
		Listener: tcpListener,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
			tcpQueries.Add(1)
			response := new(dns.Msg)
			response.SetReply(request)
			response.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{
					Name: request.Question[0].Name, Rrtype: dns.TypeA,
					Class: dns.ClassINET, Ttl: 30,
				},
				A: net.ParseIP("8.8.4.4").To4(),
			}}
			_ = w.WriteMsg(response)
		}),
	}
	go func() { _ = tcpServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = tcpServer.Shutdown() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := queryDataDNS(ctx, "probe.example", netprobe.FamilyV4,
		[]string{blackhole.LocalAddr().String()}, &net.Dialer{Timeout: 1200 * time.Millisecond})
	if err != nil {
		t.Fatalf("queryDataDNS UDP-to-TCP fallback: %v", err)
	}
	if len(got) != 1 || got[0] != "8.8.4.4" || tcpQueries.Load() != 1 {
		t.Fatalf("UDP-to-TCP fallback = %v tcpQueries=%d, want [8.8.4.4]/1", got, tcpQueries.Load())
	}
}

func TestQueryDataDNSParentCancellationSkipsTCPFallback(t *testing.T) {
	blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen UDP blackhole: %v", err)
	}
	t.Cleanup(func() { _ = blackhole.Close() })
	go func() {
		buffer := make([]byte, 512)
		for {
			if _, _, err := blackhole.ReadFrom(buffer); err != nil {
				return
			}
		}
	}()
	host, port, err := net.SplitHostPort(blackhole.LocalAddr().String())
	if err != nil {
		t.Fatalf("split UDP blackhole address: %v", err)
	}
	tcpListener, err := net.Listen("tcp4", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("listen TCP tracker: %v", err)
	}
	t.Cleanup(func() { _ = tcpListener.Close() })
	var tcpAttempts atomic.Int32
	go func() {
		for {
			conn, err := tcpListener.Accept()
			if err != nil {
				return
			}
			tcpAttempts.Add(1)
			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = queryDataDNS(ctx, "probe.example", netprobe.FamilyV4,
		[]string{blackhole.LocalAddr().String()}, &net.Dialer{Timeout: 1200 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queryDataDNS cancellation error = %v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("canceled DNS query took %v", elapsed)
	}
	if got := tcpAttempts.Load(); got != 0 {
		t.Fatalf("TCP attempts after parent cancellation = %d, want 0", got)
	}
}

func TestQueryDataDNSDoesNotTryTCPAfterUDPCancellation(t *testing.T) {
	tests := []struct {
		name     string
		response func(*dns.Msg) (*dns.Msg, error)
	}{
		{name: "transport error", response: func(*dns.Msg) (*dns.Msg, error) {
			return nil, errors.New("UDP failed")
		}},
		{name: "empty response", response: func(*dns.Msg) (*dns.Msg, error) {
			return nil, nil
		}},
		{name: "truncated response", response: func(msg *dns.Msg) (*dns.Msg, error) {
			response := new(dns.Msg)
			response.SetReply(msg)
			response.Truncated = true
			return response, nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var calls []string
			exchange := func(_ context.Context, msg *dns.Msg, server, network string, _ *net.Dialer) (*dns.Msg, error) {
				calls = append(calls, server+"/"+network)
				cancel()
				return tt.response(msg)
			}
			_, err := queryDataDNSWithExchange(ctx, "probe.example", netprobe.FamilyV4,
				[]string{"carrier:53"}, nil, exchange)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if len(calls) != 1 || calls[0] != "carrier:53/udp" {
				t.Fatalf("calls = %v, want only UDP", calls)
			}
		})
	}
}

func TestQueryDataDNSSkipsNonPublicAnswerAndUsesNextServer(t *testing.T) {
	startServer := func(answer string, calls *atomic.Int32) (net.PacketConn, *dns.Server) {
		t.Helper()
		packet, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen DNS server: %v", err)
		}
		server := &dns.Server{
			PacketConn: packet,
			Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
				calls.Add(1)
				response := new(dns.Msg)
				response.SetReply(request)
				if len(request.Question) > 0 {
					response.Answer = []dns.RR{&dns.A{
						Hdr: dns.RR_Header{
							Name:   request.Question[0].Name,
							Rrtype: dns.TypeA,
							Class:  dns.ClassINET,
							Ttl:    30,
						},
						A: net.ParseIP(answer).To4(),
					}}
				}
				_ = w.WriteMsg(response)
			}),
		}
		go func() { _ = server.ActivateAndServe() }()
		t.Cleanup(func() { _ = server.Shutdown() })
		return packet, server
	}

	var privateCalls, publicCalls atomic.Int32
	privatePacket, _ := startServer("10.10.10.10", &privateCalls)
	publicPacket, _ := startServer("8.8.4.4", &publicCalls)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := queryDataDNS(ctx, "probe.example", netprobe.FamilyV4,
		[]string{privatePacket.LocalAddr().String(), publicPacket.LocalAddr().String()},
		&net.Dialer{Timeout: 1200 * time.Millisecond})
	if err != nil {
		t.Fatalf("queryDataDNS: %v", err)
	}
	if len(got) != 1 || got[0] != "8.8.4.4" {
		t.Fatalf("queryDataDNS = %v, want public fallback answer 8.8.4.4", got)
	}
	if privateCalls.Load() != 1 || publicCalls.Load() != 1 {
		t.Fatalf("DNS calls private/public = %d/%d, want 1/1", privateCalls.Load(), publicCalls.Load())
	}
}
func TestDNSServerEndpointsRejectUnusablePlaceholders(t *testing.T) {
	got := dnsServerEndpoints([]string{
		"0.0.0.0", "::", "127.0.0.1", "::1", "255.255.255.255",
		"224.0.0.251", "ff02::fb", "10.0.0.53", "fe80::1",
	}, "wwan0")
	want := []string{"10.0.0.53:53", "[fe80::1%wwan0]:53"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dnsServerEndpoints = %v, want %v", got, want)
	}
}

func TestApplyIPv6ConfigRaisesMTUBeforeAddress(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	fnc := &fakeNetcfg{mtu: 1200}
	err := m.applyIPConfig(fnc, "wwan0", mbim.IPConfiguration{
		IPv6Address:      "2001:db8::5",
		IPv6PrefixLength: 64,
		IPv6Gateway:      "2001:db8::1",
		// A zero modem MTU must not inherit a previous IPv4-only MTU below
		// IPv6's minimum link MTU.
		IPv6MTU: 0,
	})
	if err != nil {
		t.Fatalf("applyIPConfig: %v", err)
	}
	if fnc.mtu != 1280 {
		t.Fatalf("IPv6 default MTU = %d, want 1280", fnc.mtu)
	}
	mtuIndex, v6Index := -1, -1
	for i, operation := range fnc.operations {
		switch operation {
		case "set-mtu":
			mtuIndex = i
		case "set-v6":
			v6Index = i
		}
	}
	if mtuIndex < 0 || v6Index < 0 || mtuIndex >= v6Index {
		t.Fatalf("operation order = %v, want MTU before IPv6 address", fnc.operations)
	}
}

func TestLookupPublicIPHostRequiresInterfaceBeforeDNSQuery(t *testing.T) {
	m := New("/dev/cdc-wdm0", "auto")
	m.mu.Lock()
	m.privateIPv4 = "10.0.0.5"
	m.ipv4DNS = []string{"10.0.0.53"}
	m.mu.Unlock()

	var queryCalls atomic.Int32
	query := func(context.Context, string, netprobe.Family, []string, *net.Dialer) ([]string, error) {
		queryCalls.Add(1)
		return []string{"8.8.4.4"}, nil
	}
	_, err := m.lookupPublicIPHostWithQuery(context.Background(), "probe.example", netprobe.FamilyV4, query)
	if !errors.Is(err, netprobe.ErrInterfaceRequired) {
		t.Fatalf("hostname lookup error = %v, want ErrInterfaceRequired", err)
	}
	if got := queryCalls.Load(); got != 0 {
		t.Fatalf("DNS query calls without interface = %d, want 0", got)
	}

	got, err := m.lookupPublicIPHostWithQuery(context.Background(), "8.8.4.4", netprobe.FamilyV4, query)
	if err != nil || len(got) != 1 || got[0] != "8.8.4.4" {
		t.Fatalf("IP literal lookup = %v, %v; want literal without interface", got, err)
	}
	if got := queryCalls.Load(); got != 0 {
		t.Fatalf("IP literal unexpectedly queried DNS %d times", got)
	}
}
func TestBearerAddressMatchesFamily(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		family netprobe.Family
		want   bool
	}{
		{name: "RFC1918 IPv4", value: "10.0.0.5", family: netprobe.FamilyV4, want: true},
		{name: "CGNAT IPv4", value: "100.64.0.5", family: netprobe.FamilyV4, want: true},
		{name: "ULA IPv6", value: "fd00::5", family: netprobe.FamilyV6, want: true},
		{name: "IPv4 wrong family", value: "10.0.0.5", family: netprobe.FamilyV6},
		{name: "IPv6 wrong family", value: "fd00::5", family: netprobe.FamilyV4},
		{name: "unspecified IPv4", value: "0.0.0.0", family: netprobe.FamilyV4},
		{name: "unspecified IPv6", value: "::", family: netprobe.FamilyV6},
		{name: "loopback", value: "127.0.0.1", family: netprobe.FamilyV4},
		{name: "link local", value: "fe80::1", family: netprobe.FamilyV6},
		{name: "multicast", value: "ff02::1", family: netprobe.FamilyV6},
		{name: "malformed", value: "not-an-ip", family: netprobe.FamilyV4},
		{name: "empty", family: netprobe.FamilyV4},
		{name: "unsupported family", value: "10.0.0.5", family: netprobe.FamilyAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bearerAddressMatchesFamily(tt.value, tt.family); got != tt.want {
				t.Fatalf("bearerAddressMatchesFamily(%q, %s) = %v, want %v",
					tt.value, tt.family, got, tt.want)
			}
		})
	}
}
