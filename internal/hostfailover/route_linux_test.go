//go:build linux

package hostfailover

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestSafePromotionMetricIsStrictlyPreferred(t *testing.T) {
	tests := []struct {
		name          string
		primaryMetric int
		maximum       int
		want          int
		wantErr       bool
	}{
		{name: "no primary route", primaryMetric: -1, maximum: 5, want: 5},
		{name: "normal primary", primaryMetric: 100, maximum: 5, want: 5},
		{name: "low primary", primaryMetric: 3, maximum: 5, want: 2},
		{name: "metric one", primaryMetric: 1, maximum: 5, want: 0},
		{name: "unsafe zero primary", primaryMetric: 0, maximum: 5, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := safePromotionMetric(tt.primaryMetric, tt.maximum)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("metric = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPromotedRouteCopiesPathButUsesOwnershipMarker(t *testing.T) {
	base := netlink.Route{
		LinkIndex: 7,
		Scope:     netlink.SCOPE_UNIVERSE,
		Gw:        net.ParseIP("10.0.0.1"),
		Src:       net.ParseIP("10.0.0.2"),
		Priority:  5000,
		Table:     unix.RT_TABLE_MAIN,
		Type:      unix.RTN_UNICAST,
		MTU:       1400,
	}
	got := promotedRoute(base, 9, 5)

	if got.LinkIndex != 9 || got.Priority != 5 {
		t.Fatalf("link/metric = %d/%d, want 9/5", got.LinkIndex, got.Priority)
	}
	if got.Protocol != ownedRouteProtocol {
		t.Fatalf("protocol = %d, want %d", got.Protocol, ownedRouteProtocol)
	}
	if !got.Gw.Equal(base.Gw) || !got.Src.Equal(base.Src) {
		t.Fatalf("path was not copied: gateway=%v source=%v", got.Gw, got.Src)
	}
	if !isOwnedDefaultRoute(got) {
		t.Fatalf("promoted route is not recognized as owned default: %+v", got)
	}
}

func TestOwnedRouteCheckRejectsOtherRoutes(t *testing.T) {
	defaultDst := &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	tests := []netlink.Route{
		{Dst: defaultDst, Protocol: netlink.RouteProtocol(unix.RTPROT_STATIC), Table: unix.RT_TABLE_MAIN, Family: netlink.FAMILY_V4},
		{Dst: &net.IPNet{IP: net.ParseIP("10.0.0.0"), Mask: net.CIDRMask(8, 32)}, Protocol: ownedRouteProtocol, Table: unix.RT_TABLE_MAIN, Family: netlink.FAMILY_V4},
		{Dst: defaultDst, Protocol: ownedRouteProtocol, Table: 100, Family: netlink.FAMILY_V4},
	}
	for _, route := range tests {
		if isOwnedDefaultRoute(route) {
			t.Fatalf("route must not be owned default: %+v", route)
		}
	}
}
