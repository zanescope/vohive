//go:build !linux

package mbimcore

import (
	"net"
	"time"
)

func dataBoundDialer(_ string, timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout}
}
