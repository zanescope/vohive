//go:build linux

package mbimcore

import (
	"net"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func dataBoundDialer(interfaceName string, timeout time.Duration) *net.Dialer {
	dialer := &net.Dialer{Timeout: timeout}
	interfaceName = strings.TrimSpace(interfaceName)
	if interfaceName == "" {
		return dialer
	}
	dialer.Control = func(_, _ string, raw syscall.RawConn) error {
		var socketErr error
		if err := raw.Control(func(fd uintptr) {
			socketErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, interfaceName)
		}); err != nil {
			return err
		}
		return socketErr
	}
	return dialer
}
