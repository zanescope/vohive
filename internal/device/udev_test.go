package device

import (
	"testing"
	"time"

	"github.com/zanescope/vohive/internal/config"
)

func TestUdevWatcherTreatsWWANPortEventsAsModemEvents(t *testing.T) {
	w := NewUdevWatcher(nil)
	event := []byte("add@/devices/platform/soc@0/4080000.remoteproc/wwan/wwan0/wwan0qmi0\x00ACTION=add\x00SUBSYSTEM=wwan\x00DEVTYPE=wwan_port\x00DEVNAME=/dev/wwan0qmi0\x00")

	if !w.isModemEvent(event) {
		t.Fatal("isModemEvent() = false, want true for SUBSYSTEM=wwan QMI port")
	}
}

func TestUdevWatcherKeepsIgnoringNonWWANNetEvents(t *testing.T) {
	w := NewUdevWatcher(nil)
	event := []byte("add@/devices/virtual/net/eth0\x00ACTION=add\x00SUBSYSTEM=net\x00INTERFACE=eth0\x00")

	if w.isModemEvent(event) {
		t.Fatal("isModemEvent() = true, want false for eth0 net event")
	}
}

func TestModemUeventMatchesOnlySameUSBTopology(t *testing.T) {
	event, ok := parseModemUevent([]byte("add@/devices/pci0000:00/usb1/1-10/1-10:1.4/usbmisc/cdc-wdm0\x00ACTION=add\x00SUBSYSTEM=usbmisc\x00DEVNAME=/dev/cdc-wdm0\x00"))
	if !ok {
		t.Fatal("parseModemUevent() = false, want true")
	}
	if modemUeventMatchesRecoveryIdentity(event, modemRebootRecoveryIdentity{
		USBPath:       "/sys/bus/usb/devices/1-1",
		ControlDevice: "/dev/cdc-wdm0",
	}) {
		t.Fatal("event for USB 1-10 matched stale cdc-wdm0 identity on USB 1-1")
	}
	if !modemUeventMatchesRecoveryIdentity(event, modemRebootRecoveryIdentity{
		USBPath:       "/sys/bus/usb/devices/1-10",
		ControlDevice: "/dev/cdc-wdm7",
	}) {
		t.Fatal("event for USB 1-10 did not match the same physical topology")
	}
}

func TestModemUeventFallsBackToWWANPortIdentityWithoutUSBTopology(t *testing.T) {
	event, ok := parseModemUevent([]byte("add@/devices/platform/soc/wwan/wwan2/wwan2qmi0\x00ACTION=add\x00SUBSYSTEM=wwan\x00DEVNAME=/dev/wwan2qmi0\x00"))
	if !ok {
		t.Fatal("parseModemUevent() = false, want true")
	}
	if !modemUeventMatchesRecoveryIdentity(event, modemRebootRecoveryIdentity{
		USBPath:       "/sys/class/wwan/wwan2",
		ControlDevice: "/dev/wwan2qmi0",
	}) {
		t.Fatal("WWAN port event did not match control device identity")
	}
}

func TestModemUeventDoesNotUseDeviceNameWhenOnlyOneSideHasUSBTopology(t *testing.T) {
	usbEvent, ok := parseModemUevent([]byte("add@/devices/pci0000:00/usb1/1-4/1-4:1.4/usbmisc/cdc-wdm0\x00ACTION=add\x00SUBSYSTEM=usbmisc\x00DEVNAME=/dev/cdc-wdm0\x00"))
	if !ok {
		t.Fatal("parse USB modem event failed")
	}
	if modemUeventMatchesRecoveryIdentity(usbEvent, modemRebootRecoveryIdentity{
		ControlDevice: "/dev/cdc-wdm0",
	}) {
		t.Fatal("USB event matched an identity with unknown topology by reused device name")
	}

	platformEvent, ok := parseModemUevent([]byte("add@/devices/platform/soc/wwan/wwan0/wwan0qmi0\x00ACTION=add\x00SUBSYSTEM=wwan\x00DEVNAME=/dev/wwan0qmi0\x00"))
	if !ok {
		t.Fatal("parse platform modem event failed")
	}
	if modemUeventMatchesRecoveryIdentity(platformEvent, modemRebootRecoveryIdentity{
		USBPath:       "/sys/bus/usb/devices/1-4",
		ControlDevice: "/dev/wwan0qmi0",
	}) {
		t.Fatal("platform event matched a USB identity by reused device name")
	}
}

func TestUdevAddArmsImmediateReprobeForMatchingTerminalQMIWorker(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	worker := &Worker{
		ID: "dev-a",
		Config: config.DeviceConfig{
			ID:            "dev-a",
			USBPath:       "/sys/bus/usb/devices/1-4.2.4",
			ControlDevice: "/dev/cdc-wdm12",
			Interface:     "wwan12",
			DeviceBackend: "qmi",
		},
	}
	p.mu.Lock()
	p.workers[worker.ID] = worker
	p.mu.Unlock()

	if allowed, _ := p.transportRecovery.AllowTerminalWorkerReprobe(worker.ID); !allowed {
		t.Fatal("failed to seed terminal reprobe cooldown")
	}
	event, ok := parseModemUevent([]byte("add@/devices/pci0000:00/usb1/1-4/1-4.2/1-4.2.4/1-4.2.4:1.4/usbmisc/cdc-wdm12\x00ACTION=add\x00SUBSYSTEM=usbmisc\x00DEVNAME=/dev/cdc-wdm12\x00"))
	if !ok {
		t.Fatal("parseModemUevent() = false")
	}

	watcher := NewUdevWatcher(p)
	watcher.debounce = time.Hour
	defer watcher.Stop()
	watcher.scheduleModemEvent(event)

	// The kernel can publish the add event before the old Worker observes the
	// disconnect. The permit must survive that ordering, but is consumed only
	// after terminal health is recorded and a rescan requests a reprobe.
	worker.RecordWatchdogEvent(WatchdogEvent{
		Layer:     HealthLayerQMI,
		State:     HealthStateFailed,
		EventType: "transport_recovery_giveup",
	})
	if allowed, retryAfter := p.transportRecovery.AllowTerminalWorkerReprobe(worker.ID); !allowed || retryAfter != 0 {
		t.Fatalf("matching udev add reprobe = (%v, %s), want (true, 0)", allowed, retryAfter)
	}
	if allowed, _ := p.transportRecovery.AllowTerminalWorkerReprobe(worker.ID); allowed {
		t.Fatal("matching udev add permit was not single-use")
	}
}

func TestUdevEventsDoNotArmWrongTerminalWorker(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{
			name: "remove for matching topology",
			raw:  "remove@/devices/pci0000:00/usb1/1-4/1-4.2/1-4.2.4/1-4.2.4:1.4/usbmisc/cdc-wdm12\x00ACTION=remove\x00SUBSYSTEM=usbmisc\x00DEVNAME=/dev/cdc-wdm12\x00",
		},
		{
			name: "add for another topology",
			raw:  "add@/devices/pci0000:00/usb1/1-4/1-4.2/1-4.2.3/1-4.2.3:1.4/usbmisc/cdc-wdm11\x00ACTION=add\x00SUBSYSTEM=usbmisc\x00DEVNAME=/dev/cdc-wdm11\x00",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := NewPool(&config.Config{})
			defer p.cancel()
			worker := &Worker{
				ID: "dev-a",
				Config: config.DeviceConfig{
					ID:            "dev-a",
					USBPath:       "/sys/bus/usb/devices/1-4.2.4",
					ControlDevice: "/dev/cdc-wdm12",
					Interface:     "wwan12",
					DeviceBackend: "qmi",
				},
			}
			worker.RecordWatchdogEvent(WatchdogEvent{
				Layer:     HealthLayerQMI,
				State:     HealthStateFailed,
				EventType: "transport_recovery_giveup",
			})
			p.mu.Lock()
			p.workers[worker.ID] = worker
			p.mu.Unlock()

			if allowed, _ := p.transportRecovery.AllowTerminalWorkerReprobe(worker.ID); !allowed {
				t.Fatal("failed to seed terminal reprobe cooldown")
			}
			event, ok := parseModemUevent([]byte(test.raw))
			if !ok {
				t.Fatal("parseModemUevent() = false")
			}
			if noted := p.noteTerminalWorkerUdevAdd(event); noted {
				t.Fatal("unrelated udev event armed a terminal Worker reprobe")
			}
			if allowed, _ := p.transportRecovery.AllowTerminalWorkerReprobe(worker.ID); allowed {
				t.Fatal("unrelated udev event bypassed terminal Worker cooldown")
			}
		})
	}
}

func TestPoolAttributesUeventToOneActiveRecovery(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	p.workers["dev-a"] = &Worker{
		ID: "dev-a",
		Config: config.DeviceConfig{
			ID:            "dev-a",
			USBPath:       "/sys/bus/usb/devices/1-1",
			ControlDevice: "/dev/cdc-wdm0",
		},
	}
	p.workers["dev-b"] = &Worker{
		ID: "dev-b",
		Config: config.DeviceConfig{
			ID:            "dev-b",
			USBPath:       "/sys/bus/usb/devices/1-10",
			ControlDevice: "/dev/cdc-wdm1",
		},
	}
	if !p.beginModemRebootRecovery("dev-a") || !p.beginModemRebootRecovery("dev-b") {
		t.Fatal("failed to start recovery fixtures")
	}
	defer p.finishModemRebootRecovery("dev-a")
	defer p.finishModemRebootRecovery("dev-b")

	event, ok := parseModemUevent([]byte("add@/devices/pci0000:00/usb1/1-10/1-10:1.4/usbmisc/cdc-wdm1\x00ACTION=add\x00SUBSYSTEM=usbmisc\x00DEVNAME=/dev/cdc-wdm1\x00"))
	if !ok {
		t.Fatal("parseModemUevent() = false, want true")
	}
	target, matched := p.modemRebootRecoveryTargetForUevent(event)
	if !matched || target.deviceID != "dev-b" {
		t.Fatalf("target = (%q, %v), want (dev-b, true)", target.deviceID, matched)
	}
	if !p.wakeModemRebootRecoveryTarget(target, "test") {
		t.Fatal("targeted wake = false, want true")
	}
	select {
	case <-p.modemRebootWakeChannel("dev-b"):
	default:
		t.Fatal("dev-b did not receive targeted wake")
	}
	select {
	case <-p.modemRebootWakeChannel("dev-a"):
		t.Fatal("unrelated dev-a recovery was woken")
	default:
	}
}

func TestPoolDoesNotAttributeUnknownUeventToAnyRecovery(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	p.workers["dev-a"] = &Worker{
		ID:     "dev-a",
		Config: config.DeviceConfig{ID: "dev-a", USBPath: "/sys/bus/usb/devices/1-1"},
	}
	if !p.beginModemRebootRecovery("dev-a") {
		t.Fatal("failed to start recovery fixture")
	}
	defer p.finishModemRebootRecovery("dev-a")

	event, ok := parseModemUevent([]byte("add@/devices/pci0000:00/usb2/2-3/2-3:1.4/usbmisc/cdc-wdm9\x00ACTION=add\x00SUBSYSTEM=usbmisc\x00DEVNAME=/dev/cdc-wdm9\x00"))
	if !ok {
		t.Fatal("parseModemUevent() = false, want true")
	}
	if target, matched := p.modemRebootRecoveryTargetForUevent(event); matched || target.deviceID != "" {
		t.Fatalf("unknown target = (%q, %v), want empty", target.deviceID, matched)
	}
}

func TestStaleUeventCannotWakeReplacementRecoveryGeneration(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	p.workers["dev-a"] = &Worker{
		ID:     "dev-a",
		Config: config.DeviceConfig{ID: "dev-a", USBPath: "/sys/bus/usb/devices/1-1"},
	}
	if !p.beginModemRebootRecovery("dev-a") {
		t.Fatal("failed to start first recovery")
	}
	event, _ := parseModemUevent([]byte("add@/devices/pci0000:00/usb1/1-1/1-1:1.4/usbmisc/cdc-wdm0\x00ACTION=add\x00SUBSYSTEM=usbmisc\x00DEVNAME=/dev/cdc-wdm0\x00"))
	staleTarget, matched := p.modemRebootRecoveryTargetForUevent(event)
	if !matched {
		t.Fatal("failed to attribute first recovery")
	}
	p.finishModemRebootRecovery("dev-a")
	if !p.beginModemRebootRecovery("dev-a") {
		t.Fatal("failed to start replacement recovery")
	}
	defer p.finishModemRebootRecovery("dev-a")
	if p.wakeModemRebootRecoveryTarget(staleTarget, "stale_test") {
		t.Fatal("stale uevent target woke replacement recovery generation")
	}
}

func TestCapturedRecoveryIdentitySurvivesWorkerRemovalBeforeBegin(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	p.workers["dev-a"] = &Worker{
		ID: "dev-a",
		Config: config.DeviceConfig{
			ID:            "dev-a",
			USBPath:       "/sys/bus/usb/devices/1-4",
			ControlDevice: "/dev/cdc-wdm4",
		},
	}
	captured := p.captureModemRebootRecoveryIdentity("dev-a")
	if captured == nil {
		t.Fatal("captureModemRebootRecoveryIdentity() = nil")
	}
	p.mu.Lock()
	delete(p.workers, "dev-a")
	p.mu.Unlock()
	if !p.beginModemRebootRecovery("dev-a", *captured) {
		t.Fatal("beginModemRebootRecovery() = false")
	}
	defer p.finishModemRebootRecovery("dev-a")

	event, ok := parseModemUevent([]byte("add@/devices/pci0000:00/usb1/1-4/1-4:1.4/usbmisc/cdc-wdm4\x00ACTION=add\x00SUBSYSTEM=usbmisc\x00DEVNAME=/dev/cdc-wdm4\x00"))
	if !ok {
		t.Fatal("parseModemUevent() = false")
	}
	target, matched := p.modemRebootRecoveryTargetForUevent(event)
	if !matched || target.deviceID != "dev-a" {
		t.Fatalf("captured recovery target = (%q, %v), want dev-a", target.deviceID, matched)
	}
}

func TestUdevDebounceEpochRejectsExpiredCallback(t *testing.T) {
	p := NewPool(&config.Config{})
	defer p.cancel()
	rescanned := make(chan struct{}, 2)
	p.rescanAndReconnectForTest = func() error {
		rescanned <- struct{}{}
		return nil
	}

	watcher := NewUdevWatcher(p)
	defer watcher.Stop()
	firstCallback := make(chan struct{})
	releaseFirst := make(chan struct{})
	watcher.beforeTimerCallback = func(epoch uint64) {
		if epoch == 1 {
			close(firstCallback)
			<-releaseFirst
		}
	}
	watcher.debounce = time.Millisecond
	watcher.scheduleRescan()
	select {
	case <-firstCallback:
	case <-time.After(time.Second):
		t.Fatal("first timer callback did not start")
	}

	watcher.debounce = 80 * time.Millisecond
	watcher.scheduleRescan()
	close(releaseFirst)
	select {
	case <-rescanned:
		t.Fatal("expired callback executed the newer debounce batch early")
	case <-time.After(30 * time.Millisecond):
	}
	select {
	case <-rescanned:
	case <-time.After(time.Second):
		t.Fatal("current debounce epoch did not execute")
	}
	select {
	case <-rescanned:
		t.Fatal("debounce epochs executed more than one rescan")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestKernelUeventSubscriptionUsesMulticastGroupOne(t *testing.T) {
	if kernelUeventMulticastGroup != 1 {
		t.Fatalf("kernel uevent multicast group=%d want 1", kernelUeventMulticastGroup)
	}
}
