package config

import (
	"fmt"
	"os"

	"github.com/zanescope/vohive/internal/netprobe"
	yaml "go.yaml.in/yaml/v3"
)

const maxPublicIPProbeURLsPerFamily = 4

var (
	defaultPublicIPv4ProbeURLs = []string{
		"https://api.ipify.org",
		"https://4.ident.me",
	}
	defaultPublicIPv6ProbeURLs = []string{
		"https://api6.ipify.org",
		"https://6.ident.me",
	}
)

// PublicIPProbeConfig is loaded only from the root YAML document and is fixed
// for the process lifetime. Runtime settings APIs intentionally do not expose it.
type PublicIPProbeConfig struct {
	IPv4URLs []string `mapstructure:"ipv4_urls" yaml:"ipv4_urls"`
	IPv6URLs []string `mapstructure:"ipv6_urls" yaml:"ipv6_urls"`
}

func DefaultPublicIPProbeConfig() PublicIPProbeConfig {
	return PublicIPProbeConfig{
		IPv4URLs: append([]string(nil), defaultPublicIPv4ProbeURLs...),
		IPv6URLs: append([]string(nil), defaultPublicIPv6ProbeURLs...),
	}
}

// NormalizePublicIPProbeConfig resolves defaults and validates an explicit
// configuration. An empty family uses defaults; a non-empty family replaces
// defaults completely while preserving source priority.
func NormalizePublicIPProbeConfig(input PublicIPProbeConfig) (PublicIPProbeConfig, error) {
	ipv4, err := normalizeProbeURLs("ipv4_urls", input.IPv4URLs, defaultPublicIPv4ProbeURLs, netprobe.FamilyV4)
	if err != nil {
		return PublicIPProbeConfig{}, err
	}
	ipv6, err := normalizeProbeURLs("ipv6_urls", input.IPv6URLs, defaultPublicIPv6ProbeURLs, netprobe.FamilyV6)
	if err != nil {
		return PublicIPProbeConfig{}, err
	}
	return PublicIPProbeConfig{IPv4URLs: ipv4, IPv6URLs: ipv6}, nil
}

func normalizeProbeURLs(field string, values, defaults []string, family netprobe.Family) ([]string, error) {
	if len(values) == 0 {
		return append([]string(nil), defaults...), nil
	}
	if len(values) > maxPublicIPProbeURLsPerFamily {
		return nil, fmt.Errorf("%s: at most %d URLs are allowed", field, maxPublicIPProbeURLsPerFamily)
	}

	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for index, raw := range values {
		normalized, err := netprobe.ValidateTargetURL(raw, family)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", field, index, err)
		}
		key := netprobe.TargetKey(normalized)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, normalized)
	}
	return result, nil
}

// Read this section directly from YAML after Viper has loaded the rest of the
// application config. This deliberately prevents AutomaticEnv from claiming
// support for slice values whose parsing semantics are not stable.
func loadPublicIPProbeFromYAML(path string) (PublicIPProbeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PublicIPProbeConfig{}, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return PublicIPProbeConfig{}, err
	}
	if len(document.Content) == 0 {
		return NormalizePublicIPProbeConfig(PublicIPProbeConfig{})
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return PublicIPProbeConfig{}, fmt.Errorf("root YAML document must be a mapping")
	}

	var section *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Value != "public_ip_probe" {
			continue
		}
		if section != nil {
			return PublicIPProbeConfig{}, fmt.Errorf("duplicate public_ip_probe section")
		}
		section = value
	}
	if section == nil || section.Tag == "!!null" {
		return NormalizePublicIPProbeConfig(PublicIPProbeConfig{})
	}
	if section.Kind != yaml.MappingNode {
		return PublicIPProbeConfig{}, fmt.Errorf("public_ip_probe must be a mapping")
	}

	allowed := map[string]struct{}{"ipv4_urls": {}, "ipv6_urls": {}}
	seen := make(map[string]struct{}, len(allowed))
	for i := 0; i+1 < len(section.Content); i += 2 {
		key := section.Content[i]
		if key.Kind != yaml.ScalarNode {
			return PublicIPProbeConfig{}, fmt.Errorf("public_ip_probe field name must be a string")
		}
		if _, ok := allowed[key.Value]; !ok {
			return PublicIPProbeConfig{}, fmt.Errorf("unknown field %q (allowed: ipv4_urls, ipv6_urls)", key.Value)
		}
		if _, duplicate := seen[key.Value]; duplicate {
			return PublicIPProbeConfig{}, fmt.Errorf("duplicate field %q", key.Value)
		}
		seen[key.Value] = struct{}{}
	}

	var cfg PublicIPProbeConfig
	if err := section.Decode(&cfg); err != nil {
		return PublicIPProbeConfig{}, err
	}
	return NormalizePublicIPProbeConfig(cfg)
}
