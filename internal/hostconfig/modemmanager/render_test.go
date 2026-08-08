package modemmanager

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func testEntries() []Entry {
	return []Entry{
		{
			TargetID: "modem-b",
			Matcher: Matcher{
				Kind: MatcherKernelPath, VendorID: "1199", KernelPath: "2-1.4",
			},
		},
		{
			TargetID: "modem-a",
			Matcher: Matcher{
				Kind: MatcherSerial, VendorID: "2c7c", Serial: "SERIAL-001",
			},
		},
	}
}

func TestRenderIsDeterministicAndUsesExactMatchers(t *testing.T) {
	entries := testEntries()
	first, err := Render(entries)
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}
	reversed := []Entry{entries[1], entries[0]}
	second, err := Render(reversed)
	if err != nil {
		t.Fatalf("Render(reversed): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("render output depends on input order")
	}
	text := string(first)
	if strings.Contains(text, "idProduct") {
		t.Fatalf("render unexpectedly pins USB product ID:\n%s", text)
	}
	if !strings.Contains(text, `ATTRS{serial}=="SERIAL-001"`) ||
		!strings.Contains(text, `ATTRS{idVendor}=="1199", KERNELS=="2-1.4"`) {
		t.Fatalf("render lacks exact serial or kernel matcher:\n%s", text)
	}
	if strings.Index(text, "modem-a") >= 0 || strings.Index(text, "modem-b") >= 0 {
		t.Fatal("raw target IDs must not be inserted into udev syntax")
	}
	if !strings.Contains(text, "payload-sha256=") {
		t.Fatal("render lacks payload checksum header")
	}
}

func TestInspectClassifiesAbsentManagedForeignAndTampered(t *testing.T) {
	layout := newTestLayout(t)
	manager := layout.manager(t, nil)

	status, err := manager.Inspect()
	if err != nil {
		t.Fatalf("Inspect(absent): %v", err)
	}
	if status.State != StateAbsent || status.Revision != AbsentRevision {
		t.Fatalf("absent status = %+v", status)
	}

	writeTestFile(t, layout.rulePath, "# administrator-owned rule\n")
	status, err = manager.Inspect()
	if err != nil {
		t.Fatalf("Inspect(foreign): %v", err)
	}
	if status.State != StateForeign || status.Revision == "" {
		t.Fatalf("foreign status = %+v", status)
	}

	data, err := Render(testEntries())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.rulePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Inspect()
	if err != nil {
		t.Fatalf("Inspect(managed): %v", err)
	}
	if status.State != StateManaged || len(status.Entries) != 2 || status.Revision == "" {
		t.Fatalf("managed status = %+v", status)
	}
	managedRevision := status.Revision

	tampered := bytes.Replace(data, []byte("SERIAL-001"), []byte("SERIAL-002"), 1)
	if err := os.WriteFile(layout.rulePath, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Inspect()
	if err != nil {
		t.Fatalf("Inspect(tampered): %v", err)
	}
	if status.State != StateTampered || status.Revision == managedRevision {
		t.Fatalf("tampered status = %+v", status)
	}
}

func TestInspectRejectsManagedPathSymlink(t *testing.T) {
	layout := newTestLayout(t)
	foreign := layout.rulePath + ".foreign"
	writeTestFile(t, foreign, string(mustRender(t, testEntries())))
	if err := os.Symlink(foreign, layout.rulePath); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	status, err := layout.manager(t, nil).Inspect()
	if err != nil {
		t.Fatalf("Inspect(): %v", err)
	}
	if status.State != StateTampered || !strings.Contains(status.Reason, "symbolic link") {
		t.Fatalf("symlink status = %+v", status)
	}
}

func mustRender(t *testing.T, entries []Entry) []byte {
	t.Helper()
	data, err := Render(entries)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
