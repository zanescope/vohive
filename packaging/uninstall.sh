#!/bin/sh
set -eu

umask 077

PURGE=0
ASSUME_YES=0
KEEP_CONFIG=0
LOCK_FILE='/var/lib/vohive/update/update.lock'
MANAGED_MODEMMANAGER_RULE='/etc/udev/rules.d/78-mm-vohive-managed.rules'
LOCK_HELD=0
LOCK_BOOT_ID=''
LOCK_PROCESS_START_TICKS=''
MAIN_SERVICE_RESTART=''

say() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Usage: uninstall.sh [options]

  --purge        also remove configuration and user data after making a backup
  --yes          confirm --purge non-interactively
  --keep-config  with --purge, retain /etc/vohive
  -h, --help     show this help

Without --purge, only programs and service definitions are removed. Configuration,
data, logs, backups, and update history are retained.
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--purge) PURGE=1 ;;
		--yes) ASSUME_YES=1 ;;
		--keep-config) KEEP_CONFIG=1 ;;
		-h|--help) usage; exit 0 ;;
		*) die "unknown option: $1" ;;
	esac
	shift
done

[ "$(id -u)" -eq 0 ] || die 'run as root (for example: sudo sh uninstall.sh)'
[ "$ASSUME_YES" -eq 0 ] || [ "$PURGE" -eq 1 ] || die '--yes is only valid with --purge'
[ "$KEEP_CONFIG" -eq 0 ] || [ "$PURGE" -eq 1 ] || die '--keep-config is only valid with --purge'

safe_remove_tree() {
	case "$1" in
		/opt/vohive/releases|/opt/vohive/control|/opt/vohive/config|/opt/vohive/data|/opt/vohive/logs|/etc/vohive|/var/lib/vohive) ;;
		*) die "refusing unsafe recursive target: $1" ;;
	esac
	rm -rf -- "$1"
}

find_active_transaction_service() {
	if command -v systemctl >/dev/null 2>&1; then
		for service in vohive-update.service vohive-recover.service vohive-host-config.service; do
			if systemctl is-active --quiet "$service" 2>/dev/null; then
				printf '%s\n' "$service"
				return 0
			fi
		done
	fi
	for service in vohive-update vohive-recover; do
		if [ -x "/etc/init.d/$service" ] && "/etc/init.d/$service" running >/dev/null 2>&1; then
			printf 'OpenWrt %s\n' "$service"
			return 0
		fi
	done
	return 1
}

refuse_active_transaction_services() {
	if active_service=$(find_active_transaction_service); then
		die "$active_service is active; wait for it to finish before uninstalling"
	fi
}

restart_main_service_after_cleanup_failure() {
	case "$MAIN_SERVICE_RESTART" in
		'') return 0 ;;
		systemd|openwrt) ;;
		*) return 1 ;;
	esac
	# guard-start rejects a service start while the uninstall/update lock is
	# still held. Release only after deciding the previous service was active.
	release_uninstall_lock || return 1
	case "$MAIN_SERVICE_RESTART" in
		systemd) systemctl start vohive.service >/dev/null 2>&1 ;;
		openwrt) /etc/init.d/vohive start >/dev/null 2>&1 ;;
	esac
}

abort_host_config_cleanup() {
	message=$1
	if ! restart_main_service_after_cleanup_failure; then
		warn 'could not restart the previously active VoHive service after the cleanup failure'
	fi
	die "$message"
}

refuse_active_transaction_services_after_stop() {
	if active_service=$(find_active_transaction_service); then
		abort_host_config_cleanup "$active_service became active after VoHive stopped; service definitions and programs were retained"
	fi
}

release_uninstall_lock() {
	[ "$LOCK_HELD" -eq 1 ] || return 0
	[ -f "$LOCK_FILE" ] && [ ! -L "$LOCK_FILE" ] || return 1
	locked_pid=$(sed -n 's/^pid=//p' "$LOCK_FILE" | sed -n '1p')
	locked_boot_id=$(sed -n 's/^boot_id=//p' "$LOCK_FILE" | sed -n '1p')
	locked_start_ticks=$(sed -n 's/^process_start_ticks=//p' "$LOCK_FILE" | sed -n '1p')
	if [ "$locked_pid" != "$$" ] ||
		[ "$locked_boot_id" != "$LOCK_BOOT_ID" ] ||
		[ "$locked_start_ticks" != "$LOCK_PROCESS_START_TICKS" ]; then
		return 1
	fi
	rm -f -- "$LOCK_FILE" || return 1
	LOCK_HELD=0
}

cleanup_uninstall_lock() {
	if ! release_uninstall_lock; then
		warn "could not safely release $LOCK_FILE; inspect it before installing or updating"
	fi
}

trap cleanup_uninstall_lock 0

acquire_uninstall_lock() {
	for directory in /var/lib/vohive /var/lib/vohive/update; do
		if [ -L "$directory" ] || { [ -e "$directory" ] && [ ! -d "$directory" ]; }; then
			die "refusing unsafe update state directory: $directory"
		fi
	done
	mkdir -p /var/lib/vohive/update
	chmod 0700 /var/lib/vohive /var/lib/vohive/update

	[ -r /proc/sys/kernel/random/boot_id ] && [ -r "/proc/$$/stat" ] ||
		die 'Linux process identity files are unavailable'
	LOCK_BOOT_ID=$(sed -n '1p' /proc/sys/kernel/random/boot_id)
	LOCK_PROCESS_START_TICKS=$(sed 's/^[^)]*) //' "/proc/$$/stat" | awk '{print $20}')
	case "$LOCK_BOOT_ID" in
		''|*[!0-9A-Fa-f-]*) die 'invalid Linux boot identity' ;;
	esac
	case "$LOCK_PROCESS_START_TICKS" in
		''|*[!0-9]*) die 'invalid uninstaller process start time' ;;
	esac
	started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	if ! (
		set -C
		printf 'pid=%s\nstarted=%s\nboot_id=%s\nprocess_start_ticks=%s\n' \
			"$$" "$started_at" "$LOCK_BOOT_ID" "$LOCK_PROCESS_START_TICKS" >"$LOCK_FILE"
	) 2>/dev/null; then
		die 'an install or update transaction is unresolved; wait for it to finish, or reboot for boot recovery and then run vohivectl doctor'
	fi
	LOCK_HELD=1
}

stop_services() {
	if command -v systemctl >/dev/null 2>&1; then
		if systemctl is-active --quiet vohive.service 2>/dev/null; then
			MAIN_SERVICE_RESTART='systemd'
		fi
		systemctl stop vohive.service 2>/dev/null || true
		if systemctl is-active --quiet vohive.service 2>/dev/null; then
			die 'vohive.service is still active; refusing to remove files'
		fi
	fi
	if [ -x /etc/init.d/vohive ]; then
		if [ -z "$MAIN_SERVICE_RESTART" ] && /etc/init.d/vohive running >/dev/null 2>&1; then
			MAIN_SERVICE_RESTART='openwrt'
		fi
		/etc/init.d/vohive stop 2>/dev/null || true
		if /etc/init.d/vohive running >/dev/null 2>&1; then
			die 'OpenWrt VoHive service is still active; refusing to remove files'
		fi
	fi
}

remove_services() {
	if command -v systemctl >/dev/null 2>&1; then
		systemctl disable vohive.service vohive-recover.service 2>/dev/null || true
	fi
	rm -f -- /etc/systemd/system/vohive.service /etc/systemd/system/vohive-update.service /etc/systemd/system/vohive-recover.service /etc/systemd/system/vohive-host-config.service
	if command -v systemctl >/dev/null 2>&1; then systemctl daemon-reload 2>/dev/null || true; fi
	for service in vohive vohive-update vohive-recover; do
		if [ -x "/etc/init.d/$service" ]; then "/etc/init.d/$service" disable 2>/dev/null || true; fi
		rm -f -- "/etc/init.d/$service"
	done
}

cleanup_managed_host_config() {
	control='/opt/vohive/control/vohivectl'
	if [ ! -e "$MANAGED_MODEMMANAGER_RULE" ] && [ ! -L "$MANAGED_MODEMMANAGER_RULE" ]; then
		return 0
	fi
	if [ -f "$control" ] && [ ! -L "$control" ] && [ -x "$control" ]; then
		if ! "$control" host-config --cleanup-managed-for-uninstall; then
			abort_host_config_cleanup 'could not safely remove the VoHive-managed ModemManager isolation rule; service definitions and programs were retained'
		fi
		say 'VoHive-managed ModemManager isolation is absent or removed.'
		say 'Reconnect affected modems or reboot before expecting existing udev device state to change.'
		return
	fi
	if [ -e "$control" ] || [ -L "$control" ]; then
		abort_host_config_cleanup 'the host-configuration cleanup command is unsafe; service definitions and programs were retained'
	fi
	abort_host_config_cleanup 'the managed ModemManager rule exists but the trusted cleanup command is unavailable; service definitions and programs were retained'
}

make_purge_backup() {
	command -v tar >/dev/null 2>&1 || die 'tar is required to back up data before purge'
	mkdir -p /var/backups
	backup="/var/backups/vohive-purge-$(date +%Y%m%d%H%M%S).tar.gz"
	set --
	[ ! -e /etc/vohive ] || set -- "$@" etc/vohive
	[ ! -e /var/lib/vohive ] || set -- "$@" var/lib/vohive
	[ ! -e /opt/vohive/config ] || set -- "$@" opt/vohive/config
	[ ! -e /opt/vohive/data ] || set -- "$@" opt/vohive/data
	[ ! -e /opt/vohive/logs ] || set -- "$@" opt/vohive/logs
	if [ "$#" -eq 0 ]; then
		say 'No configuration or data exists; no purge backup was needed.'
		return
	fi
	if ! tar -czf "$backup" -C / "$@"; then
		rm -f -- "$backup"
		die 'purge backup failed; nothing was deleted'
	fi
	chmod 0600 "$backup"
	say "Recovery backup: $backup"
}

refuse_active_transaction_services
acquire_uninstall_lock
refuse_active_transaction_services
stop_services

if [ "$PURGE" -eq 1 ]; then
	if [ "$ASSUME_YES" -ne 1 ]; then
		say 'This will permanently remove VoHive configuration, databases, logs, and backups.'
		printf 'Type PURGE to continue: '
		IFS= read -r answer || die 'confirmation was not received'
		[ "$answer" = PURGE ] || die 'purge cancelled'
	fi
	make_purge_backup
fi

# The main service can no longer launch a new host-config helper. Recheck after
# any interactive purge confirmation before changing the managed rule.
refuse_active_transaction_services_after_stop
cleanup_managed_host_config
remove_services

if [ -L /usr/local/sbin/vohivectl ]; then
	target=$(readlink /usr/local/sbin/vohivectl || true)
	case "$target" in /opt/vohive/control/vohivectl) rm -f -- /usr/local/sbin/vohivectl ;; esac
fi

rm -f -- /opt/vohive/current /opt/vohive/last-good /opt/vohive/bin/vohive /opt/vohive/bin/vohive.bak
[ ! -e /opt/vohive/releases ] || safe_remove_tree /opt/vohive/releases
[ ! -e /opt/vohive/control ] || safe_remove_tree /opt/vohive/control

if [ "$PURGE" -eq 1 ]; then
	if [ "$KEEP_CONFIG" -ne 1 ]; then
		[ ! -e /etc/vohive ] || safe_remove_tree /etc/vohive
		[ ! -e /opt/vohive/config ] || safe_remove_tree /opt/vohive/config
	fi
	[ ! -e /var/lib/vohive ] || safe_remove_tree /var/lib/vohive
	LOCK_HELD=0
	[ ! -e /opt/vohive/data ] || safe_remove_tree /opt/vohive/data
	[ ! -e /opt/vohive/logs ] || safe_remove_tree /opt/vohive/logs
fi

rmdir /opt/vohive/bin /opt/vohive 2>/dev/null || true

if [ "$PURGE" -eq 1 ]; then
	say 'VoHive programs and user data were removed. The recovery backup remains under /var/backups.'
else
	release_uninstall_lock ||
		die "programs were removed, but $LOCK_FILE could not be safely released; inspect it before reinstalling"
	say 'VoHive programs were removed. Configuration and user data were retained.'
fi
