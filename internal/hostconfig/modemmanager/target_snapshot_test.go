package modemmanager

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestInstallRejectsTargetSnapshotChangeBeforeWrite(t *testing.T) {
	layout := newTestLayout(t)
	path := layout.addUSBDevice(t, "1-2", "2c7c", "0125", "ACTUAL-SERIAL")
	runner := &fakeRunner{}
	manager := layout.manager(t, runner)

	_, err := manager.Install(context.Background(), Request{
		Targets: []Target{{ID: "device-1", USBPath: path}},
		ExpectedEntries: []Entry{{
			TargetID: "device-1",
			Matcher: Matcher{
				Kind: MatcherSerial, VendorID: "2c7c", Serial: "PREVIEW-SERIAL",
			},
		}},
	})
	if !errors.Is(err, ErrTargetSnapshotConflict) {
		t.Fatalf("Install() error = %v, want ErrTargetSnapshotConflict", err)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("snapshot conflict reloaded udev rules: %+v", calls)
	}
	if _, statErr := os.Lstat(layout.rulePath); !os.IsNotExist(statErr) {
		t.Fatalf("snapshot conflict wrote the rule: %v", statErr)
	}
}
