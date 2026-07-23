package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerGuardsEveryUnresolvedTransactionBeforeInstalledEarlyExit(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	guard := strings.Index(text, `existing_phase=$(extract_json_string phase "$STATE_ROOT/state.json")`)
	record := strings.LastIndex(text, "record_existing_deployment\n")
	if guard < 0 || record < 0 || guard > record {
		t.Fatalf("transaction-state guard must run before the installed-deployment early exit: guard=%d record=%d", guard, record)
	}
	for _, required := range []string{
		"completed|rolled_back|failed",
		"manual_recovery_required)",
		"existing update state is invalid",
		"an install or update transaction is unresolved",
		`[ ! -L "$STATE_ROOT/state.json" ]`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("installer transaction-state guard is missing %q", required)
		}
	}
}

func TestInstallerBootRecoveryCancelsPendingSystemdStartWithoutBlocking(t *testing.T) {
	data, err := os.ReadFile("installer_recovery.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `Run(ctx, "systemctl", "stop", "--no-block", "vohive.service")`) {
		t.Fatal("installer boot recovery can block on a VoHive start job that is waiting for recovery")
	}
}

func TestUninstallerNeverStopsAnActiveTransactionWorker(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "uninstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"refuse_active_transaction_services()",
		"vohive-update.service vohive-recover.service",
		"vohive-update vohive-recover",
		"refuse_unresolved_lock()",
		"reboot for boot recovery",
		"vohivectl doctor",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("uninstaller transaction preflight is missing %q", required)
		}
	}
	if strings.Contains(text, "run vohivectl recover before uninstalling") {
		t.Fatal("uninstaller recommends normal recovery even though it cannot clear an orphan lock")
	}
	stopStart := strings.Index(text, "stop_services() {")
	removeStart := strings.Index(text, "remove_services() {")
	if stopStart < 0 || removeStart <= stopStart {
		t.Fatal("could not locate uninstaller service functions")
	}
	stopBody := text[stopStart:removeStart]
	if strings.Contains(stopBody, "vohive-update") || strings.Contains(stopBody, "vohive-recover") {
		t.Fatal("stop_services must not terminate transaction workers")
	}
	firstPreflight := strings.Index(text, "\nrefuse_active_transaction_services\n")
	firstStop := strings.Index(text, "\nstop_services\n")
	if firstPreflight < 0 || firstStop < 0 || firstPreflight > firstStop {
		t.Fatalf("transaction workers must be checked before the main service is stopped: preflight=%d stop=%d", firstPreflight, firstStop)
	}
	if strings.Count(text, "\nrefuse_active_transaction_services\n") < 2 || strings.Count(text, "\nrefuse_unresolved_lock\n") < 2 {
		t.Fatal("uninstaller must repeat transaction preflight after stopping the main service")
	}
}
