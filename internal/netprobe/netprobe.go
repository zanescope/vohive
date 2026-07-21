package netprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTimeout       = 10 * time.Second
	defaultMaxConcurrent = 8
	maxResponseBytes     = 128
)

var (
	ErrNoURLs              = errors.New("no public IP probe URLs configured")
	ErrNoUsableResult      = errors.New("no public IP probe returned a usable address")
	ErrInterfaceRequired   = errors.New("network interface is required for a bound public IP probe")
	ErrHostCooling         = errors.New("public IP probe host is cooling down")
	sharedProbeCoordinator = NewCoordinator(defaultMaxConcurrent)
)

type Family int

const (
	FamilyAny Family = iota
	FamilyV4
	FamilyV6
)

func (f Family) String() string {
	switch f {
	case FamilyV4:
		return "ipv4"
	case FamilyV6:
		return "ipv6"
	default:
		return "any"
	}
}

// LookupFunc resolves only addresses usable by the requested family. Lookup is
// retained in Config solely for callers that have not migrated to this form.
type LookupFunc func(ctx context.Context, host string, family Family) ([]string, error)

type Config struct {
	Interface string

	// IPv4URLs and IPv6URLs are ordered source lists. URLs is the legacy common
	// list and is used only when the matching family-specific list is empty.
	URLs     []string
	IPv4URLs []string
	IPv6URLs []string

	// Timeout applies to each HTTP request. Coordinator queue time is controlled
	// by the caller context and therefore remains independently cancellable.
	Timeout time.Duration

	LookupFamily LookupFunc
	Lookup       func(ctx context.Context, host string) ([]string, error)

	// Coordinator is normally nil, selecting the process-wide coordinator.
	// Transport exists for deterministic TLS tests. Production callers should
	// leave it nil so interface binding and destination checks cannot be bypassed.
	Coordinator *Coordinator
	Transport   http.RoundTripper
}

type Result struct {
	IP     string
	Source string
}

type Prober struct {
	cfg         Config
	client      *http.Client
	coordinator *Coordinator
}

type familyKey struct{}

func New(cfg Config) *Prober {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	p := &Prober{cfg: cfg, coordinator: cfg.Coordinator}
	if p.coordinator == nil {
		p.coordinator = sharedProbeCoordinator
	}

	transport := cfg.Transport
	if transport == nil {
		transport = &http.Transport{
			DialContext:       p.dialContext,
			DisableKeepAlives: true,
			Proxy:             nil,
		}
	}
	p.client = &http.Client{
		Transport: transport,
		Timeout:   cfg.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return p
}

// Probe preserves the historical empty-string-on-failure API. New code should
// use ProbeResult so the winning source and cancellation errors remain visible.
func (p *Prober) Probe(ctx context.Context, family Family) string {
	result, err := p.ProbeResult(ctx, family)
	if err != nil {
		return ""
	}
	return result.IP
}

// ProbeResult tries sources in configured order, avoiding an all-source burst.
func (p *Prober) ProbeResult(ctx context.Context, family Family) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	urls := p.urlsForFamily(family)
	if len(urls) == 0 {
		return Result{}, ErrNoURLs
	}

	var lastErr error
	for _, target := range urls {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		result, err := p.probeOne(ctx, target, family)
		if err == nil {
			return result, nil
		}
		lastErr = err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if lastErr == nil {
		return Result{}, ErrNoUsableResult
	}
	return Result{}, errors.Join(ErrNoUsableResult, lastErr)
}

func (p *Prober) urlsForFamily(family Family) []string {
	switch family {
	case FamilyV4:
		if len(p.cfg.IPv4URLs) > 0 {
			return p.cfg.IPv4URLs
		}
	case FamilyV6:
		if len(p.cfg.IPv6URLs) > 0 {
			return p.cfg.IPv6URLs
		}
	case FamilyAny:
		if len(p.cfg.URLs) == 0 {
			out := make([]string, 0, len(p.cfg.IPv4URLs)+len(p.cfg.IPv6URLs))
			out = append(out, p.cfg.IPv4URLs...)
			out = append(out, p.cfg.IPv6URLs...)
			return out
		}
	}
	return p.cfg.URLs
}

func (p *Prober) probeOne(ctx context.Context, target string, family Family) (Result, error) {
	u, _, err := parseTargetURL(target)
	source := safeSource(target)
	if err != nil {
		return Result{}, fmt.Errorf("probe %s: invalid target", source)
	}

	release, err := p.coordinator.acquire(ctx, strings.ToLower(u.Hostname()))
	if err != nil {
		return Result{}, err
	}
	defer release()

	requestCtx, cancel := context.WithTimeout(withFamily(ctx, family), p.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, fmt.Errorf("probe %s: cannot build request", source)
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", "VoHive-public-ip-probe")

	resp, err := p.client.Do(req)
	if err != nil {
		if requestCtx.Err() != nil {
			return Result{}, requestCtx.Err()
		}
		return Result{}, fmt.Errorf("probe %s: request failed: %w", source, sanitizedURLError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		p.coordinator.cooldown(strings.ToLower(u.Hostname()), retryAfter(resp.Header.Get("Retry-After"), time.Now()))
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("probe %s: unexpected HTTP status %d", source, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Result{}, fmt.Errorf("probe %s: response read failed", source)
	}
	if len(body) > maxResponseBytes {
		return Result{}, fmt.Errorf("probe %s: response is too large", source)
	}
	addr, err := parseResponseAddress(string(body), family)
	if err != nil {
		return Result{}, fmt.Errorf("probe %s: %w", source, err)
	}
	return Result{IP: addr.String(), Source: source}, nil
}
