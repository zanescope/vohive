//go:build !linux

package hostfailover

import (
	"errors"
	"runtime"
)

func NewRouteManager() (RouteManager, error) {
	return nil, errors.New("host network failover is unsupported on " + runtime.GOOS)
}
