package netprobe

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

func (p *Prober) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	dialer, err := platformBoundDialer(strings.TrimSpace(p.cfg.Interface), p.cfg.Timeout)
	if err != nil {
		return nil, err
	}
	family := familyFromContext(ctx)
	ips, err := p.lookupHost(ctx, host, family)
	if err != nil {
		return nil, err
	}
	ips = filterDialAddresses(ips, family)
	if len(ips) == 0 {
		return nil, fmt.Errorf("host %s: no public address for %s", host, family)
	}

	var lastErr error
	for _, ip := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("host %s: no dialable address", host)
	}
	return nil, lastErr
}

func (p *Prober) lookupHost(ctx context.Context, host string, family Family) ([]string, error) {
	if addr, err := netip.ParseAddr(strings.TrimSpace(host)); err == nil {
		return []string{addr.String()}, nil
	}
	if p.cfg.LookupFamily != nil {
		return p.cfg.LookupFamily(ctx, host, family)
	}
	if p.cfg.Lookup != nil {
		return p.cfg.Lookup(ctx, host)
	}
	network := "ip"
	if family == FamilyV4 {
		network = "ip4"
	} else if family == FamilyV6 {
		network = "ip6"
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, network, host)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out, nil
}

func withFamily(ctx context.Context, family Family) context.Context {
	return context.WithValue(ctx, familyKey{}, family)
}

func familyFromContext(ctx context.Context) Family {
	if family, ok := ctx.Value(familyKey{}).(Family); ok {
		return family
	}
	return FamilyAny
}

func filterDialAddresses(ips []string, family Family) []string {
	out := make([]string, 0, len(ips))
	seen := make(map[netip.Addr]struct{}, len(ips))
	for _, raw := range ips {
		addr, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil || !IsPublicAddress(addr) || !matchesFamily(addr, family) {
			continue
		}
		if _, exists := seen[addr]; exists {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, addr.String())
	}
	return out
}
