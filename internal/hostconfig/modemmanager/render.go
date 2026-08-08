package modemmanager

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	managedMarker       = "# Managed by VoHive. DO NOT EDIT.\n"
	managedMarkerPrefix = "# Managed by VoHive"
	integrityPrefix     = "# vohive-modemmanager-isolation schema=1 payload-sha256="
	actionLine          = `ACTION!="add|change|move|bind", GOTO="vohive_mm_end"`
	targetPrefix        = "# vohive-target="
	labelLine           = `LABEL="vohive_mm_end"`
)

var (
	integrityLinePattern = regexp.MustCompile(`^# vohive-modemmanager-isolation schema=1 payload-sha256=([0-9a-f]{64})$`)
	serialRulePattern    = regexp.MustCompile(`^SUBSYSTEMS=="usb", ATTRS\{idVendor\}=="([0-9a-f]{4})", ATTRS\{serial\}=="([A-Za-z0-9._:+-]{1,128})", ENV\{ID_MM_DEVICE_IGNORE\}="1"$`)
	kernelRulePattern    = regexp.MustCompile(`^SUBSYSTEMS=="usb", ATTRS\{idVendor\}=="([0-9a-f]{4})", KERNELS=="([0-9]+-[0-9]+(?:\.[0-9]+)*)", ENV\{ID_MM_DEVICE_IGNORE\}="1"$`)
)

func Render(entries []Entry) ([]byte, error) {
	normalized, err := normalizeEntries(entries)
	if err != nil {
		return nil, err
	}

	var payload strings.Builder
	payload.WriteString(actionLine)
	payload.WriteByte('\n')
	for _, entry := range normalized {
		payload.WriteString(targetPrefix)
		payload.WriteString(base64.RawURLEncoding.EncodeToString([]byte(entry.TargetID)))
		payload.WriteByte('\n')
		switch entry.Matcher.Kind {
		case MatcherSerial:
			fmt.Fprintf(&payload,
				`SUBSYSTEMS=="usb", ATTRS{idVendor}=="%s", ATTRS{serial}=="%s", ENV{ID_MM_DEVICE_IGNORE}="1"`+"\n",
				entry.Matcher.VendorID, entry.Matcher.Serial)
		case MatcherKernelPath:
			fmt.Fprintf(&payload,
				`SUBSYSTEMS=="usb", ATTRS{idVendor}=="%s", KERNELS=="%s", ENV{ID_MM_DEVICE_IGNORE}="1"`+"\n",
				entry.Matcher.VendorID, entry.Matcher.KernelPath)
		default:
			return nil, fmt.Errorf("%w: unsupported matcher kind %q", ErrInvalidRequest, entry.Matcher.Kind)
		}
	}
	payload.WriteString(labelLine)
	payload.WriteByte('\n')

	payloadBytes := []byte(payload.String())
	digest := sha256.Sum256(payloadBytes)
	var output strings.Builder
	output.Grow(len(managedMarker) + len(integrityPrefix) + hex.EncodedLen(len(digest)) + 1 + len(payloadBytes))
	output.WriteString(managedMarker)
	output.WriteString(integrityPrefix)
	output.WriteString(hex.EncodeToString(digest[:]))
	output.WriteByte('\n')
	output.Write(payloadBytes)
	return []byte(output.String()), nil
}

func normalizeEntries(entries []Entry) ([]Entry, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: at least one target is required", ErrInvalidRequest)
	}
	normalized := make([]Entry, 0, len(entries))
	targets := make(map[string]struct{}, len(entries))
	matchers := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		entry.TargetID = strings.TrimSpace(entry.TargetID)
		if entry.TargetID == "" || len(entry.TargetID) > 256 {
			return nil, fmt.Errorf("%w: target ID must contain 1 to 256 bytes", ErrInvalidRequest)
		}
		if _, duplicate := targets[entry.TargetID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate target ID %q", ErrInvalidRequest, entry.TargetID)
		}
		targets[entry.TargetID] = struct{}{}

		entry.Matcher.VendorID = strings.ToLower(strings.TrimSpace(entry.Matcher.VendorID))
		if !hexIDPattern.MatchString(entry.Matcher.VendorID) {
			return nil, fmt.Errorf("%w: invalid USB vendor ID for %q", ErrInvalidRequest, entry.TargetID)
		}
		var matcherKey string
		switch entry.Matcher.Kind {
		case MatcherSerial:
			entry.Matcher.Serial = strings.TrimSpace(entry.Matcher.Serial)
			entry.Matcher.KernelPath = ""
			if !safeSerialPattern.MatchString(entry.Matcher.Serial) {
				return nil, fmt.Errorf("%w: unsafe USB serial for %q", ErrInvalidRequest, entry.TargetID)
			}
			matcherKey = string(entry.Matcher.Kind) + "\x00" + entry.Matcher.VendorID + "\x00" + entry.Matcher.Serial
		case MatcherKernelPath:
			entry.Matcher.KernelPath = strings.TrimSpace(entry.Matcher.KernelPath)
			entry.Matcher.Serial = ""
			if !kernelPathPattern.MatchString(entry.Matcher.KernelPath) {
				return nil, fmt.Errorf("%w: invalid USB kernel path for %q", ErrInvalidRequest, entry.TargetID)
			}
			matcherKey = string(entry.Matcher.Kind) + "\x00" + entry.Matcher.VendorID + "\x00" + entry.Matcher.KernelPath
		default:
			return nil, fmt.Errorf("%w: unsupported matcher kind %q", ErrInvalidRequest, entry.Matcher.Kind)
		}
		if _, duplicate := matchers[matcherKey]; duplicate {
			return nil, fmt.Errorf("%w: duplicate matcher for %q", ErrInvalidRequest, entry.TargetID)
		}
		matchers[matcherKey] = struct{}{}
		normalized = append(normalized, entry)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].TargetID < normalized[j].TargetID
	})
	return normalized, nil
}

func parseManagedRule(data []byte) ([]Entry, error) {
	if !bytes.HasPrefix(data, []byte(managedMarker)) {
		return nil, fmt.Errorf("%w: managed marker is missing", ErrTamperedRule)
	}
	remaining := data[len(managedMarker):]
	lineEnd := bytes.IndexByte(remaining, '\n')
	if lineEnd < 0 {
		return nil, fmt.Errorf("%w: integrity header is incomplete", ErrTamperedRule)
	}
	integrityLine := string(remaining[:lineEnd])
	match := integrityLinePattern.FindStringSubmatch(integrityLine)
	if match == nil {
		return nil, fmt.Errorf("%w: integrity header is invalid", ErrTamperedRule)
	}
	payload := remaining[lineEnd+1:]
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != match[1] {
		return nil, fmt.Errorf("%w: payload checksum mismatch", ErrTamperedRule)
	}

	lines := strings.Split(string(payload), "\n")
	if len(lines) < 5 || lines[len(lines)-1] != "" {
		return nil, fmt.Errorf("%w: payload is incomplete", ErrTamperedRule)
	}
	lines = lines[:len(lines)-1]
	if lines[0] != actionLine || lines[len(lines)-1] != labelLine {
		return nil, fmt.Errorf("%w: payload boundary is invalid", ErrTamperedRule)
	}
	body := lines[1 : len(lines)-1]
	if len(body) == 0 || len(body)%2 != 0 {
		return nil, fmt.Errorf("%w: payload entries are invalid", ErrTamperedRule)
	}

	entries := make([]Entry, 0, len(body)/2)
	for i := 0; i < len(body); i += 2 {
		if !strings.HasPrefix(body[i], targetPrefix) {
			return nil, fmt.Errorf("%w: target marker is invalid", ErrTamperedRule)
		}
		encodedID := strings.TrimPrefix(body[i], targetPrefix)
		targetID, err := base64.RawURLEncoding.DecodeString(encodedID)
		if err != nil || len(targetID) == 0 {
			return nil, fmt.Errorf("%w: target ID encoding is invalid", ErrTamperedRule)
		}
		entry := Entry{TargetID: string(targetID)}
		switch {
		case serialRulePattern.MatchString(body[i+1]):
			rule := serialRulePattern.FindStringSubmatch(body[i+1])
			entry.Matcher = Matcher{Kind: MatcherSerial, VendorID: rule[1], Serial: rule[2]}
		case kernelRulePattern.MatchString(body[i+1]):
			rule := kernelRulePattern.FindStringSubmatch(body[i+1])
			entry.Matcher = Matcher{Kind: MatcherKernelPath, VendorID: rule[1], KernelPath: rule[2]}
		default:
			return nil, fmt.Errorf("%w: udev rule is not canonical", ErrTamperedRule)
		}
		entries = append(entries, entry)
	}

	canonical, err := Render(entries)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTamperedRule, err)
	}
	if !bytes.Equal(canonical, data) {
		return nil, fmt.Errorf("%w: managed rule is not canonical", ErrTamperedRule)
	}
	return entries, nil
}
