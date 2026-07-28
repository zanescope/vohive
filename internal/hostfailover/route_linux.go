//go:build linux

package hostfailover

import (
	"errors"
	"fmt"
	"net"
	"strings"

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
		if !isOwnedFailoverRoute(route) {
			continue
		}
		if err := netlink.RouteDel(&route); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.ENOENT) {
			errs = append(errs, fmt.Errorf("delete stale route %s: %w", route.String(), err))
		}
	}
	return errors.Join(errs...)
}

func (m *linuxRouteManager) ResolvePrimary(excludedInterfaces []string) (string, error) {
	excludedIndexes := make(map[int]struct{}, len(excludedInterfaces))
	for _, interfaceName := range excludedInterfaces {
		link, err := netlink.LinkByName(strings.TrimSpace(interfaceName))
		if err == nil && link != nil && link.Attrs() != nil {
			excludedIndexes[link.Attrs().Index] = struct{}{}
		}
	}

	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", fmt.Errorf("list IPv4 routes: %w", err)
	}
	linkIndex := bestPrimaryRouteLinkIndex(routes, excludedIndexes)
	if linkIndex == 0 {
		return "", fmt.Errorf("no non-candidate IPv4 default route")
	}
	link, err := netlink.LinkByIndex(linkIndex)
	if err != nil {
		return "", fmt.Errorf("resolve primary route link %d: %w", linkIndex, err)
	}
	if link == nil || link.Attrs() == nil || strings.TrimSpace(link.Attrs().Name) == "" {
		return "", fmt.Errorf("primary route link %d has no interface name", linkIndex)
	}
	return strings.TrimSpace(link.Attrs().Name), nil
}

func bestPrimaryRouteLinkIndex(routes []netlink.Route, excludedIndexes map[int]struct{}) int {
	bestIndex := 0
	bestMetric := 0
	for _, route := range routes {
		if !isMainIPv4Default(route) || route.Protocol == ownedRouteProtocol {
			continue
		}
		consider := func(linkIndex int) {
			if linkIndex <= 0 {
				return
			}
			if _, excluded := excludedIndexes[linkIndex]; excluded {
				return
			}
			if bestIndex == 0 || route.Priority < bestMetric || (route.Priority == bestMetric && linkIndex < bestIndex) {
				bestIndex = linkIndex
				bestMetric = route.Priority
			}
		}
		consider(route.LinkIndex)
		for _, hop := range route.MultiPath {
			if hop != nil {
				consider(hop.LinkIndex)
			}
		}
	}
	return bestIndex
}

func (m *linuxRouteManager) Activate(primaryInterface, candidateInterface string, maximumMetric int) (RouteLease, error) {
	candidate, err := netlink.LinkByName(candidateInterface)
	if err != nil {
		return RouteLease{}, fmt.Errorf("find candidate interface %q: %w", candidateInterface, err)
	}

	var primary netlink.Link
	if primaryInterface = strings.TrimSpace(primaryInterface); primaryInterface != "" {
		resolvedPrimary, lookupErr := netlink.LinkByName(primaryInterface)
		if lookupErr != nil {
			var notFound netlink.LinkNotFoundError
			if !errors.As(lookupErr, &notFound) {
				return RouteLease{}, fmt.Errorf("find primary interface %q: %w", primaryInterface, lookupErr)
			}
		} else {
			primary = resolvedPrimary
			if primary.Attrs().Index == candidate.Attrs().Index {
				return RouteLease{}, fmt.Errorf("primary and candidate interfaces must be different")
			}
		}
	}

	base, err := bestDefaultRoute(candidate)
	if err != nil {
		return RouteLease{}, err
	}
	routes, err := promotionRoutes(base, candidate.Attrs().Index, primary, maximumMetric)
	if err != nil {
		return RouteLease{}, err
	}
	added := make([]netlink.Route, 0, len(routes))
	cleanup := func() {
		for index := len(added) - 1; index >= 0; index-- {
			route := added[index]
			_ = netlink.RouteDel(&route)
		}
	}
	for index := range routes {
		if err := netlink.RouteAdd(&routes[index]); err != nil {
			cleanup()
			return RouteLease{}, fmt.Errorf("add promoted route: %w", err)
		}
		added = append(added, routes[index])
	}

	for _, target := range []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("129.0.0.1")} {
		selected, err := netlink.RouteGet(target)
		if err != nil {
			cleanup()
			return RouteLease{}, fmt.Errorf("verify promoted route: %w", err)
		}
		if !routeListUsesLink(selected, candidate.Attrs().Index) {
			cleanup()
			return RouteLease{}, fmt.Errorf("promoted route did not become the IPv4 path for %s", target)
		}
	}
	return RouteLease{CandidateInterface: candidateInterface, platform: routes}, nil
}

func (m *linuxRouteManager) Deactivate(lease RouteLease) error {
	routes, ok := lease.platform.([]netlink.Route)
	if !ok {
		route, legacy := lease.platform.(netlink.Route)
		if !legacy {
			return fmt.Errorf("invalid Linux route lease")
		}
		routes = []netlink.Route{route}
	}
	var errs []error
	for index := range routes {
		route := routes[index]
		if !isOwnedFailoverRoute(route) {
			errs = append(errs, fmt.Errorf("refusing to delete a route not owned by VoHive"))
			continue
		}
		if err := netlink.RouteDel(&route); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.ENOENT) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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

func promotionRoutes(base netlink.Route, linkIndex int, primary netlink.Link, maximum int) ([]netlink.Route, error) {
	if maximum < 1 {
		return nil, fmt.Errorf("maximum route metric must be positive")
	}
	if primary == nil {
		return []netlink.Route{promotedRoute(base, linkIndex, maximum)}, nil
	}
	primaryMetric, err := primaryDefaultMetric(primary)
	if err != nil {
		return nil, err
	}
	if primaryMetric == 0 {
		return promotedTakeoverRoutes(base, linkIndex, maximum), nil
	}
	metric, err := safePromotionMetric(primaryMetric, maximum)
	if err != nil {
		return nil, err
	}
	return []netlink.Route{promotedRoute(base, linkIndex, metric)}, nil
}

func primaryDefaultMetric(primary netlink.Link) (int, error) {
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
	return primaryMetric, nil
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

func promotedTakeoverRoutes(base netlink.Route, linkIndex, metric int) []netlink.Route {
	low := promotedRoute(base, linkIndex, metric)
	low.Dst = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(1, 32)}
	high := promotedRoute(base, linkIndex, metric)
	high.Dst = &net.IPNet{IP: net.IPv4(128, 0, 0, 0), Mask: net.CIDRMask(1, 32)}
	return []netlink.Route{low, high}
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

func isOwnedFailoverRoute(route netlink.Route) bool {
	if route.Protocol != ownedRouteProtocol {
		return false
	}
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
	if bits != 32 || (ones != 0 && ones != 1) {
		return false
	}
	network := route.Dst.IP.To4()
	if network == nil || ones == 0 {
		return network != nil
	}
	return (network[0] == 0 || network[0] == 128) && network[1] == 0 && network[2] == 0 && network[3] == 0
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
