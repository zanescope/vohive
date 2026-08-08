package device

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/zanescope/vohive/internal/config"
	"github.com/zanescope/vohive/pkg/logger"
)

type configuredDeviceBootstrapPlan struct {
	devices   []config.DeviceConfig
	discovery *qmiBootstrapDiscoveryCache
}

func (p *Pool) prepareConfiguredDeviceBootstraps(devices []config.DeviceConfig) configuredDeviceBootstrapPlan {
	planned := append([]config.DeviceConfig(nil), devices...)
	discovered, err := discoverQMIDevicesFn()
	if errors.Is(err, ErrNoQMIDevices) {
		discovered = nil
		err = nil
	}
	cache := &qmiBootstrapDiscoveryCache{
		loaded: true,
		list:   append([]QMIDevice(nil), discovered...),
		err:    err,
	}
	if err != nil {
		logger.Warn("startup QMI discovery failed; trying existing runtime attachments", "err", err)
		return configuredDeviceBootstrapPlan{devices: planned, discovery: cache}
	}

	liveWorkerIndex := BuildWorkerDiscoveryIndex(p.GetAllWorkers(), false)
	hardware, err := p.collectRescanHardwareForDevicesComplete(discovered, liveWorkerIndex, devices, cache)
	if err != nil {
		logger.Warn("startup compatible modem discovery failed; trying existing runtime attachments", "err", err)
		return configuredDeviceBootstrapPlan{devices: planned, discovery: cache}
	}
	resolved := ResolveDeviceIdentities(hardware, devices)
	matchedByID := make(map[string]MatchedPair, len(resolved.Matched))
	for _, pair := range resolved.Matched {
		matchedByID[pair.Config.ID] = pair
	}

	for i := range planned {
		pair, ok := matchedByID[planned[i].ID]
		if !ok {
			continue
		}
		cfg := pair.Config
		if pair.BackfillIMEI != "" {
			cfg.ModemIMEI = pair.BackfillIMEI
		}
		planned[i] = applyStartupHardwareAttachment(cfg, pair.Hardware)
	}

	cache.mu.Lock()
	cache.list = append([]QMIDevice(nil), discovered...)
	cache.mu.Unlock()
	logger.Info("startup hardware identity resolution completed",
		"configured", len(devices),
		"discovered_qmi", len(discovered),
		"matched", len(resolved.Matched),
		"offline", len(resolved.Offline),
		"degraded", len(resolved.Degraded))
	return configuredDeviceBootstrapPlan{devices: planned, discovery: cache}
}

func applyStartupHardwareAttachment(cfg config.DeviceConfig, hardware CompatibleModem) config.DeviceConfig {
	if requiresQMICore(cfg) {
		return applyQMIManagedAttachment(cfg, QMIDevice{
			ControlPath:  strings.TrimSpace(hardware.ControlPath),
			NetInterface: strings.TrimSpace(hardware.NetInterface),
			USBPath:      strings.TrimSpace(hardware.USBPath),
			ATPort:       strings.TrimSpace(hardware.ATPort),
			AudioDevice:  strings.TrimSpace(hardware.AudioDevice),
		})
	}
	if value := strings.TrimSpace(hardware.NetInterface); value != "" {
		cfg.Interface = value
	}
	if value := strings.TrimSpace(hardware.USBPath); value != "" {
		cfg.USBPath = value
	}
	if value := strings.TrimSpace(hardware.ATPort); value != "" {
		cfg.ATPort = value
		cfg.ManagePort = value
	}
	if value := strings.TrimSpace(hardware.ControlPath); value != "" {
		cfg.ControlDevice = value
		cfg.QMIDevice = value
	}
	if value := strings.TrimSpace(hardware.AudioDevice); value != "" {
		cfg.AudioDevice = value
	}
	return cfg
}

func (p *Pool) startConfiguredDeviceBootstrapBatch(devices []config.DeviceConfig) {
	if p == nil {
		return
	}
	if len(devices) == 0 {
		p.startPoolBackgroundServicesOnce()
		return
	}

	// Keep the cold-start identity pass mutually exclusive with udev/manual
	// rescans. No Worker opens a long-lived QMI client until this pass finishes.
	p.rescanMu.Lock()
	plan := p.prepareConfiguredDeviceBootstraps(devices)
	p.rescanMu.Unlock()
	p.startPoolBackgroundServicesOnce()

	bootstrapConcurrency := p.cfg.EffectiveWorkerBootstrapConcurrency()
	logger.Info("starting configured worker bootstrap batch",
		"devices", len(plan.devices),
		"worker_bootstrap_concurrency", bootstrapConcurrency,
		"state_sync_concurrency", cap(p.startupSyncSem))
	runBoundedConfiguredDeviceBootstraps(p.ctx, plan.devices, bootstrapConcurrency, func(cfg config.DeviceConfig) {
		p.startConfiguredDeviceBootstrapWithDiscovery(cfg, "start_all", plan.discovery)
	})
}

func runBoundedConfiguredDeviceBootstraps(ctx context.Context, devices []config.DeviceConfig, concurrency int, start func(config.DeviceConfig)) {
	if start == nil || len(devices) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if concurrency < 1 {
		concurrency = 1
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, cfg := range devices {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(devCfg config.DeviceConfig) {
			defer wg.Done()
			defer func() { <-sem }()
			start(devCfg)
		}(cfg)
	}
	wg.Wait()
}
