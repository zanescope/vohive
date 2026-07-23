package qmicore

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	qmimanager "github.com/zanescope/quectel-qmi-go/pkg/manager"
	"github.com/zanescope/vohive/internal/netprobe"
)

func TestResolveWithBoundDNSMovesToNextServerAfterUDPAndTCPFailure(t *testing.T) {
	var calls []string
	exchange := func(_ context.Context, msg *dns.Msg, server, network string, _ *net.Dialer) (*dns.Msg, error) {
		calls = append(calls, server+"/"+network)
		if server == "first:53" {
			return nil, errors.New(network + " timeout")
		}
		return dnsAResponse(msg, "8.8.8.8"), nil
	}
	ips, err := resolveWithBoundDNSExchange(context.Background(), "probe.example",
		[]string{"first:53", "second:53"}, nil, dns.TypeA, exchange)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"8.8.8.8"}; !reflect.DeepEqual(ips, want) {
		t.Fatalf("IPs = %v, want %v", ips, want)
	}
	if want := []string{"first:53/udp", "first:53/tcp", "second:53/udp"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestResolveWithBoundDNSUsesTCPAfterUDPFailure(t *testing.T) {
	var calls []string
	exchange := func(_ context.Context, msg *dns.Msg, server, network string, _ *net.Dialer) (*dns.Msg, error) {
		calls = append(calls, server+"/"+network)
		if network == "udp" {
			return nil, errors.New("UDP is blocked")
		}
		return dnsAResponse(msg, "8.8.4.4"), nil
	}
	ips, err := resolveWithBoundDNSExchange(context.Background(), "probe.example",
		[]string{"carrier:53"}, nil, dns.TypeA, exchange)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"8.8.4.4"}; !reflect.DeepEqual(ips, want) {
		t.Fatalf("IPs = %v, want %v", ips, want)
	}
	if want := []string{"carrier:53/udp", "carrier:53/tcp"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}
func TestResolveWithBoundDNSRejectsPrivateAnswerAndFallsBack(t *testing.T) {
	var calls []string
	exchange := func(_ context.Context, msg *dns.Msg, server, network string, _ *net.Dialer) (*dns.Msg, error) {
		calls = append(calls, server+"/"+network)
		if server == "carrier:53" {
			return dnsAResponse(msg, "10.0.0.9"), nil
		}
		return dnsAResponse(msg, "8.8.8.8"), nil
	}
	ips, err := resolveWithBoundDNSExchange(context.Background(), "probe.example",
		[]string{"carrier:53", "fallback:53"}, nil, dns.TypeA, exchange)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"8.8.8.8"}; !reflect.DeepEqual(ips, want) {
		t.Fatalf("IPs = %v, want %v", ips, want)
	}
	if want := []string{"carrier:53/udp", "fallback:53/udp"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}
func TestResolveWithBoundDNSUsesTCPForTruncatedUDP(t *testing.T) {
	var calls []string
	exchange := func(_ context.Context, msg *dns.Msg, server, network string, _ *net.Dialer) (*dns.Msg, error) {
		calls = append(calls, server+"/"+network)
		if network == "udp" {
			resp := new(dns.Msg)
			resp.SetReply(msg)
			resp.Truncated = true
			return resp, nil
		}
		return dnsAResponse(msg, "1.1.1.1"), nil
	}
	ips, err := resolveWithBoundDNSExchange(context.Background(), "probe.example",
		[]string{"carrier:53"}, nil, dns.TypeA, exchange)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.1.1.1"}; !reflect.DeepEqual(ips, want) {
		t.Fatalf("IPs = %v, want %v", ips, want)
	}
	if want := []string{"carrier:53/udp", "carrier:53/tcp"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestResolveWithBoundDNSDoesNotTryTCPAfterUDPCancellation(t *testing.T) {
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
			_, err := resolveWithBoundDNSExchange(ctx, "probe.example", []string{"carrier:53"}, nil, dns.TypeA, exchange)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if want := []string{"carrier:53/udp"}; !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls = %v, want %v", calls, want)
			}
		})
	}
}
func TestResolveWithBoundDNSStopsBeforeExchangeWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	exchange := func(context.Context, *dns.Msg, string, string, *net.Dialer) (*dns.Msg, error) {
		calls++
		return nil, errors.New("must not be called")
	}
	_, err := resolveWithBoundDNSExchange(ctx, "probe.example", []string{"carrier:53"}, nil, dns.TypeA, exchange)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("exchange calls = %d, want 0", calls)
	}
}

func TestResolveWithBoundDNSRecordFamilyIsIndependentOfServerFamily(t *testing.T) {
	tests := []struct {
		name, server string
		queryType    uint16
		want         []string
	}{
		{"AAAA over v4 DNS server", "10.0.0.53:53", dns.TypeAAAA, []string{"2606:4700:4700::1111"}},
		{"A over v6 DNS server", "[fd00::53]:53", dns.TypeA, []string{"8.8.8.8"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exchange := func(_ context.Context, msg *dns.Msg, _ string, network string, _ *net.Dialer) (*dns.Msg, error) {
				if network != "udp" {
					return nil, errors.New("unexpected TCP query")
				}
				if tt.queryType == dns.TypeAAAA {
					return dnsAAAAResponse(msg, tt.want[0]), nil
				}
				return dnsAResponse(msg, tt.want[0]), nil
			}
			got, err := resolveWithBoundDNSExchange(context.Background(), "probe.example",
				[]string{tt.server}, nil, tt.queryType, exchange)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOrderedPublicIPDNSServersDecouplesQueryAndTransportFamilies(t *testing.T) {
	v4 := []string{"10.0.0.53:53"}
	v6 := []string{"[fd00::53]:53"}
	tests := []struct {
		name                   string
		family                 netprobe.Family
		v4, v6                 []string
		reachable4, reachable6 bool
		want                   []string
	}{
		{"A prefers v4 then v6 carrier", netprobe.FamilyV4, v4, v6, true, true,
			concatStrings(v4, v6, fallbackPublicIPDNSServersV4, fallbackPublicIPDNSServersV6)},
		{"AAAA prefers v6 then v4 carrier", netprobe.FamilyV6, v4, v6, true, true,
			concatStrings(v6, v4, fallbackPublicIPDNSServersV6, fallbackPublicIPDNSServersV4)},
		{"AAAA uses only v4 carrier DNS", netprobe.FamilyV6, v4, nil, true, false,
			concatStrings(v4, fallbackPublicIPDNSServersV4)},
		{"A uses only v6 carrier DNS", netprobe.FamilyV4, nil, v6, false, true,
			concatStrings(v6, fallbackPublicIPDNSServersV6)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := orderedPublicIPDNSServers(tt.family, tt.v4, tt.v6, tt.reachable4, tt.reachable6)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOrderedPublicIPDNSServersOmitsUnreachableFallbacks(t *testing.T) {
	if got := orderedPublicIPDNSServers(netprobe.FamilyV4, nil, nil, false, false); len(got) != 0 {
		t.Fatalf("no bearer yielded %v", got)
	}
	if got := orderedPublicIPDNSServers(netprobe.FamilyV6, nil, nil, true, false); !reflect.DeepEqual(got, fallbackPublicIPDNSServersV4) {
		t.Fatalf("v4-only bearer yielded %v", got)
	}
	if got := orderedPublicIPDNSServers(netprobe.FamilyV4, nil, nil, false, true); !reflect.DeepEqual(got, fallbackPublicIPDNSServersV6) {
		t.Fatalf("v6-only bearer yielded %v", got)
	}
	v4Domestic := []string{"119.29.29.29:53", "223.5.5.5:53", "223.6.6.6:53"}
	v6Domestic := []string{"[2402:4e00::]:53", "[2402:4e00:1::]:53", "[2400:3200::1]:53", "[2400:3200:baba::1]:53"}
	if !reflect.DeepEqual(fallbackPublicIPDNSServersV4[:len(v4Domestic)], v4Domestic) {
		t.Fatalf("v4 fallback order = %v", fallbackPublicIPDNSServersV4)
	}
	if !reflect.DeepEqual(fallbackPublicIPDNSServersV6[:len(v6Domestic)], v6Domestic) {
		t.Fatalf("v6 fallback order = %v", fallbackPublicIPDNSServersV6)
	}
}

func TestAppendDNSServerForInterfaceValidatesEndpoint(t *testing.T) {
	tests := []struct {
		name, ip, iface string
		want            []string
	}{
		{"private v4", "10.0.0.53", "", []string{"10.0.0.53:53"}},
		{"link-local v6 zone", "fe80::53", "wwan0", []string{"[fe80::53%wwan0]:53"}},
		{"link-local v6 without zone", "fe80::53", "", nil},
		{"unspecified v4", "0.0.0.0", "", nil},
		{"unspecified v6", "::", "", nil},
		{"multicast v4", "224.0.0.251", "", nil},
		{"multicast v6", "ff02::fb", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := appendDNSServerForInterface(nil, net.ParseIP(tt.ip), tt.iface)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBearerAddressMatchesFamilyRejectsUnusableAddresses(t *testing.T) {
	tests := []struct {
		value  string
		family netprobe.Family
		want   bool
	}{
		{"10.0.0.2", netprobe.FamilyV4, true},
		{"2606:4700:4700::1111", netprobe.FamilyV6, true},
		{"10.0.0.2", netprobe.FamilyV6, false},
		{"0.0.0.0", netprobe.FamilyV4, false},
		{"::", netprobe.FamilyV6, false},
		{"127.0.0.1", netprobe.FamilyV4, false},
		{"::1", netprobe.FamilyV6, false},
		{"169.254.1.1", netprobe.FamilyV4, false},
		{"fe80::1", netprobe.FamilyV6, false},
		{"224.0.0.1", netprobe.FamilyV4, false},
		{"ff02::1", netprobe.FamilyV6, false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := bearerAddressMatchesFamily(tt.value, tt.family); got != tt.want {
				t.Fatalf("bearerAddressMatchesFamily(%q, %s) = %v, want %v", tt.value, tt.family, got, tt.want)
			}
		})
	}
}

func TestGetPublicIPv4AndV6ContextUsesActualBearerFamilies(t *testing.T) {
	var mu sync.Mutex
	calls := make(map[netprobe.Family]int)
	manager := &Manager{
		hasIPv4Bearer: func() bool { return true },
		hasIPv6Bearer: func() bool { return true },
		publicIPProbeResult: func(_ context.Context, family netprobe.Family) (netprobe.Result, error) {
			mu.Lock()
			calls[family]++
			mu.Unlock()
			if family == netprobe.FamilyV4 {
				return netprobe.Result{IP: "8.8.4.4"}, nil
			}
			return netprobe.Result{IP: "2606:4700:4700::1111"}, nil
		},
	}
	gotV4, gotV6 := manager.GetPublicIPv4AndV6Context(context.Background())
	if gotV4 != "8.8.4.4" || gotV6 != "2606:4700:4700::1111" {
		t.Fatalf("got (%q, %q)", gotV4, gotV6)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls[netprobe.FamilyV4] != 1 || calls[netprobe.FamilyV6] != 1 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestGetPublicIPv4AndV6ContextSkipsAbsentBearers(t *testing.T) {
	tests := []struct {
		name   string
		v4     bool
		calls  int32
		wantV4 string
	}{
		{"v4 only", true, 1, "1.1.1.1"},
		{"no bearer", false, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			var wrongFamily atomic.Int32
			manager := &Manager{
				hasIPv4Bearer: func() bool { return tt.v4 },
				hasIPv6Bearer: func() bool { return false },
				publicIPProbeResult: func(_ context.Context, family netprobe.Family) (netprobe.Result, error) {
					calls.Add(1)
					if family != netprobe.FamilyV4 {
						wrongFamily.Add(1)
						return netprobe.Result{}, errors.New("unexpected probe family")
					}
					return netprobe.Result{IP: "1.1.1.1"}, nil
				},
			}
			gotV4, gotV6 := manager.GetPublicIPv4AndV6Context(context.Background())
			if gotV4 != tt.wantV4 || gotV6 != "" || calls.Load() != tt.calls || wrongFamily.Load() != 0 {
				t.Fatalf("got (%q, %q), calls=%d, wrong-family=%d", gotV4, gotV6, calls.Load(), wrongFamily.Load())
			}
		})
	}
}

func TestGetPublicIPv4AndV6ContextHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	manager := &Manager{
		hasIPv4Bearer: func() bool { return true },
		hasIPv6Bearer: func() bool { return false },
		publicIPProbeResult: func(ctx context.Context, _ netprobe.Family) (netprobe.Result, error) {
			close(started)
			<-ctx.Done()
			return netprobe.Result{}, ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if v4, v6 := manager.GetPublicIPv4AndV6Context(ctx); v4 != "" || v6 != "" {
			t.Errorf("canceled probe returned (%q, %q)", v4, v6)
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("probe ignored cancellation")
	}
}

func TestPublicIPLookupRequiresBoundInterfaceBeforeResolver(t *testing.T) {
	var calls atomic.Int32
	manager := &Manager{publicIPLookup: func(context.Context, string) ([]string, error) {
		calls.Add(1)
		return []string{"8.8.8.8"}, nil
	}}

	_, err := manager.lookupPublicIPHostFamily(context.Background(), "probe.example", netprobe.FamilyV4)
	if !errors.Is(err, netprobe.ErrInterfaceRequired) {
		t.Fatalf("lookup error = %v, want ErrInterfaceRequired", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("unbound lookup called resolver %d times", got)
	}
}
func TestPublicIPLookupCacheResetsOnNetworkChange(t *testing.T) {
	var calls atomic.Int32
	manager := &Manager{publicIPLookup: func(context.Context, string) ([]string, error) {
		calls.Add(1)
		return []string{"8.8.8.8"}, nil
	}}
	manager.cfg.Interface = "wwan0"
	manager.resetPublicIPLookupCache()
	for i := 0; i < 2; i++ {
		ips, err := manager.lookupPublicIPHostFamily(context.Background(), "probe.example", netprobe.FamilyV4)
		if err != nil || !reflect.DeepEqual(ips, []string{"8.8.8.8"}) {
			t.Fatalf("lookup %d = %v, %v", i, ips, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls before reset = %d", calls.Load())
	}
	manager.dispatchNetworkEvent(qmimanager.Event{Type: qmimanager.EventIPChanged})
	if _, err := manager.lookupPublicIPHostFamily(context.Background(), "probe.example", netprobe.FamilyV4); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls after reset = %d", calls.Load())
	}
}

func dnsAResponse(query *dns.Msg, address string) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(query)
	resp.Answer = append(resp.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
		A:   net.ParseIP(address).To4(),
	})
	return resp
}

func dnsAAAAResponse(query *dns.Msg, address string) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(query)
	resp.Answer = append(resp.Answer, &dns.AAAA{
		Hdr:  dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 30},
		AAAA: net.ParseIP(address),
	})
	return resp
}

func concatStrings(groups ...[]string) []string {
	var out []string
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}
