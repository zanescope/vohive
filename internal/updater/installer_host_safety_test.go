package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerGuardsEveryUnresolvedTransactionBeforeInstalledEarlyExit(t *testing.T) {
	text := readPackagingScript(t, "install.sh")
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
	text := readPackagingScript(t, "uninstall.sh")
	for _, required := range []string{
		"refuse_active_transaction_services()",
		"vohive-update.service vohive-recover.service",
		"vohive-update vohive-recover",
		"acquire_uninstall_lock()",
		"set -C",
		"process_start_ticks=%s",
		"release_uninstall_lock()",
		"trap cleanup_uninstall_lock 0",
		"LOCK_HELD=0",
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
	lock := strings.Index(text, "\nacquire_uninstall_lock\n")
	secondPreflight := -1
	if lock >= 0 {
		offset := strings.Index(text[lock+1:], "\nrefuse_active_transaction_services\n")
		if offset >= 0 {
			secondPreflight = lock + 1 + offset
		}
	}
	firstStop := strings.Index(text, "\nstop_services\n")
	thirdPreflight := -1
	if firstStop >= 0 {
		offset := strings.Index(text[firstStop+1:], "\nrefuse_active_transaction_services_after_stop\n")
		if offset >= 0 {
			thirdPreflight = firstStop + 1 + offset
		}
	}
	cleanup := strings.Index(text, "\ncleanup_managed_host_config\n")
	remove := strings.Index(text, "\nremove_services\n")
	if firstPreflight < 0 || lock < 0 || secondPreflight < 0 || firstStop < 0 ||
		thirdPreflight < 0 || cleanup < 0 || remove < 0 ||
		!(firstPreflight < lock && lock < secondPreflight && secondPreflight < firstStop &&
			firstStop < thirdPreflight && thirdPreflight < cleanup && cleanup < remove) {
		t.Fatalf(
			"unsafe uninstall ordering: first=%d lock=%d second=%d stop=%d third=%d cleanup=%d remove=%d",
			firstPreflight, lock, secondPreflight, firstStop, thirdPreflight, cleanup, remove,
		)
	}
}

func TestHostConfigUnitParticipatesInInstallerRollbackAndUninstall(t *testing.T) {
	installer := readPackagingScript(t, "install.sh")
	for _, required := range []string{
		"save_path /etc/systemd/system/vohive-host-config.service unit_host_config",
		"restore_path /etc/systemd/system/vohive-host-config.service unit_host_config",
		"cat >/etc/systemd/system/vohive-host-config.service",
		"/etc/systemd/system/vohive-recover.service /etc/systemd/system/vohive-host-config.service",
	} {
		if !strings.Contains(installer, required) {
			t.Errorf("installer host-config integration is missing %q", required)
		}
	}
	mainStart := strings.Index(installer, "cat >/etc/systemd/system/vohive.service")
	updateStart := strings.Index(installer, "cat >/etc/systemd/system/vohive-update.service")
	if mainStart < 0 || updateStart <= mainStart {
		t.Fatal("could not locate generated main systemd unit")
	}
	if strings.Contains(installer[mainStart:updateStart], "/etc/udev/rules.d") {
		t.Fatal("generated main service grants access to udev rules")
	}

	recoveryData, err := os.ReadFile("installer_recovery.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recoveryData), `name: "unit_host_config", destination: "/etc/systemd/system/vohive-host-config.service"`) {
		t.Fatal("boot recovery does not restore the host-config unit marker")
	}

	uninstaller := readPackagingScript(t, "uninstall.sh")
	if !strings.Contains(uninstaller, "vohive-update.service vohive-recover.service vohive-host-config.service") {
		t.Fatal("uninstaller does not refuse to race an active host-config helper")
	}
	if !strings.Contains(uninstaller, "/etc/systemd/system/vohive-recover.service /etc/systemd/system/vohive-host-config.service") {
		t.Fatal("uninstaller does not remove the host-config unit")
	}
	for _, required := range []string{
		"MANAGED_MODEMMANAGER_RULE='/etc/udev/rules.d/78-mm-vohive-managed.rules'",
		`"$control" host-config --cleanup-managed-for-uninstall`,
		"service definitions and programs were retained",
		"Reconnect affected modems or reboot",
		`[ ! -e "$MANAGED_MODEMMANAGER_RULE" ] && [ ! -L "$MANAGED_MODEMMANAGER_RULE" ]`,
		"restart_main_service_after_cleanup_failure",
		"abort_host_config_cleanup",
		"refuse_active_transaction_services_after_stop",
	} {
		if !strings.Contains(uninstaller, required) {
			t.Errorf("safe managed-rule cleanup is missing %q", required)
		}
	}
	if strings.Contains(uninstaller, `rm -f -- "$MANAGED_MODEMMANAGER_RULE"`) {
		t.Fatal("uninstaller directly removes the managed udev rule without ownership checks")
	}
	cleanup := strings.Index(uninstaller, "\ncleanup_managed_host_config\n")
	removeUnits := strings.Index(uninstaller, "\nremove_services\n")
	removeControl := strings.Index(uninstaller, "[ ! -e /opt/vohive/control ] || safe_remove_tree /opt/vohive/control")
	if cleanup < 0 || removeUnits <= cleanup || removeControl <= removeUnits {
		t.Fatalf(
			"managed rule must be cleaned before helper unit/control removal: cleanup=%d units=%d control=%d",
			cleanup, removeUnits, removeControl,
		)
	}
	cleanupDefinition := strings.Index(uninstaller, "cleanup_managed_host_config() {")
	absentNoOp := strings.Index(
		uninstaller,
		`[ ! -e "$MANAGED_MODEMMANAGER_RULE" ] && [ ! -L "$MANAGED_MODEMMANAGER_RULE" ]`,
	)
	cleanupCommand := strings.Index(uninstaller, `"$control" host-config --cleanup-managed-for-uninstall`)
	if cleanupDefinition < 0 || absentNoOp <= cleanupDefinition || cleanupCommand <= absentNoOp {
		t.Fatalf(
			"absent managed rule must no-op before invoking a possibly old control binary: definition=%d absent=%d command=%d",
			cleanupDefinition, absentNoOp, cleanupCommand,
		)
	}
	abortDefinition := strings.Index(uninstaller, "abort_host_config_cleanup() {")
	if abortDefinition < 0 {
		t.Fatal("could not locate cleanup abort helper")
	}
	restartCall := strings.Index(
		uninstaller[abortDefinition:],
		"restart_main_service_after_cleanup_failure",
	)
	dieCall := strings.Index(uninstaller[abortDefinition:], `die "$message"`)
	if abortDefinition < 0 || restartCall < 0 || dieCall < 0 || restartCall >= dieCall {
		t.Fatalf(
			"cleanup failure must restart a previously active main service before aborting: definition=%d restart=%d die=%d",
			abortDefinition, restartCall, dieCall,
		)
	}
	restartDefinition := strings.Index(uninstaller, "restart_main_service_after_cleanup_failure() {")
	if restartDefinition < 0 {
		t.Fatal("could not locate cleanup-failure restart helper")
	}
	releaseBeforeStart := strings.Index(uninstaller[restartDefinition:], "release_uninstall_lock || return 1")
	startAfterRelease := strings.Index(uninstaller[restartDefinition:], "systemctl start vohive.service")
	if restartDefinition < 0 || releaseBeforeStart < 0 || startAfterRelease <= releaseBeforeStart {
		t.Fatalf(
			"restart must release the guard-start lock first: definition=%d release=%d start=%d",
			restartDefinition, releaseBeforeStart, startAfterRelease,
		)
	}
	stopDefinition := strings.Index(uninstaller, "stop_services() {")
	if stopDefinition < 0 {
		t.Fatal("could not locate main service stop helper")
	}
	recordSystemd := strings.Index(uninstaller[stopDefinition:], "MAIN_SERVICE_RESTART='systemd'")
	stopSystemd := strings.Index(uninstaller[stopDefinition:], "systemctl stop vohive.service")
	if stopDefinition < 0 || recordSystemd < 0 || stopSystemd < 0 || recordSystemd >= stopSystemd {
		t.Fatalf(
			"uninstaller must remember an active main service before stopping it: definition=%d record=%d stop=%d",
			stopDefinition, recordSystemd, stopSystemd,
		)
	}

	cliData, err := os.ReadFile(filepath.Join("..", "..", "cmd", "vohivectl", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	cli := string(cliData)
	if !strings.Contains(cli, `"cleanup-managed-for-uninstall"`) ||
		!strings.Contains(cli, "hostconfig.CleanupManagedForUninstall(ctx, nil)") {
		t.Fatal("vohivectl does not expose the fixed managed-rule cleanup mode")
	}
	for _, forbidden := range []string{"cleanup-rule-path", "cleanup-target", "cleanup-managed-path"} {
		if strings.Contains(cli, forbidden) {
			t.Fatalf("cleanup CLI exposes forbidden caller-controlled scope %q", forbidden)
		}
	}
}
