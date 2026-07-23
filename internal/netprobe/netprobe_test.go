package netprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tlsProber(server *httptest.Server, coordinator *Coordinator, urls ...string) *Prober {
	return New(Config{
		URLs:        urls,
		Timeout:     time.Second,
		Coordinator: coordinator,
		Transport:   server.Client().Transport,
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestValidateTargetURLCanonicalizesHTTPSSchemeForRequests(t *testing.T) {
	normalized, err := ValidateTargetURL("HTTPS://Probe.Example/ip", FamilyV4)
	if err != nil {
		t.Fatalf("ValidateTargetURL() error = %v", err)
	}
	if normalized != "https://Probe.Example/ip" {
		t.Fatalf("normalized URL = %q", normalized)
	}

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" {
			return nil, fmt.Errorf("unexpected scheme %q", request.URL.Scheme)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("8.8.8.8")),
			Request:    request,
		}, nil
	})
	result, err := New(Config{URLs: []string{normalized}, Transport: transport}).ProbeResult(context.Background(), FamilyV4)
	if err != nil {
		t.Fatalf("ProbeResult() error = %v", err)
	}
	if result.IP != "8.8.8.8" {
		t.Fatalf("ProbeResult() = %+v", result)
	}
}
func TestProbeResultReturnsStrictPlainPublicIPAndRedactedSource(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("8.8.8.8\n"))
	}))
	defer server.Close()

	p := tlsProber(server, NewCoordinator(1), server.URL+"/ip?token=secret")
	result, err := p.ProbeResult(context.Background(), FamilyV4)
	if err != nil {
		t.Fatalf("ProbeResult() error = %v", err)
	}
	if result.IP != "8.8.8.8" {
		t.Fatalf("IP = %q, want 8.8.8.8", result.IP)
	}
	if result.Source != server.URL {
		t.Fatalf("Source = %q, want origin-only source", result.Source)
	}
}

func TestProbeUsesFamilySpecificOrderedSources(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v4-first":
			http.Error(w, "8.8.8.8", http.StatusBadGateway)
		case "/v4-second":
			_, _ = w.Write([]byte("8.8.4.4"))
		case "/v6":
			_, _ = w.Write([]byte("2606:4700:4700::1111"))
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	p := New(Config{
		URLs:        []string{server.URL + "/legacy"},
		IPv4URLs:    []string{server.URL + "/v4-first", server.URL + "/v4-second"},
		IPv6URLs:    []string{server.URL + "/v6"},
		Timeout:     time.Second,
		Coordinator: NewCoordinator(2),
		Transport:   server.Client().Transport,
	})
	if got := p.Probe(context.Background(), FamilyV4); got != "8.8.4.4" {
		t.Fatalf("v4 Probe() = %q, want 8.8.4.4", got)
	}
	if got := p.Probe(context.Background(), FamilyV6); got != "2606:4700:4700::1111" {
		t.Fatalf("v6 Probe() = %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"/v4-first", "/v4-second", "/v6"}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("request order = %v, want %v", paths, want)
	}
}

func TestProbeFallbackSourcesShareCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	callerDeadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("caller context has no deadline")
	}

	var calls atomic.Int32
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		requestDeadline, hasDeadline := request.Context().Deadline()
		if !hasDeadline || !requestDeadline.Equal(callerDeadline) {
			return nil, fmt.Errorf("request deadline = %v, want caller deadline %v", requestDeadline, callerDeadline)
		}
		response := &http.Response{
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    request,
			StatusCode: http.StatusBadGateway,
		}
		if request.URL.Hostname() == "backup.example" {
			response.StatusCode = http.StatusOK
			response.Body = io.NopCloser(strings.NewReader("8.8.4.4"))
		}
		return response, nil
	})
	prober := New(Config{
		URLs:        []string{"https://primary.example/ip", "https://backup.example/ip"},
		Timeout:     10 * time.Second,
		Coordinator: NewCoordinator(1),
		Transport:   transport,
	})
	result, err := prober.ProbeResult(ctx, FamilyV4)
	if err != nil {
		t.Fatalf("ProbeResult() error = %v", err)
	}
	if result.IP != "8.8.4.4" || calls.Load() != 2 {
		t.Fatalf("ProbeResult() = %+v, calls=%d", result, calls.Load())
	}
}
func TestProbeRejectsNon200RedirectAndErrorBody(t *testing.T) {
	var okHits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/ok":
			okHits.Add(1)
			_, _ = w.Write([]byte("8.8.8.8"))
		case "/error":
			http.Error(w, "8.8.8.8", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	for _, path := range []string{"/redirect", "/error"} {
		p := tlsProber(server, NewCoordinator(1), server.URL+path)
		if got := p.Probe(context.Background(), FamilyV4); got != "" {
			t.Fatalf("Probe(%s) = %q, want empty", path, got)
		}
	}
	if got := okHits.Load(); got != 0 {
		t.Fatalf("redirect target hits = %d, want 0", got)
	}
}

func TestProbeRejectsWrongFamilyStructuredAndSpecialAddresses(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		family Family
	}{
		{name: "json", body: `{"ip":"8.8.8.8"}`, family: FamilyV4},
		{name: "wrong-v4", body: "2606:4700:4700::1111", family: FamilyV4},
		{name: "wrong-v6", body: "8.8.8.8", family: FamilyV6},
		{name: "private-v4", body: "10.0.0.1", family: FamilyV4},
		{name: "cgnat", body: "100.64.0.1", family: FamilyV4},
		{name: "loopback", body: "127.0.0.1", family: FamilyV4},
		{name: "link-local-v4", body: "169.254.1.1", family: FamilyV4},
		{name: "documentation-v4", body: "203.0.113.7", family: FamilyV4},
		{name: "as112-v4", body: "192.31.196.1", family: FamilyV4},
		{name: "amt-v4", body: "192.52.193.1", family: FamilyV4},
		{name: "benchmark", body: "198.18.0.1", family: FamilyV4},
		{name: "ula", body: "fd00::1", family: FamilyV6},
		{name: "link-local-v6", body: "fe80::1", family: FamilyV6},
		{name: "documentation-v6", body: "2001:db8::1", family: FamilyV6},
		{name: "nat64", body: "64:ff9b::808:808", family: FamilyV6},
		{name: "as112-v6", body: "2620:4f:8000::1", family: FamilyV6},
		{name: "outside-global-v6", body: "4000::1", family: FamilyV6},
		{name: "mapped", body: "::ffff:8.8.8.8", family: FamilyV6},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			p := tlsProber(server, NewCoordinator(1), server.URL)
			if got := p.Probe(context.Background(), test.family); got != "" {
				t.Fatalf("Probe() = %q, want empty", got)
			}
		})
	}
}

func TestProbeRejectsOversizedBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("8", maxResponseBytes+1)))
	}))
	defer server.Close()
	p := tlsProber(server, NewCoordinator(1), server.URL)
	if got := p.Probe(context.Background(), FamilyV4); got != "" {
		t.Fatalf("Probe() = %q, want empty", got)
	}
}

func TestProbeEmptyURLsReturnsImmediately(t *testing.T) {
	p := New(Config{Timeout: time.Hour})
	started := time.Now()
	_, err := p.ProbeResult(context.Background(), FamilyV4)
	if !errors.Is(err, ErrNoURLs) {
		t.Fatalf("error = %v, want ErrNoURLs", err)
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("empty probe took %v", elapsed)
	}
}

func TestCoordinatorLimitsConcurrencyAndQueueIsCancellable(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		_, _ = w.Write([]byte("8.8.8.8"))
	}))
	defer server.Close()

	p := tlsProber(server, NewCoordinator(1), server.URL)
	firstDone := make(chan error, 1)
	go func() {
		_, err := p.ProbeResult(context.Background(), FamilyV4)
		firstDone <- err
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.ProbeResult(ctx, FamilyV4)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued error = %v, want deadline exceeded", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first probe error = %v", err)
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrency = %d, want 1", got)
	}
}

func TestTooManyRequestsCreatesCancellableHostCooldown(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	p := tlsProber(server, NewCoordinator(1), server.URL)
	if _, err := p.ProbeResult(context.Background(), FamilyV4); err == nil {
		t.Fatal("first ProbeResult() unexpectedly succeeded")
	}
	_, err := p.ProbeResult(context.Background(), FamilyV4)
	if !errors.Is(err, ErrHostCooling) {
		t.Fatalf("cooldown error = %v, want ErrHostCooling", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("handler hits = %d, want 1", got)
	}
}

func TestTooManyRequestsFallsBackWithoutWaitingForPrimaryCooldown(t *testing.T) {
	var primaryHits atomic.Int32
	var backupHits atomic.Int32
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := &http.Response{
			Header:  make(http.Header),
			Body:    http.NoBody,
			Request: request,
		}
		switch request.URL.Hostname() {
		case "primary.example":
			primaryHits.Add(1)
			response.StatusCode = http.StatusTooManyRequests
			response.Header.Set("Retry-After", "60")
		case "backup.example":
			backupHits.Add(1)
			response.StatusCode = http.StatusOK
			response.Body = io.NopCloser(strings.NewReader("8.8.4.4"))
		default:
			return nil, fmt.Errorf("unexpected host %q", request.URL.Hostname())
		}
		return response, nil
	})
	p := New(Config{
		URLs: []string{
			"https://primary.example/probe",
			"https://backup.example/probe",
		},
		Timeout:     time.Second,
		Coordinator: NewCoordinator(1),
		Transport:   transport,
	})

	for attempt := 0; attempt < 2; attempt++ {
		result, err := p.ProbeResult(context.Background(), FamilyV4)
		if err != nil {
			t.Fatalf("ProbeResult() attempt %d error = %v", attempt+1, err)
		}
		if result.IP != "8.8.4.4" || result.Source != "https://backup.example" {
			t.Fatalf("ProbeResult() = %+v, want backup result", result)
		}
	}
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary hits = %d, want 1", got)
	}
	if got := backupHits.Load(); got != 2 {
		t.Fatalf("backup hits = %d, want 2", got)
	}
}

func TestFamilyAwareLookupAndPublicDestinationFiltering(t *testing.T) {
	var gotFamily Family
	p := New(Config{LookupFamily: func(_ context.Context, _ string, family Family) ([]string, error) {
		gotFamily = family
		return []string{"10.0.0.1", "2606:4700:4700::1111", "2606:4700:4700::1111"}, nil
	}})
	addresses, err := p.lookupHost(context.Background(), "probe.example", FamilyV6)
	if err != nil {
		t.Fatalf("lookupHost() error = %v", err)
	}
	filtered := filterDialAddresses(addresses, FamilyV6)
	if gotFamily != FamilyV6 {
		t.Fatalf("lookup family = %v, want v6", gotFamily)
	}
	if fmt.Sprint(filtered) != fmt.Sprint([]string{"2606:4700:4700::1111"}) {
		t.Fatalf("filtered = %v", filtered)
	}
}

func TestLookupCacheCoalescesAndCachesPerFamily(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	cache := NewLookupCache(func(_ context.Context, _ string, _ Family) ([]string, error) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return []string{"8.8.8.8"}, nil
	}, time.Minute)

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			got, err := cache.Lookup(context.Background(), "EXAMPLE.com", FamilyV4)
			if err != nil || fmt.Sprint(got) != "[8.8.8.8]" {
				t.Errorf("Lookup() = %v, %v", got, err)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver calls = %d, want 1", got)
	}
	if _, err := cache.Lookup(context.Background(), "example.COM", FamilyV4); err != nil {
		t.Fatalf("cached Lookup() error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("resolver calls after cache hit = %d, want 1", got)
	}
	cache.Forget("example.com", FamilyV4)
	if _, err := cache.Lookup(context.Background(), "example.com", FamilyV4); err != nil {
		t.Fatalf("Lookup() after Forget error = %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("resolver calls after Forget = %d, want 2", got)
	}
}

func TestIsPublicAddress(t *testing.T) {
	for _, raw := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !IsPublicAddress(netip.MustParseAddr(raw)) {
			t.Fatalf("%s should be public", raw)
		}
	}
}

func TestCoordinatorHostRateWaitDoesNotOccupyGlobalSlot(t *testing.T) {
	coordinator := NewCoordinator(1)
	coordinator.mu.Lock()
	coordinator.nextRequest["busy.example"] = time.Now().Add(500 * time.Millisecond)
	coordinator.mu.Unlock()

	busyCtx, cancelBusy := context.WithCancel(context.Background())
	defer cancelBusy()
	busyDone := make(chan error, 1)
	go func() {
		release, err := coordinator.acquire(busyCtx, "busy.example")
		if err == nil {
			release()
		}
		busyDone <- err
	}()

	time.Sleep(20 * time.Millisecond)
	otherCtx, cancelOther := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelOther()
	release, err := coordinator.acquire(otherCtx, "other.example")
	if err != nil {
		t.Fatalf("different host was blocked by host-rate waiter: %v", err)
	}
	release()

	cancelBusy()
	select {
	case err := <-busyDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("busy host waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("busy host waiter did not stop")
	}
}

func TestCoordinatorSameHostGateWaitIsCancellableWhileGlobalSlotIsFull(t *testing.T) {
	coordinator := NewCoordinator(1)
	blockerRelease, err := coordinator.acquire(context.Background(), "blocker.example")
	if err != nil {
		t.Fatalf("acquire blocker: %v", err)
	}
	blockerReleased := false
	defer func() {
		if !blockerReleased {
			blockerRelease()
		}
	}()

	type acquireResult struct {
		release func()
		err     error
	}
	firstDone := make(chan acquireResult, 1)
	go func() {
		release, acquireErr := coordinator.acquire(context.Background(), "same.example")
		firstDone <- acquireResult{release: release, err: acquireErr}
	}()

	gate := coordinator.hostGate("same.example")
	deadline := time.Now().Add(time.Second)
	for len(gate) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(gate) != 0 {
		t.Fatal("first same-host waiter did not acquire the host gate")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	started := time.Now()
	_, err = coordinator.acquire(ctx, "same.example")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-host queued error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("same-host cancellation took %v", elapsed)
	}

	blockerRelease()
	blockerReleased = true
	select {
	case result := <-firstDone:
		if result.err != nil {
			t.Fatalf("first same-host waiter: %v", result.err)
		}
		result.release()
	case <-time.After(time.Second):
		t.Fatal("first same-host waiter did not acquire after global release")
	}
}
