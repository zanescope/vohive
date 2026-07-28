package device

import (
	"context"
	"fmt"
	"strings"

	"github.com/zanescope/vohive/internal/hostfailover"
)

type contextualPublicIPProber interface {
	GetPublicIPv4AndV6Context(context.Context) (string, string)
}

// Candidates implements hostfailover.CandidateSource. The returned order is
// exactly the configured device-ID order; only runtime interface names are
// used, so USB re-enumeration cannot silently select the wrong modem.
func (p *Pool) Candidates(deviceIDs []string) []hostfailover.Candidate {
	if p == nil {
		return nil
	}

	type snapshot struct {
		id         string
		iface      string
		worker     *Worker
		controller NetworkController
	}
	snapshots := make([]snapshot, 0, len(deviceIDs))
	p.mu.RLock()
	for _, rawID := range deviceIDs {
		deviceID := strings.TrimSpace(rawID)
		worker := p.workers[deviceID]
		if worker == nil {
			continue
		}
		controller := worker.NetworkController()
		snapshots = append(snapshots, snapshot{
			id:         deviceID,
			iface:      strings.TrimSpace(worker.Config.Interface),
			worker:     worker,
			controller: controller,
		})
	}
	p.mu.RUnlock()

	candidates := make([]hostfailover.Candidate, 0, len(snapshots))
	for _, item := range snapshots {
		connected := item.controller != nil && item.controller.IsConnected()
		candidate := hostfailover.Candidate{
			DeviceID:  item.id,
			Interface: item.iface,
			Connected: connected,
		}
		if prober, ok := item.controller.(contextualPublicIPProber); ok {
			worker := item.worker
			controller := item.controller
			deviceID := item.id
			candidate.Probe = func(ctx context.Context) error {
				if p.GetWorker(deviceID) != worker {
					return fmt.Errorf("worker was replaced")
				}
				if !controller.IsConnected() {
					return fmt.Errorf("data connection is down")
				}
				publicV4, _ := prober.GetPublicIPv4AndV6Context(ctx)
				if strings.TrimSpace(publicV4) == "" {
					return fmt.Errorf("IPv4 internet probe failed")
				}
				return nil
			}
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

var _ hostfailover.CandidateSource = (*Pool)(nil)
