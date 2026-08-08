package modemmanager

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	kernelPathPattern = regexp.MustCompile(`^[0-9]+-[0-9]+(?:\.[0-9]+)*$`)
	safeSerialPattern = regexp.MustCompile(`^[A-Za-z0-9._:+-]{1,128}$`)
	hexIDPattern      = regexp.MustCompile(`^[0-9A-Fa-f]{4}$`)
)

func (m *Manager) ResolveMatcher(target Target) (Matcher, error) {
	if m == nil {
		return Matcher{}, fmt.Errorf("%w: manager is nil", ErrInvalidRequest)
	}
	if strings.TrimSpace(target.ID) == "" {
		return Matcher{}, fmt.Errorf("%w: target ID is required", ErrInvalidRequest)
	}

	usbRoot, vendorID, err := m.resolveUSBDeviceRoot(target.USBPath)
	if err != nil {
		return Matcher{}, fmt.Errorf("resolve target %q: %w", target.ID, err)
	}
	kernelPath := filepath.Base(usbRoot)

	serial, serialErr := readSysfsAttribute(filepath.Join(usbRoot, "serial"))
	if serialErr == nil && serialLooksStable(serial) && m.serialIsUnique(serial) {
		return Matcher{
			Kind:     MatcherSerial,
			VendorID: vendorID,
			Serial:   serial,
		}, nil
	}
	if !kernelPathPattern.MatchString(kernelPath) {
		return Matcher{}, fmt.Errorf("%w: target %q has no unique serial or USB port path", ErrNoStableMatcher, target.ID)
	}
	return Matcher{
		Kind:       MatcherKernelPath,
		VendorID:   vendorID,
		KernelPath: kernelPath,
	}, nil
}

func serialLooksStable(serial string) bool {
	serial = strings.TrimSpace(serial)
	if len(serial) < 6 || !safeSerialPattern.MatchString(serial) {
		return false
	}

	normalized := strings.ToLower(serial)
	switch normalized {
	case "unknown", "default", "none", "null", "serial", "unset", "notset",
		"0123456789", "1234567890", "0123456789abcdef", "1234567890abcdef":
		return false
	}

	distinct := make(map[byte]struct{}, 3)
	for i := 0; i < len(normalized); i++ {
		distinct[normalized[i]] = struct{}{}
		if len(distinct) >= 3 {
			return true
		}
	}
	return false
}

func (m *Manager) resolveUSBDeviceRoot(input string) (string, string, error) {
	input = strings.TrimSpace(input)
	if input == "" || !filepath.IsAbs(input) {
		return "", "", fmt.Errorf("%w: path must be absolute", ErrUnsafeUSBPath)
	}
	allowed, err := pathWithin(m.sysfsMount, input)
	if err != nil || !allowed {
		return "", "", fmt.Errorf("%w: %q is outside %s", ErrUnsafeUSBPath, input, m.sysfsMount)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Clean(input))
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve %q: %v", ErrNoStableMatcher, input, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", fmt.Errorf("%w: inspect %q: %v", ErrNoStableMatcher, input, err)
	}
	current := resolved
	if !info.IsDir() {
		current = filepath.Dir(current)
	}

	for {
		base := filepath.Base(current)
		if kernelPathPattern.MatchString(base) {
			vendorID, valid := readUSBIdentity(current)
			if valid && m.crossChecksUSBRoot(current, base) {
				return current, vendorID, nil
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", "", fmt.Errorf("%w: %q does not resolve to a USB device", ErrNoStableMatcher, input)
}

func (m *Manager) crossChecksUSBRoot(candidate, kernelPath string) bool {
	if !kernelPathPattern.MatchString(kernelPath) {
		return false
	}
	indexed := filepath.Join(m.sysfsRoot, kernelPath)
	indexedResolved, err := filepath.EvalSymlinks(indexed)
	if err != nil {
		return false
	}
	candidateInfo, err := os.Stat(candidate)
	if err != nil {
		return false
	}
	indexedInfo, err := os.Stat(indexedResolved)
	if err != nil {
		return false
	}
	return os.SameFile(candidateInfo, indexedInfo)
}

func (m *Manager) serialIsUnique(serial string) bool {
	if !safeSerialPattern.MatchString(serial) {
		return false
	}
	children, err := os.ReadDir(m.sysfsRoot)
	if err != nil {
		return false
	}
	seen := make(map[string]struct{})
	matches := 0
	for _, child := range children {
		if !kernelPathPattern.MatchString(child.Name()) {
			continue
		}
		path, err := filepath.EvalSymlinks(filepath.Join(m.sysfsRoot, child.Name()))
		if err != nil {
			continue
		}
		canonical, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		canonical = filepath.Clean(canonical)
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		if _, valid := readUSBIdentity(canonical); !valid {
			continue
		}
		value, err := readSysfsAttribute(filepath.Join(canonical, "serial"))
		if err != nil || value != serial {
			continue
		}
		matches++
		if matches > 1 {
			return false
		}
	}
	return matches == 1
}

func readUSBIdentity(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", false
	}
	vendorID, err := readSysfsAttribute(filepath.Join(path, "idVendor"))
	if err != nil || !hexIDPattern.MatchString(vendorID) {
		return "", false
	}
	productID, err := readSysfsAttribute(filepath.Join(path, "idProduct"))
	if err != nil || !hexIDPattern.MatchString(productID) {
		return "", false
	}
	return strings.ToLower(vendorID), true
}

func readSysfsAttribute(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return "", err
	}
	if len(data) > 1024 {
		return "", errors.New("sysfs attribute is too large")
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("sysfs attribute is empty")
	}
	return value, nil
}

func pathWithin(root, candidate string) (bool, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(candidateAbs))
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}
