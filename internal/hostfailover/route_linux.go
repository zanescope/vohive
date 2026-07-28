//go:build linux

package hostfailover

import (
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// A private protocol value lets VoHive remove only routes it owns after a
// restart or crash. It must not reuse RTPROT_STATIC because other managers use
// that value.
const ownedRouteProtocol netlink.RouteProtocol = 186

type linuxRouteManager struct{}

func NewRouteManager() (RouteManager, error) {
	return &linuxRouteManager{}, nil
}

func (m *linuxRouteManager) Reconcile() error {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	var errs []error
	for i := range routes {
		route := routes[i]
		if !isOwnedDefaultRoute(route) {
			continue
		}
		if err := netlink.RouteDel(&route); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.ENOENT) {
			errs = append(errs, fmt.Errorf("delete stale route %s: %w", route.String(), err))
		}
	}
	return errors.Join(errs...)
}

func (m *linuxRouteManager) Activate(primaryInterface, candidateInterface string, maximumMetric int) (RouteLease, error) {
	primary, err := netlink.LinkByName(primaryInterface)
	if err != nil {
		return RouteLease{}, fmt.Errorf("find primary interface %q: %w", primaryInterface, err)
	}
	candidate, err := netlink.LinkByName(candidateInterface)
	if err != nil {
		return RouteLease{}, fmt.Errorf("find candidate interface %q: %w", candidateInterface, err)
	}
	if primary.Attrs().Index == candidate.Attrs().Index {
		return RouteLease{}, fmt.Errorf("primary and candidate interfaces must be different")
	}

	base, err := bestDefaultRoute(candidate)
	if err != nil {
		return RouteLease{}, err
	}
	metric, err := promotionMetric(primary, maximumMetric)
	if err != nil {
		return RouteLease{}, err
	}

	route := promotedRoute(base, candidate.Attrs().Index, metric)
	if err := netlink.RouteAdd(&route); err != nil {
		return RouteLease{}, fmt.Errorf("add promoted default route: %w", err)
	}

	selected, err := netlink.RouteGet(net.ParseIP("1.1.1.1"))
	if err != nil {
		_ = netlink.RouteDel(&route)
		return RouteLease{}, fmt.Errorf("verify promoted default route: %w", err)
	}
	if !routeListUsesLink(selected, candidate.Attrs().Index) {
		_ = netlink.RouteDel(&route)
		return RouteLease{}, fmt.Errorf("promoted route did not become the IPv4 default path")
	}
	return RouteLease{CandidateInterface: candidateInterface, platform: route}, nil
}

func (m *linuxRouteManager) Deactivate(lease RouteLease) error {
	route, ok := lease.platform.(netlink.Route)
	if !ok {
		return fmt.Errorf("invalid Linux route lease")
	}
	if !isOwnedDefaultRoute(route) {
		return fmt.Errorf("refusing to delete a route not owned by VoHive")
	}
	if err := netlink.RouteDel(&route); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func bestDefaultRoute(link netlink.Link) (netlink.Route, error) {
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return netlink.Route{}, fmt.Errorf("list candidate routes: %w", err)
	}
	var best *netlink.Route
	for i := range routes {
		route := routes[i]
		if !isMainIPv4Default(route) || route.Protocol == ownedRouteProtocol {
			continue
		}
		if best == nil || route.Priority < best.Priority {
			copy := route
			best = &copy
		}
	}
	if best == nil {
		return netlink.Route{}, fmt.Errorf("candidate interface %q has no IPv4 default route", link.Attrs().Name)
	}
	return *best, nil
}

func promotionMetric(primary netlink.Link, maximum int) (int, error) {
	if maximum < 1 {
		return 0, fmt.Errorf("maximum route metric must be positive")
	}
	routes, err := netlink.RouteList(primary, netlink.FAMILY_V4)
	if err != nil {
		return 0, fmt.Errorf("list primary routes: %w", err)
	}
	primaryMetric := -1
	for _, route := range routes {
		if !isMainIPv4Default(route) || route.Protocol == ownedRouteProtocol {
			continue
		}
		if primaryMetric < 0 || route.Priority < primaryMetric {
			primaryMetric = route.Priority
		}
	}
	return safePromotionMetric(primaryMetric, maximum)
}

func safePromotionMetric(primaryMetric, maximum int) (int, error) {
	if maximum < 1 {
		return 0, fmt.Errorf("maximum route metric must be positive")
	}
	if primaryMetric < 0 {
		return maximum, nil
	}
	if primaryMetric == 0 {
		return 0, fmt.Errorf("primary default route has metric 0; cannot safely install a strictly preferred backup route")
	}
	if maximum >= primaryMetric {
		return primaryMetric - 1, nil
	}
	return maximum, nil
}

func promotedRoute(base netlink.Route, linkIndex, metric int) netlink.Route {
	routeType := base.Type
	if routeType == 0 {
		routeType = unix.RTN_UNICAST
	}
	table := base.Table
	if table == 0 {
		table = unix.RT_TABLE_MAIN
	}
	return netlink.Route{
		LinkIndex: linkIndex,
		Scope:     base.Scope,
		Dst: &net.IPNet{
			IP:   net.IPv4zero,
			Mask: net.CIDRMask(0, 32),
		},
		Src:      append(net.IP(nil), base.Src...),
		Gw:       append(net.IP(nil), base.Gw...),
		Protocol: ownedRouteProtocol,
		Priority: metric,
		Family:   netlink.FAMILY_V4,
		Table:    table,
		Type:     routeType,
		MTU:      base.MTU,
		AdvMSS:   base.AdvMSS,
	}
}

func routeListUsesLink(routes []netlink.Route, linkIndex int) bool {
	for _, route := range routes {
		if route.LinkIndex == linkIndex {
			return true
		}
		for _, hop := range route.MultiPath {
			if hop.LinkIndex == linkIndex {
				return true
			}
		}
	}
	return false
}

func isOwnedDefaultRoute(route netlink.Route) bool {
	return route.Protocol == ownedRouteProtocol && isMainIPv4Default(route)
}

func isMainIPv4Default(route netlink.Route) bool {
	if route.Table != 0 && route.Table != unix.RT_TABLE_MAIN {
		return false
	}
	if route.Family != 0 && route.Family != netlink.FAMILY_V4 {
		return false
	}
	if route.Dst == nil {
		return true
	}
	ones, bits := route.Dst.Mask.Size()
	return bits == 32 && ones == 0
}
