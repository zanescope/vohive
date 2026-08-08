//go:build !linux

package modemmanager

import "errors"

var errAtomicExchangeUnsupported = errors.New("atomic rule exchange requires Linux renameat2(RENAME_EXCHANGE)")

func exchangePaths(string, string) error {
	return errAtomicExchangeUnsupported
}
