package qmicore

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDetectModemManagerOwnershipFromHolderCgroup(t *testing.T) {
	procRoot := t.TempDir()
	writeProcessFixture(t, procRoot, 320, "qmi-proxy", 1, "0::/system.slice/ModemManager.service")

	got := detectModemManagerOwnershipAt(procRoot, 320)
	if !got.Owned || got.Reason != "holder_cgroup" {
		t.Fatalf("ownership=%+v, want holder_cgroup", got)
	}
	if got.Cgroup != "0::/system.slice/ModemManager.service" {
		t.Fatalf("cgroup=%q, want ModemManager service cgroup", got.Cgroup)
	}
}

func TestDetectModemManagerOwnershipFromParentProcess(t *testing.T) {
	procRoot := t.TempDir()
	writeProcessFixture(t, procRoot, 410, "ModemManager", 1, "0::/system.slice/example.service")
	writeProcessFixture(t, procRoot, 411, "qmi-proxy", 410, "0::/user.slice/session.scope")

	got := detectModemManagerOwnershipAt(procRoot, 411)
	if !got.Owned || got.Reason != "ancestor_process" {
		t.Fatalf("ownership=%+v, want ancestor_process", got)
	}
}

func TestDetectModemManagerOwnershipIgnoresUnrelatedProxy(t *testing.T) {
	procRoot := t.TempDir()
	writeProcessFixture(t, procRoot, 520, "qmi-proxy", 1, "0::/system.slice/vohive.service")

	got := detectModemManagerOwnershipAt(procRoot, 520)
	if got.Owned {
		t.Fatalf("ownership=%+v, want unrelated proxy", got)
	}
}

func TestDetectModemManagerOwnershipHandlesVanishedProcess(t *testing.T) {
	got := detectModemManagerOwnershipAt(t.TempDir(), 99999)
	if got.Owned || got.Reason != "" || got.Cgroup != "" {
		t.Fatalf("ownership=%+v, want empty result for vanished process", got)
	}
}

func TestModemManagerServiceCgroupMatchesExactComponent(t *testing.T) {
	if !modemManagerServiceCgroup("7:name=systemd:/system.slice/ModemManager.service") {
		t.Fatal("expected ModemManager.service component to match")
	}
	if modemManagerServiceCgroup("0::/system.slice/NotModemManager.service") {
		t.Fatal("unexpected partial service-name match")
	}
}

func writeProcessFixture(t *testing.T, procRoot string, pid int, comm string, parentPID int, cgroup string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create proc fixture: %v", err)
	}
	files := map[string]string{
		"comm":   comm + "\n",
		"status": "Name:\t" + comm + "\nPPid:\t" + strconv.Itoa(parentPID) + "\n",
		"cgroup": cgroup + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write proc fixture %s: %v", name, err)
		}
	}
}
