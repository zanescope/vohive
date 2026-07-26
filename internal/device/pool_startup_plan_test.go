package device

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
)

func TestPrepareConfiguredDeviceBootstrapsProbesEachQMIOnce(t *testing.T) {
	origDiscover := discoverQMIDevicesFn
	origResolve := resolveDiscoveredQMIDeviceFn
	t.Cleanup(func() {
		discoverQMIDevicesFn = origDiscover
		resolveDiscoveredQMIDeviceFn = origResolve
	})

	discoverCalls := 0
	discovered := make([]QMIDevice, 3)
	devices := make([]config.DeviceConfig, 3)
	wantedIMEI := make(map[string]string, 3)
	for i := range discovered {
		control := fmt.Sprintf("/dev/cdc-wdm%d", i)
		imei := fmt.Sprintf("860000000000%d00", i)
		discovered[i] = QMIDevice{
			ControlPath:  control,
			NetInterface: fmt.Sprintf("wwan%d", i),
			USBPath:      fmt.Sprintf("/sys/bus/usb/devices/1-%d", i+1),
		}
		devices[i] = config.DeviceConfig{
			ID:            fmt.Sprintf("dev%d", i),
			ModemIMEI:     imei,
			DeviceBackend: "qmi",
		}
		wantedIMEI[control] = imei
	}
	discoverQMIDevicesFn = func() ([]QMIDevice, error) {
		discoverCalls++
		return append([]QMIDevice(nil), discovered...), nil
	}
	probeCalls := make(map[string]int, len(discovered))
	resolveDiscoveredQMIDeviceFn = func(dev QMIDevice, _ time.Duration, _ bool) (QMIDevice, string) {
		probeCalls[dev.ControlPath]++
		return dev, wantedIMEI[dev.ControlPath]
	}

	p := NewPool(&config.Config{Devices: devices})
	defer p.cancel()
	plan := p.prepareConfiguredDeviceBootstraps(devices)

	if discoverCalls != 1 {
		t.Fatalf("discovery calls = %d, want 1", discoverCalls)
	}
	for i, planned := range plan.devices {
		if probeCalls[discovered[i].ControlPath] != 1 {
			t.Fatalf("probe calls for %s = %d, want 1", discovered[i].ControlPath, probeCalls[discovered[i].ControlPath])
		}
		if planned.ControlDevice != discovered[i].ControlPath || planned.Interface != discovered[i].NetInterface {
			t.Fatalf("planned attachment %d = %#v, want control=%s interface=%s", i, planned, discovered[i].ControlPath, discovered[i].NetInterface)
		}
		if got, ok := plan.discovery.Identity(discovered[i]); !ok || got != devices[i].ModemIMEI {
			t.Fatalf("cached identity for %s = %q, %v", discovered[i].ControlPath, got, ok)
		}
	}
}

func TestQMIBootstrapDiscoveryCacheIsConcurrentSingleFlight(t *testing.T) {
	origDiscover := discoverQMIDevicesFn
	t.Cleanup(func() { discoverQMIDevicesFn = origDiscover })

	var calls atomic.Int32
	discoverQMIDevicesFn = func() ([]QMIDevice, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return []QMIDevice{{ControlPath: "/dev/cdc-wdm0"}}, nil
	}
	cache := &qmiBootstrapDiscoveryCache{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cache.Get(); err != nil {
				t.Errorf("Get() error = %v", err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("discover calls = %d, want 1", got)
	}
}

func TestRunBoundedConfiguredDeviceBootstrapsLimitsConcurrency(t *testing.T) {
	devices := make([]config.DeviceConfig, 8)
	for i := range devices {
		devices[i].ID = fmt.Sprintf("dev%d", i)
	}

	var active atomic.Int32
	var maximum atomic.Int32
	var completed atomic.Int32
	runBoundedConfiguredDeviceBootstraps(context.Background(), devices, 2, func(config.DeviceConfig) {
		current := active.Add(1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		active.Add(-1)
		completed.Add(1)
	})

	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", got)
	}
	if got := completed.Load(); got != int32(len(devices)) {
		t.Fatalf("completed = %d, want %d", got, len(devices))
	}
}
