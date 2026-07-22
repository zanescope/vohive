#!/bin/sh
set -eu

umask 077

PURGE=0
ASSUME_YES=0
KEEP_CONFIG=0

say() { printf '%s\n' "$*"; }
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

refuse_active_transaction_services() {
	if command -v systemctl >/dev/null 2>&1; then
		for service in vohive-update.service vohive-recover.service; do
			if systemctl is-active --quiet "$service" 2>/dev/null; then
				die "$service is active; wait for it to finish before uninstalling"
			fi
		done
	fi
	for service in vohive-update vohive-recover; do
		if [ -x "/etc/init.d/$service" ] && "/etc/init.d/$service" running >/dev/null 2>&1; then
			die "OpenWrt $service is active; wait for it to finish before uninstalling"
		fi
	done
}

refuse_unresolved_lock() {
	if [ -e /var/lib/vohive/update/update.lock ] || [ -L /var/lib/vohive/update/update.lock ]; then
		die 'an install or update transaction is unresolved; wait for it to finish, or reboot for boot recovery and then run vohivectl doctor'
	fi
}

stop_services() {
	if command -v systemctl >/dev/null 2>&1; then
		systemctl stop vohive.service 2>/dev/null || true
		if systemctl is-active --quiet vohive.service 2>/dev/null; then
			die 'vohive.service is still active; refusing to remove files'
		fi
	fi
	if [ -x /etc/init.d/vohive ]; then
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
	rm -f -- /etc/systemd/system/vohive.service /etc/systemd/system/vohive-update.service /etc/systemd/system/vohive-recover.service
	if command -v systemctl >/dev/null 2>&1; then systemctl daemon-reload 2>/dev/null || true; fi
	for service in vohive vohive-update vohive-recover; do
		if [ -x "/etc/init.d/$service" ]; then "/etc/init.d/$service" disable 2>/dev/null || true; fi
		rm -f -- "/etc/init.d/$service"
	done
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
refuse_unresolved_lock
stop_services
# Close the small preflight-to-stop race without ever killing a transaction
# worker. A worker that started meanwhile keeps its files intact.
refuse_active_transaction_services
refuse_unresolved_lock

if [ "$PURGE" -eq 1 ]; then
	if [ "$ASSUME_YES" -ne 1 ]; then
		say 'This will permanently remove VoHive configuration, databases, logs, and backups.'
		printf 'Type PURGE to continue: '
		IFS= read -r answer || die 'confirmation was not received'
		[ "$answer" = PURGE ] || die 'purge cancelled'
	fi
	make_purge_backup
fi

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
	[ ! -e /opt/vohive/data ] || safe_remove_tree /opt/vohive/data
	[ ! -e /opt/vohive/logs ] || safe_remove_tree /opt/vohive/logs
fi

rmdir /opt/vohive/bin /opt/vohive 2>/dev/null || true

if [ "$PURGE" -eq 1 ]; then
	say 'VoHive programs and user data were removed. The recovery backup remains under /var/backups.'
else
	say 'VoHive programs were removed. Configuration and user data were retained.'
fi
