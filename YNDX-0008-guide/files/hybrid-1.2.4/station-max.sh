#!/usr/bin/env bash
# Station Max hybrid: one interactive entrypoint.
#
#   ./station-max.sh              menu
#   ./station-max.sh status       classify the current state
#   ./station-max.sh install ...  first root + install hybrid
#   ./station-max.sh verify       read-only post-boot verification
#   ./station-max.sh rollback     restore a checked backup
#   ./station-max.sh diag         read-only diagnostic zip
#   ./station-max.sh cmd '...'    explicit raw root-queue command
#
# Requires adb in PATH or ADB=/path/to/adb.  Device selection uses the
# standard ANDROID_SERIAL variable; the selected serial is pinned for the
# whole operation.  ADB is a single executable path, never a command line
# evaluated by this script.
set -u
umask 077

ADB=${ADB:-adb}
HERE=$(cd "$(dirname "$0")" && pwd)
QUEUE=/data/local/tmp
STATE_FILE=/data/local/tmp/rootd-op.state
LOCK_DIR=/data/local/tmp/rootd-op.lock
INFLIGHT_FILE=/data/local/tmp/rootd-op.inflight
VERSION=1.2.4

OP_TIMEOUT=${OP_TIMEOUT:-1800}
ADB_TIMEOUT=${ADB_TIMEOUT:-30}
# Bootstrap retries.  The first Mali cycle of CVE-2022-36449 is probabilistic:
# the freed AIO ring page must be reused by a fresh L3 page table of this
# process, and on a busy or freshly booted station that race can be lost many
# times in a row (observed on a user station: 15).  A retry runs only while
# the device proves that nothing was written yet and that no other
# root/bootstrap channel or operation lock exists, so a retry can never hide
# a partial write or start a second channel.
BOOTSTRAP_ATTEMPTS=${BOOTSTRAP_ATTEMPTS:-20}
BOOTSTRAP_BUDGET=${BOOTSTRAP_BUDGET:-600}
BOOTSTRAP_BACKOFF=${BOOTSTRAP_BACKOFF:-2 3 5 5 10 10 15 20}
BOOTSTRAP_STARTED=0
BOOTSTRAP_USED=0
# rootd runs /odm/bin/permissive for its own boot bootstrap and that binary
# renames itself to esc36449, exactly like our bootstrap does.  When only that
# name is seen and rootd is still booting, waiting is correct; a foreign
# channel is not.  The wait is bounded and every check inside it is read-only.
CHANNEL_WAIT=${CHANNEL_WAIT:-60}
HOST_UNAME=$(uname -s 2>/dev/null || true)
REBOOT_TIMEOUT=${REBOOT_TIMEOUT:-180}
BOOT_TIMEOUT=${BOOT_TIMEOUT:-180}
PROTECTED_TIMEOUT=${PROTECTED_TIMEOUT:-240}
VERIFY_WAIT=${VERIFY_WAIT:-90}
RECONNECT_TIMEOUT=${RECONNECT_TIMEOUT:-60}
QUEUE_WAIT=${QUEUE_WAIT:-90}
DEADLINE=$(( $(date +%s) + OP_TIMEOUT ))

FAIL=0
CHECK_FAILS=0
FIRST_FAIL=
VERIFY_QUIET=0
ADB_READY=0
ADB_SEL=()
SERIAL=
ADB_STDOUT=
ADB_STDERR=
ADB_ERROR=
NEW_BOOT_ID=
ROOT_OUTPUT=
ROOT_ERROR=
ROOT_EXIT=
ROOT_WARN=
DEADLINE_HIT=0
CALL_TIMEOUT=0
STATE_STATUS=
OP_ID=
STAGING_NONCE=
STAGING_CREATED=0
INFLIGHT_TEXT=

# Trusted archive code: common helpers and constants.  Configuration is
# never sourced; this file is part of the hash-checked delivery tree.
COMMON_FILE="$HERE/hybrid/common.sh"
if [ ! -f "$COMMON_FILE" ] || [ -L "$COMMON_FILE" ]; then
    printf 'ERROR: missing hybrid/common.sh next to %s\n' "$0" >&2
    exit 1
fi
# shellcheck disable=SC1090
. "$COMMON_FILE"

EXPECT_ROOTD=$ROOTD_SHA
EXPECT_PERM=$PERM_SHA
EXPECT_RC=$RC_SHA
EXPECT_CONF=
STATE_TEXT=
ROOTD_MODE=

say() { printf '%s\n' "$*"; }
ok()  { printf 'PASS  %s\n' "$*"; }
bad() { printf 'FAIL  %s\n' "$*"; FAIL=1; }
warn() { printf 'WARN  %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

WORK=$(mktemp -d "${TMPDIR:-/tmp}/station-max.XXXXXX" 2>/dev/null) || {
    printf 'ERROR: cannot create a private temporary directory\n' >&2
    exit 1
}
chmod 0700 "$WORK" || exit 1

cleanup() {
    local rc=$?
    trap - EXIT INT TERM HUP
    rm -rf "$WORK"
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

usage() {
    cat <<EOF
Station Max hybrid $VERSION

Usage:
  $0                         interactive menu
  $0 status                  classify current state
  $0 install --router-ip IP --dns-ip IP [--ipv6 none|drop] [--no-reboot] [--yes]
  $0 verify                  read-only checks after boot
  $0 rollback [BACKUP] [--no-reboot] [--yes] [--bootstrap] [--allow-unmarked] [--replace-config]
  $0 diag                    read-only diagnostic zip
  $0 cmd 'command'           explicit raw root queue command

Environment:
  ADB=/path/to/adb           adb executable (default: adb; no arguments)
  ANDROID_SERIAL=<serial>    select one device when several are attached
  BASE=/data/local/tmp/...   fresh staging path below /data/local/tmp (must not exist)
  OP_TIMEOUT=seconds         overall operation bound (default 1800)
  ADB_TIMEOUT=seconds        per-ADB-call bound (default 30)
  BOOTSTRAP_ATTEMPTS=n       bootstrap attempts before giving up (default 20, 1..100)
  BOOTSTRAP_BACKOFF='a b c'  seconds between attempts (default '2 3 5 5 10 10 15 20', last repeats)
  BOOTSTRAP_BUDGET=seconds   bound for the whole bootstrap phase (default 600, 30..3600)
  CHANNEL_WAIT=seconds       wait for rootd's own esc36449 bootstrap to end (default 60, 0..300)

Rollback: with no BACKUP the path recorded in /data/local/tmp/rootd-op.state
is used; there is no heuristic "newest backup" selection.  An explicitly
named old backup without a marker needs --allow-unmarked and is still fully
checked under uid 0.  --replace-config allows a deliberately changed
rootd.conf after saving it.  --bootstrap forces the documented bootstrap
recovery path when no rootd is running.
EOF
}

sha256() {
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        sha256sum "$1" | awk '{print $1}'
    fi
}

shq() {
    local s=$1
    s=${s//\'/\'\\\'\'}
    printf "'%s'" "$s"
}

is_ipv4() {
    is_ipv4_strict "$1"
}

# ------------------------------------------------------------ timeouts ----

reset_deadline() {
    DEADLINE=$(( $(date +%s) + OP_TIMEOUT ))
}

# Return a bounded call time.  Never call die from a command substitution:
# the subshell would only exit, leaving an empty value and a one-second call.
# 124 from this function is a real operation deadline; adb_run also returns
# 124 for a per-call timeout, which adb_call records separately in
# CALL_TIMEOUT and never reports as OP_TIMEOUT.
bound_time() {
    local want=$1 now rem
    now=$(date +%s)
    rem=$((DEADLINE - now))
    [ "$rem" -gt 0 ] || return 124
    [ "$want" -gt "$rem" ] && want=$rem
    [ "$want" -ge 1 ] || return 124
    printf '%s' "$want"
}

deadline_ok() {
    [ "$(date +%s)" -lt "$DEADLINE" ]
}

# Run one ADB client process with a hard bound.  On timeout only the client
# process is killed; the ADB server is never touched.
adb_run() {
    local secs=$1 out=$2 errf=$3
    shift 3
    local pid waited=0
    local -a adb_args=("$@")
    local msys=0
    [ "$secs" -ge 1 ] 2>/dev/null || secs=1
    case $HOST_UNAME in
        MINGW*|MSYS*|CYGWIN*) msys=1 ;;
    esac
    # Git Bash rewrites POSIX-looking arguments passed to native adb.exe.
    # Disable that rewrite for device paths, but convert the local side of
    # push/pull explicitly with cygpath.
    if [ "$msys" = 1 ]; then
        case ${adb_args[0]:-} in
            push)
                if [ ${#adb_args[@]} -lt 2 ] || ! adb_args[1]=$(cygpath -w -- "${adb_args[1]}" 2>/dev/null); then
                    printf 'cannot convert local ADB push path\n' >"$errf"
                    return 1
                fi
                ;;
            pull)
                if [ ${#adb_args[@]} -lt 3 ] || ! adb_args[2]=$(cygpath -w -- "${adb_args[2]}" 2>/dev/null); then
                    printf 'cannot convert local ADB pull path\n' >"$errf"
                    return 1
                fi
                ;;
        esac
    fi
    if [ ${#ADB_SEL[@]} -gt 0 ]; then
        if [ "$msys" = 1 ]; then
            MSYS2_ARG_CONV_EXCL='*' MSYS_NO_PATHCONV=1 "$ADB" "${ADB_SEL[@]}" "${adb_args[@]}" >"$out" 2>"$errf" &
        else
            "$ADB" "${ADB_SEL[@]}" "${adb_args[@]}" >"$out" 2>"$errf" &
        fi
    else
        if [ "$msys" = 1 ]; then
            MSYS2_ARG_CONV_EXCL='*' MSYS_NO_PATHCONV=1 "$ADB" "${adb_args[@]}" >"$out" 2>"$errf" &
        else
            "$ADB" "${adb_args[@]}" >"$out" 2>"$errf" &
        fi
    fi
    pid=$!
    while kill -0 "$pid" 2>/dev/null; do
        if [ "$waited" -ge $((secs * 10)) ]; then
            kill -TERM "$pid" 2>/dev/null
            sleep 1
            kill -KILL "$pid" 2>/dev/null
            wait "$pid" 2>/dev/null
            return 124
        fi
        sleep 0.1
        waited=$((waited + 1))
    done
    wait "$pid"
    return $?
}

adb_call() {
    local secs=$1
    shift
    local rc
    if ! secs=$(bound_time "${secs:-$ADB_TIMEOUT}"); then
        ADB_STDOUT=
        ADB_STDERR=
        ADB_ERROR="operation timed out (OP_TIMEOUT=$OP_TIMEOUT); no result is trusted"
        DEADLINE_HIT=1
        CALL_TIMEOUT=0
        return 124
    fi
    adb_run "$secs" "$WORK/adb.out" "$WORK/adb.err" "$@"
    rc=$?
    ADB_STDOUT=$(cat "$WORK/adb.out" 2>/dev/null || true)
    ADB_STDERR=$(cat "$WORK/adb.err" 2>/dev/null || true)
    ADB_STDOUT=${ADB_STDOUT//$'\r'/}
    ADB_STDERR=${ADB_STDERR//$'\r'/}
    if [ "$rc" = 124 ]; then
        # The per-call bound expired while the operation deadline still had
        # time.  This is not OP_TIMEOUT: the operation may continue after the
        # caller re-checks the device.
        CALL_TIMEOUT=1
        ADB_ERROR="attempt timed out after ${secs}s; no result is trusted"
    else
        CALL_TIMEOUT=0
    fi
    return $rc
}

adb_shell() {
    local secs=$1 cmd=$2
    adb_call "$secs" shell "$cmd"
}

adb_push() {
    local secs=$1 src=$2 dst=$3
    adb_call "$secs" push "$src" "$dst"
}

adb_resolve() {
    ADB_ERROR=
    case $ADB in
        *[[:space:]]*) ADB_ERROR="ADB must be one executable path, not a command line; use ANDROID_SERIAL for device selection"; return 1 ;;
    esac
    if [ -z "$ADB" ]; then
        ADB_ERROR="ADB is empty"
        return 1
    fi
    case $ADB in
        */*) [ -x "$ADB" ] || { ADB_ERROR="adb is not executable: $ADB"; return 1; } ;;
        *) command -v "$ADB" >/dev/null 2>&1 || { ADB_ERROR="adb not found in PATH: $ADB"; return 1; } ;;
    esac
    return 0
}

# Return 0 when a selected device answers get-state with "device".
adb_get_state() {
    adb_call 10 get-state >/dev/null 2>&1 || return 1
    [ "$(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]')" = device ]
}

adb_init() {
    ADB_ERROR=
    adb_resolve || return 1
    if ! adb_call 10 version >/dev/null 2>&1; then
        ADB_ERROR="adb cannot run (${ADB_STDERR:-no error text}); do not kill the ADB server"
        return 1
    fi
    if ! adb_call 15 devices >/dev/null 2>&1; then
        ADB_ERROR="adb server did not answer 'adb devices' (${ADB_STDERR:-no error text}); do not kill the ADB server"
        return 1
    fi
    local devices offline unauthorized
    devices=$(printf '%s\n' "$ADB_STDOUT" | awk 'NF >= 2 && $2 == "device" { print $1 }')
    offline=$(printf '%s\n' "$ADB_STDOUT" | awk 'NF >= 2 && $2 == "offline" { print $1 }')
    unauthorized=$(printf '%s\n' "$ADB_STDOUT" | awk 'NF >= 2 && $2 == "unauthorized" { print $1 }')
    local n_dev n_off n_una
    n_dev=$(printf '%s\n' "$devices" | awk 'NF { c++ } END { print c + 0 }')
    n_off=$(printf '%s\n' "$offline" | awk 'NF { c++ } END { print c + 0 }')
    n_una=$(printf '%s\n' "$unauthorized" | awk 'NF { c++ } END { print c + 0 }')
    if [ -n "${ANDROID_SERIAL:-}" ]; then
        SERIAL=$ANDROID_SERIAL
        ADB_SEL=(-s "$SERIAL")
        if ! adb_get_state; then
            if printf '%s\n' "$offline" | grep -qx -- "$SERIAL"; then
                ADB_ERROR="device $SERIAL is offline"
            elif printf '%s\n' "$unauthorized" | grep -qx -- "$SERIAL"; then
                ADB_ERROR="device $SERIAL is unauthorized; confirm the RSA prompt"
            else
                ADB_ERROR="ANDROID_SERIAL=$SERIAL is not an available device"
            fi
            return 1
        fi
        ADB_READY=1
        return 0
    fi
    if [ "$n_dev" -eq 0 ]; then
        if [ "$n_off" -gt 0 ]; then
            ADB_ERROR="no online device; offline: $(printf '%s' "$offline" | tr '\n' ' ')"
        elif [ "$n_una" -gt 0 ]; then
            ADB_ERROR="no authorized device; unauthorized: $(printf '%s' "$unauthorized" | tr '\n' ' '); confirm the RSA prompt"
        else
            ADB_ERROR="no ADB device (not connected, unauthorized, or offline)"
        fi
        return 1
    fi
    if [ "$n_dev" -gt 1 ]; then
        ADB_ERROR="multiple devices; set ANDROID_SERIAL to one of: $(printf '%s' "$devices" | tr '\n' ' ')"
        return 1
    fi
    SERIAL=$devices
    ADB_SEL=(-s "$SERIAL")
    if ! adb_get_state; then
        ADB_ERROR="device $SERIAL did not report state=device"
        return 1
    fi
    ADB_READY=1
    return 0
}

# --------------------------------------------------------- device reads ---

read_device_boot_id() {
    adb_shell 15 'cat /proc/sys/kernel/random/boot_id' || return 1
    local id
    id=$(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]')
    case $id in
        ''|*[!A-Za-z0-9._-]*) return 1 ;;
    esac
    [ ${#id} -le 64 ] || return 1
    printf '%s' "$id"
}

read_optional_file() {
    local path=$1
    adb_shell 20 "if [ -f $(shq "$path") ]; then cat $(shq "$path"); else echo __ABSENT__; fi" || return 1
    if [ "$ADB_STDOUT" = __ABSENT__ ]; then
        printf ''
        return 0
    fi
    printf '%s' "$ADB_STDOUT"
}

state_get() {
    local text=$1 key=$2
    printf '%s\n' "$text" | sed -n "s/^$key=//p" | head -n 1
}

# Age in seconds of the marker's host-local timestamp, or "unknown".  BSD
# date (macOS) and GNU date (Git Bash/WSL/Linux) parse it differently, so
# both forms are tried; neither changes a decision, it is display only.
iso_age() {
    local iso=$1 now epoch
    [ -n "$iso" ] || { printf 'unknown'; return 0; }
    now=$(date +%s)
    if epoch=$(date -j -f '%Y-%m-%dT%H:%M:%S' "$iso" +%s 2>/dev/null); then
        :
    elif epoch=$(date -d "$iso" +%s 2>/dev/null); then
        :
    else
        printf 'unknown'
        return 0
    fi
    case $epoch in
        ''|*[!0-9]*) printf 'unknown' ;;
        *) [ "$epoch" -le "$now" ] && printf '%s' "$(( now - epoch ))" || printf '0' ;;
    esac
}

# Diagnostic output is sent to other people.  Never copy a free-form
# rootstatus reason or failure string: it may contain an address or token.
reason_class() {
    case $1 in
        '') printf 'absent' ;;
        protected) printf 'protected' ;;
        bootstrap-pending|pending) printf 'bootstrapping' ;;
        *timeout*) printf 'timeout' ;;
        *unreachable*) printf 'unreachable' ;;
        *invalid*config*|*config*invalid*) printf 'invalid-config' ;;
        *disconnect*) printf 'disconnected' ;;
        *refused*) printf 'refused' ;;
        *) printf 'unclassified' ;;
    esac
}

failure_class() {
    case $1 in
        '') printf 'unknown-check' ;;
        *second*channel*|*root-bridge*|*esc36449*) printf 'second-channel' ;;
        *deadline*|*timed*out*) printf 'timeout' ;;
        *ps*fail*|*process*state*) printf 'ps-failed' ;;
        *mount*|*/odm*) printf 'mount' ;;
        *hash*) printf 'hash-mismatch' ;;
        *metadata*|*mode*mismatch*|*owner*|*label*) printf 'metadata' ;;
        *DNS*|*DoT*|*iptables*|*IPv6*) printf 'network-rules' ;;
        *rootd.conf*|*config*) printf 'config' ;;
        *rootstatus*|*PROTECTED*) printf 'rootstatus' ;;
        *) printf 'check-failed' ;;
    esac
}

dev_prop_quiet() {
    adb_shell 15 "getprop $(shq "$1")" || return 1
    printf '%s' "$ADB_STDOUT" | sed 's/[[:space:]]*$//'
}

dev_sha256_quiet() {
    adb_shell 20 "sha256sum $(shq "$1")" || return 1
    printf '%s' "$ADB_STDOUT" | awk 'NR == 1 { print $1 }'
}

dev_mount_state_quiet() {
    adb_shell 20 'mount' || return 1
    printf '%s\n' "$ADB_STDOUT" | parse_mount_state
}

dev_ps_names_quiet() {
    local names
    if adb_shell 20 'ps -A -o NAME' 2>/dev/null; then
        names=$(printf '%s\n' "$ADB_STDOUT" | parse_process_names name)
    elif adb_shell 20 'ps -A' 2>/dev/null; then
        names=$(printf '%s\n' "$ADB_STDOUT" | parse_process_names full)
    else
        return 1
    fi
    [ -n "$names" ] || return 1
    printf '%s\n' "$names"
}

count_name() {
    count_named_process "$1" "$2"
}

dev_check_commands() {
    adb_shell 30 'for c in sh sha256sum awk sed grep tr cut head tail wc date cp mv rm mkdir chmod chown chcon sync mount df ps getprop getenforce id stat readlink mktemp cat ls; do command -v "$c" >/dev/null 2>&1 || echo "MISSING $c"; done; exit 0' || return 1
    [ -z "$(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]')" ]
}

dev_free_kb() {
    local path=$1 out
    adb_shell 20 "df -k $(shq "$path")" || return 1
    out=$(printf '%s\n' "$ADB_STDOUT" | tail -n 1 | awk '{print $4}')
    case $out in
        ''|*[!0-9]*) return 1 ;;
    esac
    printf '%s' "$out"
}

# ----------------------------------------------------------- build gate ---

DEV_BUILD_VALUE=
dev_build_gate() {
    local build fp kernel uname_r
    build=$(dev_prop_quiet ro.build.display.id) || { ADB_ERROR="cannot read ro.build.display.id"; return 1; }
    fp=$(dev_prop_quiet ro.build.fingerprint) || { ADB_ERROR="cannot read ro.build.fingerprint"; return 1; }
    uname_r=$(adb_shell 15 'uname -r' && printf '%s' "$ADB_STDOUT") || { ADB_ERROR="cannot read uname -r"; return 1; }
    [ "$build" = "$DISPLAY_ID" ] || { ADB_ERROR="unsupported build: $build"; return 1; }
    [ "$fp" = "$FINGERPRINT" ] || { ADB_ERROR="fingerprint mismatch"; return 1; }
    [ "$uname_r" = "$KERNEL_RELEASE" ] || { ADB_ERROR="kernel mismatch: $uname_r"; return 1; }
    DEV_BUILD_VALUE=$build
    return 0
}

dev_boot_completed() {
    adb_shell 15 'getprop sys.boot_completed' || return 1
    [ "$(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]')" = 1 ]
}

# --------------------------------------------------------- local files ----

check_local_archive() {
    local rel want file
    for spec in \
        "bootstrap/root-bridge:$BRIDGE_SHA" \
        "hybrid/rootd:$ROOTD_SHA" \
        "hybrid/permissive:$PERM_SHA" \
        "hybrid/rootd.rc:$RC_SHA"; do
        rel=${spec%%:*}
        want=${spec#*:}
        file="$HERE/$rel"
        [ -f "$file" ] && [ ! -L "$file" ] || die "archive file missing or symlink: $rel"
        [ "$(sha256 "$file")" = "$want" ] || die "archive hash mismatch: $rel"
    done
    for rel in hybrid/common.sh hybrid/install-run.sh hybrid/install-device.sh hybrid/rollback-device.sh hybrid/rollback-run.sh; do
        file="$HERE/$rel"
        [ -s "$file" ] && [ ! -L "$file" ] || die "archive script missing or empty: $rel"
    done
    [ "$(wc -c < "$HERE/hybrid/permissive" | awk '{print $1}')" = "$PERM_SIZE" ] \
        || die "permissive size mismatch"
}

# -------------------------------------------------------------- deliver ---

# Refuse a staging path whose parent components are symlinks.  The lexical
# is_safe_tmp_path check alone cannot see this, and pushing would follow a
# symlinked parent.
dev_base_symlink_check() {
    local rest=${BASE#/data/local/tmp/}
    local path=/data/local/tmp comp list
    list=$(shq "$path")
    local oldifs=$IFS
    IFS=/
    for comp in $rest; do
        path="$path/$comp"
        list="$list $(shq "$path")"
    done
    IFS=$oldifs
    adb_shell 15 "for p in $list; do [ -L \"\$p\" ] && echo SYMLINK:\$p; done; exit 0" || return 1
    if [ -n "$(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]')" ]; then
        ADB_ERROR="symlink component in BASE: $ADB_STDOUT"
        return 1
    fi
    return 0
}

new_operation_id() {
    printf 'sm-%s-%s-%s' "$(date +%s)" "$$" "$RANDOM"
}

# Create a fresh staging directory.  The default is a mktemp directory, so a
# later install cannot delete a user directory at a fixed path.  A custom
# BASE is accepted only when it does not exist; this release never runs
# rm -rf on a caller-supplied path.
prepare_staging() {
    local out sentinel tmp
    if [ -n "${BASE:-}" ]; then
        is_safe_tmp_path "$BASE" || die "BASE must be a safe staging path below /data/local/tmp"
        [ "$BASE" != /data/local/tmp ] || die "BASE cannot be the /data/local/tmp root"
        dev_base_symlink_check || die "refusing staging path: ${ADB_ERROR:-symlink check failed}"
        adb_shell 20 "if [ -e $(shq "$BASE") ] || [ -L $(shq "$BASE") ]; then echo EXISTS; else umask 077; mkdir $(shq "$BASE") && echo CREATED; fi" \
            || die "cannot inspect or create staging directory $BASE"
        case $(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]') in
            CREATED) ;;
            EXISTS) die "BASE already exists: $BASE (use a new path or unset BASE for a mktemp directory)" ;;
            *) die "unexpected staging response for $BASE" ;;
        esac
    else
        adb_shell 20 'umask 077; mktemp -d /data/local/tmp/station-max-hybrid.XXXXXX' \
            || die "cannot create a unique staging directory"
        BASE=$(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]')
        is_safe_tmp_path "$BASE" || die "device returned an unsafe staging path"
        case ${BASE##*/} in
            station-max-hybrid.*) ;;
            *) die "device returned an unexpected staging directory: $BASE" ;;
        esac
    fi
    STAGING_CREATED=1
    [ -n "${OP_ID:-}" ] || OP_ID=$(new_operation_id)
    [ -n "${STAGING_NONCE:-}" ] || STAGING_NONCE="$(date +%s)-$$-$RANDOM"
    case $OP_ID in *[!A-Za-z0-9._-]*|'') die "invalid operation id" ;; esac
    case $STAGING_NONCE in *[!A-Za-z0-9._-]*|'') die "invalid staging nonce" ;; esac
    sentinel="$BASE/.station-max-stage"
    tmp="$BASE/.station-max-stage.tmp"
    adb_shell 20 "umask 077; printf 'format=1\noperation_id=%s\nnonce=%s\n' $(shq "$OP_ID") $(shq "$STAGING_NONCE") > $(shq "$tmp") && chmod 0600 $(shq "$tmp") && mv -f $(shq "$tmp") $(shq "$sentinel")" \
        || die "cannot write staging sentinel"
    adb_shell 15 "cat $(shq "$sentinel")" || die "cannot read staging sentinel"
    [ "$(state_get "$ADB_STDOUT" format)" = 1 ] || die "staging sentinel has no format"
    [ "$(state_get "$ADB_STDOUT" operation_id)" = "$OP_ID" ] || die "staging sentinel operation id mismatch"
    [ "$(state_get "$ADB_STDOUT" nonce)" = "$STAGING_NONCE" ] || die "staging sentinel nonce mismatch"
    say "staging: $BASE"
}

# Deliver the staging tree and verify every byte on the device before any
# root code runs.  Config is generated only from validated values.
deliver_tree() {
    dev_base_symlink_check || die "refusing staging path: ${ADB_ERROR:-symlink check failed}"
    adb_shell 15 "cat $(shq "$BASE/.station-max-stage")" || die "staging sentinel is missing; refusing to push into $BASE"
    [ "$(state_get "$ADB_STDOUT" operation_id)" = "$OP_ID" ] || die "staging sentinel belongs to another operation"
    [ "$(state_get "$ADB_STDOUT" nonce)" = "$STAGING_NONCE" ] || die "staging sentinel nonce mismatch"
    adb_shell 30 "mkdir -p $(shq "$BASE/hybrid")" || die "cannot create staging directory $BASE"
    adb_shell 20 "[ ! -L $(shq "$BASE") ] && [ -d $(shq "$BASE") ] && [ ! -L $(shq "$BASE/hybrid") ] && [ -d $(shq "$BASE/hybrid") ]"         || die "staging path is not a regular directory: $BASE"

    adb_push 30 "$HERE/bootstrap/root-bridge" "$BASE/root-bridge" || die "push root-bridge failed"
    for rel in hybrid/common.sh hybrid/rootd hybrid/permissive hybrid/rootd.rc hybrid/install-run.sh hybrid/install-device.sh hybrid/rollback-device.sh hybrid/rollback-run.sh; do
        adb_push 30 "$HERE/$rel" "$BASE/$rel" || die "push $rel failed"
    done
    adb_shell 20 "chmod 755 $(shq "$BASE/root-bridge") $(shq "$BASE/hybrid/permissive") $(shq "$BASE/hybrid/install-run.sh") $(shq "$BASE/hybrid/install-device.sh") $(shq "$BASE/hybrid/rollback-device.sh") $(shq "$BASE/hybrid/rollback-run.sh")" \
        || die "chmod staging scripts failed"
    adb_shell 20 "chmod 644 $(shq "$BASE/hybrid/common.sh") $(shq "$BASE/hybrid/rootd") $(shq "$BASE/hybrid/rootd.rc")" \
        || die "chmod staging data failed"

    # Re-read every delivered file and compare with the local archive.  This
    # is done after the last push/chmod and before root-bridge is executed.
    local rel local_sha dev_sha
    for rel in hybrid/common.sh hybrid/rootd hybrid/permissive hybrid/rootd.rc hybrid/install-run.sh hybrid/install-device.sh hybrid/rollback-device.sh hybrid/rollback-run.sh; do
        local_sha=$(sha256 "$HERE/$rel")
        dev_sha=$(dev_sha256_quiet "$BASE/$rel") || die "cannot hash delivered $rel"
        [ -n "$dev_sha" ] && [ "$dev_sha" = "$local_sha" ] || die "delivered hash mismatch: $rel"
    done
    local bridge_sha
    bridge_sha=$(dev_sha256_quiet "$BASE/root-bridge") || die "cannot hash delivered root-bridge"
    [ "$bridge_sha" = "$BRIDGE_SHA" ] || die "delivered hash mismatch: root-bridge"
    say "staging tree verified in $BASE (root-bridge, permissive, rootd, rc, scripts)"
}

deliver_staging() {
    local router_ip=$1 dns_ip=$2 ipv6=$3
    local conf="$WORK/install.conf"
    printf 'ROUTER_IP=%s\nDNS_IP=%s\nIPV6=%s\n' "$router_ip" "$dns_ip" "$ipv6" > "$conf" || die "cannot write local config"
    chmod 0600 "$conf" || die "cannot protect local config"
    deliver_tree
    adb_push 30 "$conf" "$BASE/install.conf" || die "push install.conf failed"
    adb_shell 20 "chmod 600 $(shq "$BASE/install.conf")" || die "chmod install.conf failed"
    local conf_sha
    conf_sha=$(dev_sha256_quiet "$BASE/install.conf") || die "cannot hash delivered install.conf"
    [ "$conf_sha" = "$(sha256 "$conf")" ] || die "delivered hash mismatch: install.conf"
    say "staging verified in $BASE (root-bridge, permissive, rootd, rc, scripts, config)"
}

# Validate the state text read through ADB with the same parser used on the
# device.  This catches a truncated/corrupt state before a bootstrap starts.
host_state_probe() {
    local text=$1
    printf '%s' "$text" > "$WORK/state.txt" || return 1
    state_probe "$WORK/state.txt" || return 1
    [ "$STATE_STATUS" = valid ] || return 1
    return 0
}

# A marker from a previous boot can be cleared only after the state gate below
# proves that no write had started.  A marker from the *current* boot is never
# removed automatically: the process that wrote it may still be alive as sh,
# invisible to the four-name process gate until it takes the lock or writes
# state.  The operator must see it, check status, and remove it explicitly.
clear_inflight_if_safe() {
    local text=$1 state_text=$2
    [ -n "$text" ] || return 0
    local op boot created stage sop sph
    op=$(state_get "$text" operation_id)
    boot=$(state_get "$text" boot_id)
    created=$(state_get "$text" created)
    stage=$(state_get "$text" stage)
    sop=$(state_get "$state_text" operation)
    sph=$(state_get "$state_text" phase)
    case $op in
        ''|*[!A-Za-z0-9._-]*) die "in-flight marker is malformed; inspect it manually: $INFLIGHT_FILE" ;;
    esac
    [ ${#op} -le 64 ] || die "in-flight marker has an oversized operation id"
    case ${sop:-}:${sph:-} in
        install:backup_complete|install:writing|install:files_written|install:verified|install:failed)
            die "previous bootstrap left phase=${sph:-unknown}; do not start another bootstrap. Inspect the backup and run status/rollback."
            ;;
        rollback:*)
            [ "$sph" = rollback_complete ] || die "previous bootstrap left rollback phase=${sph:-unknown}; finish recovery before installing"
            ;;
    esac
    if [ -n "$boot" ] && [ "$boot" != "$BOOT_ID" ]; then
        warn "in-flight marker is from boot $boot (current $BOOT_ID); operation_id=$op created=${created:-unknown} stage=${stage:-unknown}"
        warn "no process can survive that reboot; the marker is removed after the state check"
    else
        die "in-flight marker from this boot remains (operation_id=$op created=${created:-unknown} stage=${stage:-unknown}): a bootstrap may still be running as sh, which the process gate cannot see. It was not removed automatically. Run ./station-max.sh status; if you are sure no install is running, remove it explicitly with: adb shell 'rm -f $INFLIGHT_FILE' (or ./station-max.sh cmd 'rm -f $INFLIGHT_FILE' when rootd is running)"
    fi
    adb_shell 15 "rm -f $(shq "$INFLIGHT_FILE")" || die "cannot clear stale in-flight marker"
    say "cleared stale in-flight marker from boot $boot after the state check"
}

# Read the marker without treating a failed read as "no marker".  Do not call
# die inside the command substitution: the subshell would exit and the caller
# would see an empty marker.  Store the text in INFLIGHT_TEXT instead.
read_inflight_strict() {
    INFLIGHT_TEXT=$(read_optional_file "$INFLIGHT_FILE") || {
        INFLIGHT_TEXT=
        return 1
    }
    return 0
}

# Publish one bootstrap in-flight marker before starting root-bridge.  If the
# client dies, the marker remains; a later run may clear it only after ps has
# proved that no root/bootstrap channel and no live lock remain.  The marker
# belongs to exactly one (operation_id, staging_nonce) pair: an existing
# marker is replaced only when both match this run, otherwise publication is
# refused.  An absent marker is created atomically with noclobber.
write_inflight() {
    local stage=$1 tmp="$WORK/inflight.marker"
    local boot publish rc word remote_tmp target
    boot=$(read_device_boot_id) || die "cannot read boot_id for in-flight marker"
    [ -n "$boot" ] || die "empty boot_id for in-flight marker"
    [ "$boot" = "$BOOT_ID" ] \
        || die "the station rebooted between the gates and the bootstrap start (boot $BOOT_ID -> $boot); no attempt was made"
    {
        printf 'version=1\n'
        printf 'operation_id=%s\n' "$OP_ID"
        printf 'boot_id=%s\n' "$boot"
        printf 'staging_nonce=%s\n' "$STAGING_NONCE"
        printf 'stage=%s\n' "$stage"
        printf 'created=%s\n' "$(date '+%Y-%m-%dT%H:%M:%S')"
    } > "$tmp" || die "cannot write local in-flight marker"
    chmod 0600 "$tmp" || die "cannot protect local in-flight marker"
    remote_tmp="$QUEUE/.rootd-op.inflight.$OP_ID.$STAGING_NONCE.tmp"
    target=$(shq "$INFLIGHT_FILE")
    adb_push 20 "$tmp" "$remote_tmp" || die "cannot push in-flight marker: $ADB_STDERR"
    publish="
umask 077
if [ -f $target ]; then
    _op=\$(sed -n 's/^operation_id=//p' $target 2>/dev/null | head -n 1) || exit 3
    _nonce=\$(sed -n 's/^staging_nonce=//p' $target 2>/dev/null | head -n 1) || exit 3
    if [ \"\$_op\" != $(shq "$OP_ID") ] || [ \"\$_nonce\" != $(shq "$STAGING_NONCE") ]; then
        rm -f $(shq "$remote_tmp")
        echo FOREIGN
        exit 4
    fi
    chmod 0600 $(shq "$remote_tmp") && mv -f $(shq "$remote_tmp") $target || { rm -f $(shq "$remote_tmp"); exit 5; }
    echo PUBLISHED
    exit 0
fi
if [ -e $target ] || [ -L $target ]; then
    rm -f $(shq "$remote_tmp")
    echo FOREIGN
    exit 4
fi
if ( set -C; cat $(shq "$remote_tmp") > $target ) 2>/dev/null; then
    rm -f $(shq "$remote_tmp")
    echo PUBLISHED
    exit 0
fi
rm -f $(shq "$remote_tmp")
echo RACE
exit 6
"
    rc=0
    adb_shell 20 "$publish" || rc=$?
    word=$(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]')
    if [ "$word" != PUBLISHED ] || [ "$rc" != 0 ]; then
        case $word in
            FOREIGN) die "another install is in flight or its marker cannot be attributed to this run; refusing to overwrite $INFLIGHT_FILE" ;;
            RACE) die "an in-flight marker appeared while publishing; refusing to overwrite $INFLIGHT_FILE" ;;
            *) die "cannot publish in-flight marker (rc=$rc; ${ADB_STDERR:-no error text})" ;;
        esac
    fi
    adb_shell 15 "cat $target" || die "cannot read in-flight marker back"
    [ "$(state_get "$ADB_STDOUT" operation_id)" = "$OP_ID" ] || die "in-flight marker operation id mismatch"
    [ "$(state_get "$ADB_STDOUT" staging_nonce)" = "$STAGING_NONCE" ] || die "in-flight marker staging nonce mismatch"
    [ "$(state_get "$ADB_STDOUT" boot_id)" = "$boot" ] || die "in-flight marker boot id mismatch"
}

# -------------------------------------------------- bootstrap guardrails ---
#
# Every bootstrap attempt re-proves the same preconditions as the preflight:
# no second channel, no live operation lock, /odm still ro, and an in-flight
# marker that belongs to exactly this run (operation_id and staging_nonce).
# From the second attempt on it also requires an unchanged state: not just
# the phase, but operation_id, updated and backup.  An automatic retry is
# allowed only for a plain nonzero exit carrying a known Mali-flake line.
# Empty or truncated output is never retried: after root-bridge execs
# /system/bin/sh the installer is named sh and is invisible to the four-name
# gate until it takes the device lock or writes state.

is_uint() {
    case $1 in
        ''|*[!0-9]*) return 1 ;;
    esac
    return 0
}

bootstrap_config_check() {
    is_uint "$BOOTSTRAP_ATTEMPTS" && [ "$BOOTSTRAP_ATTEMPTS" -ge 1 ] && [ "$BOOTSTRAP_ATTEMPTS" -le 100 ] \
        || die "BOOTSTRAP_ATTEMPTS must be an integer 1..100"
    is_uint "$BOOTSTRAP_BUDGET" && [ "$BOOTSTRAP_BUDGET" -ge 30 ] && [ "$BOOTSTRAP_BUDGET" -le 3600 ] \
        || die "BOOTSTRAP_BUDGET must be an integer 30..3600"
    local d seen=0
    for d in $BOOTSTRAP_BACKOFF; do
        is_uint "$d" && [ "$d" -ge 0 ] && [ "$d" -le 120 ] \
            || die "BOOTSTRAP_BACKOFF must be a list of integers 0..120 (seconds)"
        seen=1
    done
    [ "$seen" = 1 ] || die "BOOTSTRAP_BACKOFF is empty"
    is_uint "$CHANNEL_WAIT" && [ "$CHANNEL_WAIT" -le 300 ] \
        || die "CHANNEL_WAIT must be an integer 0..300"
}

# Names of the running channel processes, so a refusal is actionable instead
# of a list of four names the operator has to guess from.  Best effort: the
# pid lookup never changes the decision.
channel_offenders() {
    local pid name found=""
    if adb_shell 20 'ps -A -o PID,NAME' >/dev/null 2>&1; then
        while read -r pid name; do
            [ -n "$name" ] || continue
            is_second_channel_name "$name" || continue
            found="$found${found:+, }$name(pid $pid)"
        done <<EOF
$ADB_STDOUT
EOF
    fi
    if [ -z "$found" ]; then
        while IFS= read -r name; do
            [ -n "$name" ] || continue
            is_second_channel_name "$name" || continue
            found="$found${found:+, }$name"
        done <<EOF
$PS_NAMES
EOF
    fi
    printf '%s' "${found:-unknown}"
}

# True when every running channel name is esc36449 - the name both rootd's
# permissive helper and our own bootstrap use.
only_esc36449_channel() {
    [ "$PS_SECOND_N" -gt 0 ] || return 1
    [ "$(count_name "$PS_NAMES" esc36449)" = "$PS_SECOND_N" ]
}

# rootd's own boot bootstrap can hold esc36449 for a minute or two after a
# reboot (8 attempts, 10 s apart).  Install started in that window used to be
# refused forever; instead wait for the window to close, then decide again.
wait_for_own_bootstrap() {
    only_esc36449_channel || return 1
    [ "$PS_ROOTD_N" = 1 ] || return 1
    local st st_state st_boot until
    st=$(read_optional_file /data/local/tmp/rootstatus 2>/dev/null) || return 1
    st_state=$(state_get "$st" state)
    st_boot=$(state_get "$st" boot_id)
    [ "$st_boot" = "$BOOT_ID" ] || return 1
    case $st_state in
        BOOTSTRAPPING|ROOT_ONLY|BOOTING) ;;
        *) return 1 ;;
    esac
    say "rootd is in its own bootstrap (esc36449 is its permissive helper); waiting up to ${CHANNEL_WAIT}s"
    until=$(( $(date +%s) + CHANNEL_WAIT ))
    while [ "$(date +%s)" -lt "$until" ]; do
        sleep 5
        PS_NAMES=$(dev_ps_names_quiet 2>/dev/null) || return 1
        PS_ROOTD_N=$(count_name "$PS_NAMES" rootd)
        PS_SECOND_N=$(count_second_channels "$PS_NAMES")
        [ "$PS_SECOND_N" = 0 ] && return 0
        only_esc36449_channel || return 1
    done
    return 1
}

assert_no_channel() {
    PS_NAMES=$(dev_ps_names_quiet) || die "check not performed: ps failed"
    PS_ROOTD_N=$(count_name "$PS_NAMES" rootd)
    PS_SECOND_N=$(count_second_channels "$PS_NAMES")
    if [ "$PS_SECOND_N" != 0 ]; then
        if wait_for_own_bootstrap; then
            say "rootd finished its bootstrap; the channel check is clear"
        else
            die "a second root/bootstrap channel is present: $(channel_offenders) (expected names: $SECOND_CHANNEL_NAMES)"
        fi
    fi
    [ "$PS_ROOTD_N" -le 1 ] || die "more than one rootd process is present ($PS_ROOTD_N)"
}

# Loads and validates the device state file.  A corrupt state is never
# trusted or overwritten; an absent one leaves STATE_STATUS=missing and the
# ST_* fields empty.
refresh_state() {
    local why=${1:-refusing to continue}
    if adb_shell 15 "[ -e $(shq "$STATE_FILE") ] || [ -L $(shq "$STATE_FILE") ]"; then
        adb_shell 15 "[ -f $(shq "$STATE_FILE") ] && [ ! -L $(shq "$STATE_FILE") ]" \
            || die "operation state is not a regular file; $why"
        STATE_TEXT=$(read_optional_file "$STATE_FILE") || STATE_TEXT=
        host_state_probe "$STATE_TEXT" \
            || die "operation state is corrupt or invalid; $why. Inspect $STATE_FILE on the device."
        return 0
    fi
    STATE_STATUS=missing
    STATE_TEXT=
    state_reset
    return 0
}

assert_no_live_lock() {
    adb_shell 15 "if [ -d $(shq "$LOCK_DIR") ]; then echo LOCK; cat $(shq "$LOCK_DIR/pid") 2>/dev/null || echo 0; cat $(shq "$LOCK_DIR/boot_id") 2>/dev/null || echo unknown; else echo NOLOCK; fi" \
        || die "check not performed: lock check failed"
    printf '%s' "$ADB_STDOUT" | grep -qx LOCK || return 0
    local lock_pid lock_boot lock_alive=0
    lock_pid=$(printf '%s\n' "$ADB_STDOUT" | sed -n '2p')
    lock_boot=$(printf '%s\n' "$ADB_STDOUT" | sed -n '3p')
    case $lock_pid in
        ''|*[!0-9]*) lock_pid=0 ;;
    esac
    if [ "$lock_boot" = "$BOOT_ID" ] && [ "$lock_pid" != 0 ]; then
        adb_shell 15 'ps -A -o PID' >/dev/null 2>&1 || true
        if printf '%s\n' "$ADB_STDOUT" | awk -v p="$lock_pid" '$1 == p { f=1 } END { exit f ? 0 : 1 }'; then
            lock_alive=1
        fi
    fi
    if [ "$lock_alive" = 1 ]; then
        die "operation lock is held by live pid $lock_pid on this boot; another install/rollback is running"
    fi
    if [ "$lock_boot" = "$BOOT_ID" ] && [ "$lock_pid" != 0 ]; then
        warn "stale operation lock (dead pid $lock_pid) will be reclaimed by the device script"
    elif [ "$lock_boot" = "$BOOT_ID" ]; then
        warn "operation lock is present but its pid is unreadable; the device script will decide"
    else
        warn "stale operation lock from a previous boot will be reclaimed by the device script"
    fi
    return 0
}

# An in-flight marker written by another run means another operator started a
# bootstrap on this device; take the decision away from the retry loop.  The
# marker is unique per run only when both operation_id and staging_nonce
# match: a repeated install from the same phase reuses operation_id.
assert_own_inflight() {
    local text op nonce boot
    read_inflight_strict \
        || die "cannot read in-flight marker $INFLIGHT_FILE; refusing to guess whether another bootstrap is running"
    text=$INFLIGHT_TEXT
    [ -n "$text" ] || return 0
    op=$(state_get "$text" operation_id)
    nonce=$(state_get "$text" staging_nonce)
    boot=$(state_get "$text" boot_id)
    case $op in
        ''|*[!A-Za-z0-9._-]*) die "in-flight marker has no valid operation_id; inspect $INFLIGHT_FILE manually" ;;
    esac
    case $nonce in
        ''|*[!A-Za-z0-9._-]*) die "in-flight marker has no valid staging_nonce; inspect $INFLIGHT_FILE manually" ;;
    esac
    if [ "$op" != "$OP_ID" ] || [ "$nonce" != "$STAGING_NONCE" ]; then
        die "another install is in flight (operation_id=${op:-absent}, staging_nonce=${nonce:-absent}); refusing to start a second bootstrap"
    fi
    if [ -n "$boot" ] && [ "$boot" != "$BOOT_ID" ]; then
        warn "in-flight marker is from boot $boot (current $BOOT_ID); no process can survive that reboot"
    fi
    return 0
}

# A retry is allowed only while the state has not moved since the attempt
# began.  The comparison is against the state seen before the first attempt,
# not against "no state": a repeated install legitimately starts from
# phase=complete, and a documented pre-backup resume starts from
# install_started.  Preflight already refused every post-backup state, so any
# change here is the installer (or another process) having done something.
# The phase alone is not enough: a same-phase rewrite of operation_id,
# updated or backup would otherwise be invisible.
assert_retry_state_unchanged() {
    refresh_state "refusing to retry"
    local now=${ST_PHASE:-}
    local now_op=${ST_OPERATION_ID:-}
    local now_updated=${ST_UPDATED:-}
    local now_backup=${ST_BACKUP:-}
    if [ "$now" != "$BOOTSTRAP_PHASE_BASELINE" ]; then
        die "the previous attempt moved the state to phase=${now:-none} (started as ${BOOTSTRAP_PHASE_BASELINE:-none}); a retry could repeat a started install. Run status, and use rollback if a write started"
    fi
    if [ "$now_op" != "$BOOTSTRAP_STATE_OP" ] || [ "$now_updated" != "$BOOTSTRAP_STATE_UPDATED" ] \
        || [ "$now_backup" != "$BOOTSTRAP_STATE_BACKUP" ]; then
        die "the operation state changed between bootstrap attempts while phase stayed ${now:-none} (operation_id ${BOOTSTRAP_STATE_OP:-none} -> ${now_op:-none}, updated ${BOOTSTRAP_STATE_UPDATED:-none} -> ${now_updated:-none}, backup ${BOOTSTRAP_STATE_BACKUP:-none} -> ${now_backup:-none}); a retry could repeat a started install. Run status, and use rollback if a write started"
    fi
    return 0
}

bootstrap_attempt_timeout() {
    local now rem t
    now=$(date +%s)
    rem=$(( BOOTSTRAP_STARTED + BOOTSTRAP_BUDGET - now ))
    [ "$rem" -ge 5 ] || return 1
    t=$rem
    [ "$t" -gt 300 ] && t=300
    printf '%s' "$t"
}

bootstrap_delay_for() {
    local i=$1 n
    local -a delays=()
    local d
    for d in $BOOTSTRAP_BACKOFF; do
        delays+=( "$d" )
    done
    n=${#delays[@]}
    [ "$n" -gt 0 ] || { printf '5'; return 0; }
    i=$(( i - 1 ))
    [ "$i" -ge 0 ] || i=0
    [ "$i" -lt "$n" ] || i=$(( n - 1 ))
    printf '%s' "${delays[$i]}"
}

# Last meaningful line of a failed attempt, for the operator's log.  Both
# captures are searched: installer decisions (common.sh fail()) go to stderr.
bootstrap_last_reason() {
    local out=$1 err=${2:-} all line
    all=$(printf '%s\n%s\n' "$out" "$err")
    line=$(printf '%s\n' "$all" | grep -E '^(RESULT|bridge|INSTALL_FAIL|ROLLBACK_FAIL|ERROR)' | tail -n 1)
    [ -n "$line" ] || line=$(printf '%s\n' "$all" | grep -v '^[[:space:]]*$' | tail -n 1)
    printf '%s' "$line" | tr -d '\r' | cut -c1-120
}

bootstrap_wait_adb() {
    local until
    adb_get_state && return 0
    # 2 means the operation deadline expired, not "ADB stayed away".
    [ "$DEADLINE_HIT" = 1 ] && return 2
    until=$(( $(date +%s) + RECONNECT_TIMEOUT ))
    while [ "$(date +%s)" -lt "$until" ]; do
        sleep 2
        adb_get_state && return 0
        [ "$DEADLINE_HIT" = 1 ] && return 2
    done
    return 1
}

# After a reconnect the station may have rebooted.  BOOT_ID is used by the
# lock, marker and CHANNEL_WAIT gates, so a stale value would make a live lock
# on the new boot look stale.  Re-read it and re-run the identity gates; the
# caller re-runs the process/lock/marker gates immediately afterwards.
refresh_boot_after_reconnect() {
    local new
    dev_boot_completed || { ADB_ERROR="sys.boot_completed is not 1 after reconnect"; return 1; }
    dev_build_gate || return 1
    new=$(read_device_boot_id) || { ADB_ERROR="boot_id is unreadable after reconnect"; return 1; }
    [ -n "$new" ] || { ADB_ERROR="empty boot_id after reconnect"; return 1; }
    DEV_BUILD=$DEV_BUILD_VALUE
    if [ "$new" = "$BOOT_ID" ]; then
        return 0
    fi
    say "station rebooted during bootstrap: boot_id $BOOT_ID -> $new; re-checking identity and the new boot's gates"
    BOOT_ID=$new
    return 0
}

# A retry exists for one specific known failure: the page-reclaim race of the
# first Mali cycle and the scan that depends on it.  It is allowed only for a
# plain nonzero exit whose stdout ends with a documented terminal failure and
# whose stderr carries no transport-loss marker and no device decision.  A
# capture that only contains an intermediate cycle line is truncated: the
# bridge may still have exec'd /system/bin/sh, where the installer is named sh
# and is invisible to the four-name gate until it takes the lock or writes
# state, so a blind retry could start a second root-bridge over a live install.
bootstrap_transient_failure() {
    local rc=$1 out=$2 err=${3:-} all
    is_uint "$rc" || return 1
    case $rc in
        0|124|126|127|137|143) return 1 ;;
    esac
    [ -n "$out" ] || return 1
    all=$(printf '%s\n%s\n' "$out" "$err")
    # A decision from the installer is never a Mali flake, even when the same
    # capture contains an earlier transient-looking line.
    if printf '%s\n' "$all" | grep -Eq '^(INSTALL_FAIL|ROLLBACK_FAIL|ERROR:)'; then
        return 1
    fi
    # Reaching the root shell means the attempt may have started install-run.sh
    # (and permissive) even if an earlier cycle printed a Mali line.  Do not
    # retry: the process may still be alive as sh.
    if printf '%s\n' "$all" | grep -Eq '^shell: exec /system/bin/sh|^RESULT: ESCALATION OK|^RESULT: PERMISSIVE'; then
        return 1
    fi
    # A transport loss leaves its own marker in stderr.  The stdout prefix may
    # still look like a clean Mali failure, but the attempt did not finish.
    if printf '%s\n' "$err" | grep -Eqi '(^|[^[:alnum:]_])(error: closed|device offline|no devices/emulators found|connection (reset|refused|closed)|transport error|protocol fault)([^[:alnum:]_]|$)'; then
        return 1
    fi
    # Refuse every RESULT line outside the two documented terminal failures.
    # This is per line, not "one allowed line makes the rest acceptable": a
    # mixed capture with an unknown RESULT must never become a retry.  Filter
    # to RESULT lines first so blank/other lines cannot satisfy grep -v.
    if printf '%s\n' "$all" | grep -Eq '^RESULT:' \
        && printf '%s\n' "$all" | grep '^RESULT:' | grep -Evq '^RESULT: (reader bring-up failed|scan phase failed; NO writes done)$'; then
        return 1
    fi
    # Retry only when stdout ends with a terminal bridge failure.  Cycle lines
    # alone are an interrupted capture, not proof that the attempt completed.
    printf '%s\n' "$out" | grep -Eq '^(RESULT: (reader bring-up failed|scan phase failed; NO writes done)|bridge: (reader bring-up failed|scan phase failed; NO writes done))$'
}

# Wait for ADB after a loss.  A real OP_TIMEOUT is reported as a deadline,
# not as a lost device; after a successful return the boot identity is
# re-read so the next gates see a reboot instead of a stale BOOT_ID.
bootstrap_reconnect_or_die() {
    local rc=0
    bootstrap_wait_adb || rc=$?
    case $rc in
        0) ;;
        2) die "operation deadline (OP_TIMEOUT=${OP_TIMEOUT}s) reached while waiting for ADB to return; nothing is trusted. Reconnect and run status before any new attempt." ;;
        *) die "ADB was lost during bootstrap and did not return; the in-flight marker $INFLIGHT_FILE remains. Reconnect, run status, and rerun install only after it proves no second channel." ;;
    esac
    say "ADB is back; re-reading boot identity and re-checking the device before the next attempt"
    refresh_boot_after_reconnect \
        || die "ADB returned but the station identity could not be verified: ${ADB_ERROR:-unknown error}. No retry was attempted."
}

# One bootstrap phase: up to BOOTSTRAP_ATTEMPTS calls of the same esc-shell
# command, each preceded by the full gate set.  Returns 0 only after the
# device state says phase=complete.
run_bootstrap_install() {
    local attempt=1 rc=0 mount tmo reason delay out_text="" err_text=""
    BOOTSTRAP_STARTED=$(date +%s)
    BOOTSTRAP_USED=0
    refresh_state "refusing to start"
    BOOTSTRAP_PHASE_BASELINE=${ST_PHASE:-}
    BOOTSTRAP_STATE_OP=${ST_OPERATION_ID:-}
    BOOTSTRAP_STATE_UPDATED=${ST_UPDATED:-}
    BOOTSTRAP_STATE_BACKUP=${ST_BACKUP:-}
    while :; do
        if [ "$DEADLINE_HIT" = 1 ] || ! deadline_ok; then
            die "operation deadline (OP_TIMEOUT=${OP_TIMEOUT}s) reached during bootstrap; no result is trusted. Reconnect, run status, and rerun install only after it proves no second channel."
        fi
        CALL_TIMEOUT=0
        if ! adb_get_state; then
            bootstrap_reconnect_or_die
        fi
        assert_no_channel
        assert_no_live_lock
        assert_own_inflight
        if [ "$attempt" -gt 1 ]; then
            assert_retry_state_unchanged
        fi
        mount=$(dev_mount_state_quiet) || die "check not performed: mount failed"
        [ "$mount" = ro ] \
            || die "/odm is $mount after a failed bootstrap; do not retry. Run status, and use rollback if a write started"
        tmo=$(bootstrap_attempt_timeout) \
            || die "bootstrap budget of ${BOOTSTRAP_BUDGET}s is exhausted after $(( attempt - 1 )) attempt(s); nothing was written to /odm. Wait for the station to settle, then rerun install"
        write_inflight install
        say "bootstrap attempt $attempt/$BOOTSTRAP_ATTEMPTS (budget left $(( BOOTSTRAP_STARTED + BOOTSTRAP_BUDGET - $(date +%s) ))s)"
        CALL_TIMEOUT=0
        if adb_shell "$tmo" "cd $(shq "$BASE") && BASE=$(shq "$BASE") OPERATION_ID=$(shq "$OP_ID") STAGING_NONCE=$(shq "$STAGING_NONCE") INFLIGHT_OWNED=1 ./root-bridge esc-shell < hybrid/install-run.sh"; then
            rc=0
        else
            rc=$?
        fi
        # ADB_STDOUT is overwritten by every later call, so keep this
        # attempt's output for the operator and for the reason line.
        out_text=$ADB_STDOUT
        err_text=$ADB_STDERR
        printf '%s\n' "$out_text"
        [ -n "$err_text" ] && printf '%s\n' "$err_text" >&2

        if ! adb_get_state; then
            bootstrap_reconnect_or_die
        fi
        refresh_state "refusing to continue"
        # A phase that moved away from the baseline (other than to complete)
        # means the installer itself ran and did not finish.  This is reported
        # before the rc is interpreted: a partial write outranks a timeout.
        if [ "${ST_PHASE:-}" != "$BOOTSTRAP_PHASE_BASELINE" ] && [ "${ST_PHASE:-}" != complete ]; then
            die "the install script did not finish and moved the state to phase=${ST_PHASE:-none}; no retry was attempted. Run status, and use rollback if a write started"
        fi
        # rc=124 is a per-call timeout (or a real deadline).  The remote
        # process may still be alive and invisible as sh; never treat
        # phase=complete as success for this attempt and never retry blindly.
        if [ "$rc" = 124 ]; then
            if [ "$DEADLINE_HIT" = 1 ] || ! deadline_ok; then
                die "operation deadline (OP_TIMEOUT=${OP_TIMEOUT}s) reached during bootstrap (the ADB attempt also timed out); nothing is trusted. Reconnect and run status before any new attempt."
            fi
            die "bootstrap attempt $attempt/$BOOTSTRAP_ATTEMPTS timed out after ${tmo}s (per-call ADB timeout, not OP_TIMEOUT; state phase=${ST_PHASE:-none}); the installer may still be running on the device. No automatic retry was made: reconnect, run status, and rerun install only after it proves no second channel."
        fi
        # Success needs the device to say complete for THIS attempt.  A state
        # that was already complete before the attempt (a reused backup on a
        # repeated install) proves nothing about this call, so it is accepted
        # only when the bootstrap itself returned 0.
        if [ "${ST_PHASE:-}" = complete ] && { [ "$rc" = 0 ] || [ "$BOOTSTRAP_PHASE_BASELINE" != complete ]; }; then
            adb_shell 15 "rm -f $(shq "$INFLIGHT_FILE")" >/dev/null 2>&1 \
                || warn "could not clear in-flight marker after a confirmed complete install"
            BOOTSTRAP_USED=$attempt
            say "bootstrap: install completed on attempt $attempt/$BOOTSTRAP_ATTEMPTS"
            return 0
        fi
        if [ "$rc" = 0 ]; then
            die "bootstrap exited 0 but the install did not complete (phase=${ST_PHASE:-none}); inspect $STATE_FILE"
        fi
        if [ "${ST_PHASE:-}" != "$BOOTSTRAP_PHASE_BASELINE" ]; then
            die "the install script moved the state to phase=${ST_PHASE:-none} but this attempt returned rc=$rc; no retry was attempted. Run status, and use rollback if a write started"
        fi
        # No device output means the attempt's outcome is unknown.  The
        # installer may be alive as sh until it takes the lock or writes
        # state, so an automatic retry could start a second root-bridge.
        if [ -z "$out_text" ]; then
            die "bootstrap attempt $attempt/$BOOTSTRAP_ATTEMPTS returned no device output (rc=$rc); the outcome is unknown, so it was not retried. The installer may still be running under a name the process gate cannot see (sh). Reconnect, run status, and rerun install only after it proves no second channel."
        fi
        reason=$(bootstrap_last_reason "$out_text" "$err_text")
        if ! bootstrap_transient_failure "$rc" "$out_text" "$err_text"; then
            die "bootstrap failed with a non-transient error (rc=$rc): ${reason:-no output}. Nothing was written to /odm and this is not the Mali reclaim race, so it was not retried. Fix the cause above and rerun install."
        fi
        if [ "$DEADLINE_HIT" = 1 ] || ! deadline_ok; then
            die "operation deadline (OP_TIMEOUT=${OP_TIMEOUT}s) reached during bootstrap (last: ${reason:-no output}); nothing is trusted. Reconnect and run status before any new attempt."
        fi
        if [ "$attempt" -ge "$BOOTSTRAP_ATTEMPTS" ]; then
            die "bootstrap failed after $attempt attempt(s) (rc=$rc, last: ${reason:-no output}); nothing was written to /odm. If this repeats, the first Mali cycle is not the only cause: run ./station-max.sh diag and check free memory (\`free -m\`), THP state and the station load."
        fi
        delay=$(bootstrap_delay_for "$attempt")
        say "attempt $attempt/$BOOTSTRAP_ATTEMPTS failed: ${reason:-no output}; retrying in ${delay}s"
        if [ "$delay" -gt 0 ]; then
            sleep "$delay"
        fi
        attempt=$(( attempt + 1 ))
    done
}

# ------------------------------------------------------------- preflight ---

preflight_identity() {
    dev_boot_completed || die "sys.boot_completed is not 1"
    dev_build_gate || die "preflight check not performed: ${ADB_ERROR:-build gate failed}"
    DEV_BUILD=$DEV_BUILD_VALUE
    BOOT_ID=$(read_device_boot_id) || die "preflight check not performed: boot_id unreadable"
    [ -n "$BOOT_ID" ] || die "preflight check not performed: empty boot_id"
    say "station=$DEV_BUILD boot_id=$BOOT_ID"
}

preflight_device() {
    adb_shell 20 '[ -d /odm ] && [ ! -L /odm ]' || die "preflight check failed: /odm is not a regular directory"
    MOUNT_STATE=$(dev_mount_state_quiet) || die "preflight check not performed: mount failed or /odm is not mounted"
    [ "$MOUNT_STATE" = ro ] || die "/odm is $MOUNT_STATE; a previous operation was interrupted. Do not install over rw /odm."

    adb_shell 20 'for p in /odm /odm/bin /odm/etc /odm/etc/init /odm/bin/rootd /odm/bin/permissive /odm/etc/init/rootd.rc /odm/etc/rootd.conf; do [ -L "$p" ] && echo "SYMLINK $p"; done; exit 0' \
        || die "preflight check not performed: symlink check failed"
    [ -z "$(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]')" ] || die "refusing symlink component: $ADB_STDOUT"

    local free_odm free_data
    free_odm=$(dev_free_kb /odm) || die "preflight check not performed: df /odm failed"
    [ "$free_odm" -ge 8192 ] || die "not enough free space on /odm (${free_odm}K)"
    free_data=$(dev_free_kb /data/local/tmp) || die "preflight check not performed: df /data/local/tmp failed"
    [ "$free_data" -ge 8192 ] || die "not enough free space for backup (${free_data}K)"

    dev_check_commands || die "preflight check failed: required device commands are missing"

    assert_no_channel
    say "rootd processes: $PS_ROOTD_N"

    refresh_state "refusing to override it"
    local phase operation backup
    operation=${ST_OPERATION:-}
    phase=${ST_PHASE:-}
    backup=${ST_BACKUP:-}

    assert_no_live_lock

    local inflight
    read_inflight_strict \
        || die "cannot read in-flight marker $INFLIGHT_FILE; refusing to guess whether another bootstrap is running"
    inflight=$INFLIGHT_TEXT
    if [ -n "$inflight" ]; then
        clear_inflight_if_safe "$inflight" "$STATE_TEXT"
    fi

    ROOTSTATUS=$(read_optional_file /data/local/tmp/rootstatus 2>/dev/null) || ROOTSTATUS=
    if [ "$PS_ROOTD_N" = 1 ]; then
        local st st_boot
        st=$(state_get "$ROOTSTATUS" state)
        st_boot=$(state_get "$ROOTSTATUS" boot_id)
        if [ "$st" != PROTECTED ] || [ -z "$st_boot" ] || [ "$st_boot" != "$BOOT_ID" ]; then
            die "rootd is present but its state is uncertain (state=${st:-absent} boot_id=${st_boot:-absent}); stop and inspect before retrying"
        fi
        if ! root_cmd strict 'id -u'; then
            die "rootd is present but the queue did not answer correctly: $ROOT_ERROR"
        fi
        [ "$ROOT_OUTPUT" = 0 ] || die "rootd queue did not run as uid 0 (got '$ROOT_OUTPUT')"
        ROOTD_MODE=queue
        say "rootd queue is healthy; installation will use it (no second Mali bootstrap)"
    else
        ROOTD_MODE=bootstrap
        case ${operation:-}:${phase:-} in
            install:install_started|install:backup_started)
                say "pre-backup phase=${phase} found; the installer will resume safely without creating a new original backup"
                ;;
            install:failed)
                if [ -n "$backup" ]; then
                    die "failed install has a backup (${backup}); use status and rollback instead of another bootstrap"
                fi
                say "pre-backup failed phase found; the installer may safely create the original backup"
                ;;
            install:backup_complete|install:writing|install:files_written|install:verified)
                die "install phase=${phase:-unknown} has passed the backup point; a new bootstrap could hide a partial write. Use status and rollback instead."
                ;;
            rollback:*)
                [ "$phase" = rollback_complete ] || die "rollback is not complete (phase=${phase:-unknown}); finish or recover it before install"
                ;;
        esac
        if [ "$phase" = complete ] && [ -n "$backup" ]; then
            say "completed installation state found; the installer will reuse $backup if the current set matches"
        fi
    fi
}

# ------------------------------------------------------------ queue cli ----

queue_publish() {
    local id=$1 cmd=$2 req="$WORK/rootcmd.$id"
    printf '%s\n' "$cmd" > "$req" || { ROOT_ERROR="cannot write local request"; return 1; }
    chmod 0600 "$req" || { ROOT_ERROR="cannot protect local request"; return 1; }
    adb_push 30 "$req" "$QUEUE/.rootcmd-$id.tmp" || { ROOT_ERROR="cannot push request: $ADB_STDERR"; return 1; }
    adb_shell 20 "chmod 0660 $(shq "$QUEUE/.rootcmd-$id.tmp") && mv -f $(shq "$QUEUE/.rootcmd-$id.tmp") $(shq "$QUEUE/rootcmd-$id")" \
        || { ROOT_ERROR="cannot publish request atomically: $ADB_STDERR"; return 1; }
    return 0
}

# Withdraw a request that has not been claimed.  If rootd already claimed it,
# report that instead of pretending the command did not happen.
queue_withdraw() {
    local id=$1 out
    if ! adb_get_state; then
        ROOT_ERROR="${ROOT_ERROR:-request failed}; request $id was published but ADB is unavailable, so it could not be withdrawn"
        return 1
    fi
    adb_shell 15 "if [ -f $(shq "$QUEUE/rootcmd-$id") ]; then rm -f $(shq "$QUEUE/rootcmd-$id"); echo withdrawn; elif [ -f $(shq "$QUEUE/rootres.$id") ]; then echo result-present; else echo gone; fi"
    if [ $? -ne 0 ]; then
        ROOT_ERROR="${ROOT_ERROR:-request failed}; request $id could not be withdrawn: ${ADB_STDERR:-no error text}"
        return 1
    fi
    out=$(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]')
    case $out in
        withdrawn)
            printf 'request %s withdrawn before execution\n' "$id" >&2
            ;;
        result-present)
            printf 'request %s was already claimed; result is present, inspect it\n' "$id" >&2
            ;;
        *)
            printf 'request %s was already claimed or removed; result is unknown\n' "$id" >&2
            ;;
    esac
    return 0
}

# Read the result for exactly our request id.  Strict mode rejects a
# truncated block and incomplete child cleanup.
root_cmd() {
    local mode=$1 cmd=$2 strict=0
    [ "$mode" = strict ] && strict=1
    ROOT_OUTPUT=
    ROOT_ERROR=
    ROOT_EXIT=
    ROOT_WARN=
    local boot_before id res res_id res_boot status result exit_code signal truncated cleanup reason
    boot_before=$(read_device_boot_id) || { ROOT_ERROR="cannot read boot_id before request"; return 1; }
    [ -n "$boot_before" ] || { ROOT_ERROR="empty boot_id before request"; return 1; }
    id="sm-$(date +%s)-$$-$RANDOM"
    case $id in
        *[!A-Za-z0-9._-]*) ROOT_ERROR="generated request id is invalid"; return 1 ;;
    esac
    [ ${#id} -ge 1 ] && [ ${#id} -le 64 ] || { ROOT_ERROR="generated request id is invalid"; return 1; }
    queue_publish "$id" "$cmd" || return 1
    local wait_deadline=$((SECONDS + QUEUE_WAIT))
    [ "$wait_deadline" -gt "$DEADLINE" ] && wait_deadline=$DEADLINE
    local adb_errors=0 rc
    while [ "$SECONDS" -lt "$wait_deadline" ]; do
        adb_shell 10 "[ -f $(shq "$QUEUE/rootres.$id") ]"
        rc=$?
        if [ "$rc" -eq 0 ]; then
            break
        fi
        if [ -n "$ADB_STDERR" ] || [ "$rc" = 124 ]; then
            adb_errors=$((adb_errors + 1))
        else
            adb_errors=0
        fi
        if [ "$adb_errors" -ge 3 ]; then
            ROOT_ERROR="ADB lost while waiting for request $id"
            adb_shell 10 "rm -f $(shq "$QUEUE/.rootcmd-$id.tmp")" >/dev/null 2>&1 || true
            queue_withdraw "$id" || true
            return 1
        fi
        sleep 1
    done
    if ! adb_shell 10 "[ -f $(shq "$QUEUE/rootres.$id") ]"; then
        ROOT_ERROR="queue timeout for request $id"
        adb_shell 10 "rm -f $(shq "$QUEUE/.rootcmd-$id.tmp")" >/dev/null 2>&1 || true
        queue_withdraw "$id" || true
        return 1
    fi

    adb_shell 20 "cat $(shq "$QUEUE/rootres.$id")" || { ROOT_ERROR="cannot read result for $id"; queue_withdraw "$id" || true; return 1; }
    res=$ADB_STDOUT
    [ -n "$res" ] || { ROOT_ERROR="empty result for $id"; return 1; }
    res_id=$(state_get "$res" id)
    res_boot=$(state_get "$res" boot_id)
    status=$(state_get "$res" status)
    result=$(state_get "$res" result)
    exit_code=$(state_get "$res" exit)
    signal=$(state_get "$res" signal)
    truncated=$(state_get "$res" truncated)
    cleanup=$(state_get "$res" cleanup)
    reason=$(state_get "$res" reason)

    [ "$res_id" = "$id" ] || { ROOT_ERROR="result id mismatch: '$res_id' != '$id'"; return 1; }
    [ "$res_boot" = "$boot_before" ] || { ROOT_ERROR="result boot_id '$res_boot' != saved '$boot_before'"; return 1; }
    [ "$status" = executed ] || { ROOT_ERROR="queue status=$status reason=${reason:-none}"; return 1; }
    case $result in
        exited) ;;
        *) ROOT_ERROR="queue result=$result (exit=$exit_code signal=$signal)${reason:+ reason=$reason}"; return 1 ;;
    esac
    case $exit_code in
        ''|*[!0-9]*) ROOT_ERROR="bad queue exit code '$exit_code'"; return 1 ;;
    esac
    [ "$exit_code" -le 255 ] || { ROOT_ERROR="queue exit code out of range: $exit_code"; return 1; }
    [ "$signal" = 0 ] || { ROOT_ERROR="exited result has signal=$signal"; return 1; }
    case $cleanup in
        none|complete|kill-sent) ;;
        *) ROOT_ERROR="queue cleanup=$cleanup; child processes may still be running"; return 1 ;;
    esac
    if [ "$strict" = 1 ]; then
        [ "$truncated" = 0 ] || { ROOT_ERROR="queue output truncated"; return 1; }
        [ "$cleanup" != incomplete ] || { ROOT_ERROR="queue cleanup incomplete"; return 1; }
    elif [ "$truncated" != 0 ]; then
        ROOT_WARN="queue output was truncated"
    fi

    # Output block: exactly our id, terminated by any contract marker
    # (exited, signaled, timeout, cancelled, spawn-failed, cleanup-blocked,
    # io-error).  A missing begin or end marker makes the block unusable.
    # Use grep -F + sed ranges: the toybox awk on the station segfaults on
    # the equivalent index()-based program.
    local blockcmd
    blockcmd=$(cat <<EOF
_b=\$(grep -nF -e "--- rootd request=$id begin ---" /data/local/tmp/rootout | head -n 1 | cut -d: -f1)
_e=\$(grep -nF -e "--- rootd request=$id " /data/local/tmp/rootout | grep ' exit=' | head -n 1 | cut -d: -f1)
[ -n "\$_b" ] && [ -n "\$_e" ] && [ "\$_e" -gt "\$_b" ] || exit 3
sed -n "\$((_b+1)),\$((_e-1))p" /data/local/tmp/rootout
EOF
)
    adb_shell 25 "$blockcmd" || { ROOT_ERROR="output block for $id is missing or incomplete"; return 1; }
    ROOT_OUTPUT=$ADB_STDOUT
    local boot_after
    boot_after=$(read_device_boot_id) || { ROOT_ERROR="cannot read boot_id after request"; return 1; }
    [ "$boot_after" = "$boot_before" ] || { ROOT_ERROR="boot_id changed during request"; return 1; }
    adb_shell 10 "rm -f $(shq "$QUEUE/rootres.$id")" >/dev/null 2>&1 || true
    ROOT_EXIT=$exit_code
    return "$exit_code"
}

# -------------------------------------------------------------- verify ----

vok() {
    [ "$VERIFY_QUIET" = 1 ] || ok "$*"
    return 0
}

vbad() {
    FAIL=1
    CHECK_FAILS=$((CHECK_FAILS + 1))
    [ -n "$FIRST_FAIL" ] || FIRST_FAIL=$*
    [ "$VERIFY_QUIET" = 1 ] || bad "$*"
    return 0
}

mount_opt_has() {
    local opts=$1 want=$2 oldifs=$IFS
    IFS=,
    for o in $opts; do
        if [ "$o" = "$want" ]; then
            IFS=$oldifs
            return 0
        fi
    done
    IFS=$oldifs
    return 1
}

extract_block() {
    local text=$1 name=$2
    printf '%s\n' "$text" | awk -v n="$name" '
        $0 == "__SM_BEGIN_" n { inb=1; next }
        inb && index($0, "__SM_RC_" n "=") == 1 { inb=0; exit }
        inb { print }
    '
}

rc_marker() {
    local text=$1 name=$2
    printf '%s\n' "$text" | sed -n "s/^__SM_RC_$name=//p" | tail -n 1
}

check_marker() {
    local text=$1 name=$2 rc
    rc=$(rc_marker "$text" "$name")
    [ -n "$rc" ] || { vbad "verification check '$name' produced no status"; return 0; }
    [ "$rc" = 0 ] || vbad "verification check '$name' exited $rc"
    return 0
}

meta_line() {
    local text=$1 path=$2
    printf '%s\n' "$text" | awk -v p="$path" '$NF == p { print $1, $2, $3; exit }'
}

label_line() {
    local text=$1 path=$2
    printf '%s\n' "$text" | awk -v p="$path" '
        index($0, p) {
            for (i = 1; i <= NF; i++) if ($i ~ /^u:object_r:/) { print $i; exit }
        }
    '
}

check_meta() {
    local meta=$1 labels=$2 path=$3 want_mode=$4 want_label=$5
    local line mode uid gid label
    line=$(meta_line "$meta" "$path")
    if [ -z "$line" ]; then
        vbad "metadata missing for $path"
        return 0
    fi
    mode=$(printf '%s' "$line" | awk '{print $1}')
    uid=$(printf '%s' "$line" | awk '{print $2}')
    gid=$(printf '%s' "$line" | awk '{print $3}')
    mode=${mode#0}; [ -n "$mode" ] || mode=0
    [ "$mode" = "$want_mode" ] || vbad "mode $path is $mode, want $want_mode"
    [ "$uid" = 0 ] && [ "$gid" = 0 ] || vbad "owner $path is $uid:$gid, want 0:0"
    label=$(label_line "$labels" "$path")
    [ "$label" = "$want_label" ] || vbad "label $path is '${label:-missing}', want $want_label"
    return 0
}

# One token walker for the three OUTPUT rules checked below.  Arguments:
#   line proto dport jump match_dest negate allow32 to_dest
# match_dest is accepted as-is or with /32 when allow32=1; negate=1 requires
# "! -d"; to_dest, when non-empty, is the required --to-destination value.
# Any token outside this grammar fails closed.  Keeping one parser means a
# rule change is one edit, not three copies that can drift apart.
rule_ok() {
    local line=$1 want_proto=$2 dport=$3 jump=$4 match_dest=$5 negate=$6 allow32=$7 to_dest=$8
    local -a t=()
    read -ra t <<< "$line"
    [ "${t[0]:-}" = -A ] && [ "${t[1]:-}" = OUTPUT ] || return 1
    local i=2 seen_p=0 seen_dport=0 seen_j=0 seen_d=0 seen_neg=0 seen_to=0
    while [ "$i" -lt ${#t[@]} ]; do
        case ${t[$i]} in
            -p)
                i=$((i+1)); [ "$i" -lt ${#t[@]} ] || return 1
                [ "${t[$i]}" = "$want_proto" ] || return 1
                seen_p=1
                ;;
            -m)
                i=$((i+1)); [ "$i" -lt ${#t[@]} ] || return 1
                [ "${t[$i]}" = "$want_proto" ] || return 1
                ;;
            --dport)
                i=$((i+1)); [ "$i" -lt ${#t[@]} ] || return 1
                [ "${t[$i]}" = "$dport" ] || return 1
                seen_dport=1
                ;;
            -j)
                i=$((i+1)); [ "$i" -lt ${#t[@]} ] || return 1
                [ "${t[$i]}" = "$jump" ] || return 1
                seen_j=1
                ;;
            --to-destination)
                i=$((i+1)); [ "$i" -lt ${#t[@]} ] || return 1
                [ -n "$to_dest" ] && [ "${t[$i]}" = "$to_dest" ] || return 1
                seen_to=1
                ;;
            '!')
                [ "$negate" = 1 ] || return 1
                [ "${t[$((i+1))]:-}" = -d ] || return 1
                seen_neg=1
                ;;
            -d)
                i=$((i+1)); [ "$i" -lt ${#t[@]} ] || return 1
                [ -n "$match_dest" ] || return 1
                [ "$negate" = 0 ] || [ "$seen_neg" = 1 ] || return 1
                if [ "$allow32" = 1 ]; then
                    case ${t[$i]} in
                        "$match_dest"|"$match_dest/32") ;;
                        *) return 1 ;;
                    esac
                else
                    [ "${t[$i]}" = "$match_dest" ] || return 1
                fi
                seen_d=1
                ;;
            *) return 1 ;;
        esac
        i=$((i+1))
    done
    [ "$seen_p" = 1 ] && [ "$seen_dport" = 1 ] && [ "$seen_j" = 1 ] || return 1
    [ -z "$match_dest" ] || [ "$seen_d" = 1 ] || return 1
    [ "$negate" = 0 ] || [ "$seen_neg" = 1 ] || return 1
    [ -z "$to_dest" ] || [ "$seen_to" = 1 ] || return 1
    return 0
}

# Try both TCP/UDP orders, exactly like a two-rule OUTPUT chain.
rule_pair_ok() {
    local kind=$1 a=$2 b=$3
    case $kind in
        dns)
            { rule_ok "$a" tcp 53 DNAT "$CFG_DNS" 0 1 "$CFG_ROUTER:53" \
                && rule_ok "$b" udp 53 DNAT "$CFG_DNS" 0 1 "$CFG_ROUTER:53"; } \
            || { rule_ok "$a" udp 53 DNAT "$CFG_DNS" 0 1 "$CFG_ROUTER:53" \
                && rule_ok "$b" tcp 53 DNAT "$CFG_DNS" 0 1 "$CFG_ROUTER:53"; }
            ;;
        dot)
            { rule_ok "$a" tcp 853 DROP "$CFG_ROUTER" 1 1 "" \
                && rule_ok "$b" udp 853 DROP "$CFG_ROUTER" 1 1 ""; } \
            || { rule_ok "$a" udp 853 DROP "$CFG_ROUTER" 1 1 "" \
                && rule_ok "$b" tcp 853 DROP "$CFG_ROUTER" 1 1 ""; }
            ;;
        ipv6)
            { rule_ok "$a" tcp 853 DROP "" 0 0 "" \
                && rule_ok "$b" udp 853 DROP "" 0 0 ""; } \
            || { rule_ok "$a" udp 853 DROP "" 0 0 "" \
                && rule_ok "$b" tcp 853 DROP "" 0 0 ""; }
            ;;
        *) return 1 ;;
    esac
}

check_first_two() {
    local label=$1 block=$2 kind=$3 msg
    local -a rules=()
    local line n=0
    while IFS= read -r line; do
        case $line in
            '-A OUTPUT '*) rules+=("$line") ;;
        esac
    done < <(printf '%s\n' "$block")
    n=${#rules[@]}
    if [ "$n" -lt 2 ]; then
        vbad "$label: expected two OUTPUT rules, found $n"
        return 0
    fi
    case $kind in
        dns) msg="first two OUTPUT rules are not exact DNS DNAT TCP/UDP rules (position matters)" ;;
        dot) msg="first two OUTPUT rules are not exact DoT DROP TCP/UDP rules (position matters)" ;;
        ipv6) msg="first two OUTPUT rules are not exact IPv6 DoT DROP TCP/UDP rules" ;;
        *) vbad "$label: unknown rule kind '$kind'"; return 0 ;;
    esac
    rule_pair_ok "$kind" "${rules[0]}" "${rules[1]}" || vbad "$label: $msg"
    return 0
}

apply_expected_from_state() {
    EXPECT_ROOTD=$ROOTD_SHA
    EXPECT_PERM=$PERM_SHA
    EXPECT_RC=$RC_SHA
    EXPECT_CONF=
    local phase expect
    phase=$(state_get "$STATE_TEXT" phase)
    expect=$(state_get "$STATE_TEXT" expect)
    case ${phase:-}:${expect:-} in
        rollback_complete:hybrid|rollback_failed:hybrid)
            [ -n "$(state_get "$STATE_TEXT" restored_rootd)" ] && EXPECT_ROOTD=$(state_get "$STATE_TEXT" restored_rootd)
            [ -n "$(state_get "$STATE_TEXT" restored_permissive)" ] && EXPECT_PERM=$(state_get "$STATE_TEXT" restored_permissive)
            [ -n "$(state_get "$STATE_TEXT" restored_rc)" ] && EXPECT_RC=$(state_get "$STATE_TEXT" restored_rc)
            EXPECT_CONF=$(state_get "$STATE_TEXT" restored_conf)
            ;;
        complete:*|*:hybrid|install_started:*|backup_started:*|backup_complete:*|writing:*|files_written:*|verified:*|failed:*)
            [ -n "$(state_get "$STATE_TEXT" installed_rootd)" ] && EXPECT_ROOTD=$(state_get "$STATE_TEXT" installed_rootd)
            [ -n "$(state_get "$STATE_TEXT" installed_permissive)" ] && EXPECT_PERM=$(state_get "$STATE_TEXT" installed_permissive)
            [ -n "$(state_get "$STATE_TEXT" installed_rc)" ] && EXPECT_RC=$(state_get "$STATE_TEXT" installed_rc)
            EXPECT_CONF=$(state_get "$STATE_TEXT" installed_conf)
            if [ -z "$EXPECT_CONF" ] && [ -n "$(state_get "$STATE_TEXT" target_conf)" ]; then
                EXPECT_CONF=$(state_get "$STATE_TEXT" target_conf)
                [ -n "$(state_get "$STATE_TEXT" target_rootd)" ] && EXPECT_ROOTD=$(state_get "$STATE_TEXT" target_rootd)
                [ -n "$(state_get "$STATE_TEXT" target_permissive)" ] && EXPECT_PERM=$(state_get "$STATE_TEXT" target_permissive)
                [ -n "$(state_get "$STATE_TEXT" target_rc)" ] && EXPECT_RC=$(state_get "$STATE_TEXT" target_rc)
            fi
            ;;
    esac
}

verify_queue_checks() {
    local cmd
    cmd=$(cat <<'EOF'
echo __SM_BEGIN_id
id -u
echo __SM_RC_id=$?
echo __SM_BEGIN_cache
stat -c '%a %u %g' /cache
echo __SM_RC_cache=$?
echo __SM_BEGIN_recovery
stat -c '%a %u %g' /cache/recovery
echo __SM_RC_recovery=$?
echo __SM_BEGIN_enforce
getenforce
echo __SM_RC_enforce=$?
echo __SM_BEGIN_rootdhash
sha256sum /odm/bin/rootd
echo __SM_RC_rootdhash=$?
echo __SM_BEGIN_permhash
sha256sum /odm/bin/permissive
echo __SM_RC_permhash=$?
echo __SM_BEGIN_rchash
sha256sum /odm/etc/init/rootd.rc
echo __SM_RC_rchash=$?
echo __SM_BEGIN_confhash
sha256sum /odm/etc/rootd.conf
echo __SM_RC_confhash=$?
echo __SM_BEGIN_meta
stat -c '%a %u %g %n' /odm/bin/rootd /odm/bin/permissive /odm/etc/init/rootd.rc /odm/etc/rootd.conf
echo __SM_RC_meta=$?
echo __SM_BEGIN_labels
ls -Z /odm/bin/rootd /odm/bin/permissive /odm/etc/init/rootd.rc /odm/etc/rootd.conf
echo __SM_RC_labels=$?
echo __SM_BEGIN_nat
iptables -t nat -S OUTPUT
echo __SM_RC_nat=$?
echo __SM_BEGIN_filter
iptables -S OUTPUT
echo __SM_RC_filter=$?
echo __SM_BEGIN_ip6
ip6tables -S OUTPUT
echo __SM_RC_ip6=$?
echo __SM_BEGIN_ipv6route
cat /proc/net/ipv6_route
echo __SM_RC_ipv6route=$?
echo __SM_BEGIN_conf
cat /odm/etc/rootd.conf
echo __SM_RC_conf=$?
echo __SM_DONE
EOF
)
    if ! root_cmd strict "$cmd"; then
        vbad "queue verification request failed: $ROOT_ERROR"
        return 0
    fi
    local q=$ROOT_OUTPUT
    if ! printf '%s\n' "$q" | grep -qx '__SM_DONE'; then
        vbad "queue verification output is incomplete (no end marker)"
        return 0
    fi
    local name
    for name in id cache recovery enforce rootdhash permhash rchash confhash meta labels nat filter ip6 ipv6route conf; do
        check_marker "$q" "$name"
    done

    local id_out cache recovery enforce rootdhash permhash rchash confhash meta labels nat filter ip6 ipv6route conf
    id_out=$(extract_block "$q" id | tr -d '[:space:]')
    cache=$(extract_block "$q" cache)
    recovery=$(extract_block "$q" recovery)
    enforce=$(extract_block "$q" enforce | tr -d '[:space:]')
    rootdhash=$(extract_block "$q" rootdhash | awk 'NR == 1 { print $1 }')
    permhash=$(extract_block "$q" permhash | awk 'NR == 1 { print $1 }')
    rchash=$(extract_block "$q" rchash | awk 'NR == 1 { print $1 }')
    confhash=$(extract_block "$q" confhash | awk 'NR == 1 { print $1 }')
    meta=$(extract_block "$q" meta)
    labels=$(extract_block "$q" labels)
    nat=$(extract_block "$q" nat)
    filter=$(extract_block "$q" filter)
    ip6=$(extract_block "$q" ip6)
    ipv6route=$(extract_block "$q" ipv6route)
    conf=$(extract_block "$q" conf)

    [ "$id_out" = 0 ] && vok "queue runs as uid 0" || vbad "queue uid is '$id_out'"
    [ "$enforce" = Permissive ] && vok "SELinux is Permissive" || vbad "SELinux is '$enforce'"

    local mode uid gid
    mode=$(printf '%s' "$cache" | awk '{print $1}'); mode=${mode#0}; [ -n "$mode" ] || mode=0
    uid=$(printf '%s' "$cache" | awk '{print $2}')
    gid=$(printf '%s' "$cache" | awk '{print $3}')
    [ "$mode" = 1770 ] && [ "$uid" = 1000 ] && [ "$gid" = 2001 ] \
        && vok "B2 /cache 1770:1000:2001" || vbad "B2 /cache is ${mode:-?}:${uid:-?}:${gid:-?}"
    mode=$(printf '%s' "$recovery" | awk '{print $1}'); mode=${mode#0}; [ -n "$mode" ] || mode=0
    uid=$(printf '%s' "$recovery" | awk '{print $2}')
    gid=$(printf '%s' "$recovery" | awk '{print $3}')
    [ "$mode" = 750 ] && [ "$uid" = 0 ] && [ "$gid" = 0 ] \
        && vok "B2 /cache/recovery 0750:0:0" || vbad "B2 /cache/recovery is ${mode:-?}:${uid:-?}:${gid:-?}"

    [ "$rootdhash" = "$EXPECT_ROOTD" ] && vok "rootd hash" || vbad "rootd hash is '${rootdhash:-missing}'"
    [ "$permhash" = "$EXPECT_PERM" ] && vok "permissive hash" || vbad "permissive hash is '${permhash:-missing}'"
    [ "$rchash" = "$EXPECT_RC" ] && vok "rootd.rc hash" || vbad "rootd.rc hash is '${rchash:-missing}'"
    if [ -n "$EXPECT_CONF" ]; then
        [ "$confhash" = "$EXPECT_CONF" ] && vok "rootd.conf hash" || vbad "rootd.conf hash is '${confhash:-missing}'"
    fi

    check_meta "$meta" "$labels" /odm/bin/rootd 755 u:object_r:adbd_exec:s0
    check_meta "$meta" "$labels" /odm/bin/permissive 755 u:object_r:system_file:s0
    check_meta "$meta" "$labels" /odm/etc/init/rootd.rc 644 u:object_r:vendor_configs_file:s0
    check_meta "$meta" "$labels" /odm/etc/rootd.conf 644 u:object_r:vendor_configs_file:s0

    local conf_file="$WORK/verified-rootd.conf"
    printf '%s\n' "$conf" > "$conf_file" || { vbad "cannot stage rootd.conf for parsing"; return 0; }
    if parse_rootd_conf "$conf_file"; then
        vok "rootd.conf keys and values are valid"
    else
        vbad "rootd.conf is missing, malformed, or has unknown/duplicate keys"
        return 0
    fi

    check_first_two "DNS DNAT" "$nat" dns
    check_first_two "DoT DROP" "$filter" dot
    if [ "$CFG_IPV6" = drop ]; then
        if [ "$(rc_marker "$q" ip6)" != 0 ] || [ -z "$ip6" ]; then
            vbad "IPv6 policy is drop but ip6tables did not produce usable rules"
        else
            check_first_two "IPv6 DoT DROP" "$ip6" ipv6
        fi
    else
        if [ "$(rc_marker "$q" ipv6route)" != 0 ]; then
            vbad "IPv6 policy is none but /proc/net/ipv6_route could not be read"
        elif printf '%s\n' "$ipv6route" | awk '
            {
                if ($1 == "00000000000000000000000000000000" && $2 == "00" && $10 != "" && $10 != "lo") found = 1
            }
            END { exit found ? 0 : 1 }'; then
            vbad "IPv6 policy is none but an IPv6 default route is present"
        else
            vok "IPv6 default route absent (policy none)"
        fi
    fi
    return 0
}

run_verify() {
    local wait_for_protected=$1
    FAIL=0
    CHECK_FAILS=0
    FIRST_FAIL=
    if [ "$DEADLINE_HIT" = 1 ] || ! deadline_ok; then
        vbad "operation deadline exceeded; no verification claim is possible"
        return 1
    fi
    local boot_id build fp kernel mount_state names rootd_n second_n status state sbid
    local boot_completed

    if ! dev_boot_completed; then
        vbad "sys.boot_completed is not 1"
        boot_completed=0
    else
        boot_completed=1
        vok "boot_completed=1"
    fi
    boot_id=$(read_device_boot_id 2>/dev/null) || boot_id=
    [ -n "$boot_id" ] && vok "boot_id is non-empty" || vbad "boot_id is empty or unreadable"

    build=$(dev_prop_quiet ro.build.display.id 2>/dev/null) || build=
    fp=$(dev_prop_quiet ro.build.fingerprint 2>/dev/null) || fp=
    kernel=$(adb_shell 15 'uname -r' >/dev/null 2>&1 && printf '%s' "$ADB_STDOUT") || kernel=
    [ "$build" = "$DISPLAY_ID" ] && vok "build is 0921" || vbad "build is '${build:-unreadable}'"
    [ "$fp" = "$FINGERPRINT" ] && vok "fingerprint matches" || vbad "fingerprint does not match"
    [ "$kernel" = "$KERNEL_RELEASE" ] && vok "kernel is $KERNEL_RELEASE" || vbad "kernel is '${kernel:-unreadable}'"

    mount_state=$(dev_mount_state_quiet 2>/dev/null) || mount_state=
    [ "$mount_state" = ro ] && vok "/odm is ro" || vbad "/odm is '${mount_state:-unreadable}', want ro"

    names=$(dev_ps_names_quiet 2>/dev/null) || names=
    if [ -z "$names" ]; then
        vbad "ps failed: process state is unknown"
        rootd_n=0
        second_n=0
    else
        rootd_n=$(count_name "$names" rootd)
        second_n=$(count_second_channels "$names")
        [ "$rootd_n" = 1 ] && vok "exactly one rootd" || vbad "rootd count=$rootd_n"
        [ "$second_n" = 0 ] && vok "no second root channel" || vbad "second root channel present"
    fi

    if [ "$wait_for_protected" = 1 ] && [ -n "$boot_id" ]; then
        local deadline=$((SECONDS + VERIFY_WAIT)) st sb
        while [ "$SECONDS" -lt "$deadline" ]; do
            st=$(read_optional_file /data/local/tmp/rootstatus 2>/dev/null) || st=
            state=$(state_get "$st" state)
            sb=$(state_get "$st" boot_id)
            [ "$state" = PROTECTED ] && [ "$sb" = "$boot_id" ] && break
            sleep 2
        done
    fi

    status=$(read_optional_file /data/local/tmp/rootstatus 2>/dev/null) || status=
    state=$(state_get "$status" state)
    sbid=$(state_get "$status" boot_id)
    [ "$state" = PROTECTED ] && vok "rootstatus state=PROTECTED" || vbad "rootstatus state is '${state:-absent}'"
    [ -n "$sbid" ] && [ "$sbid" = "$boot_id" ] && vok "rootstatus boot_id is current" \
        || vbad "rootstatus boot_id '${sbid:-absent}' != current '${boot_id:-absent}'"
    [ "$(state_get "$status" profile)" = 0921 ] && vok "profile=0921" || vbad "profile is not 0921"
    [ "$(state_get "$status" permissive)" = verified ] && vok "permissive=verified" || vbad "permissive is not verified"
    [ "$(state_get "$status" b2)" = ok ] && vok "b2=ok" || vbad "b2 is not ok"
    [ "$(state_get "$status" dns)" = ok ] && vok "dns=ok" || vbad "dns is not ok"
    [ "$(state_get "$status" dot)" = ok ] && vok "dot=ok" || vbad "dot is not ok"
    apply_expected_from_state

    if [ -n "$boot_id" ] && [ "$rootd_n" = 1 ]; then
        verify_queue_checks
    else
        vbad "queue verification skipped: no reliable boot_id/rootd"
    fi
    if [ "$DEADLINE_HIT" = 1 ] || ! deadline_ok; then
        vbad "operation deadline exceeded; verification is not trustworthy"
    fi
    return "$FAIL"
}

do_verify() {
    local keep_deadline=${1:-0}
    [ "$keep_deadline" = 1 ] || reset_deadline
    DEADLINE_HIT=0
    VERIFY_QUIET=0
    FAIL=0
    if ! adb_init; then
        bad "$ADB_ERROR"
        say "VERIFY FAIL"
        return 1
    fi
    STATE_TEXT=$(read_optional_file "$STATE_FILE" 2>/dev/null) || STATE_TEXT=
    run_verify 1
    if [ "$FAIL" = 0 ]; then
        say "VERIFY PASS"
        return 0
    fi
    say "VERIFY FAIL"
    return 1
}

# --------------------------------------------------------------- status ---

do_status() {
    reset_deadline
    DEADLINE_HIT=0
    if ! adb_init; then
        say "STATE: NO-CONNECTION"
        say "REASON: $ADB_ERROR"
        return 1
    fi
    local boot build fp kernel enforce uptime mount_state names rootd_n second_n
    local status state sbid reason backup opstate phase
    local inflight_text= inflight_ok=0 inflight_present=0 inflight_desc=absent
    local iop iboot icreated istage iage
    local boot_read=0
    if boot=$(dev_prop_quiet sys.boot_completed 2>/dev/null); then
        boot_read=1
    else
        boot=
    fi
    build=$(dev_prop_quiet ro.build.display.id 2>/dev/null) || build=
    fp=$(dev_prop_quiet ro.build.fingerprint 2>/dev/null) || fp=
    kernel=$(adb_shell 15 'uname -r' >/dev/null 2>&1 && printf '%s' "$ADB_STDOUT") || kernel=
    enforce=$(adb_shell 15 'getenforce' >/dev/null 2>&1 && printf '%s' "$ADB_STDOUT") || enforce=
    uptime=$(adb_shell 15 'cut -d. -f1 /proc/uptime' >/dev/null 2>&1 && printf '%s' "$ADB_STDOUT") || uptime=
    mount_state=$(dev_mount_state_quiet 2>/dev/null) || mount_state=
    names=$(dev_ps_names_quiet 2>/dev/null) || names=
    boot_id=$(read_device_boot_id 2>/dev/null) || boot_id=
    status=$(read_optional_file /data/local/tmp/rootstatus 2>/dev/null) || status=
    opstate=$(read_optional_file "$STATE_FILE" 2>/dev/null) || opstate=
    if read_inflight_strict; then
        inflight_ok=1
        inflight_text=$INFLIGHT_TEXT
    else
        inflight_ok=0
        inflight_text=
    fi
    if [ "$inflight_ok" = 0 ]; then
        inflight_desc="unreadable (no claim about an interrupted bootstrap is possible)"
    elif [ -n "$inflight_text" ]; then
        inflight_present=1
        iop=$(state_get "$inflight_text" operation_id)
        iboot=$(state_get "$inflight_text" boot_id)
        icreated=$(state_get "$inflight_text" created)
        istage=$(state_get "$inflight_text" stage)
        iage=$(iso_age "$icreated")
        [ "$iage" = unknown ] || iage="${iage}s"
        inflight_desc="present (operation_id=${iop:-unknown} boot_id=${iboot:-unknown} stage=${istage:-unknown} created=${icreated:-unknown} age=$iage"
        if [ -n "$boot_id" ] && [ "$iboot" = "$boot_id" ]; then
            inflight_desc="$inflight_desc, this boot)"
        elif [ -n "$iboot" ]; then
            inflight_desc="$inflight_desc, previous boot)"
        else
            inflight_desc="$inflight_desc, boot unknown)"
        fi
    fi
    state=$(state_get "$status" state)
    sbid=$(state_get "$status" boot_id)
    reason=$(state_get "$status" reason)
    backup=$(state_get "$opstate" backup)
    phase=$(state_get "$opstate" phase)

    say "ADB:        ok (${SERIAL:-single device})"
    say "boot:       ${boot:-unknown}"
    say "build:      ${build:-unknown}"
    say "fingerprint: ${fp:-unknown}"
    say "kernel:     ${kernel:-unknown}"
    say "enforce:    ${enforce:-unknown}"
    say "uptime:     ${uptime:-unknown}s"
    say "boot_id:    ${boot_id:-unknown}"
    say "rootstatus: ${state:-absent}"
    [ -n "$reason" ] && say "reason:     class=$(reason_class "$reason")"
    say "status id:  ${sbid:-absent}"
    if [ -z "$names" ]; then
        say "processes:  unknown (ps failed)"
        rootd_n=unknown
        second_n=unknown
    else
        rootd_n=$(count_name "$names" rootd)
        second_n=$(count_second_channels "$names")
        say "rootd:      $rootd_n"
        say "second:     $second_n"
    fi
    say "odm:        ${mount_state:-unknown}"
    say "backup:     ${backup:-none}"
    say "op phase:   ${phase:-none}"
    say "inflight:   $inflight_desc"

    [ -n "$build" ] || {
        say "STATE: UNKNOWN"
        say "REASON: ro.build.display.id is empty or unreadable"
        return 1
    }
    [ "$build" = "$DISPLAY_ID" ] || {
        say "STATE: UNSUPPORTED-BUILD"
        say "ACTION: stop; this archive supports only 0921"
        return 1
    }
    [ -n "$fp" ] && [ "$fp" = "$FINGERPRINT" ] || {
        say "STATE: UNSUPPORTED-BUILD"
        say "REASON: fingerprint is not the 0921 fingerprint"
        return 1
    }
    [ -n "$kernel" ] && [ "$kernel" = "$KERNEL_RELEASE" ] || {
        say "STATE: UNSUPPORTED-BUILD"
        say "REASON: kernel is ${kernel:-unreadable}, want $KERNEL_RELEASE"
        return 1
    }
    [ "$boot_read" = 1 ] || {
        say "STATE: UNKNOWN"
        say "REASON: getprop sys.boot_completed failed; check not performed"
        return 1
    }
    if [ "$DEADLINE_HIT" = 1 ] || ! deadline_ok; then
        say "STATE: UNKNOWN"
        say "REASON: operation timed out; no state claim is trusted"
        return 1
    fi
    [ "$boot" = 1 ] || {
        say "STATE: BOOTING"
        say "ACTION: wait for sys.boot_completed=1"
        return 0
    }
    [ -n "$boot_id" ] || {
        say "STATE: UNKNOWN"
        say "REASON: boot_id is empty or unreadable; no health claim is possible"
        return 1
    }
    [ -n "$enforce" ] || {
        say "STATE: UNKNOWN"
        say "REASON: getenforce failed; no health claim is possible"
        return 1
    }
    [ -n "$mount_state" ] || {
        say "STATE: UNKNOWN"
        say "REASON: mount check failed; no health claim is possible"
        return 1
    }
    [ "$names" != "" ] || {
        say "STATE: UNKNOWN"
        say "REASON: ps failed; no health claim is possible"
        return 1
    }
    [ "$second_n" = 0 ] || {
        say "STATE: SECOND-CHANNEL"
        say "ACTION: stop the root-bridge/esc36449/cve process before installing or rolling back"
        return 1
    }
    [ "$mount_state" = ro ] || {
        say "STATE: ODM-RW"
        say "ACTION: an install/rollback was interrupted; return /odm to ro before continuing"
        return 1
    }
    if [ -z "$state" ]; then
        if [ "$rootd_n" = 0 ]; then
            say "STATE: NOT-INSTALLED"
            say "ACTION: ./station-max.sh install ..."
        else
            say "STATE: NO-STATUS"
            say "ACTION: rootd is running but did not publish rootstatus; inspect the queue"
        fi
        return 1
    fi
    [ -n "$sbid" ] || {
        say "STATE: NO-STATUS"
        say "REASON: rootstatus has no boot_id; no health claim is possible"
        return 1
    }
    [ "$sbid" = "$boot_id" ] || {
        say "STATE: STALE-STATUS"
        say "ACTION: wait for the current boot to publish a new rootstatus"
        return 1
    }

    local status_age
    adb_shell 15 "stat -c %Y /data/local/tmp/rootstatus" >/dev/null 2>&1 || {
        say "STATE: UNKNOWN"
        say "REASON: cannot read rootstatus mtime; bootstrap age is unknown"
        return 1
    }
    local mtime now
    mtime=$(printf '%s' "$ADB_STDOUT" | tr -d '[:space:]')
    now=$(date +%s)
    case $mtime in
        ''|*[!0-9]*) status_age=unknown ;;
        *) [ "$mtime" -le "$now" ] && status_age=$((now - mtime)) || status_age=0 ;;
    esac

    case $state in
        PROTECTED)
            if [ "$enforce" != Permissive ]; then
                say "STATE: DEGRADED"
                say "REASON: rootstatus says PROTECTED but getenforce=$enforce; no HEALTHY"
                return 1
            fi
            if [ "$rootd_n" != 1 ]; then
                say "STATE: NO-ROOTD"
                say "ACTION: rootstatus says PROTECTED but rootd count=$rootd_n"
                return 1
            fi
            VERIFY_QUIET=1
            FAIL=0
            CHECK_FAILS=0
            FIRST_FAIL=
            STATE_TEXT=$opstate
            run_verify 0
            VERIFY_QUIET=0
            if [ "$DEADLINE_HIT" = 1 ] || ! deadline_ok; then
                say "STATE: UNKNOWN"
                say "REASON: operation timed out; HEALTHY is not claimed"
                return 1
            fi
            if [ "$inflight_ok" = 0 ]; then
                say "STATE: UNKNOWN"
                say "REASON: the in-flight marker could not be read; an interrupted bootstrap cannot be ruled out"
                return 1
            fi
            if [ "$inflight_present" = 1 ]; then
                say "STATE: IN-FLIGHT"
                say "REASON: the checks pass, but an interrupted bootstrap left a marker; its process may still be running as sh"
                say "ACTION: if no install is running, remove the marker explicitly: ./station-max.sh cmd 'rm -f $INFLIGHT_FILE'"
                return 1
            fi
            if [ "$FAIL" = 0 ]; then
                say "STATE: HEALTHY"
                say "ACTION: ./station-max.sh verify"
                return 0
            fi
            say "STATE: DEGRADED"
            say "REASON: ${FIRST_FAIL:-current checks failed}; no HEALTHY"
            return 1
            ;;
        ROOT_ONLY)
            if [ "$status_age" != unknown ] && [ "$status_age" -le 180 ]; then
                say "STATE: BOOTSTRAPPING"
                say "ACTION: wait 20-40 seconds, then status again"
            else
                say "STATE: STUCK-ROOT-ONLY"
                say "ACTION: check rootout/rootd log; rollback if it does not recover"
                return 1
            fi
            return 0
            ;;
        DEGRADED)
            say "STATE: DEGRADED"
            say "ACTION: read reason above; verify or rollback"
            return 1
            ;;
        UNSUPPORTED)
            say "STATE: UNSUPPORTED"
            say "ACTION: build/config mismatch; rollback or fix config"
            return 1
            ;;
        LEGACY_SELECTED)
            if [ "$DEADLINE_HIT" = 1 ] || ! deadline_ok; then
                say "STATE: UNKNOWN"
                say "REASON: operation timed out; no state claim is trusted"
                return 1
            fi
            say "STATE: LEGACY-SELECTED"
            say "ACTION: legacy owner is intentional; not a failure"
            return 0
            ;;
        *)
            say "STATE: UNKNOWN ($state)"
            return 1
            ;;
    esac
}

# -------------------------------------------------------------- reboot ----

wait_new_boot() {
    local old=$1 deadline=$((SECONDS + REBOOT_TIMEOUT)) id
    while [ "$SECONDS" -lt "$deadline" ]; do
        if adb_get_state; then
            id=$(read_device_boot_id 2>/dev/null) || id=
            if [ -n "$id" ] && [ "$id" != "$old" ]; then
                NEW_BOOT_ID=$id
                return 0
            fi
        fi
        sleep 2
    done
    return 1
}

wait_boot_completed() {
    local want=$1 deadline=$((SECONDS + BOOT_TIMEOUT)) id prop
    while [ "$SECONDS" -lt "$deadline" ]; do
        prop=$(dev_prop_quiet sys.boot_completed 2>/dev/null) || prop=
        id=$(read_device_boot_id 2>/dev/null) || id=
        if [ "$prop" = 1 ] && [ "$id" = "$want" ]; then
            return 0
        fi
        sleep 2
    done
    return 1
}

wait_rootstatus_protected() {
    local want=$1 timeout=$2
    local deadline=$((SECONDS + timeout)) st state sbid
    while [ "$SECONDS" -lt "$deadline" ]; do
        st=$(read_optional_file /data/local/tmp/rootstatus 2>/dev/null) || st=
        state=$(state_get "$st" state)
        sbid=$(state_get "$st" boot_id)
        if [ "$state" = PROTECTED ] && [ "$sbid" = "$want" ]; then
            return 0
        fi
        sleep 2
    done
    return 1
}

reboot_and_wait() {
    local old
    old=$(read_device_boot_id) || die "cannot read boot_id before reboot"
    [ -n "$old" ] || die "empty boot_id before reboot"
    adb_call 20 reboot || die "adb reboot failed: ${ADB_STDERR:-no error text}"
    wait_new_boot "$old" || die "reboot did not produce a new boot_id (no 'rebooted' claim)"
    say "rebooted: boot_id=$NEW_BOOT_ID"
    wait_boot_completed "$NEW_BOOT_ID" || die "new boot $NEW_BOOT_ID did not report sys.boot_completed=1"
    return 0
}

# -------------------------------------------------------------- install ---

do_install() {
    reset_deadline
    bootstrap_config_check
    local router_ip="" dns_ip="" ipv6=none reboot=1 yes=0 ans="" raw
    while [ $# -gt 0 ]; do
        case $1 in
            --router-ip)
                [ $# -ge 2 ] || die "--router-ip requires a value"
                raw=$2
                is_ipv4 "$raw" || die "bad --router-ip (one line, four decimal octets 0-255, no leading zeroes)"
                router_ip=$(canon_ipv4 "$raw")
                shift 2
                ;;
            --dns-ip)
                [ $# -ge 2 ] || die "--dns-ip requires a value"
                raw=$2
                is_ipv4 "$raw" || die "bad --dns-ip (one line, four decimal octets 0-255, no leading zeroes)"
                dns_ip=$(canon_ipv4 "$raw")
                shift 2
                ;;
            --ipv6)
                [ $# -ge 2 ] || die "--ipv6 requires a value"
                ipv6=$2
                case $ipv6 in none|drop) ;; *) die "--ipv6 must be none or drop" ;; esac
                shift 2
                ;;
            --no-reboot) reboot=0; shift ;;
            --yes|-y) yes=1; shift ;;
            -h|--help) usage; return 0 ;;
            *) die "unknown install argument: $1" ;;
        esac
    done

    if [ -n "${BASE:-}" ]; then
        is_safe_tmp_path "$BASE" || die "BASE must be a safe staging path below /data/local/tmp"
        [ "$BASE" != /data/local/tmp ] || die "BASE cannot be the /data/local/tmp root"
    fi

    adb_init || die "$ADB_ERROR"
    if [ -z "$router_ip" ] && [ -t 0 ]; then
        printf 'router IP: '
        read -r router_ip || router_ip=""
        is_ipv4 "$router_ip" || die "bad router IP"
    fi
    if [ -z "$dns_ip" ] && [ -t 0 ]; then
        printf 'DNS IP to redirect: '
        read -r dns_ip || dns_ip=""
        is_ipv4 "$dns_ip" || die "bad DNS IP"
    fi
    [ -n "$router_ip" ] || die "--router-ip is required"
    [ -n "$dns_ip" ] || die "--dns-ip is required"

    preflight_identity
    check_local_archive
    preflight_device

    OP_ID=${ST_OPERATION_ID:-}
    [ -n "$OP_ID" ] || OP_ID=$(new_operation_id)
    case $OP_ID in *[!A-Za-z0-9._-]*|'') die "invalid operation id" ;; esac
    prepare_staging

    say "router=$router_ip dns=$dns_ip ipv6=$ipv6"
    if [ "$yes" = 0 ]; then
        printf 'Install hybrid and reboot? [y/N] '
        read -r ans || ans=""
        case $ans in y|Y|yes|YES) ;; *) say "cancelled"; return 0 ;; esac
    fi

    deliver_staging "$router_ip" "$dns_ip" "$ipv6"

    if [ "$ROOTD_MODE" = queue ]; then
        local cmd="BASE=$(shq "$BASE") OPERATION_ID=$(shq "$OP_ID") STAGING_NONCE=$(shq "$STAGING_NONCE") $(shq "$BASE/hybrid/install-device.sh")"
        say "starting installer through the existing rootd queue"
        if root_cmd strict "$cmd"; then
            printf '%s\n' "$ROOT_OUTPUT"
        else
            printf '%s\n' "$ROOT_OUTPUT"
            local st phase
            st=$(read_optional_file "$STATE_FILE" 2>/dev/null) || st=
            phase=$(state_get "$st" phase)
            die "install through rootd queue failed: $ROOT_ERROR (phase=${phase:-unknown}). No retry was attempted."
        fi
    else
        run_bootstrap_install
    fi

    local st final_phase backup
    st=$(read_optional_file "$STATE_FILE" 2>/dev/null) || st=
    final_phase=$(state_get "$st" phase)
    backup=$(state_get "$st" backup)
    [ "$final_phase" = complete ] || die "installation did not reach phase=complete (phase=${final_phase:-unknown})"
    if [ "$BOOTSTRAP_USED" -gt 0 ]; then
        say "installed; backup=$backup (bootstrap attempts: $BOOTSTRAP_USED/$BOOTSTRAP_ATTEMPTS)"
    else
        say "installed; backup=$backup"
    fi

    if [ "$reboot" = 0 ]; then
        say "next: adb reboot; ./station-max.sh verify"
        return 0
    fi
    reboot_and_wait
    wait_rootstatus_protected "$NEW_BOOT_ID" "$PROTECTED_TIMEOUT" || die "new boot did not reach PROTECTED"
    STATE_TEXT=$(read_optional_file "$STATE_FILE" 2>/dev/null) || STATE_TEXT=
    if ! do_verify 1; then
        die "installation completed but VERIFY FAIL; do not treat the station as healthy"
    fi
    say "INSTALL PASS"
    return 0
}

# ------------------------------------------------------------- rollback ---

check_stock_set() {
    local fail=0 path
    for path in /odm/bin/rootd /odm/bin/permissive /odm/etc/init/rootd.rc /odm/etc/rootd.conf; do
        adb_shell 15 "[ ! -e $(shq "$path") ] && [ ! -L $(shq "$path") ]"
        if [ $? -ne 0 ]; then
            bad "stock rollback: $path still exists"
            fail=1
        else
            ok "stock rollback: $path absent"
        fi
    done
    local ms names
    ms=$(dev_mount_state_quiet 2>/dev/null) || ms=
    [ "$ms" = ro ] && ok "/odm is ro" || { bad "/odm is '${ms:-unreadable}', want ro"; fail=1; }
    names=$(dev_ps_names_quiet 2>/dev/null) || names=
    if [ -z "$names" ]; then
        bad "ps failed after stock rollback"
        fail=1
    else
        [ "$(count_name "$names" rootd)" = 0 ] && ok "no rootd after stock rollback" || { bad "rootd still running after stock rollback"; fail=1; }
        [ "$(count_second_channels "$names")" = 0 ] \
            && ok "no second channel" || { bad "second channel present"; fail=1; }
    fi
    return "$fail"
}

check_restored_partial() {
    local st phase fail=0 path expect kind
    st=$(read_optional_file "$STATE_FILE" 2>/dev/null) || st=
    for spec in \
        "/odm/bin/rootd:restored_rootd" \
        "/odm/bin/permissive:restored_permissive" \
        "/odm/etc/init/rootd.rc:restored_rc" \
        "/odm/etc/rootd.conf:restored_conf"; do
        path=${spec%%:*}
        kind=${spec#*:}
        expect=$(state_get "$st" "$kind")
        if [ -n "$expect" ]; then
            adb_shell 15 "if [ -f $(shq "$path") ] && [ ! -L $(shq "$path") ]; then sha256sum $(shq "$path"); else echo ABSENT; fi" || {
                bad "cannot inspect $path"
                fail=1
                continue
            }
            if [ "$ADB_STDOUT" = ABSENT ]; then
                bad "restored file missing: $path"
                fail=1
            else
                local got
                got=$(printf '%s' "$ADB_STDOUT" | awk '{print $1}')
                [ "$got" = "$expect" ] && ok "restored $path" || { bad "restored hash mismatch: $path"; fail=1; }
            fi
        else
            adb_shell 15 "[ ! -e $(shq "$path") ] && [ ! -L $(shq "$path") ]"
            [ $? -eq 0 ] && ok "restored absent: $path" || { bad "path should be absent: $path"; fail=1; }
        fi
    done
    local ms
    ms=$(dev_mount_state_quiet 2>/dev/null) || ms=
    [ "$ms" = ro ] && ok "/odm is ro" || { bad "/odm is '${ms:-unreadable}', want ro"; fail=1; }
    return "$fail"
}

do_rollback() {
    reset_deadline
    local bk="" reboot=1 yes=0 force_bootstrap=0 allow_unmarked=0 replace_config=0
    while [ $# -gt 0 ]; do
        case $1 in
            --no-reboot) reboot=0; shift ;;
            --yes|-y) yes=1; shift ;;
            --bootstrap) force_bootstrap=1; shift ;;
            --allow-unmarked) allow_unmarked=1; shift ;;
            --replace-config) replace_config=1; shift ;;
            -h|--help) usage; return 0 ;;
            -*) die "unknown rollback argument: $1" ;;
            *) [ -z "$bk" ] || die "more than one backup path"; bk=$1; shift ;;
        esac
    done
    if [ -n "${BASE:-}" ]; then
        is_safe_tmp_path "$BASE" || die "BASE must be a safe staging path below /data/local/tmp"
        [ "$BASE" != /data/local/tmp ] || die "BASE cannot be the /data/local/tmp root"
    fi
    adb_init || die "$ADB_ERROR"
    dev_boot_completed || die "sys.boot_completed is not 1"
    dev_build_gate || die "preflight check not performed: ${ADB_ERROR:-build gate failed}"
    DEV_BUILD=$DEV_BUILD_VALUE
    BOOT_ID=$(read_device_boot_id) || die "preflight check not performed: boot_id unreadable"
    check_local_archive

    if adb_shell 15 "[ -e $(shq "$STATE_FILE") ] || [ -L $(shq "$STATE_FILE") ]"; then
        adb_shell 15 "[ -f $(shq "$STATE_FILE") ] && [ ! -L $(shq "$STATE_FILE") ]" \
            || die "operation state is not a regular file; refusing to override it"
        STATE_TEXT=$(read_optional_file "$STATE_FILE" 2>/dev/null) || STATE_TEXT=
        host_state_probe "$STATE_TEXT" || die "operation state is corrupt or invalid; refusing to override it"
    else
        STATE_STATUS=missing
        STATE_TEXT=
    fi
    local op phase state_bk
    op=$(state_get "$STATE_TEXT" operation)
    phase=$(state_get "$STATE_TEXT" phase)
    state_bk=$(state_get "$STATE_TEXT" backup)

    if [ -z "$bk" ]; then
        [ -n "$state_bk" ] || die "no backup path in state; pass an explicit backup directory"
        case ${op:-}:${phase:-} in
            install:install_started|install:backup_started)
                die "install has not reached a completed backup; refusing automatic rollback"
                ;;
        esac
        is_safe_tmp_path "$state_bk" || die "state contains an unsafe backup path"
        bk=$state_bk
    else
        is_safe_tmp_path "$bk" || die "unsafe backup path: $bk"
        case $bk in
            */rootd-backup-*) ;;
            *) [ "$allow_unmarked" = 1 ] || warn "explicit backup does not match rootd-backup-*; it will be fully validated under uid 0" ;;
        esac
    fi
    if [ -n "$state_bk" ] && [ -n "$bk" ] && [ "$state_bk" != "$bk" ]; then
        case ${op:-}:${phase:-} in
            rollback:rollback_restoring|rollback:rollback_failed)
                die "rollback retry must use the backup recorded in state: $state_bk"
                ;;
        esac
    fi
    say "backup: $bk"
    if [ "$yes" = 0 ]; then
        printf 'Restore this backup and reboot? [y/N] '
        read -r ans || ans=""
        case $ans in y|Y|yes|YES) ;; *) say "cancelled"; return 0 ;; esac
    fi

    local names rootd_n second_n
    names=$(dev_ps_names_quiet 2>/dev/null) || die "preflight check not performed: ps failed"
    rootd_n=$(count_name "$names" rootd)
    second_n=$(count_second_channels "$names")
    [ "$second_n" = 0 ] || die "a second root/bootstrap channel is present; stop it before rollback"
    [ "$rootd_n" -le 1 ] || die "more than one rootd process"

    OP_ID=${ST_OPERATION_ID:-}
    [ -n "$OP_ID" ] || OP_ID=$(new_operation_id)
    prepare_staging
    deliver_tree
    say "staging prepared for rollback"

    local mode=queue
    if [ "$force_bootstrap" = 1 ]; then
        [ "$rootd_n" = 0 ] || die "--bootstrap requires that rootd is not running; stop it first, do not start a second channel"
        mode=bootstrap
    elif [ "$rootd_n" = 1 ]; then
        local st sbid
        st=$(read_optional_file /data/local/tmp/rootstatus 2>/dev/null) || st=
        sbid=$(state_get "$st" boot_id)
        if [ "$(state_get "$st" state)" != PROTECTED ] || [ "$sbid" != "$BOOT_ID" ]; then
            die "rootd is present but its state is uncertain; the queue is not safe to use. Reboot, or stop rootd and use --bootstrap explicitly."
        fi
        if ! root_cmd strict 'id -u'; then
            die "rootd is present but the queue did not answer: $ROOT_ERROR. Do not start a second bootstrap."
        fi
        [ "$ROOT_OUTPUT" = 0 ] || die "rootd queue is not uid 0"
        mode=queue
    else
        mode=bootstrap
    fi

    if [ "$mode" = queue ]; then
        local cmd="BASE=$(shq "$BASE") OPERATION_ID=$(shq "$OP_ID") STAGING_NONCE=$(shq "$STAGING_NONCE") $(shq "$BASE/hybrid/rollback-device.sh")"
        [ "$allow_unmarked" = 1 ] && cmd="$cmd --allow-unmarked"
        [ "$replace_config" = 1 ] && cmd="$cmd --replace-config"
        cmd="$cmd $(shq "$bk")"
        say "starting rollback through the existing rootd queue"
        if root_cmd strict "$cmd"; then
            printf '%s\n' "$ROOT_OUTPUT"
        else
            printf '%s\n' "$ROOT_OUTPUT"
            die "rollback through rootd queue failed: $ROOT_ERROR"
        fi
    else
        say "rootd is not available; using the bootstrap recovery path"
        write_inflight rollback
        if adb_shell 300 "cd $(shq "$BASE") && BASE=$(shq "$BASE") BACKUP=$(shq "$bk") ALLOW_UNMARKED=$allow_unmarked ALLOW_REPLACE_CONFIG=$replace_config OPERATION_ID=$(shq "$OP_ID") STAGING_NONCE=$(shq "$STAGING_NONCE") INFLIGHT_OWNED=1 ./root-bridge esc-shell < hybrid/rollback-run.sh"; then
            :
        else
            local brc=$?
            printf '%s\n' "$ADB_STDOUT"
            [ -n "$ADB_STDERR" ] && printf '%s\n' "$ADB_STDERR" >&2
            if ! adb_get_state; then
                die "ADB was lost during bootstrap rollback (rc=$brc); the in-flight marker remains. Reconnect and run status before any retry."
            fi
            die "bootstrap rollback failed (rc=$brc): ${ADB_STDERR:-no error text}"
        fi
        printf '%s\n' "$ADB_STDOUT"
        adb_shell 15 "rm -f $(shq "$INFLIGHT_FILE")" >/dev/null 2>&1 || warn "could not clear in-flight marker after rollback"
    fi

    local st phase expect
    st=$(read_optional_file "$STATE_FILE" 2>/dev/null) || st=
    phase=$(state_get "$st" phase)
    expect=$(state_get "$st" expect)
    [ "$phase" = rollback_complete ] || die "rollback did not reach phase=rollback_complete (phase=${phase:-unknown})"
    say "rollback restored; expect=$expect"

    if [ "$reboot" = 0 ]; then
        say "next: adb reboot; verify the expected state ($expect)"
        return 0
    fi
    reboot_and_wait
    case $expect in
        hybrid)
            wait_rootstatus_protected "$NEW_BOOT_ID" "$PROTECTED_TIMEOUT" || die "restored hybrid did not reach PROTECTED"
            STATE_TEXT=$(read_optional_file "$STATE_FILE" 2>/dev/null) || STATE_TEXT=
            if ! do_verify 1; then
                die "rollback restored files but VERIFY FAIL"
            fi
            ;;
        stock)
            check_stock_set || die "stock rollback state did not verify"
            ;;
        partial)
            check_restored_partial || die "partial rollback state did not verify"
            ;;
        *)
            die "rollback state has unknown expect='${expect:-}'"
            ;;
    esac
    say "ROLLBACK PASS"
    return 0
}

# ----------------------------------------------------------------- diag ---

do_diag() {
    reset_deadline
    DEADLINE_HIT=0
    adb_init || die "$ADB_ERROR"
    local dir out
    dir=$(mktemp -d "${TMPDIR:-/tmp}/station-max-diag.XXXXXX") || die "cannot create diagnostic directory"
    chmod 0700 "$dir" || die "cannot protect diagnostic directory"
    umask 077

    {
        printf 'display_id=%s\n' "$(dev_prop_quiet ro.build.display.id 2>/dev/null || echo unavailable)"
        printf 'fingerprint=%s\n' "$(dev_prop_quiet ro.build.fingerprint 2>/dev/null || echo unavailable)"
        printf 'android_release=%s\n' "$(dev_prop_quiet ro.build.version.release 2>/dev/null || echo unavailable)"
        printf 'kernel_release=%s\n' "$(adb_shell 15 'uname -r' >/dev/null 2>&1 && printf '%s' "$ADB_STDOUT" || echo unavailable)"
    } > "$dir/build.txt"

    {
        printf 'boot_completed=%s\n' "$(dev_prop_quiet sys.boot_completed 2>/dev/null || echo unavailable)"
        printf 'boot_id=%s\n' "$(read_device_boot_id 2>/dev/null || echo unavailable)"
        local st
        st=$(read_optional_file /data/local/tmp/rootstatus 2>/dev/null) || st=
        for key in state boot_id profile permissive b2 dns dot ipv6; do
            printf '%s=%s\n' "$key" "$(state_get "$st" "$key")"
        done
        local reason
        reason=$(state_get "$st" reason)
        printf 'reason_class=%s\n' "$(reason_class "$reason")"
        if [ -n "$reason" ]; then printf 'reason_present=1\n'; else printf 'reason_present=0\n'; fi
    } > "$dir/boot.txt"

    printf 'selinux=%s\n' "$(adb_shell 15 'getenforce' >/dev/null 2>&1 && printf '%s' "$ADB_STDOUT" || echo unavailable)" > "$dir/selinux.txt"
    printf 'odm_mount=%s\n' "$(dev_mount_state_quiet 2>/dev/null || echo unavailable)" > "$dir/mount.txt"

    {
        local names
        names=$(dev_ps_names_quiet 2>/dev/null) || names=
        if [ -z "$names" ]; then
            printf 'ps=unavailable\n'
        else
            printf 'rootd=%s\n' "$(count_name "$names" rootd)"
            printf 'root_bridge=%s\n' "$(count_name "$names" root-bridge)"
            printf 'esc_bridge=%s\n' "$(count_name "$names" esc-bridge)"
            printf 'esc36449=%s\n' "$(count_name "$names" esc36449)"
            printf 'cve36449=%s\n' "$(count_name "$names" cve36449)"
            printf 'second_channel_total=%s\n' "$(count_second_channels "$names")"
        fi
    } > "$dir/processes.txt"

    {
        local path h meta label
        for path in /odm/bin/rootd /odm/bin/permissive /odm/etc/init/rootd.rc /odm/etc/rootd.conf; do
            h=$(dev_sha256_quiet "$path" 2>/dev/null) || h=unavailable
            meta=$(adb_shell 15 "stat -c '%s %u %g %a' $(shq "$path")" >/dev/null 2>&1 && printf '%s' "$ADB_STDOUT" || echo unavailable)
            label=$(adb_shell 15 "ls -Z $(shq "$path")" >/dev/null 2>&1 && printf '%s\n' "$ADB_STDOUT" | awk '{for(i=1;i<=NF;i++) if ($i ~ /^u:object_r:/) {print $i; exit}}' || echo unavailable)
            printf '%s sha256=%s size_uid_gid_mode=%s label=%s\n' "$path" "$h" "$meta" "$label"
        done
    } > "$dir/files.txt"

    {
        local conf
        conf=$(read_optional_file /odm/etc/rootd.conf 2>/dev/null) || conf=
        if [ -z "$conf" ]; then
            printf 'conf=absent\n'
        else
            local tmp="$WORK/diag-rootd.conf"
            printf '%s\n' "$conf" > "$tmp"
            if parse_rootd_conf "$tmp"; then
                printf 'conf=valid\n'
                printf 'router_ip=present_valid\n'
                printf 'dns_ip=present_valid\n'
                printf 'ipv6=%s\n' "$CFG_IPV6"
            else
                printf 'conf=invalid\n'
                printf 'router_ip=invalid_or_unknown\n'
                printf 'dns_ip=invalid_or_unknown\n'
                printf 'ipv6=invalid_or_unknown\n'
            fi
        fi
    } > "$dir/config.txt"

    {
        VERIFY_QUIET=1
        FAIL=0
        CHECK_FAILS=0
        FIRST_FAIL=
        STATE_TEXT=$(read_optional_file "$STATE_FILE" 2>/dev/null) || STATE_TEXT=
        run_verify 0
        if [ "$FAIL" = 0 ] && [ "$DEADLINE_HIT" = 0 ] && deadline_ok; then
            printf 'verify=pass\n'
        else
            printf 'verify=fail\n'
            printf 'first_failure_class=%s\n' "$(failure_class "$FIRST_FAIL")"
            printf 'deadline_hit=%s\n' "$DEADLINE_HIT"
        fi
        VERIFY_QUIET=0
    } > "$dir/checks.txt" 2>/dev/null

    chmod 0600 "$dir"/* 2>/dev/null || true
    local final tmp
    final="$PWD/station-max-diag-$(date +%Y%m%d-%H%M%S).zip"
    if command -v zip >/dev/null 2>&1; then
        tmp=$(mktemp "$PWD/.station-max-diag.XXXXXX") || die "cannot create temporary diagnostic ZIP"
        rm -f "$tmp" || die "cannot prepare temporary diagnostic path"
        (cd "$dir" && zip -q -r "$tmp" .) || { rm -f "$tmp"; die "zip failed"; }
        chmod 0600 "$tmp" 2>/dev/null || true
        if [ -e "$final" ]; then final="$final-$$"; fi
        mv "$tmp" "$final" || { rm -f "$tmp"; die "cannot publish diagnostic ZIP"; }
        rm -rf "$dir"
        say "diag: $final"
    else
        say "zip not found; diagnostics left in $dir"
        say "directory permissions: 0700; files: 0600"
    fi
}

# ----------------------------------------------------------------- menu ---

menu() {
    while :; do
        cat <<EOF

Station Max hybrid $VERSION
  1) status
  2) install
  3) verify
  4) rollback
  5) diag
  0) exit
EOF
        printf '> '
        read -r choice || exit 0
        case "$choice" in
            1) do_status ;;
            2) do_install ;;
            3) do_verify ;;
            4) do_rollback ;;
            5) do_diag ;;
            0|q|Q) exit 0 ;;
            *) say "unknown choice" ;;
        esac
    done
}

case "${1:-}" in
    status)  shift; do_status "$@" ;;
    install) shift; do_install "$@" ;;
    verify)  shift; do_verify 0 ;;
    rollback) shift; do_rollback "$@" ;;
    diag)    shift; do_diag "$@" ;;
    cmd)
        shift
        [ $# -gt 0 ] || die "usage: $0 cmd 'command'"
        reset_deadline
        adb_init || die "$ADB_ERROR"
        root_cmd normal "$*"
        rc=$?
        [ -n "$ROOT_OUTPUT" ] && printf '%s\n' "$ROOT_OUTPUT"
        [ -n "$ROOT_WARN" ] && warn "$ROOT_WARN"
        [ -n "$ROOT_ERROR" ] && printf 'ERROR: %s\n' "$ROOT_ERROR" >&2
        exit "$rc"
        ;;
    "") if [ -t 0 ]; then menu; else usage; fi ;;
    menu) menu ;;
    -h|--help) usage ;;
    *) usage; exit 2 ;;
esac
