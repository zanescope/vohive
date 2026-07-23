//go:build linux

package netprobe

import (
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func platformBoundDialer(interfaceName string, timeout time.Duration) (*net.Dialer, error) {
	if interfaceName == "" {
		return nil, ErrInterfaceRequired
	}
	dialer := &net.Dialer{Timeout: timeout}
	dialer.Control = func(_, _ string, raw syscall.RawConn) error {
		var socketErr error
		if err := raw.Control(func(fd uintptr) {
			socketErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, interfaceName)
		}); err != nil {
			return err
		}
		return socketErr
	}
	return dialer, nil
}
