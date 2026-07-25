//go:build windows

package updater

import "os"

func readinessKeyPermissionsSecure(os.FileMode) bool {
	// Windows exposes access through ACLs rather than POSIX group/other mode bits.
	return true
}
