package modemmanager

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type testLayout struct {
	mount    string
	usbRoot  string
	rulePath string
}

func newTestLayout(t *testing.T) testLayout {
	t.Helper()
	base := t.TempDir()
	mount := filepath.Join(base, "sys")
	usbRoot := filepath.Join(mount, "bus", "usb", "devices")
	rulePath := filepath.Join(base, "etc", "udev", "rules.d", "78-mm-vohive-managed.rules")
	for _, dir := range []string{usbRoot, filepath.Dir(rulePath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", dir, err)
		}
	}
	return testLayout{mount: mount, usbRoot: usbRoot, rulePath: rulePath}
}

func (l testLayout) addUSBDevice(t *testing.T, kernelPath, vendorID, productID, serial string) string {
	t.Helper()
	path := filepath.Join(l.usbRoot, kernelPath)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
	writeTestFile(t, filepath.Join(path, "idVendor"), vendorID+"\n")
	writeTestFile(t, filepath.Join(path, "idProduct"), productID+"\n")
	if serial != "" {
		writeTestFile(t, filepath.Join(path, "serial"), serial+"\n")
	}
	return path
}

func (l testLayout) manager(t *testing.T, runner Runner) *Manager {
	t.Helper()
	manager, err := New(Options{
		RulePath: l.rulePath, SysfsRoot: l.usbRoot, SysfsMount: l.mount, Runner: runner,
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return manager
}

func writeTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func TestResolveMatcherChoosesStableSerialOrExactKernelPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		serial   string
		setup    func(testLayout, *testing.T)
		wantKind MatcherKind
	}{
		{name: "normal unique serial", serial: "UNIQUE-001", wantKind: MatcherSerial},
		{
			name:   "duplicate serial",
			serial: "DUPLICATE",
			setup: func(layout testLayout, t *testing.T) {
				layout.addUSBDevice(t, "1-9", "2c7c", "0800", "DUPLICATE")
			},
			wantKind: MatcherKernelPath,
		},
		{name: "missing serial", wantKind: MatcherKernelPath},
		{name: "unsafe serial", serial: `bad"serial`, wantKind: MatcherKernelPath},
		{name: "placeholder unknown", serial: "UnKnOwN", wantKind: MatcherKernelPath},
		{name: "placeholder default", serial: "DEFAULT", wantKind: MatcherKernelPath},
		{name: "placeholder sequential hex", serial: "0123456789ABCDEF", wantKind: MatcherKernelPath},
		{name: "placeholder shifted hex", serial: "1234567890abcdef", wantKind: MatcherKernelPath},
		{name: "all zeroes", serial: "00000000", wantKind: MatcherKernelPath},
		{name: "all f", serial: "ffffffff", wantKind: MatcherKernelPath},
		{name: "low entropy", serial: "AAAAAB", wantKind: MatcherKernelPath},
		{name: "too short", serial: "A1B2C", wantKind: MatcherKernelPath},
	} {
		t.Run(test.name, func(t *testing.T) {
			layout := newTestLayout(t)
			targetPath := layout.addUSBDevice(t, "1-2.3", "2c7c", "0125", test.serial)
			if test.setup != nil {
				test.setup(layout, t)
			}
			manager := layout.manager(t, nil)

			matcher, err := manager.ResolveMatcher(Target{ID: "modem-a", USBPath: targetPath})
			if err != nil {
				t.Fatalf("ResolveMatcher(): %v", err)
			}
			if matcher.Kind != test.wantKind || matcher.VendorID != "2c7c" {
				t.Fatalf("matcher = %+v, want kind %q and vendor 2c7c", matcher, test.wantKind)
			}
			if test.wantKind == MatcherSerial {
				if matcher.Serial != test.serial || matcher.KernelPath != "" {
					t.Fatalf("matcher = %+v, want vendor+serial matcher", matcher)
				}
			} else if matcher.KernelPath != "1-2.3" || matcher.Serial != "" {
				t.Fatalf("matcher = %+v, want exact vendor+KERNELS fallback", matcher)
			}
		})
	}
}

func TestResolveMatcherWalksClassSymlinkToIndexedUSBAncestor(t *testing.T) {
	layout := newTestLayout(t)
	realUSB := filepath.Join(layout.mount, "devices", "platform", "xhci", "usb1", "1-4.2")
	if err := os.MkdirAll(realUSB, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(realUSB, "idVendor"), "2c7c\n")
	writeTestFile(t, filepath.Join(realUSB, "idProduct"), "0125\n")
	writeTestFile(t, filepath.Join(realUSB, "serial"), "CLASS-LINK-1\n")
	if err := os.Symlink(realUSB, filepath.Join(layout.usbRoot, "1-4.2")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation is unavailable: %v", err)
		}
		t.Fatal(err)
	}

	wwanTarget := filepath.Join(realUSB, "1-4.2:1.4", "wwan", "wwan0")
	if err := os.MkdirAll(wwanTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	classRoot := filepath.Join(layout.mount, "class", "wwan")
	if err := os.MkdirAll(classRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	classPath := filepath.Join(classRoot, "wwan0")
	if err := os.Symlink(wwanTarget, classPath); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation is unavailable: %v", err)
		}
		t.Fatal(err)
	}

	matcher, err := layout.manager(t, nil).ResolveMatcher(Target{ID: "modem-a", USBPath: classPath})
	if err != nil {
		t.Fatalf("ResolveMatcher(class symlink): %v", err)
	}
	if matcher.Kind != MatcherSerial || matcher.Serial != "CLASS-LINK-1" {
		t.Fatalf("matcher = %+v, want unique serial through class symlink", matcher)
	}
}

func TestResolveMatcherRejectsPathsOutsideSysfsAndNonUSBDevices(t *testing.T) {
	layout := newTestLayout(t)
	manager := layout.manager(t, nil)
	outside := filepath.Join(filepath.Dir(layout.mount), "outside", "1-2")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(outside, "idVendor"), "2c7c\n")
	writeTestFile(t, filepath.Join(outside, "idProduct"), "0125\n")

	_, err := manager.ResolveMatcher(Target{ID: "outside", USBPath: outside})
	if !errors.Is(err, ErrUnsafeUSBPath) {
		t.Fatalf("outside path error = %v, want ErrUnsafeUSBPath", err)
	}

	mhiPath := filepath.Join(layout.mount, "bus", "mhi", "devices", "mhi0")
	if err := os.MkdirAll(mhiPath, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = manager.ResolveMatcher(Target{ID: "mhi", USBPath: mhiPath})
	if !errors.Is(err, ErrNoStableMatcher) {
		t.Fatalf("MHI path error = %v, want ErrNoStableMatcher", err)
	}
}
