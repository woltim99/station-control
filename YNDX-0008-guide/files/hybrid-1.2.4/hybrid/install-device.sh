#!/system/bin/sh
# Device-side installer for the Station Max hybrid archive.
# Runs as UID 0 under Permissive, from the bootstrap shell or the rootd queue.
#
# Safety rules:
#   - install.conf is parsed as data; it is never sourced or evaluated;
#   - only the four published /odm paths are written;
#   - an unknown existing destination is reported with its hash and is not
#     overwritten;
#   - state is loaded and validated before any state write; preflight errors
#     never replace the previous state;
#   - the backup is published only after all four entries are verified;
#   - a retry reuses the operation backup or a matching orphan and never
#     creates a second "original" backup from an already modified /odm;
#   - /odm is returned to ro on success and on every handled error.
set -eu
umask 077

BASE=${BASE:-/data/local/tmp/station-max-hybrid}
CONF="$BASE/install.conf"
PROG=INSTALL
LOCK_HELD=0
TMP_FILES=
BACKUP_DIR=
BACKUP_PUBLISHED=0
ODM_RW=0
STATE_READY=0
ST_PHASE=
ST_OPERATION=
ST_OPERATION_ID=${OPERATION_ID:-}
ST_BACKUP=
TARGET_CONF_TEXT=
TARGET_CONF_SHA=
INFLIGHT_OWNED=${INFLIGHT_OWNED:-0}
INFLIGHT_FILE=${INFLIGHT_FILE:-/data/local/tmp/rootd-op.inflight}
STAGING_NONCE=${STAGING_NONCE:-}

cleanup() {
    _cl_rc=$?
    trap - EXIT INT TERM HUP
    if [ "$STATE_READY" = 1 ] && [ "$_cl_rc" -ne 0 ]; then
        case ${ST_PHASE:-} in
            complete|failed|failed_odm_rw) ;;
            *)
                ST_PHASE=failed
                ST_REASON=unexpected-exit
                state_commit 2>/dev/null || true
                ;;
        esac
    fi
    cleanup_temps
    if [ "$BACKUP_PUBLISHED" = 0 ] && [ -n "$BACKUP_DIR" ]; then
        case $BACKUP_DIR in
            /data/local/tmp/.rootd-backup-*) rm -rf "$BACKUP_DIR" 2>/dev/null || true ;;
        esac
    fi
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
        err "recovery: backup=${BACKUP_DIR:-none} phase=${ST_PHASE:-unknown} state=${STATE_FILE}"
    fi
    exit "$_cl_rc"
}

fail_state() {
    ST_PHASE=failed
    ST_REASON=$(printf '%s' "$*" | tr -c 'A-Za-z0-9._:-' '_' | cut -c1-120)
    if [ "$STATE_READY" = 1 ]; then
        state_commit 2>/dev/null || true
    fi
    fail "$*"
}

case $BASE in
    /data/local/tmp/*) ;;
    *) echo "INSTALL_FAIL: unsafe BASE: $BASE" >&2; exit 1 ;;
esac
[ -f "$BASE/hybrid/common.sh" ] && [ ! -L "$BASE/hybrid/common.sh" ] || {
    echo "INSTALL_FAIL: missing hybrid/common.sh" >&2
    exit 1
}
. "$BASE/hybrid/common.sh"
PROG=INSTALL
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

is_safe_tmp_path "$BASE" || fail_state "unsafe BASE: $BASE"
no_symlink_components "$BASE" || fail_state "BASE path contains a symlink component"
dir_is_regular "$BASE" || fail_state "staging directory is not a regular directory: $BASE"
file_is_regular "$CONF" || fail_state "missing regular config: $CONF"

root_gate || fail_state "root/permissive gate failed"
build_gate || fail_state "build gate failed"
parse_install_conf "$CONF" || fail_state "invalid install.conf (config is data, never executed)"
require_commands || fail_state "required command check failed"

require_hash "$BASE/hybrid/rootd" "$ROOTD_SHA" hybrid/rootd || fail_state "delivered rootd hash mismatch"
require_hash "$BASE/hybrid/permissive" "$PERM_SHA" hybrid/permissive || fail_state "delivered permissive hash mismatch"
require_hash "$BASE/hybrid/rootd.rc" "$RC_SHA" hybrid/rootd.rc || fail_state "delivered rootd.rc hash mismatch"

no_symlink_components /odm || fail_state "/odm path contains a symlink component"
dir_is_regular /odm || fail_state "/odm is not a mountpoint directory"
_odm_opts=$(odm_mount_options) || fail_state "/odm is not mounted (mount check failed)"
[ -n "$_odm_opts" ] || fail_state "/odm mount entry is missing"

_avail_odm=$(df -k /odm 2>/dev/null | tail -n 1 | awk '{print $4}') || fail_state "df /odm failed"
case $_avail_odm in ''|*[!0-9]*) fail_state "df /odm returned no usable free-space value" ;; esac
[ "$_avail_odm" -ge 8192 ] || fail_state "not enough free space on /odm"
_avail_data=$(df -k /data/local/tmp 2>/dev/null | tail -n 1 | awk '{print $4}') || fail_state "df /data/local/tmp failed"
case $_avail_data in ''|*[!0-9]*) fail_state "df /data/local/tmp returned no usable free-space value" ;; esac
[ "$_avail_data" -ge 8192 ] || fail_state "not enough free space for backup"

_names=$(process_names) || fail_state "ps failed: process state is unknown"
_rootd_n=$(count_named_process "$_names" rootd)
_second_n=$(count_second_channels "$_names")
[ "$_second_n" = 0 ] || fail_state "a second root/bootstrap channel is present (expected names: $SECOND_CHANNEL_NAMES)"
[ "$_rootd_n" -le 1 ] || fail_state "more than one rootd process is present"

TARGET_CONF_TEXT=$(printf 'lan.router_ip=%s\nlan.dns_ip=%s\nlan.ipv6=%s' "$ROUTER_IP" "$DNS_IP" "$IPV6")
TARGET_CONF_SHA=$(printf '%s\n' "$TARGET_CONF_TEXT" | sha256sum 2>/dev/null | awk '{print $1}')
case $TARGET_CONF_SHA in
    ''|*[!0-9a-f]*) fail_state "cannot compute target rootd.conf hash" ;;
esac
[ ${#TARGET_CONF_SHA} -eq 64 ] || fail_state "cannot compute target rootd.conf hash"

lock_acquire install || fail_state "operation lock is held"

# Load the prior state once, before any state write.  A corrupt file is
# fail-closed and must not be replaced by a fresh "failed" record.
if ! state_probe; then
    fail_state "operation state is corrupt; refusing to overwrite it"
fi

[ -n "${OPERATION_ID:-}" ] && ST_OPERATION_ID=$OPERATION_ID
if [ -z "$ST_OPERATION_ID" ]; then
    _ts=$(date +%Y%m%d-%H%M%S 2>/dev/null || echo unknown)
    ST_OPERATION_ID="install-${_ts}-$$"
fi
case $ST_OPERATION_ID in
    *[!A-Za-z0-9._-]*|'') fail_state "invalid operation id" ;;
esac
[ ${#ST_OPERATION_ID} -le 64 ] || fail_state "oversized operation id"

REUSE_BACKUP=0
if [ "$STATE_STATUS" = valid ]; then
    case ${ST_OPERATION:-}:${ST_PHASE:-} in
        rollback:*)
            [ "$ST_PHASE" = rollback_complete ] || fail_state "a rollback operation is not complete; finish or recover it before install"
            ;;
    esac
    if [ -n "${ST_BACKUP:-}" ]; then
        is_safe_tmp_path "$ST_BACKUP" || fail_state "operation state contains an unsafe backup path"
        manifest_validate "$ST_BACKUP" 0 || fail_state "operation backup is not complete; refusing to create a new original backup"
        BACKUP_DIR=$ST_BACKUP
        BACKUP_PUBLISHED=1
        REUSE_BACKUP=1
    fi
    # A completed install whose binaries still equal the installed set is an
    # idempotent retry (a changed rootd.conf is a supported reconfiguration).
    if [ "$REUSE_BACKUP" = 1 ] && [ "${ST_PHASE:-}" = complete ]; then
        _same=1
        [ "$(sha256_file /odm/bin/rootd 2>/dev/null || true)" = "${ST_INSTALLED_ROOTD:-x}" ] || _same=0
        [ "$(sha256_file /odm/bin/permissive 2>/dev/null || true)" = "${ST_INSTALLED_PERM:-x}" ] || _same=0
        [ "$(sha256_file /odm/etc/init/rootd.rc 2>/dev/null || true)" = "${ST_INSTALLED_RC:-x}" ] || _same=0
        [ "$_same" = 1 ] || fail_state "completed installation has a different rootd/permissive/rc set; roll back before installing a different set"
    fi
fi

if [ -z "$BACKUP_DIR" ] && [ "$STATE_STATUS" = valid ]; then
    _orphans=$(find_orphan_backups "$ST_OPERATION_ID")
    _orphan_n=$(printf '%s\n' "$_orphans" | sed '/^$/d' | wc -l | tr -d '[:space:]')
    case $_orphan_n in
        ''|*[!0-9]*) fail_state "cannot count orphan backups" ;;
        0) ;;
        1)
            BACKUP_DIR=$(printf '%s\n' "$_orphans" | sed -n '1p')
            is_safe_tmp_path "$BACKUP_DIR" || fail_state "orphan backup path is unsafe"
            manifest_validate "$BACKUP_DIR" 0 || fail_state "orphan backup is not complete"
            ST_MANIFEST_SHA=$MV_MANIFEST_SHA
            BACKUP_PUBLISHED=1
            REUSE_BACKUP=1
            say "reusing published orphan backup: $BACKUP_DIR"
            ;;
        *) fail_state "more than one published backup matches operation $ST_OPERATION_ID; refusing to guess" ;;
    esac
fi

# A pre-backup phase is safe to resume: the installer only remounts rw and
# writes after backup_complete.  Later phases without a backup are not.
if [ -z "$BACKUP_DIR" ] && [ "$STATE_STATUS" = valid ]; then
    case ${ST_OPERATION:-}:${ST_PHASE:-} in
        install:backup_complete|install:writing|install:files_written|install:verified|install:complete)
            fail_state "install phase ${ST_PHASE} has no usable backup; refusing to create an original backup from a changed /odm"
            ;;
        rollback:*)
            fail_state "rollback state has no usable backup"
            ;;
    esac
fi

# Unknown destination files stop here, before any backup or write.
check_destination() {
    _cd_rel=$1
    _cd_path="/odm/$_cd_rel"
    _cd_conf=0
    case $_cd_rel in
        bin/rootd) _cd_allowed="$ROOTD_SHA $KNOWN_ROOTD_SHA" ;;
        bin/permissive) _cd_allowed="$PERM_SHA $KNOWN_PERM_SHA" ;;
        etc/init/rootd.rc) _cd_allowed="$RC_SHA $KNOWN_RC_SHA" ;;
        etc/rootd.conf) _cd_allowed="$TARGET_CONF_SHA"; _cd_conf=1 ;;
        *) fail_state "internal error: unknown path $_cd_rel" ;;
    esac
    case $_cd_rel in
        bin/rootd) [ -n "${ST_INSTALLED_ROOTD:-}" ] && _cd_allowed="$_cd_allowed $ST_INSTALLED_ROOTD" ;;
        bin/permissive) [ -n "${ST_INSTALLED_PERM:-}" ] && _cd_allowed="$_cd_allowed $ST_INSTALLED_PERM" ;;
        etc/init/rootd.rc) [ -n "${ST_INSTALLED_RC:-}" ] && _cd_allowed="$_cd_allowed $ST_INSTALLED_RC" ;;
        etc/rootd.conf) [ -n "${ST_INSTALLED_CONF:-}" ] && _cd_allowed="$_cd_allowed $ST_INSTALLED_CONF" ;;
    esac
    case $_cd_rel in
        bin/rootd) [ -n "${ST_TARGET_ROOTD:-}" ] && _cd_allowed="$_cd_allowed $ST_TARGET_ROOTD" ;;
        bin/permissive) [ -n "${ST_TARGET_PERM:-}" ] && _cd_allowed="$_cd_allowed $ST_TARGET_PERM" ;;
        etc/init/rootd.rc) [ -n "${ST_TARGET_RC:-}" ] && _cd_allowed="$_cd_allowed $ST_TARGET_RC" ;;
        etc/rootd.conf) [ -n "${ST_TARGET_CONF:-}" ] && _cd_allowed="$_cd_allowed $ST_TARGET_CONF" ;;
    esac
    if [ -n "$BACKUP_DIR" ]; then
        _cd_lookup=$(manifest_lookup "$BACKUP_DIR" "$_cd_rel" 2>/dev/null || true)
        case $_cd_lookup in
            present\ *) _cd_allowed="$_cd_allowed ${_cd_lookup#present }" ;;
        esac
    fi
    no_symlink_components "$_cd_path" || fail_state "refusing symlink component: $_cd_path"
    destination_known "$_cd_path" "$_cd_allowed" "$_cd_conf" || fail_state "unknown existing destination; nothing was written"
}

check_destination bin/rootd
check_destination bin/permissive
check_destination etc/init/rootd.rc
check_destination etc/rootd.conf

# From here on this process owns the operation record.
ST_OPERATION=install
ST_OPERATION_ID=$ST_OPERATION_ID
ST_STAGING=$BASE
ST_STAGING_NONCE=$STAGING_NONCE
ST_PHASE=install_started
ST_REASON=
STATE_READY=1
state_commit || fail_state "cannot write operation state"

if [ "$REUSE_BACKUP" = 1 ]; then
    ST_BACKUP=$BACKUP_DIR
    [ -n "${ST_MANIFEST_SHA:-}" ] || ST_MANIFEST_SHA=$(sha256_file "$BACKUP_DIR/MANIFEST" 2>/dev/null || true)
    say "reusing completed backup: $BACKUP_DIR"
else
    BACKUP_DIR=
    BACKUP_PUBLISHED=0
    ST_PHASE=backup_started
    ST_REASON=
    state_commit || fail_state "cannot write operation state"

    _ts=$(date +%Y%m%d-%H%M%S 2>/dev/null) || _ts=unknown
    _tmp=$(mktemp -d "/data/local/tmp/.rootd-backup-$_ts.XXXXXX" 2>/dev/null) || fail_state "cannot create temporary backup directory"
    case $_tmp in
        /data/local/tmp/.rootd-backup-*) ;;
        *) fail_state "mktemp returned an unexpected path; refusing to delete it" ;;
    esac
    BACKUP_DIR=$_tmp
    BACKUP_PUBLISHED=0
    chmod 0700 "$_tmp" || fail_state "cannot protect temporary backup directory"

    : > "$_tmp/MANIFEST.new" || fail_state "cannot create backup manifest"
    for _rel in $REL_PATHS; do
        _bname=$(rel_backup_name "$_rel") || fail_state "internal error: no backup name for $_rel"
        _src="/odm/$_rel"
        if [ -e "$_src" ]; then
            [ -L "$_src" ] && fail_state "refusing to back up symlink: $_src"
            [ -f "$_src" ] || fail_state "refusing to back up non-regular file: $_src"
            cp "$_src" "$_tmp/$_bname" || fail_state "cannot copy backup file $_rel"
            chmod 0600 "$_tmp/$_bname" || fail_state "cannot protect backup file $_rel"
            _bsha=$(sha256_file "$_tmp/$_bname") || fail_state "cannot hash backup file $_rel"
            [ ${#_bsha} -eq 64 ] || fail_state "cannot hash backup file $_rel"
            printf 'present %s %s\n' "$_rel" "$_bsha" >> "$_tmp/MANIFEST.new" || fail_state "cannot write backup manifest"
        else
            printf 'absent %s\n' "$_rel" >> "$_tmp/MANIFEST.new" || fail_state "cannot write backup manifest"
        fi
    done
    sync 2>/dev/null || fail_state "sync failed"
    mv "$_tmp/MANIFEST.new" "$_tmp/MANIFEST" || fail_state "cannot publish backup manifest"
    chmod 0600 "$_tmp/MANIFEST" || fail_state "cannot protect backup manifest"
    _manifest_sha=$(sha256_file "$_tmp/MANIFEST") || fail_state "cannot hash backup manifest"
    {
        printf 'version=1\n'
        printf 'manifest_sha256=%s\n' "$_manifest_sha"
        printf 'display_id=%s\n' "$DISPLAY_ID"
        printf 'fingerprint=%s\n' "$FINGERPRINT"
        printf 'kernel=%s\n' "$KERNEL_RELEASE"
        printf 'operation_id=%s\n' "$ST_OPERATION_ID"
        [ -n "$STAGING_NONCE" ] && printf 'staging_nonce=%s\n' "$STAGING_NONCE"
        printf 'created=%s\n' "$(date '+%Y-%m-%dT%H:%M:%S' 2>/dev/null || echo unknown)"
        printf 'source=station-max-hybrid-%s\n' "$VERSION"
    } > "$_tmp/COMPLETE.new" || fail_state "cannot write backup marker"
    chmod 0600 "$_tmp/COMPLETE.new" || fail_state "cannot protect backup marker"
    mv "$_tmp/COMPLETE.new" "$_tmp/COMPLETE" || fail_state "cannot publish backup marker"
    sync 2>/dev/null || fail_state "sync failed"

    _final="/data/local/tmp/rootd-backup-$_ts"
    if [ -e "$_final" ] || [ -L "$_final" ]; then
        _final="$_final-$$"
    fi
    [ ! -e "$_final" ] && [ ! -L "$_final" ] || fail_state "backup destination already exists: $_final"
    manifest_validate "$_tmp" 0 || fail_state "fresh backup failed validation"
    mv "$_tmp" "$_final" || fail_state "cannot publish backup directory"
    BACKUP_DIR=$_final
    BACKUP_PUBLISHED=1
    ST_BACKUP=$BACKUP_DIR
    ST_MANIFEST_SHA=$_manifest_sha
    ST_PHASE=backup_complete
    ST_REASON=
    state_commit || fail_state "cannot record completed backup"
    say "backup: $BACKUP_DIR"
fi

ST_TARGET_ROOTD=$ROOTD_SHA
ST_TARGET_PERM=$PERM_SHA
ST_TARGET_RC=$RC_SHA
ST_TARGET_CONF=$TARGET_CONF_SHA

# From here on /odm is modified.  Every error path must try to return it to ro.
odm_remount_rw || fail_state "remount rw failed"
ODM_RW=1

for _dir in /odm/bin /odm/etc /odm/etc/init; do
    no_symlink_components "$_dir" || fail_state "refusing symlink component: $_dir"
    [ -L "$_dir" ] && fail_state "refusing symlink: $_dir"
    if [ ! -d "$_dir" ]; then
        mkdir "$_dir" || fail_state "cannot create $_dir"
    fi
done

ST_PHASE=writing
state_commit || fail_state "cannot write operation state"

install_one() {
    _io_src=$1
    _io_dst=$2
    _io_mode=$3
    _io_label=$4
    _io_expected=$5
    _io_want_mode=$(printf '%s' "$_io_mode" | sed 's/^0*//')
    [ -n "$_io_want_mode" ] || _io_want_mode=0
    [ -L "$_io_dst" ] && fail_state "refusing symlink destination: $_io_dst"
    [ -d "$_io_dst" ] && fail_state "refusing directory destination: $_io_dst"
    # An identical file with correct metadata need not be replaced.  Besides
    # being faster, this avoids a deleted-but-mapped rootd inode blocking a
    # later /odm remount while the daemon is still running.
    if [ -f "$_io_dst" ]; then
        _io_hash=$(sha256_file "$_io_dst") || fail_state "cannot hash existing $_io_dst"
        _io_meta=$(stat_mode_uid_gid "$_io_dst") || fail_state "cannot stat existing $_io_dst"
        _io_m=$(printf '%s\n' "$_io_meta" | awk '{print $1}')
        _io_u=$(printf '%s\n' "$_io_meta" | awk '{print $2}')
        _io_g=$(printf '%s\n' "$_io_meta" | awk '{print $3}')
        _io_m=$(printf '%s' "$_io_m" | sed 's/^0*//'); [ -n "$_io_m" ] || _io_m=0
        _io_ctx=$(selinux_label "$_io_dst")
        if [ "$_io_hash" = "$_io_expected" ] && [ "$_io_m" = "$_io_want_mode" ] && \
           [ "$_io_u" = 0 ] && [ "$_io_g" = 0 ] && [ "$_io_ctx" = "$_io_label" ]; then
            say "unchanged $_io_dst"
            return 0
        fi
    fi
    _io_tmp="${_io_dst}.new.$$"
    TMP_FILES="$TMP_FILES $_io_tmp"
    rm -f "$_io_tmp" 2>/dev/null || true
    cp "$_io_src" "$_io_tmp" || fail_state "copy failed: $_io_dst"
    chown 0:0 "$_io_tmp" || fail_state "chown failed: $_io_dst"
    chmod "$_io_mode" "$_io_tmp" || fail_state "chmod failed: $_io_dst"
    chcon "$_io_label" "$_io_tmp" || fail_state "chcon failed: $_io_dst"
    sync 2>/dev/null || fail_state "sync failed"
    [ -L "$_io_dst" ] && fail_state "destination became a symlink: $_io_dst"
    mv -f "$_io_tmp" "$_io_dst" || fail_state "replace failed: $_io_dst"
    sync 2>/dev/null || fail_state "sync failed"
}

install_one "$BASE/hybrid/rootd" /odm/bin/rootd 0755 u:object_r:adbd_exec:s0 "$ROOTD_SHA"
install_one "$BASE/hybrid/permissive" /odm/bin/permissive 0755 u:object_r:system_file:s0 "$PERM_SHA"
install_one "$BASE/hybrid/rootd.rc" /odm/etc/init/rootd.rc 0644 u:object_r:vendor_configs_file:s0 "$RC_SHA"

_conf_dst=/odm/etc/rootd.conf
_conf_skip=0
if [ -f "$_conf_dst" ] && [ ! -L "$_conf_dst" ]; then
    _conf_hash=$(sha256_file "$_conf_dst") || fail_state "cannot hash existing $_conf_dst"
    _conf_meta=$(stat_mode_uid_gid "$_conf_dst") || fail_state "cannot stat existing $_conf_dst"
    _conf_m=$(printf '%s\n' "$_conf_meta" | awk '{print $1}')
    _conf_u=$(printf '%s\n' "$_conf_meta" | awk '{print $2}')
    _conf_g=$(printf '%s\n' "$_conf_meta" | awk '{print $3}')
    _conf_m=$(printf '%s' "$_conf_m" | sed 's/^0*//'); [ -n "$_conf_m" ] || _conf_m=0
    _conf_ctx=$(selinux_label "$_conf_dst")
    if [ "$_conf_hash" = "$TARGET_CONF_SHA" ] && [ "$_conf_m" = 644 ] && \
       [ "$_conf_u" = 0 ] && [ "$_conf_g" = 0 ] && [ "$_conf_ctx" = u:object_r:vendor_configs_file:s0 ]; then
        _conf_skip=1
        say "unchanged $_conf_dst"
    fi
fi
if [ "$_conf_skip" = 0 ]; then
    _conf_tmp="/odm/etc/.rootd.conf.new.$$"
    TMP_FILES="$TMP_FILES $_conf_tmp"
    rm -f "$_conf_tmp" 2>/dev/null || true
    printf '%s\n' "$TARGET_CONF_TEXT" > "$_conf_tmp" || fail_state "cannot write rootd.conf"
    chown 0:0 "$_conf_tmp" || fail_state "chown failed: /odm/etc/rootd.conf"
    chmod 0644 "$_conf_tmp" || fail_state "chmod failed: /odm/etc/rootd.conf"
    chcon u:object_r:vendor_configs_file:s0 "$_conf_tmp" || fail_state "chcon failed: /odm/etc/rootd.conf"
    sync 2>/dev/null || fail_state "sync failed"
    [ -L "$_conf_dst" ] && fail_state "refusing symlink destination: $_conf_dst"
    mv -f "$_conf_tmp" "$_conf_dst" || fail_state "replace failed: $_conf_dst"
    sync 2>/dev/null || fail_state "sync failed"
fi

ST_PHASE=files_written
ST_TARGET_ROOTD=$ROOTD_SHA
ST_TARGET_PERM=$PERM_SHA
ST_TARGET_RC=$RC_SHA
ST_TARGET_CONF=$TARGET_CONF_SHA
state_commit || fail_state "cannot record written files"

require_hash /odm/bin/rootd "$ROOTD_SHA" /odm/bin/rootd || fail_state "installed rootd hash mismatch"
require_hash /odm/bin/permissive "$PERM_SHA" /odm/bin/permissive || fail_state "installed permissive hash mismatch"
require_hash /odm/etc/init/rootd.rc "$RC_SHA" /odm/etc/init/rootd.rc || fail_state "installed rootd.rc hash mismatch"
require_hash /odm/etc/rootd.conf "$TARGET_CONF_SHA" /odm/etc/rootd.conf || fail_state "installed rootd.conf hash mismatch"
verify_metadata /odm/bin/rootd 755 u:object_r:adbd_exec:s0 || fail_state "installed rootd metadata mismatch"
verify_metadata /odm/bin/permissive 755 u:object_r:system_file:s0 || fail_state "installed permissive metadata mismatch"
verify_metadata /odm/etc/init/rootd.rc 644 u:object_r:vendor_configs_file:s0 || fail_state "installed rootd.rc metadata mismatch"
verify_metadata /odm/etc/rootd.conf 644 u:object_r:vendor_configs_file:s0 || fail_state "installed rootd.conf metadata mismatch"
parse_rootd_conf /odm/etc/rootd.conf || fail_state "installed rootd.conf does not parse"
[ "$CFG_ROUTER" = "$ROUTER_IP" ] && [ "$CFG_DNS" = "$DNS_IP" ] && [ "$CFG_IPV6" = "$IPV6" ] || fail_state "installed rootd.conf values changed"

ST_INSTALLED_ROOTD=$ROOTD_SHA
ST_INSTALLED_PERM=$PERM_SHA
ST_INSTALLED_RC=$RC_SHA
ST_INSTALLED_CONF=$TARGET_CONF_SHA
ST_PHASE=verified
state_commit || fail_state "cannot record verified installation"

odm_remount_ro || fail_state "installed files verified but remount ro failed; /odm may still be rw"
ODM_RW=0
[ "$(odm_state)" = ro ] || fail_state "/odm did not return to ro"

ST_PHASE=complete
ST_EXPECT=hybrid
ST_REASON=
state_commit || fail_state "cannot record complete installation"

say "INSTALL_OK backup=$BACKUP_DIR phase=complete"
say "rootd=$ROOTD_SHA"
say "permissive=$PERM_SHA"
say "next: reboot, then run station-max.sh verify"
exit 0
