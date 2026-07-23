package netprobe

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxTargetURLBytes = 2048
	defaultRetryAfter = 30 * time.Second
	maxRetryAfter     = 10 * time.Minute
)

func parseResponseAddress(body string, family Family) (netip.Addr, error) {
	raw := strings.TrimSpace(body)
	addr, err := netip.ParseAddr(raw)
	if err != nil || raw == "" {
		return netip.Addr{}, errors.New("response is not a plain IP address")
	}
	if !matchesFamily(addr, family) {
		return netip.Addr{}, fmt.Errorf("response address does not match %s", family)
	}
	if !IsPublicAddress(addr) {
		return netip.Addr{}, errors.New("response address is not publicly routable")
	}
	return addr, nil
}

func matchesFamily(addr netip.Addr, family Family) bool {
	if !addr.IsValid() || addr.Is4In6() {
		return false
	}
	switch family {
	case FamilyV4:
		return addr.Is4()
	case FamilyV6:
		return addr.Is6()
	default:
		return addr.Is4() || addr.Is6()
	}
}

// ValidateTargetURL validates a production source and returns its trimmed form.
// Paths and query strings are allowed; credentials and fragments are not.
func ValidateTargetURL(raw string, family Family) (string, error) {
	u, _, err := parseTargetURL(raw)
	if err != nil {
		return "", err
	}
	if literal, parseErr := netip.ParseAddr(u.Hostname()); parseErr == nil {
		if !matchesFamily(literal, family) {
			return "", fmt.Errorf("IP literal does not match %s", family)
		}
		if !IsPublicAddress(literal) {
			return "", errors.New("IP literal is not publicly routable")
		}
	}
	u.Scheme = "https"
	return u.String(), nil
}

// TargetKey is a stable de-duplication key. Call it only after validation.
func TargetKey(raw string) string {
	u, _, err := parseTargetURL(raw)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

func parseTargetURL(raw string) (*url.URL, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, "", errors.New("URL is empty")
	}
	if len(trimmed) > maxTargetURLBytes {
		return nil, "", errors.New("URL is too long")
	}
	if strings.IndexFunc(trimmed, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return nil, "", errors.New("URL contains a control character")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, "", errors.New("URL cannot be parsed")
	}
	if !u.IsAbs() || !strings.EqualFold(u.Scheme, "https") || u.Opaque != "" {
		return nil, "", errors.New("URL must be an absolute HTTPS URL")
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, "", errors.New("URL host is required")
	}
	if u.User != nil {
		return nil, "", errors.New("URL user information is not allowed")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return nil, "", errors.New("URL fragment is not allowed")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, "", errors.New("URL port is invalid")
	}
	if port := u.Port(); port != "" {
		value, convErr := strconv.Atoi(port)
		if convErr != nil || value < 1 || value > 65535 {
			return nil, "", errors.New("URL port is invalid")
		}
	}
	return u, trimmed, nil
}

func safeSource(raw string) string {
	u, _, err := parseTargetURL(raw)
	if err != nil {
		return "<invalid>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Path = ""
	u.RawPath = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

func sanitizedURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return clampRetryAfter(time.Duration(seconds) * time.Second)
	}
	if at, err := http.ParseTime(value); err == nil {
		return clampRetryAfter(at.Sub(now))
	}
	return defaultRetryAfter
}

func clampRetryAfter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return time.Second
	}
	if delay > maxRetryAfter {
		return maxRetryAfter
	}
	return delay
}

// IsPublicAddress goes beyond Addr.IsGlobalUnicast, which reports RFC1918 and
// ULA addresses as global-unicast. These special-purpose ranges must never be
// accepted as an externally observed public address or probe destination.
func IsPublicAddress(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" || addr.Is4In6() || !addr.IsGlobalUnicast() ||
		addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	if addr.Is6() && !globalIPv6UnicastPrefix.Contains(addr) {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

var globalIPv6UnicastPrefix = mustPrefix("2000::/3")

var nonPublicPrefixes = []netip.Prefix{
	mustPrefix("0.0.0.0/8"),
	mustPrefix("100.64.0.0/10"),
	mustPrefix("192.0.0.0/24"),
	mustPrefix("192.0.2.0/24"),
	mustPrefix("192.31.196.0/24"),
	mustPrefix("192.52.193.0/24"),
	mustPrefix("192.88.99.0/24"),
	mustPrefix("192.175.48.0/24"),
	mustPrefix("198.18.0.0/15"),
	mustPrefix("198.51.100.0/24"),
	mustPrefix("203.0.113.0/24"),
	mustPrefix("240.0.0.0/4"),
	mustPrefix("::/96"),
	mustPrefix("64:ff9b::/96"),
	mustPrefix("64:ff9b:1::/48"),
	mustPrefix("100::/64"),
	mustPrefix("100:0:0:1::/64"),
	mustPrefix("2001::/23"),
	mustPrefix("2001:db8::/32"),
	mustPrefix("2002::/16"),
	mustPrefix("3fff::/20"),
	mustPrefix("5f00::/16"),
	mustPrefix("2620:4f:8000::/48"),
	mustPrefix("fc00::/7"),
	mustPrefix("fec0::/10"),
}

func mustPrefix(raw string) netip.Prefix {
	return netip.MustParsePrefix(raw)
}
