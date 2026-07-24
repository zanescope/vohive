package device

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/backend"
	"github.com/zanescope/vohive/internal/config"
)

// collectRescanHardware 应把扫描到的 QMI 设备(含探到的 IMEI)转成 CompatibleModem 列表,
// 保留 ControlPath / NetInterface / USBPath / IMEI / Mode 等身份与组态信息。
func TestCollectRescanHardwarePopulatesIMEIAndPaths(t *testing.T) {
	origDiscover := discoverQMIDevicesFn
	discoverQMIDevicesFn = func() ([]QMIDevice, error) {
		return []QMIDevice{
			{ControlPath: "/dev/cdc-wdm0", NetInterface: "wwan0", USBPath: "/sys/bus/usb/devices/1-1"},
		}, nil
	}
	t.Cleanup(func() { discoverQMIDevicesFn = origDiscover })

	origResolve := resolveDiscoveredQMIDeviceFn
	resolveDiscoveredQMIDeviceFn = func(dev QMIDevice, timeout time.Duration, allowIMEIProbe bool) (QMIDevice, string) {
		return dev, "867383058993207"
	}
	t.Cleanup(func() { resolveDiscoveredQMIDeviceFn = origResolve })

	p := &Pool{}

	discovered, _ := discoverQMIDevicesFn()
	liveWorkerIndex := BuildWorkerDiscoveryIndex(nil, false)
	hardware := p.collectRescanHardware(discovered, liveWorkerIndex)

	if len(hardware) != 1 {
		t.Fatalf("expected 1 hardware, got %d", len(hardware))
	}
	hw := hardware[0]
	if hw.IMEI != "867383058993207" {
		t.Errorf("expected IMEI 867383058993207, got %q", hw.IMEI)
	}
	if hw.ControlPath != "/dev/cdc-wdm0" {
		t.Errorf("expected ControlPath /dev/cdc-wdm0, got %q", hw.ControlPath)
	}
	if hw.NetInterface != "wwan0" {
		t.Errorf("expected NetInterface wwan0, got %q", hw.NetInterface)
	}
	if hw.USBPath != "/sys/bus/usb/devices/1-1" {
		t.Errorf("expected USBPath /sys/bus/usb/devices/1-1, got %q", hw.USBPath)
	}
	if hw.TransportType != backend.BackendQMI {
		t.Errorf("expected TransportType qmi, got %q", hw.TransportType)
	}
}

func TestCollectRescanHardwarePopulatesNonQMIIMEIAndPaths(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	raw := "devices:\n- id: dev-mbim\n  device_backend: mbim\n  modem_imei: \"999999999999999\"\n  control_device: /dev/cdc-wdm1\n  interface: wwan1\n"
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitGlobalManager(configPath); err != nil {
		t.Fatalf("InitGlobalManager() error = %v", err)
	}

	origDiscover := discoverQMIDevicesFn
	discoverQMIDevicesFn = func() ([]QMIDevice, error) {
		return nil, nil
	}
	t.Cleanup(func() { discoverQMIDevicesFn = origDiscover })

	origFallback := discoverFallbackModemsFn
	discoverFallbackModemsFn = func() ([]CompatibleModem, error) {
		return []CompatibleModem{{
			ControlPath:   "/dev/cdc-wdm1",
			NetInterface:  "wwan1",
			USBPath:       "/sys/bus/usb/devices/2-2",
			TransportType: backend.BackendMBIM,
			Mode:          "mbim",
		}}, nil
	}
	t.Cleanup(func() { discoverFallbackModemsFn = origFallback })

	origResolveQMI := resolveDiscoveredQMIDeviceFn
	resolveDiscoveredQMIDeviceFn = func(dev QMIDevice, timeout time.Duration, allowIMEIProbe bool) (QMIDevice, string) {
		return dev, ""
	}
	t.Cleanup(func() { resolveDiscoveredQMIDeviceFn = origResolveQMI })

	origResolveCompat := resolveDiscoveredCompatibleModemFn
	resolveDiscoveredCompatibleModemFn = func(dev CompatibleModem, timeout time.Duration) (CompatibleModem, string) {
		if dev.ControlPath == "/dev/cdc-wdm1" {
			return dev, "999999999999999"
		}
		return dev, ""
	}
	t.Cleanup(func() { resolveDiscoveredCompatibleModemFn = origResolveCompat })

	p := &Pool{}

	discovered, _ := discoverQMIDevicesFn()
	liveWorkerIndex := BuildWorkerDiscoveryIndex(nil, false)
	hardware := p.collectRescanHardware(discovered, liveWorkerIndex)

	if len(hardware) != 1 {
		t.Fatalf("expected 1 hardware, got %d", len(hardware))
	}
	hw := hardware[0]
	if hw.IMEI != "999999999999999" {
		t.Errorf("expected IMEI 999999999999999, got %q", hw.IMEI)
	}
	if hw.TransportType != backend.BackendMBIM {
		t.Errorf("expected TransportType mbim, got %q", hw.TransportType)
	}
}

func TestCollectRescanHardwareFallsBackToDeviceScopedATIdentity(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("devices:\n- id: wwan6\n  device_backend: qmi\n  modem_imei: \"866069053194211\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitGlobalManager(configPath); err != nil {
		t.Fatalf("InitGlobalManager() error = %v", err)
	}

	originalResolve := resolveDiscoveredQMIDeviceFn
	originalATProbe := probeIMEICachedFn
	t.Cleanup(func() {
		resolveDiscoveredQMIDeviceFn = originalResolve
		probeIMEICachedFn = originalATProbe
	})

	resolveDiscoveredQMIDeviceFn = func(dev QMIDevice, _ time.Duration, _ bool) (QMIDevice, string) {
		return dev, ""
	}
	var probedPorts []string
	probeIMEICachedFn = func(port string, _ time.Duration) (string, error) {
		probedPorts = append(probedPorts, port)
		if port == "/dev/ttyUSB6" {
			return "866069053194211", nil
		}
		return "", os.ErrNotExist
	}

	discovered := []QMIDevice{{
		ControlPath:  "/dev/cdc-wdm6",
		NetInterface: "wwan6",
		ATPorts:      []string{"/dev/ttyUSB7", "/dev/ttyUSB6"},
	}}
	hardware := (&Pool{}).collectRescanHardware(discovered, BuildWorkerDiscoveryIndex(nil, false))

	if len(hardware) != 1 {
		t.Fatalf("hardware count = %d, want 1", len(hardware))
	}
	if hardware[0].IMEI != "866069053194211" || hardware[0].ATPort != "/dev/ttyUSB6" {
		t.Fatalf("fallback identity = %+v", hardware[0])
	}
	if len(probedPorts) != 1 || probedPorts[0] != "/dev/ttyUSB6" {
		t.Fatalf("probed ports = %v, want only device-scoped /dev/ttyUSB6", probedPorts)
	}
}
