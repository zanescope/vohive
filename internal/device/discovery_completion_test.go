package device

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/internal/config"
)

func initNonQMIRescanConfig(t *testing.T, deviceID, imei string) config.DeviceConfig {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	raw := "devices:\n- id: " + deviceID + "\n  device_backend: at\n  modem_imei: \"" + imei + "\"\n"
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitGlobalManager(configPath); err != nil {
		t.Fatalf("InitGlobalManager() error = %v", err)
	}
	return config.DeviceConfig{
		ID:            deviceID,
		DeviceBackend: backend.BackendAT,
		ModemIMEI:     imei,
		ATPort:        "/dev/ttyUSB2",
		ManagePort:    "/dev/ttyUSB2",
	}
}

func registerInvalidNonQMIWorker(p *Pool, cfg config.DeviceConfig) *Worker {
	worker := &Worker{
		ID:     cfg.ID,
		Config: cfg,
		Pool:   p,
		stop:   make(chan struct{}),
	}
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer:               HealthLayerAT,
		State:               HealthStateInvalid,
		EventType:           "control_health_check_failed",
		Reason:              "control_health_check_failed",
		ConsecutiveFailures: controlHealthFailureThreshold,
		Threshold:           controlHealthFailureThreshold,
	})
	p.mu.Lock()
	p.workers[cfg.ID] = worker
	p.mu.Unlock()
	return worker
}

func TestRescanNoQMIStillRunsCompatibleDiscoveryAndEvictsInvalidNonQMIWorker(t *testing.T) {
	cfg := initNonQMIRescanConfig(t, "at-missing", "860000000000001")
	p := NewPool(&config.Config{Devices: []config.DeviceConfig{cfg}})
	defer p.cancel()
	registerInvalidNonQMIWorker(p, cfg)

	originalQMI := discoverQMIDevicesFn
	discoverQMIDevicesFn = func() ([]QMIDevice, error) {
		return nil, ErrNoQMIDevices
	}
	t.Cleanup(func() { discoverQMIDevicesFn = originalQMI })

	originalFallback := discoverFallbackModemsFn
	var fallbackCalls atomic.Int32
	discoverFallbackModemsFn = func() ([]CompatibleModem, error) {
		fallbackCalls.Add(1)
		return nil, nil
	}
	t.Cleanup(func() { discoverFallbackModemsFn = originalFallback })

	if err := p.RescanAndReconnect(); err != nil {
		t.Fatalf("RescanAndReconnect() error = %v", err)
	}
	if fallbackCalls.Load() != 1 {
		t.Fatalf("compatible discovery calls = %d, want 1", fallbackCalls.Load())
	}
	if worker := p.GetWorker(cfg.ID); worker != nil {
		t.Fatalf("invalid offline non-QMI worker was retained: %p", worker)
	}
}

func TestRescanCompatibleDiscoveryFailureKeepsInvalidNonQMIWorker(t *testing.T) {
	cfg := initNonQMIRescanConfig(t, "at-inconclusive", "860000000000002")
	p := NewPool(&config.Config{Devices: []config.DeviceConfig{cfg}})
	defer p.cancel()
	worker := registerInvalidNonQMIWorker(p, cfg)

	originalQMI := discoverQMIDevicesFn
	discoverQMIDevicesFn = func() ([]QMIDevice, error) {
		return nil, ErrNoQMIDevices
	}
	t.Cleanup(func() { discoverQMIDevicesFn = originalQMI })

	originalFallback := discoverFallbackModemsFn
	fallbackErr := errors.New("sysfs unavailable")
	discoverFallbackModemsFn = func() ([]CompatibleModem, error) {
		return nil, fallbackErr
	}
	t.Cleanup(func() { discoverFallbackModemsFn = originalFallback })

	err := p.RescanAndReconnect()
	if !errors.Is(err, fallbackErr) {
		t.Fatalf("RescanAndReconnect() error = %v, want %v", err, fallbackErr)
	}
	if current := p.GetWorker(cfg.ID); current != worker {
		t.Fatalf("worker changed after incomplete compatible discovery: got %p want %p", current, worker)
	}
}

func TestPrepareConfiguredDeviceBootstrapsUsesFallbackAfterNoQMI(t *testing.T) {
	device := config.DeviceConfig{
		ID:            "at-bootstrap",
		DeviceBackend: backend.BackendAT,
		ModemIMEI:     "860000000000003",
	}
	originalQMI := discoverQMIDevicesFn
	discoverQMIDevicesFn = func() ([]QMIDevice, error) {
		return nil, ErrNoQMIDevices
	}
	t.Cleanup(func() { discoverQMIDevicesFn = originalQMI })

	originalFallback := discoverFallbackModemsFn
	discoverFallbackModemsFn = func() ([]CompatibleModem, error) {
		return []CompatibleModem{{
			USBPath:       "/sys/bus/usb/devices/2-3",
			ATPort:        "/dev/ttyUSB7",
			ATPorts:       []string{"/dev/ttyUSB7"},
			TransportType: backend.BackendAT,
			Mode:          backend.BackendAT,
		}}, nil
	}
	t.Cleanup(func() { discoverFallbackModemsFn = originalFallback })

	originalResolve := resolveDiscoveredCompatibleModemFn
	resolveDiscoveredCompatibleModemFn = func(modem CompatibleModem, _ time.Duration) (CompatibleModem, string) {
		return modem, device.ModemIMEI
	}
	t.Cleanup(func() { resolveDiscoveredCompatibleModemFn = originalResolve })

	p := NewPool(&config.Config{Devices: []config.DeviceConfig{device}})
	defer p.cancel()
	plan := p.prepareConfiguredDeviceBootstraps([]config.DeviceConfig{device})

	if len(plan.devices) != 1 {
		t.Fatalf("planned devices = %d, want 1", len(plan.devices))
	}
	if got := plan.devices[0].ATPort; got != "/dev/ttyUSB7" {
		t.Fatalf("planned AT port = %q, want /dev/ttyUSB7", got)
	}
	if got := plan.devices[0].USBPath; got != "/sys/bus/usb/devices/2-3" {
		t.Fatalf("planned USB path = %q, want /sys/bus/usb/devices/2-3", got)
	}
	if _, err := plan.discovery.Get(); err != nil {
		t.Fatalf("bootstrap discovery cache retained no-QMI sentinel: %v", err)
	}
}
