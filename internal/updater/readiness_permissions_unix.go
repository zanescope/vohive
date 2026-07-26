//go:build !windows

package updater

import "os"

func readinessKeyPermissionsSecure(mode os.FileMode) bool {
	return mode.Perm()&0o077 == 0
}
