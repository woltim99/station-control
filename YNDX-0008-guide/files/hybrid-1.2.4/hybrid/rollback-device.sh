#!/system/bin/sh
# Device-side rollback for the Station Max hybrid archive.
# Runs as UID 0 under Permissive, from the rootd queue or the bootstrap shell.
#
# The whole backup is validated and the current four-file tuple is compared
# with the exact known operation/backup set before /odm is remounted rw.
# A changed rootd.conf is not accepted implicitly; --replace-config must be
# explicit and the changed file is saved first.
set -eu
umask 077

BASE=${BASE:-/data/local/tmp/station-max-hybrid}
PROG=ROLLBACK
ALLOW_UNMARKED=0
ALLOW_REPLACE_CONFIG=0
BK=
LOCK_HELD=0
TMP_FILES=
ODM_RW=0
STATE_READY=0
ST_PHASE=
ST_OPERATION=
ST_OPERATION_ID=${OPERATION_ID:-}
ST_BACKUP=
INFLIGHT_OWNED=${INFLIGHT_OWNED:-0}
INFLIGHT_FILE=${INFLIGHT_FILE:-/data/local/tmp/rootd-op.inflight}
STAGING_NONCE=${STAGING_NONCE:-}

cleanup() {
    _cl_rc=$?
    trap - EXIT INT TERM HUP
    if [ "$STATE_READY" = 1 ] && [ "$_cl_rc" -ne 0 ]; then
        case ${ST_PHASE:-} in
            rollback_complete|rollback_failed) ;;
            *)
                ST_PHASE=rollback_failed
                ST_REASON=unexpected-exit
                state_commit 2>/dev/null || true
                ;;
        esac
    fi
    cleanup_temps
    if [ "$ODM_RW" = 1 ]; then
        if odm_remount_ro; then
            ODM_RW=0
        else
            err "WARNING: /odm is still rw; run: mount -o ro,remount /odm"
        fi
    fi
    lock_release
    if [ "$INFLIGHT_OWNED" = 1 ] && [ -f "$INFLIGHT_FILE" ] && [ ! -L "$INFLIGHT_FILE" ]; then
        _cl_op=$(sed -n 's/^operation_id=//p' "$INFLIGHT_FILE" 2>/dev/null | head -n 1)
        if [ -n "$ST_OPERATION_ID" ] && [ "$_cl_op" = "$ST_OPERATION_ID" ]; then
            rm -f "$INFLIGHT_FILE" 2>/dev/null || true
        fi
    fi
    if [ "$_cl_rc" -ne 0 ]; then
        err "recovery: backup=${BK:-none} phase=${ST_PHASE:-unknown} state=${STATE_FILE}"
    fi
    exit "$_cl_rc"
}

fail_state() {
    ST_PHASE=rollback_failed
    ST_REASON=$(printf '%s' "$*" | tr -c 'A-Za-z0-9._:-' '_' | cut -c1-120)
    if [ "$STATE_READY" = 1 ]; then
        state_commit 2>/dev/null || true
    fi
    fail "$*"
}

[ -f "$BASE/hybrid/common.sh" ] && [ ! -L "$BASE/hybrid/common.sh" ] || { echo "ROLLBACK_FAIL: missing hybrid/common.sh" >&2; exit 1; }
. "$BASE/hybrid/common.sh"
PROG=ROLLBACK
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

while [ $# -gt 0 ]; do
    case $1 in
        --allow-unmarked)
            ALLOW_UNMARKED=1
            shift
            ;;
        --replace-config)
            ALLOW_REPLACE_CONFIG=1
            shift
            ;;
        --)
            shift
            break
            ;;
        -*)
            echo "ROLLBACK_FAIL: unknown argument: $1" >&2
            exit 1
            ;;
        *)
            [ -z "$BK" ] || { echo "ROLLBACK_FAIL: more than one backup path" >&2; exit 1; }
            BK=$1
            shift
            ;;
    esac
done
[ -n "$BK" ] || { echo "ROLLBACK_FAIL: usage: rollback-device.sh [--allow-unmarked] [--replace-config] BACKUP_DIR" >&2; exit 1; }
is_safe_tmp_path "$BK" || fail_state "unsafe backup path: $BK"
is_safe_tmp_path "$BASE" || fail_state "unsafe BASE: $BASE"
no_symlink_components "$BK" || fail_state "backup path contains a symlink component: $BK"
dir_is_regular "$BK" || fail_state "backup is not a regular directory: $BK"
no_symlink_components "$BASE" || fail_state "BASE path contains a symlink component: $BASE"

root_gate || fail_state "root/permissive gate failed"
build_gate || fail_state "build gate failed"
require_commands || fail_state "required command check failed"
lock_acquire rollback || fail_state "operation lock is held"

if ! state_probe; then
    fail_state "operation state is corrupt; refusing to overwrite it"
fi
if [ "$STATE_STATUS" = valid ]; then
    case ${ST_OPERATION:-}:${ST_PHASE:-} in
        install:install_started|install:backup_started)
            fail_state "install has not reached a completed backup; rollback cannot restore anything"
            ;;
    esac
    if [ -n "${ST_BACKUP:-}" ]; then
        is_safe_tmp_path "$ST_BACKUP" || fail_state "state contains an unsafe backup path"
        case ${ST_PHASE:-} in
            rollback_restoring|rollback_failed)
                [ "$ST_BACKUP" = "$BK" ] || fail_state "rollback retry must use the backup recorded in state: $ST_BACKUP"
                ;;
        esac
    fi
fi

# Full backup validation happens before any remount and before any state write.
manifest_validate "$BK" "$ALLOW_UNMARKED" || fail_state "backup validation failed; nothing was changed"
MANIFEST_SHA=$MV_MANIFEST_SHA

_odm_opts=$(odm_mount_options) || fail_state "/odm is not mounted (mount check failed)"
[ -n "$_odm_opts" ] || fail_state "/odm mount entry is missing"
no_symlink_components /odm || fail_state "/odm path contains a symlink component"

_names=$(process_names) || fail_state "ps failed: process state is unknown"
_rootd_n=$(count_named_process "$_names" rootd)
_second_n=$(count_second_channels "$_names")
[ "$_second_n" = 0 ] || fail_state "a second root/bootstrap channel is present (expected names: $SECOND_CHANNEL_NAMES)"
[ "$_rootd_n" -le 1 ] || fail_state "more than one rootd process is present"

build_known_tuples "$BK" || fail_state "cannot derive the known operation set; refusing to touch /odm"
_current_tuple=$(current_odm_tuple) || fail_state "cannot read the current four-file set; refusing to touch /odm"

REPLACE_CONFIG_SAVED=
if ! tuple_allowed "$_current_tuple"; then
    if [ "$ALLOW_REPLACE_CONFIG" = 1 ] && tuple_binaries_allowed "$(tuple_binaries "$_current_tuple")"; then
        if [ -e /odm/etc/rootd.conf ] || [ -L /odm/etc/rootd.conf ]; then
            [ -L /odm/etc/rootd.conf ] && fail_state "refusing symlink rootd.conf"
            [ -f /odm/etc/rootd.conf ] || fail_state "refusing non-regular rootd.conf"
            REPLACE_CONFIG_SAVED=$(mktemp /data/local/tmp/rootd-conf-replaced.XXXXXX 2>/dev/null) || fail_state "cannot create rootd.conf rescue file"
            is_safe_tmp_path "$REPLACE_CONFIG_SAVED" || fail_state "unsafe rootd.conf rescue path"
            cp /odm/etc/rootd.conf "$REPLACE_CONFIG_SAVED" || fail_state "cannot save changed rootd.conf"
            chmod 0600 "$REPLACE_CONFIG_SAVED" || fail_state "cannot protect changed rootd.conf"
            sync 2>/dev/null || true
            say "changed rootd.conf saved to: $REPLACE_CONFIG_SAVED"
        fi
    else
        fail_state "current four-file set is not the installed set, the target set, or the backup set; refusing to delete or overwrite it"
    fi
fi

# Record the set that was current at the start.  A retry can then accept the
# same pre-rollback or partially restored values without an open allow-list.
set -- $_current_tuple
if [ "$1" = ABSENT ]; then ST_INSTALLED_ROOTD=; else ST_INSTALLED_ROOTD=$1; fi
if [ "$2" = ABSENT ]; then ST_INSTALLED_PERM=; else ST_INSTALLED_PERM=$2; fi
if [ "$3" = ABSENT ]; then ST_INSTALLED_RC=; else ST_INSTALLED_RC=$3; fi
if [ "$4" = ABSENT ]; then ST_INSTALLED_CONF=; else ST_INSTALLED_CONF=$4; fi

[ -n "${OPERATION_ID:-}" ] && ST_OPERATION_ID=$OPERATION_ID
if [ -z "$ST_OPERATION_ID" ]; then
    _ts=$(date +%Y%m%d-%H%M%S 2>/dev/null || echo unknown)
    ST_OPERATION_ID="rollback-${_ts}-$$"
fi
case $ST_OPERATION_ID in
    *[!A-Za-z0-9._-]*|'') fail_state "invalid operation id" ;;
esac
[ ${#ST_OPERATION_ID} -le 64 ] || fail_state "oversized operation id"

ST_OPERATION=rollback
ST_PHASE=rollback_restoring
ST_BACKUP=$BK
ST_MANIFEST_SHA=$MANIFEST_SHA
ST_STAGING=$BASE
ST_STAGING_NONCE=$STAGING_NONCE
ST_REASON=
STATE_READY=1
state_commit || fail_state "cannot write operation state"

odm_remount_rw || fail_state "remount rw failed"
ODM_RW=1

restore_one() {
    _ro_rel=$1
    _ro_kind=$2
    _ro_sha=$3
    _ro_dst="/odm/$_ro_rel"
    _ro_name=$(rel_backup_name "$_ro_rel") || fail_state "internal error: no backup name for $_ro_rel"
    if [ "$_ro_kind" = absent ]; then
        if [ ! -e "$_ro_dst" ] && [ ! -L "$_ro_dst" ]; then
            say "unchanged absent $_ro_dst"
            return 0
        fi
        if [ -e "$_ro_dst" ] || [ -L "$_ro_dst" ]; then
            [ -L "$_ro_dst" ] && fail_state "refusing symlink destination: $_ro_dst"
            [ -f "$_ro_dst" ] || fail_state "refusing non-regular destination: $_ro_dst"
            rm -f "$_ro_dst" || fail_state "cannot remove $_ro_dst"
        fi
        sync 2>/dev/null || fail_state "sync failed"
        return 0
    fi
    [ -f "$BK/$_ro_name" ] && [ ! -L "$BK/$_ro_name" ] || fail_state "missing backup file for $_ro_rel"
    _ro_got=$(sha256_file "$BK/$_ro_name") || fail_state "cannot hash backup file $_ro_rel"
    [ "$_ro_got" = "$_ro_sha" ] || fail_state "backup hash changed for $_ro_rel"
    case $_ro_rel in
        bin/rootd) _ro_mode=0755; _ro_label=u:object_r:adbd_exec:s0 ;;
        bin/permissive) _ro_mode=0755; _ro_label=u:object_r:system_file:s0 ;;
        etc/init/rootd.rc|etc/rootd.conf) _ro_mode=0644; _ro_label=u:object_r:vendor_configs_file:s0 ;;
        *) fail_state "internal error: unknown path $_ro_rel" ;;
    esac
    _ro_want_mode=$(printf '%s' "$_ro_mode" | sed 's/^0*//')
    [ -n "$_ro_want_mode" ] || _ro_want_mode=0
    no_symlink_components "$_ro_dst" || fail_state "refusing symlink component: $_ro_dst"
    [ -L "$_ro_dst" ] && fail_state "refusing symlink destination: $_ro_dst"
    [ -d "$_ro_dst" ] && fail_state "refusing directory destination: $_ro_dst"
    if [ -f "$_ro_dst" ]; then
        _ro_cur=$(sha256_file "$_ro_dst") || fail_state "cannot hash existing $_ro_dst"
        _ro_meta=$(stat_mode_uid_gid "$_ro_dst") || fail_state "cannot stat existing $_ro_dst"
        _ro_m=$(printf '%s\n' "$_ro_meta" | awk '{print $1}')
        _ro_u=$(printf '%s\n' "$_ro_meta" | awk '{print $2}')
        _ro_g=$(printf '%s\n' "$_ro_meta" | awk '{print $3}')
        _ro_m=$(printf '%s' "$_ro_m" | sed 's/^0*//'); [ -n "$_ro_m" ] || _ro_m=0
        _ro_ctx=$(selinux_label "$_ro_dst")
        if [ "$_ro_cur" = "$_ro_sha" ] && [ "$_ro_m" = "$_ro_want_mode" ] && \
           [ "$_ro_u" = 0 ] && [ "$_ro_g" = 0 ] && [ "$_ro_ctx" = "$_ro_label" ]; then
            say "unchanged $_ro_dst"
            return 0
        fi
    fi
    _ro_tmp="${_ro_dst}.rollback.$$"
    TMP_FILES="$TMP_FILES $_ro_tmp"
    rm -f "$_ro_tmp" 2>/dev/null || true
    cp "$BK/$_ro_name" "$_ro_tmp" || fail_state "copy failed: $_ro_dst"
    chown 0:0 "$_ro_tmp" || fail_state "chown failed: $_ro_dst"
    chmod "$_ro_mode" "$_ro_tmp" || fail_state "chmod failed: $_ro_dst"
    chcon "$_ro_label" "$_ro_tmp" || fail_state "chcon failed: $_ro_dst"
    sync 2>/dev/null || fail_state "sync failed"
    [ -L "$_ro_dst" ] && fail_state "destination became a symlink: $_ro_dst"
    mv -f "$_ro_tmp" "$_ro_dst" || fail_state "replace failed: $_ro_dst"
    sync 2>/dev/null || fail_state "sync failed"
}

while IFS=' ' read -r _kind _rel _sha _extra || [ -n "${_kind:-}" ]; do
    case $_kind in
        present) restore_one "$_rel" present "$_sha" ;;
        absent) restore_one "$_rel" absent "" ;;
        *) fail_state "bad MANIFEST entry" ;;
    esac
done < "$BK/MANIFEST"

# Re-check the restored set against the manifest after writing.
while IFS=' ' read -r _kind _rel _sha _extra || [ -n "${_kind:-}" ]; do
    _path="/odm/$_rel"
    case $_kind in
        absent)
            if [ -e "$_path" ] || [ -L "$_path" ]; then
                fail_state "restored path should be absent: $_path"
            fi
            ;;
        present)
            require_hash "$_path" "$_sha" "$_path" || fail_state "restored hash mismatch: $_path"
            case $_rel in
                bin/rootd) verify_metadata "$_path" 755 u:object_r:adbd_exec:s0 || fail_state "restored metadata mismatch: $_path" ;;
                bin/permissive) verify_metadata "$_path" 755 u:object_r:system_file:s0 || fail_state "restored metadata mismatch: $_path" ;;
                etc/init/rootd.rc|etc/rootd.conf) verify_metadata "$_path" 644 u:object_r:vendor_configs_file:s0 || fail_state "restored metadata mismatch: $_path" ;;
            esac
            ;;
        *) fail_state "bad MANIFEST entry" ;;
    esac
done < "$BK/MANIFEST"

_present_n=0
for _rel in $REL_PATHS; do
    _lookup=$(manifest_lookup "$BK" "$_rel") || fail_state "backup manifest has no entry for $_rel"
    case $_lookup in present\ *) _present_n=$((_present_n + 1)) ;; esac
done
case $_present_n in
    0) _expect=stock ;;
    4) _expect=hybrid ;;
    *) _expect=partial ;;
esac

ST_RESTORED_ROOTD=
ST_RESTORED_PERM=
ST_RESTORED_RC=
ST_RESTORED_CONF=
for _rel in $REL_PATHS; do
    _lookup=$(manifest_lookup "$BK" "$_rel") || fail_state "backup manifest has no entry for $_rel"
    case $_lookup in
        present\ *)
            _h=${_lookup#present }
            case $_rel in
                bin/rootd) ST_RESTORED_ROOTD=$_h ;;
                bin/permissive) ST_RESTORED_PERM=$_h ;;
                etc/init/rootd.rc) ST_RESTORED_RC=$_h ;;
                etc/rootd.conf) ST_RESTORED_CONF=$_h ;;
            esac
            ;;
    esac
done

ST_EXPECT=$_expect
ST_PHASE=rollback_complete
ST_REASON=
state_commit || fail_state "cannot record completed rollback"

odm_remount_ro || fail_state "files restored but remount ro failed; /odm may still be rw"
ODM_RW=0
[ "$(odm_state)" = ro ] || fail_state "/odm did not return to ro"

say "ROLLBACK_OK backup=$BK expect=$_expect"
[ -n "$REPLACE_CONFIG_SAVED" ] && say "changed rootd.conf saved: $REPLACE_CONFIG_SAVED"
say "next: reboot, then check the expected state"
exit 0
