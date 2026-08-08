#!/bin/sh
set -eu
umask 077

REPOSITORY='zanescope/vohive'
API_BASE='https://api.github.com/repos/zanescope/vohive'
RELEASE_BASE='https://github.com/zanescope/vohive/releases'

# These values are rendered by the release workflow. They are deliberately not
# environment variables: callers cannot replace the bootstrap trust root.
BOOTSTRAP_VERSION='@VOHIVE_BOOTSTRAP_VERSION@'
TRUSTED_MINISIGN_PUBLIC_KEYS='@VOHIVE_MINISIGN_PUBLIC_KEYS@'
VERIFY_SHA256_MAP='@VOHIVE_VERIFY_SHA256@'

INSTALL_ROOT='/opt/vohive'
CONFIG_FILE='/etc/vohive/config.yaml'
DATA_ROOT='/var/lib/vohive/data'
DEPLOYMENT_FILE='/etc/vohive/deployment.json'
STATE_ROOT='/var/lib/vohive/update'
BACKUP_ROOT='/var/lib/vohive/backups'
LOCK_FILE='/var/lib/vohive/update/update.lock'
TRUST_FILE='/etc/vohive/update.pub'
LAYOUT='v2'

CHANNEL='stable'
VERSION=''
VERSION_SET=0
CHANNEL_SET=0
DRY_RUN=0
NO_SERVICE=0
REPAIR=0
SERVICE_TYPE='portable'
ARCH=''
TMP_DIR=''
STAGING_DIR=''
RELEASE_DIR=''
RELEASE_NAME=''
TARGET_HOLD=''
TARGET_HOLD_MODE=''
LEGACY_RELEASE=''
LEGACY_SOURCE=''
VERIFY_BIN=''
INITIAL_PASSWORD=''
WEB_UI_URL=''
MANIFEST=''
MANIFEST_SHA=''
MANIFEST_SIZE=''
ASSET=''

HAD_DEPLOYMENT=0
INSTALLED_VERSION=''
OLD_VERSION=''
OLD_LAST_GOOD_VERSION=''
OLD_TARGET=''
OLD_TARGET_ABS=''
OLD_LAST_GOOD_TARGET=''
OLD_LAST_GOOD_TARGET_ABS=''
RECOVERY_PREVIOUS_TARGET=''
OLD_SERVICE_ACTIVE=0
OLD_MAIN_ENABLED=0
OLD_RECOVER_ENABLED=0

TRANSACTION_ID=''
TRANSACTION_STARTED_AT=''
TRANSACTION_BACKUP=''
TRANSACTION_PHASE='checking'
INSTALLED_RELEASE_PATH=''
TRANSACTION_ACTIVE=0
BACKUP_READY=0
LOCK_HELD=0
NEW_RELEASE_INSTALLED=0
SERVICE_MAY_BE_RUNNING=0
RELEASE_PREPARED=0

say() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Usage: vohive-install.sh [options]
  --version TAG      install an exact signed release
  --channel CHANNEL  stable or beta (default: stable)
  --dry-run          resolve and verify signed metadata without changing the host
  --no-service       explicit advanced mode: install files without starting a service
  --repair           transactionally repair the installed version and service files
  -h, --help         show this help
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version) [ "$#" -ge 2 ] || die '--version requires a value'; VERSION=$2; VERSION_SET=1; shift 2 ;;
		--version=*) VERSION=${1#*=}; VERSION_SET=1; shift ;;
		--channel) [ "$#" -ge 2 ] || die '--channel requires a value'; CHANNEL=$2; CHANNEL_SET=1; shift 2 ;;
		--channel=*) CHANNEL=${1#*=}; CHANNEL_SET=1; shift ;;
		--dry-run) DRY_RUN=1; shift ;;
		--no-service|--no-systemd) NO_SERVICE=1; shift ;;
		--repair) REPAIR=1; shift ;;
		-h|--help) usage; exit 0 ;;
		*) die "unknown option: $1" ;;
	esac
done

case "$CHANNEL" in stable|beta) ;; *) die '--channel must be stable or beta' ;; esac

valid_version() {
	printf '%s\n' "$1" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
}
is_sha256() { [ "${#1}" -eq 64 ] && ! printf '%s' "$1" | grep -Eq '[^0-9a-fA-F]'; }
[ -z "$VERSION" ] || valid_version "$VERSION" || die "invalid version tag: $VERSION"

need_command() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

remove_managed_tree() {
	path=$1
	[ -n "$path" ] || return 1
	if [ "$path" = "$STAGING_DIR" ] || [ "$path" = "$RELEASE_DIR" ] || [ "$path" = "$TARGET_HOLD" ] || [ "$path" = "$LEGACY_RELEASE" ] || [ "$path" = "$DATA_ROOT" ]; then
		rm -rf -- "$path"
		return $?
	fi
	case "$path" in
		/opt/vohive/releases/.staging-*|/opt/vohive/releases/.repair-*|/opt/vohive/releases/.displaced-*) rm -rf -- "$path" ;;
		*) return 1 ;;
	esac
}

atomic_link() {
	link_path=$1
	target=$2
	case "$link_path" in "$INSTALL_ROOT/current"|"$INSTALL_ROOT/last-good") ;; *) return 1 ;; esac
	tmp_link="$link_path.new.$$"
	rm -f -- "$tmp_link"
	ln -s "$target" "$tmp_link" || return 1
	if mv -Tf "$tmp_link" "$link_path" 2>/dev/null; then return 0; fi
	# BusyBox without mv -T: the service is stopped, and the target is an exact
	# managed link, so a narrow unlink+rename fallback is safe.
	rm -f -- "$link_path" || { rm -f -- "$tmp_link"; return 1; }
	mv "$tmp_link" "$link_path"
}

validate_release_target() {
	target=$1
	case "$target" in
		releases/*) name=${target#releases/} ;;
		"$INSTALL_ROOT"/releases/*) name=${target#"$INSTALL_ROOT"/releases/} ;;
		*) return 1 ;;
	esac
	case "$name" in ''|.|..|*/*) return 1 ;; esac
	[ -f "$INSTALL_ROOT/releases/$name/vohive" ] && [ ! -L "$INSTALL_ROOT/releases/$name/vohive" ]
}

absolute_release_target() {
	case "$1" in
		releases/*) printf '%s/%s\n' "$INSTALL_ROOT" "$1" ;;
		*) printf '%s\n' "$1" ;;
	esac
}

save_path() {
	source=$1
	name=$2
	if [ -L "$source" ]; then
		readlink "$source" >"$TRANSACTION_BACKUP/$name.link"
	elif [ -f "$source" ]; then
		cp -p "$source" "$TRANSACTION_BACKUP/$name.file"
	elif [ -e "$source" ]; then
		return 1
	else
		: >"$TRANSACTION_BACKUP/$name.absent"
	fi
}

restore_path() {
	destination=$1
	name=$2
	[ ! -d "$destination" ] || return 1
	rm -f -- "$destination" || return 1
	mkdir -p "$(dirname "$destination")" || return 1
	if [ -f "$TRANSACTION_BACKUP/$name.link" ]; then
		target=$(sed -n '1p' "$TRANSACTION_BACKUP/$name.link")
		ln -s "$target" "$destination"
	elif [ -f "$TRANSACTION_BACKUP/$name.file" ]; then
		cp -p "$TRANSACTION_BACKUP/$name.file" "$destination"
	elif [ -f "$TRANSACTION_BACKUP/$name.absent" ]; then
		return 0
	else
		return 1
	fi
}

save_data() {
	if [ -d "$DATA_ROOT" ] && [ ! -L "$DATA_ROOT" ]; then
		cp -R "$DATA_ROOT" "$TRANSACTION_BACKUP/data"
		: >"$TRANSACTION_BACKUP/data.present"
	elif [ -e "$DATA_ROOT" ] || [ -L "$DATA_ROOT" ]; then
		return 1
	else
		: >"$TRANSACTION_BACKUP/data.absent"
	fi
}

restore_data() {
	remove_managed_tree "$DATA_ROOT" || return 1
	if [ -f "$TRANSACTION_BACKUP/data.present" ]; then
		cp -R "$TRANSACTION_BACKUP/data" "$DATA_ROOT"
	elif [ -f "$TRANSACTION_BACKUP/data.absent" ]; then
		return 0
	else
		return 1
	fi
}

stop_service_best_effort() {
	case "$SERVICE_TYPE" in
		systemd) systemctl stop vohive.service >/dev/null 2>&1 ;;
		openwrt) /etc/init.d/vohive stop >/dev/null 2>&1 ;;
		portable) return 0 ;;
	esac
}

start_service() {
	case "$SERVICE_TYPE" in
		systemd) systemctl start vohive.service ;;
		openwrt) /etc/init.d/vohive start ;;
		portable) return 0 ;;
	esac
}

restore_enablement() {
	case "$SERVICE_TYPE" in
		systemd)
			if [ -f /etc/systemd/system/vohive.service ]; then
				if [ "$OLD_MAIN_ENABLED" -eq 1 ]; then systemctl enable vohive.service >/dev/null 2>&1; else systemctl disable vohive.service >/dev/null 2>&1; fi || return 1
			fi
			if [ -f /etc/systemd/system/vohive-recover.service ]; then
				if [ "$OLD_RECOVER_ENABLED" -eq 1 ]; then systemctl enable vohive-recover.service >/dev/null 2>&1; else systemctl disable vohive-recover.service >/dev/null 2>&1; fi || return 1
			fi
			;;
		openwrt)
			if [ -x /etc/init.d/vohive ]; then
				if [ "$OLD_MAIN_ENABLED" -eq 1 ]; then /etc/init.d/vohive enable; else /etc/init.d/vohive disable; fi >/dev/null 2>&1 || return 1
			fi
			if [ -x /etc/init.d/vohive-recover ]; then
				if [ "$OLD_RECOVER_ENABLED" -eq 1 ]; then /etc/init.d/vohive-recover enable; else /etc/init.d/vohive-recover disable; fi >/dev/null 2>&1 || return 1
			fi
			;;
	esac
}

write_manual_recovery_state() {
	tmp_state="$STATE_ROOT/state.json.tmp.$$"
	mkdir -p "$STATE_ROOT" || return 1
	state_backup_json=''
	state_installed_json=''
	[ "$BACKUP_READY" -eq 0 ] || state_backup_json=",\"backup_path\":\"$TRANSACTION_BACKUP\""
	[ -z "$INSTALLED_RELEASE_PATH" ] || state_installed_json=",\"installed_release_path\":\"$INSTALLED_RELEASE_PATH\""
	cat >"$tmp_state" <<EOF
{"schema":1,"id":"$TRANSACTION_ID","operation":"install","phase":"manual_recovery_required","current_version":"$OLD_VERSION","target_version":"$VERSION"$state_backup_json,"staging_path":"$STAGING_DIR"$state_installed_json,"error":"installer rollback was incomplete","started_at":"$TRANSACTION_STARTED_AT","updated_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
	chmod 0600 "$tmp_state" && mv -f "$tmp_state" "$STATE_ROOT/state.json"
}

write_terminal_state() {
	phase=$1
	error_text=$2
	current_version=$3
	tmp_state="$STATE_ROOT/state.json.tmp.$$"
	mkdir -p "$STATE_ROOT"
	state_backup_json=''
	[ "$BACKUP_READY" -eq 0 ] || state_backup_json=",\"backup_path\":\"$TRANSACTION_BACKUP\""
	cat >"$tmp_state" <<EOF
{"schema":1,"id":"$TRANSACTION_ID","operation":"install","phase":"$phase","current_version":"$current_version","target_version":"$VERSION"$state_backup_json,"error":"$error_text","started_at":"$TRANSACTION_STARTED_AT","updated_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
	chmod 0600 "$tmp_state" && mv -f "$tmp_state" "$STATE_ROOT/state.json"
}

write_nonterminal_state() {
	control_touched=$1
	tmp_state="$STATE_ROOT/state.json.tmp.$$"
	mkdir -p "$STATE_ROOT"
	state_backup_json=''
	state_installed_json=''
	[ "$BACKUP_READY" -eq 0 ] || state_backup_json=",\"backup_path\":\"$TRANSACTION_BACKUP\""
	[ -z "$INSTALLED_RELEASE_PATH" ] || state_installed_json=",\"installed_release_path\":\"$INSTALLED_RELEASE_PATH\""
	cat >"$tmp_state" <<EOF
{"schema":1,"id":"$TRANSACTION_ID","operation":"install","phase":"$TRANSACTION_PHASE","current_version":"$OLD_VERSION","target_version":"$VERSION","previous_version":"$OLD_VERSION","previous_target":"$RECOVERY_PREVIOUS_TARGET","previous_last_good_target":"$OLD_LAST_GOOD_TARGET_ABS","control_touched":$control_touched$state_backup_json,"staging_path":"$STAGING_DIR"$state_installed_json,"started_at":"$TRANSACTION_STARTED_AT","updated_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
	chmod 0600 "$tmp_state" && mv -f "$tmp_state" "$STATE_ROOT/state.json"
}

release_owned_by_transaction() {
	marker="$RELEASE_DIR/.vohive-transaction"
	[ -f "$marker" ] && [ ! -L "$marker" ] || return 1
	marker_size=$(file_size "$marker") || return 1
	[ "$marker_size" -eq $(( ${#TRANSACTION_ID} + 1 )) ] || return 1
	[ "$(sed -n '1p' "$marker")" = "$TRANSACTION_ID" ]
}

rollback_transaction() {
	[ "$TRANSACTION_ACTIVE" -eq 1 ] || return 0
	TRANSACTION_ACTIVE=0
	warn 'installation failed; restoring the complete previous deployment'
	set +e
	restore_failed=0
	if [ "$SERVICE_MAY_BE_RUNNING" -eq 1 ]; then
		stop_service_best_effort || restore_failed=1
	fi

	if [ "$NEW_RELEASE_INSTALLED" -eq 1 ] && [ -d "$RELEASE_DIR" ]; then
		if release_owned_by_transaction; then
			remove_managed_tree "$RELEASE_DIR" || restore_failed=1
		else
			warn 'refusing to remove a release slot not owned by this installer transaction'
			restore_failed=1
		fi
	fi
	if [ -n "$TARGET_HOLD" ] && [ -d "$TARGET_HOLD" ]; then
		mv "$TARGET_HOLD" "$RELEASE_DIR" || restore_failed=1
	fi
	if [ -n "$LEGACY_RELEASE" ] && [ -d "$LEGACY_RELEASE" ]; then
		remove_managed_tree "$LEGACY_RELEASE" || restore_failed=1
	fi

	if [ "$BACKUP_READY" -eq 1 ]; then
		restore_path "$INSTALL_ROOT/current" current_link || restore_failed=1
		restore_path "$INSTALL_ROOT/last-good" last_good_link || restore_failed=1
		restore_path "$CONFIG_FILE" config || restore_failed=1
		restore_path "$TRUST_FILE" trust || restore_failed=1
		restore_data || restore_failed=1
		restore_path "$DEPLOYMENT_FILE" deployment || restore_failed=1
		restore_path "$INSTALL_ROOT/control/vohivectl" control || restore_failed=1
		restore_path "$INSTALL_ROOT/control/vohivectl.previous" control_previous || restore_failed=1
		restore_path /usr/local/sbin/vohivectl convenience || restore_failed=1
		case "$SERVICE_TYPE" in
			systemd)
				restore_path /etc/systemd/system/vohive.service unit_main || restore_failed=1
				restore_path /etc/systemd/system/vohive-update.service unit_update || restore_failed=1
				restore_path /etc/systemd/system/vohive-recover.service unit_recover || restore_failed=1
				restore_path /etc/systemd/system/vohive-host-config.service unit_host_config || restore_failed=1
				restore_path /etc/systemd/system/multi-user.target.wants/vohive.service enable_main || restore_failed=1
				restore_path /etc/systemd/system/multi-user.target.wants/vohive-recover.service enable_recover || restore_failed=1
				systemctl daemon-reload >/dev/null 2>&1 || restore_failed=1
				;;
			openwrt)
				restore_path /etc/init.d/vohive unit_main || restore_failed=1
				restore_path /etc/init.d/vohive-update unit_update || restore_failed=1
				restore_path /etc/init.d/vohive-recover unit_recover || restore_failed=1
				restore_path /etc/rc.d/S99vohive enable_main || restore_failed=1
				restore_path /etc/rc.d/S97vohive-recover enable_recover || restore_failed=1
				;;
		esac
		restore_enablement || restore_failed=1
	fi

	if [ "$restore_failed" -eq 0 ] && [ "$OLD_SERVICE_ACTIVE" -eq 1 ]; then
		start_service >/dev/null 2>&1 || restore_failed=1
	fi
	if [ "$restore_failed" -eq 0 ]; then
		write_terminal_state rolled_back 'installation failed and was rolled back' "$OLD_VERSION" >/dev/null 2>&1
		warn 'the previous deployment was restored'
	else
		write_manual_recovery_state >/dev/null 2>&1
		warn "automatic restoration was incomplete; service left stopped; backup: $TRANSACTION_BACKUP"
	fi
	if [ "$LOCK_HELD" -eq 1 ]; then rm -f -- "$LOCK_FILE"; LOCK_HELD=0; fi
	set -e
	[ "$restore_failed" -eq 0 ]
}

cleanup() {
	rc=$?
	trap - EXIT HUP INT TERM
	set +e
	if [ "$TRANSACTION_ACTIVE" -eq 1 ]; then rollback_transaction || rc=1; fi
	if [ -n "$STAGING_DIR" ] && [ -d "$STAGING_DIR" ]; then remove_managed_tree "$STAGING_DIR" >/dev/null 2>&1; fi
	if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
		case "$TMP_DIR" in /tmp/vohive-install.*|${TMPDIR:-/tmp}/vohive-install.*) rm -rf -- "$TMP_DIR" ;; esac
	fi
	if [ "$LOCK_HELD" -eq 1 ]; then rm -f -- "$LOCK_FILE"; fi
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

for command_name in awk grep sed tar mktemp date tr sync; do need_command "$command_name"; done
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/vohive-install.XXXXXXXX")

download() {
	url=$1
	destination=$2
	case "$url" in
		https://api.github.com/repos/zanescope/vohive/*|https://github.com/zanescope/vohive/releases/*) ;;
		*) die "refusing untrusted download URL: $url" ;;
	esac
	if command -v curl >/dev/null 2>&1; then
		curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --retry 3 --connect-timeout 15 -o "$destination" "$url"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$destination" "$url"
	else
		die 'curl or wget is required'
	fi
}

sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
	elif command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "$1" | awk '{print $NF}'
	else die 'sha256sum, shasum, or openssl is required'
	fi
}

file_size() { wc -c <"$1" | tr -d '[:space:]'; }

extract_json_string() {
	sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$2" | sed -n '1p'
}

validate_trust_material() {
	case "$BOOTSTRAP_VERSION" in ''|*@VOHIVE_*@*) die 'installer bootstrap version was not injected' ;; esac
	valid_version "$BOOTSTRAP_VERSION" || die 'invalid injected bootstrap version'
	case "$TRUSTED_MINISIGN_PUBLIC_KEYS" in ''|*@VOHIVE_*@*) die 'installer public keys were not injected' ;; esac
	case "$VERIFY_SHA256_MAP" in ''|*@VOHIVE_*@*) die 'installer verifier hashes were not injected' ;; esac
	old_ifs=$IFS; IFS=';'; key_count=0
	for key in $TRUSTED_MINISIGN_PUBLIC_KEYS; do
		case "$key" in RW*) key_count=$((key_count + 1)) ;; *) IFS=$old_ifs; die 'invalid minisign public key' ;; esac
	done
	IFS=$old_ifs
	[ "$key_count" -ge 1 ] || die 'no minisign public key was injected'
	for wanted_arch in amd64 arm64 armv7; do
		found=0; old_ifs=$IFS; IFS=';'
		for item in $VERIFY_SHA256_MAP; do
			case "$item" in
				"$wanted_arch"=*) value=${item#*=}; is_sha256 "$value" || { IFS=$old_ifs; die "invalid verifier hash for $wanted_arch"; }; found=$((found + 1)) ;;
				amd64=*|arm64=*|armv7=*) ;;
				*) IFS=$old_ifs; die 'verifier hash map has an unknown key' ;;
			esac
		done
		IFS=$old_ifs
		[ "$found" -eq 1 ] || die "verifier hash map must contain $wanted_arch exactly once"
	done
}

verifier_hash() {
	old_ifs=$IFS; IFS=';'; result=''
	for item in $VERIFY_SHA256_MAP; do case "$item" in "$ARCH"=*) result=${item#*=} ;; esac; done
	IFS=$old_ifs
	printf '%s\n' "$result"
}

detect_arch() {
	case "$(uname -m)" in
		x86_64|amd64) ARCH='amd64' ;;
		aarch64|arm64) ARCH='arm64' ;;
		armv7l|armv7|armhf) ARCH='armv7' ;;
		*) die "unsupported architecture: $(uname -m)" ;;
	esac
}

detect_service() {
	if [ "$NO_SERVICE" -eq 1 ]; then SERVICE_TYPE='portable'
	elif [ -f /etc/openwrt_release ] && [ -x /sbin/procd ]; then SERVICE_TYPE='openwrt'
	elif command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then SERVICE_TYPE='systemd'
	else
		die 'no supported service manager detected; install systemd/OpenWrt procd, or use --no-service only if you will start and monitor VoHive yourself'
	fi
}

detect_layout() {
	if [ ! -f "$DEPLOYMENT_FILE" ] && [ -f /opt/vohive/config/config.yaml ]; then
		LAYOUT='v1'
		CONFIG_FILE='/opt/vohive/config/config.yaml'
		DATA_ROOT='/opt/vohive/data'
		TRUST_FILE='/opt/vohive/config/update.pub'
	fi
}

highest_semver() {
	awk '
	function cmp(a,b, x,y,ac,bc,ap,bp,ai,bi,n,i,an,bn) {
		sub(/^v/,"",a); sub(/^v/,"",b); sub(/\+.*/,"",a); sub(/\+.*/,"",b)
		x=index(a,"-"); if(x){ap=substr(a,x+1); ac=substr(a,1,x-1)}else{ap=""; ac=a}
		x=index(b,"-"); if(x){bp=substr(b,x+1); bc=substr(b,1,x-1)}else{bp=""; bc=b}
		split(ac,ai,"."); split(bc,bi,".")
		for(i=1;i<=3;i++){an=ai[i]+0; bn=bi[i]+0; if(an<bn)return -1; if(an>bn)return 1}
		if(ap==""&&bp!="")return 1; if(ap!=""&&bp=="")return -1
		n=split(ap,ai,"."); x=split(bp,bi,".")
		for(i=1;i<=n||i<=x;i++){
			if(i>n)return -1; if(i>x)return 1
			an=(ai[i]~/^[0-9]+$/); bn=(bi[i]~/^[0-9]+$/)
			if(an&&bn){if((ai[i]+0)<(bi[i]+0))return -1;if((ai[i]+0)>(bi[i]+0))return 1}
			else if(an&&!bn)return -1; else if(!an&&bn)return 1
			else{if(ai[i]<bi[i])return -1;if(ai[i]>bi[i])return 1}
		}
		return 0
	}
	NF && (!set || cmp($0,best)>0){best=$0;set=1} END{if(set)print best}'
}

resolve_version() {
	[ -z "$VERSION" ] || return 0
	metadata="$TMP_DIR/releases.json"
	if [ "$CHANNEL" = stable ]; then
		download "$API_BASE/releases/latest" "$metadata"
		VERSION=$(extract_json_string tag_name "$metadata")
	else
		download "$API_BASE/releases?per_page=50" "$metadata"
		VERSION=$(awk '
			/"tag_name"[[:space:]]*:/ {line=$0;sub(/^.*"tag_name"[[:space:]]*:[[:space:]]*"/,"",line);sub(/".*$/,"",line);tag=line;draft=""}
			/"draft"[[:space:]]*:/ && draft=="" {draft=($0~/true/)?"true":"false"}
			/"prerelease"[[:space:]]*:/ && tag!="" {if($0~/true/&&draft!="true")print tag;tag=""}
		' "$metadata" | highest_semver)
	fi
	[ -n "$VERSION" ] && valid_version "$VERSION" || die "could not resolve a valid $CHANNEL release"
}

prepare_verifier() {
	command -v minisign >/dev/null 2>&1 && return 0
	VERIFY_BIN="$TMP_DIR/vohive-verify"
	name="vohive-verify_${BOOTSTRAP_VERSION}_linux_${ARCH}"
	download "$RELEASE_BASE/download/$BOOTSTRAP_VERSION/$name" "$VERIFY_BIN"
	[ "$(sha256_file "$VERIFY_BIN")" = "$(verifier_hash)" ] || die 'bootstrap verifier SHA-256 mismatch'
	chmod 0700 "$VERIFY_BIN"
}

verify_signature() {
	message=$1
	signature=$2
	if command -v minisign >/dev/null 2>&1; then
		old_ifs=$IFS; IFS=';'
		for key in $TRUSTED_MINISIGN_PUBLIC_KEYS; do
			if minisign -Vm "$message" -x "$signature" -P "$key" >/dev/null 2>&1; then IFS=$old_ifs; return 0; fi
		done
		IFS=$old_ifs
		return 1
	fi
	"$VERIFY_BIN" -public-keys "$TRUSTED_MINISIGN_PUBLIC_KEYS" -file "$message" -signature "$signature"
}

manifest_artifact_value() {
	awk -v asset="$1" -v field="$2" '
	$0~/"name"[[:space:]]*:/ {line=$0;sub(/^.*"name"[[:space:]]*:[[:space:]]*"/,"",line);sub(/".*$/,"",line);inside=(line==asset)}
	inside && index($0,"\"" field "\"") {line=$0;sub(/^.*:[[:space:]]*/,"",line);gsub(/[,"[:space:]]/,"",line);print line;exit}' "$MANIFEST"
}

verify_metadata() {
	base="$RELEASE_BASE/download/$VERSION"
	MANIFEST="$TMP_DIR/release-manifest.json"
	manifest_sig="$MANIFEST.minisig"
	sums="$TMP_DIR/SHA256SUMS"
	sums_sig="$sums.minisig"
	download "$base/release-manifest.json" "$MANIFEST"
	download "$base/release-manifest.json.minisig" "$manifest_sig"
	download "$base/SHA256SUMS" "$sums"
	download "$base/SHA256SUMS.minisig" "$sums_sig"
	verify_signature "$MANIFEST" "$manifest_sig" || die 'release manifest signature verification failed'
	verify_signature "$sums" "$sums_sig" || die 'SHA256SUMS signature verification failed'
	grep -Eq '"schema"[[:space:]]*:[[:space:]]*1([,[:space:]]|$)' "$MANIFEST" || die 'unsupported manifest schema'
	grep -Eq '"product"[[:space:]]*:[[:space:]]*"vohive"' "$MANIFEST" || die 'manifest product mismatch'
	grep -Eq '"repository"[[:space:]]*:[[:space:]]*"zanescope/vohive"' "$MANIFEST" || die 'manifest repository mismatch'
	grep -Fq "\"version\": \"$VERSION\"" "$MANIFEST" || die 'manifest version mismatch'
	grep -Fq "\"channel\": \"$CHANNEL\"" "$MANIFEST" || die 'manifest channel mismatch'
	ASSET="vohive_${VERSION}_linux_${ARCH}.tar.gz"
	grep -Fq "\"name\": \"$ASSET\"" "$MANIFEST" || die "manifest has no linux/$ARCH artifact"
	MANIFEST_SHA=$(manifest_artifact_value "$ASSET" sha256)
	MANIFEST_SIZE=$(manifest_artifact_value "$ASSET" size)
	artifact_goos=$(manifest_artifact_value "$ASSET" goos)
	artifact_goarch=$(manifest_artifact_value "$ASSET" goarch)
	artifact_format=$(manifest_artifact_value "$ASSET" format)
	artifact_binary=$(manifest_artifact_value "$ASSET" binary_path)
	is_sha256 "$MANIFEST_SHA" || die 'invalid manifest artifact hash'
	case "$MANIFEST_SIZE" in ''|*[!0-9]*|0) die 'invalid manifest artifact size' ;; esac
	[ "$artifact_goos" = linux ] && [ "$artifact_goarch" = "$ARCH" ] || die 'manifest artifact platform mismatch'
	[ "$artifact_format" = tar.gz ] && [ "$artifact_binary" = vohive ] || die 'manifest artifact format mismatch'
	sums_sha=$(awk -v file="$ASSET" '{name=$2;sub(/^\*/,"",name);if(name==file){print $1;exit}}' "$sums")
	is_sha256 "$sums_sha" && [ "$sums_sha" = "$MANIFEST_SHA" ] || die 'signed hash metadata disagrees'
}

random_password() {
	if [ -r /dev/urandom ] && command -v od >/dev/null 2>&1; then od -An -N24 -tx1 /dev/urandom | tr -d ' \n'
	elif command -v openssl >/dev/null 2>&1; then openssl rand -hex 24
	else return 1
	fi
}

write_readiness_key() {
	if [ -r /dev/urandom ] && command -v od >/dev/null 2>&1; then
		readiness_key=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
	elif command -v openssl >/dev/null 2>&1; then
		readiness_key=$(openssl rand -hex 32)
	else
		die 'a secure random source is required for readiness verification'
	fi
	case "$readiness_key" in
		????????????????????????????????????????????????????????????????) ;;
		*) die 'secure readiness key generation failed' ;;
	esac
	tmp_key="$STATE_ROOT/readiness.key.tmp.$$"
	printf '%s\n' "$readiness_key" >"$tmp_key"
	chmod 0600 "$tmp_key"
	mv -f "$tmp_key" "$STATE_ROOT/readiness.key"
	readiness_key=''
}

write_initial_config() {
	[ ! -e "$CONFIG_FILE" ] || return 0
	INITIAL_PASSWORD=$(random_password) || die 'a secure random source is required for initial credentials'
	[ "${#INITIAL_PASSWORD}" -ge 48 ] || die 'secure password generation failed'
	config_root=$(dirname "$CONFIG_FILE")
	mkdir -p "$config_root"
	chmod 0700 "$config_root"
	tmp_config="$CONFIG_FILE.tmp.$$"
	cat >"$tmp_config" <<EOF
config_schema: 1
server:
  port: 7575
  debug: false
web:
  username: admin
  password: "$INITIAL_PASSWORD"
free_device_limit: 5
vowifi:
  enabled: false
EOF
	chmod 0600 "$tmp_config"
	mv -f "$tmp_config" "$CONFIG_FILE"
}

write_trust_root() {
	mkdir -p "$(dirname "$TRUST_FILE")"
	tmp_trust="$TRUST_FILE.tmp.$$"
	printf '%s\n' "$TRUSTED_MINISIGN_PUBLIC_KEYS" | tr ';' '\n' >"$tmp_trust"
	chmod 0600 "$tmp_trust"
	mv -f "$tmp_trust" "$TRUST_FILE"
}

record_existing_deployment() {
	if [ -f "$DEPLOYMENT_FILE" ]; then
		HAD_DEPLOYMENT=1
		installed_product=$(extract_json_string product "$DEPLOYMENT_FILE")
		installed_repository=$(extract_json_string repository "$DEPLOYMENT_FILE")
		[ "$installed_product" = vohive ] && [ "$installed_repository" = "$REPOSITORY" ] || die 'deployment identity mismatch'
		OLD_VERSION=$(extract_json_string current_version "$DEPLOYMENT_FILE")
		INSTALLED_VERSION=$OLD_VERSION
		OLD_LAST_GOOD_VERSION=$(extract_json_string last_good_version "$DEPLOYMENT_FILE")
		installed_channel=$(extract_json_string channel "$DEPLOYMENT_FILE")
		installed_service=$(extract_json_string install_type "$DEPLOYMENT_FILE")
		installed_layout=$(extract_json_string layout "$DEPLOYMENT_FILE")
		installed_config=$(extract_json_string config_path "$DEPLOYMENT_FILE")
		installed_data=$(extract_json_string data_path "$DEPLOYMENT_FILE")
		case "$installed_config" in /etc/vohive/config.yaml|/opt/vohive/config/config.yaml) CONFIG_FILE=$installed_config ;; *) die 'deployment config path is outside the supported scope' ;; esac
		case "$installed_data" in /var/lib/vohive/data|/opt/vohive/data) DATA_ROOT=$installed_data ;; *) die 'deployment data path is outside the supported scope' ;; esac
		case "$installed_layout" in v1|v2) LAYOUT=$installed_layout ;; *) die 'invalid installed layout' ;; esac
		TRUST_FILE="$(dirname "$CONFIG_FILE")/update.pub"
		if [ "$REPAIR" -eq 0 ]; then say "VoHive $OLD_VERSION is already installed. Use the Web update button or 'sudo vohivectl update'."; exit 0; fi
		case "$installed_service" in systemd|openwrt|portable) ;; *) die 'invalid installed service type' ;; esac
		[ "$SERVICE_TYPE" = "$installed_service" ] || die 'repair cannot change the installed service type; use the matching service mode'
		[ "$VERSION_SET" -eq 1 ] || VERSION=$OLD_VERSION
		if [ "$CHANNEL_SET" -eq 0 ] && [ -n "$installed_channel" ]; then CHANNEL=$installed_channel; fi
		case "$CHANNEL" in stable|beta) ;; *) die 'installed channel is not repairable by this installer' ;; esac
	fi

	if [ -L "$INSTALL_ROOT/current" ]; then
		OLD_TARGET=$(readlink "$INSTALL_ROOT/current")
		validate_release_target "$OLD_TARGET" || die 'current link is outside the managed release root'
		OLD_TARGET_ABS=$(absolute_release_target "$OLD_TARGET")
		if [ -z "$OLD_VERSION" ]; then
			current_name=$(basename "$OLD_TARGET_ABS")
			case "$current_name" in v*) OLD_VERSION=$current_name ;; *) OLD_VERSION="v$current_name" ;; esac
			valid_version "$OLD_VERSION" || OLD_VERSION=''
		fi
	elif [ -e "$INSTALL_ROOT/current" ]; then
		die 'current path exists but is not a symlink'
	fi
	if [ -L "$INSTALL_ROOT/last-good" ]; then
		OLD_LAST_GOOD_TARGET=$(readlink "$INSTALL_ROOT/last-good")
		validate_release_target "$OLD_LAST_GOOD_TARGET" || die 'last-good link is outside the managed release root'
		OLD_LAST_GOOD_TARGET_ABS=$(absolute_release_target "$OLD_LAST_GOOD_TARGET")
	elif [ -e "$INSTALL_ROOT/last-good" ]; then
		die 'last-good path exists but is not a symlink'
	fi
	if [ -z "$OLD_TARGET" ] && { [ -f "$INSTALL_ROOT/bin/vohive" ] || [ -f "$INSTALL_ROOT/vohive" ]; }; then
		if [ -f "$INSTALL_ROOT/bin/vohive" ]; then LEGACY_SOURCE="$INSTALL_ROOT/bin/vohive"; else LEGACY_SOURCE="$INSTALL_ROOT/vohive"; fi
		OLD_VERSION="v0.0.0-legacy.$(date +%Y%m%d%H%M%S)"
		OLD_TARGET="releases/$OLD_VERSION"
		OLD_TARGET_ABS="$INSTALL_ROOT/$OLD_TARGET"
	fi
	if [ -z "$LEGACY_SOURCE" ]; then RECOVERY_PREVIOUS_TARGET=$OLD_TARGET_ABS; fi
}

record_service_state() {
	case "$SERVICE_TYPE" in
		systemd)
		if systemctl is-active --quiet vohive.service 2>/dev/null; then OLD_SERVICE_ACTIVE=1; fi
		if systemctl is-enabled --quiet vohive.service 2>/dev/null; then OLD_MAIN_ENABLED=1; fi
		if systemctl is-enabled --quiet vohive-recover.service 2>/dev/null; then OLD_RECOVER_ENABLED=1; fi
		;;
		openwrt)
		if [ -x /etc/init.d/vohive ] && /etc/init.d/vohive running >/dev/null 2>&1; then OLD_SERVICE_ACTIVE=1; fi
		if [ -x /etc/init.d/vohive ] && /etc/init.d/vohive enabled >/dev/null 2>&1; then OLD_MAIN_ENABLED=1; fi
		if [ -x /etc/init.d/vohive-recover ] && /etc/init.d/vohive-recover enabled >/dev/null 2>&1; then OLD_RECOVER_ENABLED=1; fi
		;;
	esac
}

backup_transaction() {
	save_path "$INSTALL_ROOT/current" current_link
	save_path "$INSTALL_ROOT/last-good" last_good_link
	save_path "$CONFIG_FILE" config
	save_path "$TRUST_FILE" trust
	save_data
	save_path "$DEPLOYMENT_FILE" deployment
	save_path "$INSTALL_ROOT/control/vohivectl" control
	save_path "$INSTALL_ROOT/control/vohivectl.previous" control_previous
	save_path /usr/local/sbin/vohivectl convenience
	case "$SERVICE_TYPE" in
		systemd)
			save_path /etc/systemd/system/vohive.service unit_main
			save_path /etc/systemd/system/vohive-update.service unit_update
			save_path /etc/systemd/system/vohive-recover.service unit_recover
			save_path /etc/systemd/system/vohive-host-config.service unit_host_config
			save_path /etc/systemd/system/multi-user.target.wants/vohive.service enable_main
			save_path /etc/systemd/system/multi-user.target.wants/vohive-recover.service enable_recover
			;;
		openwrt)
			save_path /etc/init.d/vohive unit_main
			save_path /etc/init.d/vohive-update unit_update
			save_path /etc/init.d/vohive-recover unit_recover
			save_path /etc/rc.d/S99vohive enable_main
			save_path /etc/rc.d/S97vohive-recover enable_recover
			;;
	esac
	config_present=false; data_present=false; deployment_present=false; control_present=false
	if [ -f "$TRANSACTION_BACKUP/config.file" ]; then
		config_present=true
		cp -p "$TRANSACTION_BACKUP/config.file" "$TRANSACTION_BACKUP/config.yaml"
		chmod 0600 "$TRANSACTION_BACKUP/config.yaml"
	fi
	[ -f "$TRANSACTION_BACKUP/data.present" ] && data_present=true
	if [ -f "$TRANSACTION_BACKUP/deployment.file" ]; then
		deployment_present=true
		cp -p "$TRANSACTION_BACKUP/deployment.file" "$TRANSACTION_BACKUP/deployment.json"
		chmod 0600 "$TRANSACTION_BACKUP/deployment.json"
	fi
	if [ -f "$TRANSACTION_BACKUP/control.file" ]; then
		control_present=true
		mkdir -p "$TRANSACTION_BACKUP/control"
		chmod 0700 "$TRANSACTION_BACKUP/control"
		cp -p "$TRANSACTION_BACKUP/control.file" "$TRANSACTION_BACKUP/control/vohivectl"
		chmod 0755 "$TRANSACTION_BACKUP/control/vohivectl"
	fi
	main_enabled=false; recover_enabled=false; service_active=false
	[ "$OLD_MAIN_ENABLED" -eq 1 ] && main_enabled=true
	[ "$OLD_RECOVER_ENABLED" -eq 1 ] && recover_enabled=true
	[ "$OLD_SERVICE_ACTIVE" -eq 1 ] && service_active=true
	source_version=$OLD_VERSION; [ -n "$source_version" ] || source_version='v0.0.0-uninitialized'
	cat >"$TRANSACTION_BACKUP/metadata.json" <<EOF
{"schema":1,"created_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","source_version":"$source_version","config_path":"$CONFIG_FILE","data_path":"$DATA_ROOT","config_present":$config_present,"data_present":$data_present,"deployment_present":$deployment_present,"control_present":$control_present,"install_type":"$SERVICE_TYPE","main_enabled":$main_enabled,"recover_enabled":$recover_enabled,"service_active":$service_active}
EOF
	chmod 0600 "$TRANSACTION_BACKUP/metadata.json"
	BACKUP_READY=1
}

install_systemd_units() {
	config_parent=$(dirname "$CONFIG_FILE")
	data_parent=$(dirname "$DATA_ROOT")
	rm -f -- /etc/systemd/system/vohive.service /etc/systemd/system/vohive-update.service /etc/systemd/system/vohive-recover.service /etc/systemd/system/vohive-host-config.service
	cat >/etc/systemd/system/vohive.service <<EOF
[Unit]
Description=VoHive cellular modem manager
Documentation=https://github.com/zanescope/vohive
Wants=network-online.target
After=network-online.target vohive-recover.service

[Service]
Type=simple
User=root
Group=root
WorkingDirectory=$data_parent
ExecStartPre=/opt/vohive/control/vohivectl guard-start
ExecStart=/opt/vohive/current/vohive -c $CONFIG_FILE
Restart=on-failure
RestartSec=5s
TimeoutStopSec=20s
KillSignal=SIGTERM
UMask=0077
RuntimeDirectory=vohive
RuntimeDirectoryMode=0700
PrivateTmp=true
ProtectHome=true
ProtectSystem=full
ReadWritePaths=$config_parent $data_parent
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
	cat >/etc/systemd/system/vohive-update.service <<'EOF'
[Unit]
Description=VoHive transactional update
Documentation=https://github.com/zanescope/vohive
Wants=network-online.target
After=network-online.target vohive.service
ConditionPathExists=/var/lib/vohive/update/request.json

[Service]
Type=oneshot
User=root
Group=root
WorkingDirectory=/var/lib/vohive
ExecStart=/opt/vohive/control/vohivectl update --request /var/lib/vohive/update/request.json
TimeoutStartSec=30min
UMask=0077
PrivateTmp=true
ProtectHome=true
ProtectSystem=full
ReadWritePaths=/opt/vohive /etc/vohive /var/lib/vohive
EOF
	cat >/etc/systemd/system/vohive-recover.service <<'EOF'
[Unit]
Description=Recover an interrupted VoHive update
Documentation=https://github.com/zanescope/vohive
DefaultDependencies=no
After=local-fs.target
Before=vohive.service

[Service]
Type=oneshot
User=root
Group=root
WorkingDirectory=/var/lib/vohive
ExecStart=/opt/vohive/control/vohivectl recover --boot
TimeoutStartSec=5min
UMask=0077
PrivateTmp=true
ProtectHome=true
ProtectSystem=full
ReadWritePaths=/opt/vohive /etc/vohive /var/lib/vohive

[Install]
WantedBy=multi-user.target
EOF
	cat >/etc/systemd/system/vohive-host-config.service <<'EOF'
[Unit]
Description=Apply VoHive managed host configuration
Documentation=https://github.com/zanescope/vohive
After=local-fs.target
ConditionPathExists=/var/lib/vohive/host-config/request.json

[Service]
Type=oneshot
User=root
Group=root
WorkingDirectory=/var/lib/vohive
ExecStart=/opt/vohive/control/vohivectl host-config --request /var/lib/vohive/host-config/request.json
TimeoutStartSec=30s
UMask=0077
PrivateTmp=true
PrivateDevices=true
ProtectHome=true
ProtectSystem=strict
ReadOnlyPaths=/sys
ReadWritePaths=/etc/udev/rules.d /var/lib/vohive/host-config /var/lib/vohive/update
NoNewPrivileges=true
CapabilityBoundingSet=
AmbientCapabilities=
ProtectControlGroups=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectClock=true
RestrictSUIDSGID=true
LockPersonality=true
RestrictAddressFamilies=AF_UNIX
EOF
	chmod 0644 /etc/systemd/system/vohive.service /etc/systemd/system/vohive-update.service /etc/systemd/system/vohive-recover.service /etc/systemd/system/vohive-host-config.service
	systemctl daemon-reload
	systemctl enable vohive.service vohive-recover.service >/dev/null
}

install_openwrt_units() {
	data_parent=$(dirname "$DATA_ROOT")
	rm -f -- /etc/init.d/vohive /etc/init.d/vohive-update /etc/init.d/vohive-recover
	cat >/etc/init.d/vohive <<EOF
#!/bin/sh /etc/rc.common
START=99
STOP=10
USE_PROCD=1
start_service() {
	[ -x /opt/vohive/control/vohivectl ] || return 1
	/opt/vohive/control/vohivectl guard-start || return 1
	mkdir -p "$data_parent/logs" "$DATA_ROOT"
	procd_open_instance
	procd_set_param command /opt/vohive/current/vohive -c "$CONFIG_FILE"
	procd_set_param respawn 3600 5 5
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_set_param file "$CONFIG_FILE"
	procd_set_param cwd "$data_parent"
	procd_set_param limits core="0"
	procd_close_instance
}
service_triggers() {
	procd_add_reload_trigger vohive
}
EOF
	cat >/etc/init.d/vohive-update <<'EOF'
#!/bin/sh /etc/rc.common
STOP=11
USE_PROCD=1
start_service() {
	[ -x /opt/vohive/control/vohivectl ] || return 1
	[ -f /var/lib/vohive/update/request.json ] || return 0
	procd_open_instance
	procd_set_param command /opt/vohive/control/vohivectl update --request /var/lib/vohive/update/request.json
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_set_param cwd /var/lib/vohive
	procd_set_param limits core="0"
	procd_close_instance
}
EOF
	cat >/etc/init.d/vohive-recover <<'EOF'
#!/bin/sh /etc/rc.common
START=97
STOP=12
start_service() {
	[ -x /opt/vohive/control/vohivectl ] || return 0
	/opt/vohive/control/vohivectl recover --boot
}
EOF
	chmod 0755 /etc/init.d/vohive /etc/init.d/vohive-update /etc/init.d/vohive-recover
	/etc/init.d/vohive enable
	/etc/init.d/vohive-recover enable
}

ready_once() {
	"$INSTALL_ROOT/control/vohivectl" --deployment "$DEPLOYMENT_FILE" \
		probe --expected-version "$VERSION" >/dev/null 2>&1
}

wait_ready() {
	[ "$SERVICE_TYPE" != portable ] || return 0
	deadline=$(( $(date +%s) + 90 )); successes=0
	while [ "$(date +%s)" -lt "$deadline" ]; do
		if ready_once; then successes=$((successes + 1)); else successes=0; fi
		[ "$successes" -ge 3 ] && break
		sleep 2
	done
	[ "$successes" -ge 3 ] || return 1
	stable_deadline=$(( $(date +%s) + 30 ))
	while [ "$(date +%s)" -lt "$stable_deadline" ]; do sleep 2; ready_once || return 1; done
}

prepare_release_archive() {
	resolve_version
	prepare_verifier
	verify_metadata
	archive="$TMP_DIR/$ASSET"
	download "$RELEASE_BASE/download/$VERSION/$ASSET" "$archive"
	[ "$(sha256_file "$archive")" = "$MANIFEST_SHA" ] || die 'program archive SHA-256 mismatch'
	[ "$(file_size "$archive")" = "$MANIFEST_SIZE" ] || die 'program archive size mismatch'
	archive_list="$TMP_DIR/archive.list"
	tar -tzf "$archive" >"$archive_list"
	awk '{name=$0;sub(/^\.\//,"",name);if(name!="vohive"&&name!="vohivectl"&&name!="LICENSE")exit 1;seen[name]++}
END{if(seen["vohive"]!=1||seen["vohivectl"]!=1||seen["LICENSE"]!=1)exit 1}' "$archive_list" || die 'unsafe release archive'
	RELEASE_PREPARED=1
}

detect_arch
validate_trust_material
[ "$DRY_RUN" -eq 1 ] || [ "$(id -u)" -eq 0 ] || die 'run as root (for example: sudo sh vohive-install.sh)'
detect_service
if [ "$DRY_RUN" -eq 1 ]; then
	[ "$REPAIR" -eq 0 ] || die '--repair cannot be combined with --dry-run'
	resolve_version
	prepare_verifier
	verify_metadata
	say "Repository  : $REPOSITORY"
	say "Version     : $VERSION"
	say "Channel     : $CHANNEL"
	say "Architecture: $ARCH"
	say "Service     : $SERVICE_TYPE"
	say 'Dry run complete: signed metadata is valid and no system file changed.'
	exit 0
fi

# Resolve and verify targets that do not depend on installed state before
# taking the host lock. Repair-without-version is resolved after discovery.
if [ "$REPAIR" -eq 0 ] || [ "$VERSION_SET" -eq 1 ]; then
	prepare_release_archive
fi

mkdir -p "$STATE_ROOT"
chmod 0700 /var/lib/vohive "$STATE_ROOT"
TRANSACTION_ID="install-$(date +%Y%m%dT%H%M%S)-$$"
TRANSACTION_STARTED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
TRANSACTION_BACKUP="$BACKUP_ROOT/bootstrap-$(date +%Y%m%dT%H%M%S)-$$"
[ -r /proc/sys/kernel/random/boot_id ] && [ -r "/proc/$$/stat" ] || die 'Linux process identity files are unavailable'
LOCK_BOOT_ID=$(sed -n '1p' /proc/sys/kernel/random/boot_id)
LOCK_PROCESS_START_TICKS=$(sed 's/^[^)]*) //' "/proc/$$/stat" | awk '{print $20}')
case "$LOCK_BOOT_ID" in ''|*[!0-9A-Fa-f-]*) die 'invalid Linux boot identity' ;; esac
case "$LOCK_PROCESS_START_TICKS" in ''|*[!0-9]*) die 'invalid installer process start time' ;; esac
# A first-install crash may precede installation of the recovery unit. Reclaim
# only a no-state lock whose complete Linux process identity is demonstrably
# stale; incomplete or ambiguous locks remain fail-closed.
if [ ! -e "$STATE_ROOT/state.json" ] && [ ! -L "$STATE_ROOT/state.json" ] && [ -f "$LOCK_FILE" ] && [ ! -L "$LOCK_FILE" ]; then
	locked_pid=$(sed -n 's/^pid=//p' "$LOCK_FILE" | sed -n '1p')
	locked_boot_id=$(sed -n 's/^boot_id=//p' "$LOCK_FILE" | sed -n '1p')
	locked_start_ticks=$(sed -n 's/^process_start_ticks=//p' "$LOCK_FILE" | sed -n '1p')
	case "$locked_pid:$locked_start_ticks" in *[!0-9:]*|:*|*:) lock_identity_complete=0 ;; *) lock_identity_complete=1 ;; esac
	if [ "$lock_identity_complete" -eq 1 ]; then
		lock_is_stale=0
		if [ "$locked_boot_id" != "$LOCK_BOOT_ID" ]; then
			lock_is_stale=1
		elif [ ! -r "/proc/$locked_pid/stat" ]; then
			lock_is_stale=1
		else
			observed_start_ticks=$(sed 's/^[^)]*) //' "/proc/$locked_pid/stat" | awk '{print $20}')
			[ "$observed_start_ticks" = "$locked_start_ticks" ] || lock_is_stale=1
		fi
		[ "$lock_is_stale" -eq 0 ] || rm -f -- "$LOCK_FILE"
	fi
fi
if ! (set -C; printf 'pid=%s\nstarted=%s\nboot_id=%s\nprocess_start_ticks=%s\n' "$$" "$TRANSACTION_STARTED_AT" "$LOCK_BOOT_ID" "$LOCK_PROCESS_START_TICKS" >"$LOCK_FILE") 2>/dev/null; then
	die 'another install or update transaction is active'
fi
LOCK_HELD=1

# Everything below observes mutable deployment and release state only while
# this transaction owns update.lock.
if [ -e "$STATE_ROOT/state.json" ] || [ -L "$STATE_ROOT/state.json" ]; then
	[ -f "$STATE_ROOT/state.json" ] && [ ! -L "$STATE_ROOT/state.json" ] || die 'existing update state is not a regular file; run vohivectl doctor before installing'
	existing_phase=$(extract_json_string phase "$STATE_ROOT/state.json")
	case "$existing_phase" in
		completed|rolled_back|failed) ;;
		manual_recovery_required) die 'manual recovery is required; restore the recorded backup before running install or repair again' ;;
		'') die 'existing update state is invalid; run vohivectl doctor before installing' ;;
		*) die "an install or update transaction is unresolved (phase: $existing_phase); reboot for recovery, then run vohivectl doctor" ;;
	esac
fi
detect_layout
record_existing_deployment
[ "$RELEASE_PREPARED" -eq 1 ] || prepare_release_archive
[ "$HAD_DEPLOYMENT" -eq 0 ] || [ "$REPAIR" -eq 0 ] || [ "$VERSION" = "$INSTALLED_VERSION" ] || die '--repair only repairs the installed version; use vohivectl update for a version change'

say "Repository  : $REPOSITORY"
say "Version     : $VERSION"
say "Channel     : $CHANNEL"
say "Architecture: $ARCH"
say "Service     : $SERVICE_TYPE"
say "Layout      : $LAYOUT"

mkdir -p "$INSTALL_ROOT/releases" "$INSTALL_ROOT/control" "$STATE_ROOT" "$BACKUP_ROOT" /var/lib/vohive/logs /usr/local/sbin
chmod 0755 "$INSTALL_ROOT" "$INSTALL_ROOT/releases" "$INSTALL_ROOT/control"
chmod 0700 /var/lib/vohive "$STATE_ROOT" "$BACKUP_ROOT"
RELEASE_NAME=$(printf %s "$VERSION" | tr -cd 'A-Za-z0-9.-')
RELEASE_DIR="$INSTALL_ROOT/releases/$RELEASE_NAME"
STAGING_DIR="$INSTALL_ROOT/releases/.staging-$RELEASE_NAME.$$"
remove_managed_tree "$STAGING_DIR" >/dev/null 2>&1 || true
mkdir "$STAGING_DIR"
chmod 0755 "$STAGING_DIR"
tar -xzf "$archive" -C "$STAGING_DIR"
for staged_name in vohive vohivectl LICENSE; do
	[ -f "$STAGING_DIR/$staged_name" ] && [ ! -L "$STAGING_DIR/$staged_name" ] || die "unsafe archive member: $staged_name"
done
chmod 0755 "$STAGING_DIR/vohive" "$STAGING_DIR/vohivectl"
chmod 0644 "$STAGING_DIR/LICENSE"

if [ -d "$RELEASE_DIR" ]; then
	[ -n "$OLD_TARGET_ABS" ] && [ "$OLD_TARGET_ABS" = "$RELEASE_DIR" ] || die 'target release directory already exists and is not the current managed release; run vohivectl doctor'
	TARGET_HOLD_MODE='repair'
	TARGET_HOLD="$INSTALL_ROOT/releases/v0.0.0-repair.$(date +%Y%m%d%H%M%S).$$"
elif [ -e "$RELEASE_DIR" ] || [ -L "$RELEASE_DIR" ]; then
	die 'target release path exists but is not a directory'
fi

record_service_state
TRANSACTION_ACTIVE=1
# Persist a minimal journal before the first stop. BACKUP_READY=0 keeps the
# incomplete backup path out of state, so boot recovery can safely fail it.
write_nonterminal_state false
mkdir "$TRANSACTION_BACKUP"
if [ "$OLD_SERVICE_ACTIVE" -eq 1 ]; then stop_service_best_effort || die 'could not stop the existing VoHive service'; fi
backup_transaction
TRANSACTION_PHASE='backing_up'
write_nonterminal_state false

if [ -n "$LEGACY_SOURCE" ] && [ ! -d "$OLD_TARGET_ABS" ]; then
	if [ -f "$LEGACY_SOURCE" ]; then
		LEGACY_RELEASE="$INSTALL_ROOT/releases/$OLD_VERSION"
		mkdir "$LEGACY_RELEASE"
		cp -p "$LEGACY_SOURCE" "$LEGACY_RELEASE/vohive"
		chmod 0755 "$LEGACY_RELEASE/vohive"
		OLD_TARGET_ABS="$LEGACY_RELEASE"
		OLD_TARGET="releases/$OLD_VERSION"
		RECOVERY_PREVIOUS_TARGET=$OLD_TARGET_ABS
	fi
fi
write_nonterminal_state false

# Make boot recovery reachable before touching the canonical release slot.
# The state write is deliberately ahead of the control/unit mutations.
write_nonterminal_state true
if [ -f "$INSTALL_ROOT/control/vohivectl" ]; then
	control_previous_tmp="$INSTALL_ROOT/control/.vohivectl.previous.$$"
	cp -p "$INSTALL_ROOT/control/vohivectl" "$control_previous_tmp"
	chmod 0755 "$control_previous_tmp"
	mv -f "$control_previous_tmp" "$INSTALL_ROOT/control/vohivectl.previous"
else
	rm -f -- "$INSTALL_ROOT/control/vohivectl.previous"
fi
control_tmp="$INSTALL_ROOT/control/.vohivectl.$$"
cp -p "$STAGING_DIR/vohivectl" "$control_tmp"
chmod 0755 "$control_tmp"
mv -f "$control_tmp" "$INSTALL_ROOT/control/vohivectl"
rm -f -- /usr/local/sbin/vohivectl
ln -s "$INSTALL_ROOT/control/vohivectl" /usr/local/sbin/vohivectl
case "$SERVICE_TYPE" in
	systemd) install_systemd_units ;;
	openwrt) install_openwrt_units ;;
	portable) warn 'automatic update is disabled without a supported service manager' ;;
esac

write_initial_config
write_trust_root
mkdir -p "$DATA_ROOT" "$(dirname "$DATA_ROOT")/logs"
chmod 0700 "$DATA_ROOT"

if [ -n "$TARGET_HOLD" ]; then
	if [ "$TARGET_HOLD_MODE" = repair ]; then
		cp -R "$RELEASE_DIR" "$TARGET_HOLD"
		validate_release_target "$TARGET_HOLD" || die 'could not create a valid repair recovery slot'
		RECOVERY_PREVIOUS_TARGET=$TARGET_HOLD
		write_nonterminal_state true
		remove_managed_tree "$RELEASE_DIR"
	fi
fi
release_marker="$STAGING_DIR/.vohive-transaction"
if ! (set -C; printf '%s\n' "$TRANSACTION_ID" >"$release_marker") 2>/dev/null; then
	die 'could not create the release transaction marker'
fi
chmod 0600 "$release_marker"
sync
INSTALLED_RELEASE_PATH=$RELEASE_DIR
TRANSACTION_PHASE='switching'
write_nonterminal_state true
sync
NEW_RELEASE_INSTALLED=1
mv "$STAGING_DIR" "$RELEASE_DIR"
STAGING_DIR=''

if [ "$TARGET_HOLD_MODE" != repair ]; then
	if [ -n "$OLD_TARGET" ]; then atomic_link "$INSTALL_ROOT/last-good" "$OLD_TARGET" || die 'could not update last-good pointer'
	else rm -f -- "$INSTALL_ROOT/last-good"
	fi
fi
atomic_link "$INSTALL_ROOT/current" "releases/$RELEASE_NAME" || die 'could not activate target release'

write_nonterminal_state true

if [ "$TARGET_HOLD_MODE" = repair ]; then
	last_good=$OLD_LAST_GOOD_VERSION
elif [ -n "$OLD_TARGET" ]; then
	last_good=$OLD_VERSION
else
	last_good=''
fi
tmp_deployment="$DEPLOYMENT_FILE.tmp.$$"
mkdir -p "$(dirname "$DEPLOYMENT_FILE")"
cat >"$tmp_deployment" <<EOF
{"schema":1,"product":"vohive","repository":"$REPOSITORY","channel":"$CHANNEL","install_type":"$SERVICE_TYPE","layout":"$LAYOUT","current_version":"$VERSION","last_good_version":"$last_good","install_root":"$INSTALL_ROOT","config_path":"$CONFIG_FILE","data_path":"$DATA_ROOT","state_root":"$STATE_ROOT"}
EOF
chmod 0600 "$tmp_deployment"
mv -f "$tmp_deployment" "$DEPLOYMENT_FILE"
write_readiness_key

SERVICE_MAY_BE_RUNNING=1
start_service
wait_ready
if [ "$SERVICE_TYPE" != portable ]; then
	WEB_UI_URL=$("$INSTALL_ROOT/control/vohivectl" --deployment "$DEPLOYMENT_FILE" probe --expected-version "$VERSION" --print-url)
fi

write_terminal_state completed '' "$VERSION"
if [ -n "$TARGET_HOLD" ] && [ -d "$TARGET_HOLD" ]; then remove_managed_tree "$TARGET_HOLD"; TARGET_HOLD=''; fi
TRANSACTION_ACTIVE=0
if [ "$LOCK_HELD" -eq 1 ]; then rm -f -- "$LOCK_FILE"; LOCK_HELD=0; fi
if [ "$SERVICE_TYPE" = portable ]; then
	say "VoHive $VERSION files installed in explicit --no-service mode."
else
	say "VoHive $VERSION installed successfully."
fi
if [ -n "$WEB_UI_URL" ]; then
	say "Web UI: $WEB_UI_URL"
else
	say 'No service was started; launch VoHive with the configured server.port before opening the Web UI.'
fi
if [ -n "$INITIAL_PASSWORD" ]; then
	say 'Initial administrator username: admin'
	say "Initial administrator password: $INITIAL_PASSWORD"
	say 'Store it now; this password is shown only once.'
fi
