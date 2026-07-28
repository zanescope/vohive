package config

import (
	"fmt"
	"strings"
)

const (
	minHostFailoverProbeInterval = 2
	maxHostFailoverProbeInterval = 300
	minHostFailoverProbeTimeout  = 1
	maxHostFailoverProbeTimeout  = 60
	maxHostFailoverThreshold     = 20
	maxHostFailoverBackupSeconds = 3600
	maxHostFailoverRouteMetric   = 4096
)

func validateHostFailoverConfig(cfg HostFailoverConfig) error {
	if cfg.ProbeIntervalSeconds < minHostFailoverProbeInterval || cfg.ProbeIntervalSeconds > maxHostFailoverProbeInterval {
		return fmt.Errorf("host_network_failover.probe_interval_seconds must be between %d and %d",
			minHostFailoverProbeInterval, maxHostFailoverProbeInterval)
	}
	if cfg.ProbeTimeoutSeconds < minHostFailoverProbeTimeout || cfg.ProbeTimeoutSeconds > maxHostFailoverProbeTimeout {
		return fmt.Errorf("host_network_failover.probe_timeout_seconds must be between %d and %d",
			minHostFailoverProbeTimeout, maxHostFailoverProbeTimeout)
	}
	if cfg.FailureThreshold < 1 || cfg.FailureThreshold > maxHostFailoverThreshold {
		return fmt.Errorf("host_network_failover.failure_threshold must be between 1 and %d", maxHostFailoverThreshold)
	}
	if cfg.RecoveryThreshold < 1 || cfg.RecoveryThreshold > maxHostFailoverThreshold {
		return fmt.Errorf("host_network_failover.recovery_threshold must be between 1 and %d", maxHostFailoverThreshold)
	}
	if cfg.MinimumBackupSeconds < 0 || cfg.MinimumBackupSeconds > maxHostFailoverBackupSeconds {
		return fmt.Errorf("host_network_failover.minimum_backup_seconds must be between 0 and %d", maxHostFailoverBackupSeconds)
	}
	if cfg.MaximumRouteMetric < 1 || cfg.MaximumRouteMetric > maxHostFailoverRouteMetric {
		return fmt.Errorf("host_network_failover.maximum_route_metric must be between 1 and %d", maxHostFailoverRouteMetric)
	}
	if !cfg.Enabled {
		return nil
	}
	if strings.TrimSpace(cfg.PrimaryInterface) == "" {
		return fmt.Errorf("host_network_failover.primary_interface is required when enabled")
	}
	if len(cfg.CandidateDeviceIDs) == 0 {
		return fmt.Errorf("host_network_failover.candidate_device_ids must contain at least one device when enabled")
	}
	seen := make(map[string]struct{}, len(cfg.CandidateDeviceIDs))
	for index, raw := range cfg.CandidateDeviceIDs {
		deviceID := strings.TrimSpace(raw)
		if deviceID == "" {
			return fmt.Errorf("host_network_failover.candidate_device_ids[%d] cannot be empty", index)
		}
		if _, duplicate := seen[deviceID]; duplicate {
			return fmt.Errorf("host_network_failover.candidate_device_ids contains duplicate device %q", deviceID)
		}
		seen[deviceID] = struct{}{}
	}
	return nil
}
