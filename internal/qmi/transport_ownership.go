package qmicore

import (
	"fmt"
	"strings"

	"github.com/zanescope/vohive/internal/config"
)

func validateQMITransportOwnership(cfg config.DeviceConfig) error {
	if strings.ToLower(strings.TrimSpace(cfg.DeviceBackend)) != "qmi" {
		return nil
	}

	decision := decideQMITransport(cfg, "qmi")
	if !decision.ControlDeviceScanned {
		return nil
	}

	controlDevice := strings.TrimSpace(cfg.ControlDevice)
	if controlDevice == "" {
		controlDevice = strings.TrimSpace(cfg.QMIDevice)
	}

	if decision.HolderScanError != "" {
		return fmt.Errorf(
			"qmi_transport_ownership_preflight: holder scan failed for %s: %s",
			controlDevice,
			decision.HolderScanError,
		)
	}
	if decision.HolderScanUnknown {
		return fmt.Errorf(
			"qmi_transport_ownership_preflight: holder scan incomplete for %s",
			controlDevice,
		)
	}
	if len(decision.ModemManagerHolders) > 0 {
		return fmt.Errorf(
			"qmi_transport_ownership_preflight: %s is owned by ModemManager (pids=%v)",
			controlDevice,
			qmiControlHolderPIDs(decision.ModemManagerHolders),
		)
	}
	if decision.HolderCount == 0 {
		return nil
	}
	if decision.UseProxy && decision.OnlyQMIProxy {
		return nil
	}
	if decision.UseProxy {
		return fmt.Errorf(
			"qmi_transport_ownership_preflight: proxy mode requires qmi-proxy-only ownership for %s (pids=%v)",
			controlDevice,
			qmiControlHolderPIDs(decision.Holders),
		)
	}
	return fmt.Errorf(
		"qmi_transport_ownership_preflight: direct mode requires an unowned control device %s (pids=%v)",
		controlDevice,
		qmiControlHolderPIDs(decision.Holders),
	)
}

func qmiControlHolderPIDs(holders []qmiControlDeviceHolder) []int {
	pids := make([]int, 0, len(holders))
	for _, holder := range holders {
		pids = append(pids, holder.PID)
	}
	return pids
}
