//go:build linux

package modemmanager

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func exchangePaths(first, second string) error {
	err := unix.Renameat2(
		unix.AT_FDCWD, first,
		unix.AT_FDCWD, second,
		unix.RENAME_EXCHANGE,
	)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("atomic rule exchange is unavailable: %w", err)
	}
	return err
}
