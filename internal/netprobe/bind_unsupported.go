//go:build !linux

package netprobe

import (
	"fmt"
	"net"
	"time"
)

func platformBoundDialer(interfaceName string, _ time.Duration) (*net.Dialer, error) {
	if interfaceName == "" {
		return nil, ErrInterfaceRequired
	}
	return nil, fmt.Errorf("interface-bound public IP probes are unsupported on this platform")
}
