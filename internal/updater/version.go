package updater

import (
	"strings"

	"golang.org/x/mod/semver"
)

func normalizeVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return ""
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return version
}

func validVersion(version string) bool {
	normalized := normalizeVersion(version)
	return !strings.Contains(normalized, "+") && semver.IsValid(normalized)
}

func compareVersions(left, right string) int {
	return semver.Compare(normalizeVersion(left), normalizeVersion(right))
}

func isHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') &&
			(character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}
func validJobID(value string) bool {
	if value == "" {
		return true
	}
	if value == "." || value == ".." || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}
