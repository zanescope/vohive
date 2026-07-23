package device

import (
	mbimcore "github.com/zanescope/vohive/internal/mbim"
	qmicore "github.com/zanescope/vohive/internal/qmi"
)

type NetworkController interface {
	Connect() error
	Disconnect() error
	IsConnected() bool
	RotateIP() error
	GetPrivateIP() string
	GetPrivateIPv6() string
	GetPublicIPv4AndV6NoCache() (publicV4 string, publicV6 string)
}

// NetworkAddressSnapshot samples one public-IP state revision for API output.
// It rejects a disconnect or state transition observed while sampling, so the
// connected flag and addresses do not come from separate controller calls.
type NetworkAddressSnapshot struct {
	Connected   bool
	PrivateIPv4 string
	PrivateIPv6 string
	PublicIPv4  string
	PublicIPv6  string
}

var (
	_ NetworkController = (*qmicore.Manager)(nil)
	_ NetworkController = (*mbimcore.Manager)(nil)
)

func (w *Worker) NetworkController() NetworkController {
	if w == nil {
		return nil
	}
	if w.netOverride != nil {
		return w.netOverride
	}
	if w.QMICore != nil {
		return w.QMICore
	}
	if w.MBIMCore != nil {
		return w.MBIMCore
	}
	return nil
}

func (w *Worker) NetworkAddressSnapshot() NetworkAddressSnapshot {
	if w == nil {
		return NetworkAddressSnapshot{}
	}
	nc := w.NetworkController()
	if nc == nil {
		return NetworkAddressSnapshot{}
	}

	state := &w.publicIP
	for attempt := 0; attempt < 2; attempt++ {
		if !nc.IsConnected() {
			return NetworkAddressSnapshot{}
		}
		state.mu.Lock()
		if !state.initialized || !state.connected {
			state.mu.Unlock()
			return NetworkAddressSnapshot{}
		}
		epoch, revision := state.epoch, state.revision
		snapshot := NetworkAddressSnapshot{
			Connected:   true,
			PrivateIPv4: state.privateV4,
			PrivateIPv6: state.privateV6,
			PublicIPv4:  state.publishedV4,
			PublicIPv6:  state.publishedV6,
		}
		state.mu.Unlock()

		if !nc.IsConnected() {
			return NetworkAddressSnapshot{}
		}
		state.mu.Lock()
		current := state.initialized && state.connected && state.epoch == epoch && state.revision == revision
		state.mu.Unlock()
		if current {
			return snapshot
		}
	}
	return NetworkAddressSnapshot{}
}
