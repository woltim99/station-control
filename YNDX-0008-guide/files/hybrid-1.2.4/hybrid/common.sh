# Shared device-side helpers for the Station Max hybrid archive.
#
# POSIX sh only: this file is sourced by /system/bin/sh scripts.  It is
# trusted archive code, delivered to the staging directory and hash-checked
# by the host before execution.  It never sources install.conf and never
# evaluates configuration values as shell code.

STATION_MAX_COMMON=1
VERSION=1.2.4

DISPLAY_ID=PPR1.180610.011.5000000921
FINGERPRINT='Yandex/yandexstation_2/yandexstation_2:9/PPR1.180610.011/5000000921:user/release-keys'
KERNEL_RELEASE=4.9.113
KERNEL_PREFIX='Linux version 4.9.113 '

ROOTD_SHA=30ee1e1f92623322ccdc77a3ff1d548ebab4f3ee003d5fc3258b473b490a927f
PERM_SHA=f580ff0283a181936309d2d1fcf2dae0e7da6e21a7888bdc10e6675947d3746b
PERM_SIZE=85904
RC_SHA=0940cfb282167e54193a10126de0c9cc1e62a2390201884220a42788d0b1bbf4
BRIDGE_SHA=f53c3e127a95b8f78b0934ccea73382964d35fd7b3dd21b8b0ce258f50704474

BASE_DEFAULT=/data/local/tmp/station-max-hybrid
STATE_FILE=/data/local/tmp/rootd-op.state
LOCK_DIR=/data/local/tmp/rootd-op.lock
TMP_ROOT=/data/local/tmp
ODM=/odm

REL_ROOTD=bin/rootd
REL_PERM=bin/permissive
REL_RC=etc/init/rootd.rc
REL_CONF=etc/rootd.conf
REL_PATHS="$REL_ROOTD $REL_PERM $REL_RC $REL_CONF"

# Process names that mean a second root/bootstrap channel.  root-bridge sets
# its comm to esc36449 via prctl(PR_SET_NAME), so the binary name alone is not
# enough; status/verify/install/rollback must treat every one of these as a
# conflict.
SECOND_CHANNEL_NAMES='zzz_no_such_channel'

# Accepted prior installations.  A destination file outside this list and
# outside the operation state is never overwritten silently.
KNOWN_ROOTD_SHA="$ROOTD_SHA 4a6e9abb6b8b60b5a573b82e755fac2748a4fff284452f2e29aa277546f3508e 30ee1e1f92623322ccdc77a3ff1d548ebab4f3ee003d5fc3258b473b490a927f"
KNOWN_PERM_SHA="$PERM_SHA 4fff91e358fdc304fa8265d75439730ac241b01662d17761e175f87a161cf38d 93889361f49cd074e3b4daeb8c679e3f4e3dd9fd5f9873ae820cbf93e4d59346 ba9471ff75cf762e1bccf8bf9832bb9b9cc0a3a8976471b0a780a9b69e3e2307"
KNOWN_RC_SHA="$RC_SHA 0940cfb282167e54193a10126de0c9cc1e62a2390201884220a42788d0b1bbf4"

PROG=${PROG:-INSTALL}

say() { printf '%s\n' "$*"; }
err() { printf '%s: %s\n' "$PROG" "$*" >&2; }
fail() { printf '%s_FAIL: %s\n' "$PROG" "$*" >&2; exit 1; }

# ---------------------------------------------------------------- basics ---

sha256_file() {
    _sha_out=$(sha256sum "$1" 2>/dev/null) || return 1
    printf '%s\n' "$_sha_out" | awk '{print $1}'
}

require_hash() {
    _rh_file=$1
    _rh_want=$2
    _rh_label=${3:-$_rh_file}
    _rh_got=$(sha256_file "$_rh_file") || {
        err "cannot hash $_rh_label"
        return 1
    }
    [ "$_rh_got" = "$_rh_want" ] || {
        err "hash mismatch: $_rh_label (got $_rh_got want $_rh_want)"
        return 1
    }
    return 0
}

trim_value() {
    _tv=$1
    while :; do
        case $_tv in
            ' '*) _tv=${_tv#?} ;;
            '	'*) _tv=${_tv#?} ;;
            *) break ;;
        esac
    done
    while :; do
        case $_tv in
            *' ') _tv=${_tv%?} ;;
            *'	') _tv=${_tv%?} ;;
            *) break ;;
        esac
    done
    printf '%s' "$_tv"
}

# Strict dotted quad.  Rejects whitespace, newlines, signs, hex, an empty
# octet and ambiguous leading zeroes; accepts 0..255 in decimal only.
is_ipv4_strict() {
    case $1 in
        ''|.*|*.|*..*|*[!0-9.]*) return 1 ;;
    esac
    _ip_oldifs=$IFS
    IFS=.
    # The value contains digits and dots only, so pathname expansion cannot
    # introduce anything but the literal octets.
    set -- $1
    IFS=$_ip_oldifs
    [ $# -eq 4 ] || return 1
    for _ip_octet in "$@"; do
        case $_ip_octet in
            ''|*[!0-9]*) return 1 ;;
            0) ;;
            0*) return 1 ;;
        esac
        [ ${#_ip_octet} -le 3 ] || return 1
        [ "$_ip_octet" -le 255 ] 2>/dev/null || return 1
    done
    return 0
}

# Canonical form is the input form for the strict parser above.
canon_ipv4() {
    is_ipv4_strict "$1" || return 1
    printf '%s' "$1"
}

# A safe staging/backup path below /data/local/tmp: no root, no dot
# components, no spaces or shell metacharacters, no trailing slash.
is_safe_tmp_path() {
    case $1 in
        /data/local/tmp/*) ;;
        *) return 1 ;;
    esac
    case $1 in
        */|*//*|*' '*|*'	'*) return 1 ;;
    esac
    _sp_len=${#1}
    [ "$_sp_len" -le 240 ] || return 1
    _sp_rest=${1#/data/local/tmp/}
    [ -n "$_sp_rest" ] || return 1
    case $_sp_rest in
        *[!A-Za-z0-9._/-]*) return 1 ;;
    esac
    _sp_oldifs=$IFS
    IFS=/
    set -- $_sp_rest
    IFS=$_sp_oldifs
    for _sp_comp in "$@"; do
        case $_sp_comp in
            ''|.|..) return 1 ;;
        esac
    done
    return 0
}

is_backup_path() {
    is_safe_tmp_path "$1" || return 1
    case ${1##*/} in
        rootd-backup-*) return 0 ;;
        *) return 1 ;;
    esac
}

# True when every component of PATH below / is not a symlink.  Missing
# components are allowed here; callers check existence separately.
no_symlink_components() {
    _ns_rest=${1#/}
    _ns_prefix=
    _ns_oldifs=$IFS
    IFS=/
    set -- $_ns_rest
    IFS=$_ns_oldifs
    for _ns_comp in "$@"; do
        _ns_prefix="$_ns_prefix/$_ns_comp"
        [ -L "$_ns_prefix" ] && return 1
    done
    return 0
}

file_is_regular() {
    [ -f "$1" ] && [ ! -L "$1" ]
}

dir_is_regular() {
    [ -d "$1" ] && [ ! -L "$1" ]
}

# ------------------------------------------------------------- config -----

# Parse the three-key install.conf as data.  Unknown keys, duplicate keys,
# blank lines, CR/LF tricks and malformed values are all rejected.  No
# shell evaluation happens anywhere in this function.
parse_install_conf() {
    _pc_file=$1
    [ -f "$_pc_file" ] && [ ! -L "$_pc_file" ] || {
        err "config is not a regular file: $_pc_file"
        return 1
    }
    _pc_r=0
    _pc_d=0
    _pc_i=0
    ROUTER_IP=
    DNS_IP=
    IPV6=
    while IFS= read -r _pc_line || [ -n "$_pc_line" ]; do
        case $_pc_line in
            '') err "blank line in config"; return 1 ;;
        esac
        [ ${#_pc_line} -le 160 ] || {
            err "config line too long"
            return 1
        }
        case $_pc_line in
            ROUTER_IP=*|DNS_IP=*|IPV6=*) ;;
            *) err "unknown config line"; return 1 ;;
        esac
        _pc_key=${_pc_line%%=*}
        _pc_val=${_pc_line#*=}
        case $_pc_val in
            *'='*) err "extra '=' in config value"; return 1 ;;
            *' '*|*'	'*) err "whitespace in config value"; return 1 ;;
        esac
        case $_pc_key in
            ROUTER_IP)
                [ "$_pc_r" -eq 0 ] || { err "duplicate ROUTER_IP"; return 1; }
                is_ipv4_strict "$_pc_val" || { err "ROUTER_IP is not IPv4"; return 1; }
                ROUTER_IP=$_pc_val
                _pc_r=1
                ;;
            DNS_IP)
                [ "$_pc_d" -eq 0 ] || { err "duplicate DNS_IP"; return 1; }
                is_ipv4_strict "$_pc_val" || { err "DNS_IP is not IPv4"; return 1; }
                DNS_IP=$_pc_val
                _pc_d=1
                ;;
            IPV6)
                [ "$_pc_i" -eq 0 ] || { err "duplicate IPV6"; return 1; }
                case $_pc_val in
                    none|drop) ;;
                    *) err "IPV6 must be none or drop"; return 1 ;;
                esac
                IPV6=$_pc_val
                _pc_i=1
                ;;
        esac
    done < "$_pc_file"
    [ "$_pc_r" -eq 1 ] && [ "$_pc_d" -eq 1 ] && [ "$_pc_i" -eq 1 ] || {
        err "config must contain ROUTER_IP, DNS_IP and IPV6 exactly once"
        return 1
    }
    return 0
}

# Validate an already installed rootd.conf.  This mirrors the daemon parser:
# comments and blank lines are allowed, keys are lan.router_ip / lan.dns_ip /
# lan.ipv6, duplicates and unknown keys are rejected, values must be valid.
parse_rootd_conf() {
    _rc_file=$1
    _CR_CHAR=$(printf '\r')
    [ -f "$_rc_file" ] && [ ! -L "$_rc_file" ] || return 1
    _rc_r=0
    _rc_d=0
    _rc_i=0
    CFG_ROUTER=
    CFG_DNS=
    CFG_IPV6=
    while IFS= read -r _rc_line || [ -n "$_rc_line" ]; do
        case $_rc_line in
            *"$_CR_CHAR"*) return 1 ;;
        esac
        _rc_line=$(trim_value "$_rc_line")
        case $_rc_line in
            ''|'#'*) continue ;;
        esac
        case $_rc_line in
            *=*) ;;
            *) return 1 ;;
        esac
        _rc_key=$(trim_value "${_rc_line%%=*}")
        _rc_val=$(trim_value "${_rc_line#*=}")
        case $_rc_key in
            lan.router_ip)
                [ "$_rc_r" -eq 0 ] || return 1
                is_ipv4_strict "$_rc_val" || return 1
                CFG_ROUTER=$_rc_val
                _rc_r=1
                ;;
            lan.dns_ip)
                [ "$_rc_d" -eq 0 ] || return 1
                is_ipv4_strict "$_rc_val" || return 1
                CFG_DNS=$_rc_val
                _rc_d=1
                ;;
            lan.ipv6)
                [ "$_rc_i" -eq 0 ] || return 1
                case $_rc_val in
                    none|drop) ;;
                    *) return 1 ;;
                esac
                CFG_IPV6=$_rc_val
                _rc_i=1
                ;;
            *) return 1 ;;
        esac
    done < "$_rc_file"
    [ "$_rc_r" -eq 1 ] && [ "$_rc_d" -eq 1 ] && [ "$_rc_i" -eq 1 ] || return 1
    return 0
}

# --------------------------------------------------------------- gates ----

build_gate() {
    _bg_build=$(getprop ro.build.display.id) || {
        err "cannot read ro.build.display.id"
        return 1
    }
    _bg_fp=$(getprop ro.build.fingerprint) || {
        err "cannot read ro.build.fingerprint"
        return 1
    }
    [ "$_bg_build" = "$DISPLAY_ID" ] || {
        err "unsupported build: $_bg_build"
        return 1
    }
    [ "$_bg_fp" = "$FINGERPRINT" ] || {
        err "fingerprint mismatch"
        return 1
    }
    _bg_kernel=$(cat /proc/version 2>/dev/null) || {
        err "cannot read /proc/version"
        return 1
    }
    case $_bg_kernel in
        "$KERNEL_PREFIX"*) ;;
        *) err "kernel mismatch: $(uname -r 2>/dev/null || echo unknown)"; return 1 ;;
    esac
    _bg_uname=$(uname -r 2>/dev/null) || {
        err "cannot read uname -r"
        return 1
    }
    [ "$_bg_uname" = "$KERNEL_RELEASE" ] || {
        err "kernel release mismatch: $_bg_uname"
        return 1
    }
    return 0
}

root_gate() {
    _rg_uid=$(id -u) || {
        err "cannot read uid"
        return 1
    }
    [ "$_rg_uid" = 0 ] || {
        err "not uid 0; use the bootstrap root shell or the rootd queue"
        return 1
    }
    _rg_enforce=$(getenforce) || {
        err "cannot read SELinux state"
        return 1
    }
    [ "$_rg_enforce" = Permissive ] || {
        err "SELinux is $_rg_enforce; run hybrid/permissive esc-permissive first"
        return 1
    }
    return 0
}

# -------------------------------------------------------------- device ----

# Echo the mount options for /odm, one line per matching mount entry.
# Returns non-zero when mount failed or /odm is not mounted.
odm_mount_options() {
    _om_out=$(mount 2>/dev/null) || return 1
    _om_lines=$(printf '%s\n' "$_om_out" | grep -F ' on /odm ' 2>/dev/null) || true
    [ -n "$_om_lines" ] || return 1
    printf '%s\n' "$_om_lines" | sed -n 's/.*(\([^)]*\)).*/\1/p'
}
has_mount_opt() {
    _hm_opts=$1
    _hm_want=$2
    _hm_oldifs=$IFS
    IFS=,
    for _hm_opt in $_hm_opts; do
        if [ "$_hm_opt" = "$_hm_want" ]; then
            IFS=$_hm_oldifs
            return 0
        fi
    done
    IFS=$_hm_oldifs
    return 1
}

# Parse `mount` output from stdin and echo ro or rw for /odm.  One rw entry
# wins over ro.  Fails when /odm is not mounted or has neither option.  This
# is the only mount-state parser: the host feeds it the captured text and the
# device feeds it a local `mount`, so the two sides cannot drift apart.
parse_mount_state() {
    _pms_line=
    _pms_opts=
    _pms_state=unknown
    _pms_seen=0
    while IFS= read -r _pms_line; do
        case $_pms_line in
            *' on /odm '*) ;;
            *) continue ;;
        esac
        _pms_seen=1
        _pms_opts=$(printf '%s\n' "$_pms_line" | sed -n 's/.*(\([^)]*\)).*/\1/p')
        if has_mount_opt "$_pms_opts" rw; then
            _pms_state=rw
        elif has_mount_opt "$_pms_opts" ro && [ "$_pms_state" = unknown ]; then
            _pms_state=ro
        fi
    done
    [ "$_pms_seen" = 1 ] && [ "$_pms_state" != unknown ] || return 1
    printf '%s' "$_pms_state"
}

# Echo ro, rw or absent for /odm on this device.
odm_state() {
    _os_out=$(mount 2>/dev/null) || {
        printf 'absent'
        return 0
    }
    _os_state=$(printf '%s\n' "$_os_out" | parse_mount_state) || {
        printf 'absent'
        return 0
    }
    printf '%s' "$_os_state"
}

# Parse ps output from stdin into process names, one per line.  mode=name is
# `ps -A -o NAME` (header plus one column), mode=full is `ps -A` (name is the
# last column).  One parser for the host and the device; callers still decide
# whether ps itself succeeded and reject an empty result.
parse_process_names() {
    case ${1:-} in
        name) sed '1{/^[[:space:]]*NAME[[:space:]]*$/d;}' | sed 's/[[:space:]].*$//' | sed '/^$/d' ;;
        full) sed '1d' | sed 's/.*[[:space:]]//' | sed '/^$/d' ;;
        *) return 1 ;;
    esac
}

# Echo process names, one per line.  Fails when ps itself fails: an empty
# answer must never be mistaken for "no such process".
process_names() {
    if _pn_out=$(ps -A -o NAME 2>/dev/null); then
        _pn_names=$(printf '%s\n' "$_pn_out" | parse_process_names name)
    elif _pn_out=$(ps -A 2>/dev/null); then
        _pn_names=$(printf '%s\n' "$_pn_out" | parse_process_names full)
    else
        return 1
    fi
    [ -n "$_pn_names" ] || return 1
    printf '%s\n' "$_pn_names"
}

is_second_channel_name() {
    return 1
}

# Count a process name in newline-separated output.  POSIX shell only: the
# station's toybox awk is not used for security-relevant gates.
count_named_process() {
    _cnp_names=$1
    _cnp_want=$2
    _cnp_n=0
    _cnp_oldifs=$IFS
    IFS='
'
    for _cnp_name in $_cnp_names; do
        IFS=$_cnp_oldifs
        [ "$_cnp_name" = "$_cnp_want" ] && _cnp_n=$((_cnp_n + 1))
        IFS='
'
    done
    IFS=$_cnp_oldifs
    printf '%s' "$_cnp_n"
}

count_second_channels() {
    _csc_names=$1
    _csc_n=0
    _csc_oldifs=$IFS
    IFS='
'
    for _csc_name in $_csc_names; do
        IFS=$_csc_oldifs
        if is_second_channel_name "$_csc_name"; then
            _csc_n=$((_csc_n + 1))
        fi
        IFS='
'
    done
    IFS=$_csc_oldifs
    printf '%s' "$_csc_n"
}

count_process() {
    _cp_name=$1
    _cp_names=$(process_names) || return 1
    count_named_process "$_cp_names" "$_cp_name"
}

require_commands() {
    _rq_missing=
    for _rq_cmd in sh sha256sum awk sed grep tr cut head tail wc date cp mv rm mkdir chmod chown chcon sync mount df ps getprop getenforce id stat readlink mktemp cat ls; do
        command -v "$_rq_cmd" >/dev/null 2>&1 || _rq_missing="$_rq_missing $_rq_cmd"
    done
    if [ -n "$_rq_missing" ]; then
        err "required device commands missing:$_rq_missing"
        return 1
    fi
    return 0
}

# -------------------------------------------------------------- state -----

state_reset() {
    ST_VERSION=1
    ST_OPERATION=
    ST_OPERATION_ID=
    ST_PHASE=
    ST_BACKUP=
    ST_BOOT_ID=
    ST_MANIFEST_SHA=
    ST_STAGING=
    ST_STAGING_NONCE=
    ST_INSTALLED_ROOTD=
    ST_INSTALLED_PERM=
    ST_INSTALLED_RC=
    ST_INSTALLED_CONF=
    ST_TARGET_ROOTD=
    ST_TARGET_PERM=
    ST_TARGET_RC=
    ST_TARGET_CONF=
    ST_RESTORED_ROOTD=
    ST_RESTORED_PERM=
    ST_RESTORED_RC=
    ST_RESTORED_CONF=
    ST_EXPECT=
    ST_REASON=
    ST_UPDATED=
}

state_field_safe() {
    case $1 in
        *[!A-Za-z0-9._:/@+=-]*) return 1 ;;
    esac
    return 0
}

state_hash_safe() {
    case $1 in
        '') return 0 ;;
        *[!0-9a-f]*) return 1 ;;
    esac
    [ ${#1} -eq 64 ]
}

state_phase_known() {
    case $1 in
        ''|install_started|backup_started|backup_complete|writing|files_written|verified|complete|failed|failed_odm_rw|rollback_restoring|rollback_complete|rollback_failed) return 0 ;;
        *) return 1 ;;
    esac
}

# Load and validate the operation state.  The path argument is for local
# host-side validation of the text read through ADB; on the device it
# defaults to the published state file.
#
# STATE_STATUS is missing, valid or corrupt.  A corrupt state is never
# reset or overwritten: callers must stop.  This is deliberately stricter
# than the old loader, which accepted partial files and silently lost the
# original backup path.
state_probe() {
    _sp_file=${1:-$STATE_FILE}
    state_reset
    STATE_STATUS=missing
    if [ ! -e "$_sp_file" ] && [ ! -L "$_sp_file" ]; then
        return 0
    fi
    if [ -L "$_sp_file" ] || [ ! -f "$_sp_file" ]; then
        STATE_STATUS=corrupt
        err "operation state is not a regular file: $_sp_file"
        return 1
    fi
    _sp_size=$(wc -c < "$_sp_file" 2>/dev/null | tr -d '[:space:]')
    case $_sp_size in
        ''|*[!0-9]*) STATE_STATUS=corrupt; err "cannot size operation state"; return 1 ;;
    esac
    [ "$_sp_size" -gt 0 ] && [ "$_sp_size" -le 4096 ] || {
        STATE_STATUS=corrupt
        err "operation state is empty or oversized"
        return 1
    }
    _sp_seen=' '
    _sp_version=
    while IFS= read -r _sp_line || [ -n "$_sp_line" ]; do
        case $_sp_line in
            ''|*'='*) ;;
            *) STATE_STATUS=corrupt; err "malformed operation state line"; return 1 ;;
        esac
        [ -n "$_sp_line" ] || { STATE_STATUS=corrupt; err "blank operation state line"; return 1; }
        _sp_key=${_sp_line%%=*}
        _sp_val=${_sp_line#*=}
        case $_sp_key in
            version|operation|operation_id|phase|backup|boot_id|manifest_sha256|staging|staging_nonce|installed_rootd|installed_permissive|installed_rc|installed_conf|target_rootd|target_permissive|target_rc|target_conf|restored_rootd|restored_permissive|restored_rc|restored_conf|expect|reason|updated) ;;
            *) STATE_STATUS=corrupt; err "unknown operation state key: $_sp_key"; return 1 ;;
        esac
        case " $_sp_seen " in
            *" $_sp_key "*) STATE_STATUS=corrupt; err "duplicate operation state key: $_sp_key"; return 1 ;;
        esac
        _sp_seen="$_sp_seen$_sp_key "
        case $_sp_key in
            installed_rootd|installed_permissive|installed_rc|installed_conf|target_rootd|target_permissive|target_rc|target_conf|restored_rootd|restored_permissive|restored_rc|restored_conf|manifest_sha256)
                state_hash_safe "$_sp_val" || { STATE_STATUS=corrupt; err "bad hash in operation state: $_sp_key"; return 1; }
                ;;
            backup|staging)
                [ -z "$_sp_val" ] || is_safe_tmp_path "$_sp_val" || { STATE_STATUS=corrupt; err "unsafe path in operation state: $_sp_key"; return 1; }
                ;;
            operation_id|staging_nonce)
                [ ${#_sp_val} -le 64 ] || { STATE_STATUS=corrupt; err "oversized operation id"; return 1; }
                case $_sp_val in *[!A-Za-z0-9._-]*) STATE_STATUS=corrupt; err "bad operation id"; return 1 ;; esac
                ;;
            *)
                state_field_safe "$_sp_val" || { STATE_STATUS=corrupt; err "bad value in operation state: $_sp_key"; return 1; }
                ;;
        esac
        case $_sp_key in
            version) _sp_version=$_sp_val; ST_VERSION=$_sp_val ;;
            operation) ST_OPERATION=$_sp_val ;;
            operation_id) ST_OPERATION_ID=$_sp_val ;;
            phase) ST_PHASE=$_sp_val ;;
            backup) ST_BACKUP=$_sp_val ;;
            boot_id) ST_BOOT_ID=$_sp_val ;;
            manifest_sha256) ST_MANIFEST_SHA=$_sp_val ;;
            staging) ST_STAGING=$_sp_val ;;
            staging_nonce) ST_STAGING_NONCE=$_sp_val ;;
            installed_rootd) ST_INSTALLED_ROOTD=$_sp_val ;;
            installed_permissive) ST_INSTALLED_PERM=$_sp_val ;;
            installed_rc) ST_INSTALLED_RC=$_sp_val ;;
            installed_conf) ST_INSTALLED_CONF=$_sp_val ;;
            target_rootd) ST_TARGET_ROOTD=$_sp_val ;;
            target_permissive) ST_TARGET_PERM=$_sp_val ;;
            target_rc) ST_TARGET_RC=$_sp_val ;;
            target_conf) ST_TARGET_CONF=$_sp_val ;;
            restored_rootd) ST_RESTORED_ROOTD=$_sp_val ;;
            restored_permissive) ST_RESTORED_PERM=$_sp_val ;;
            restored_rc) ST_RESTORED_RC=$_sp_val ;;
            restored_conf) ST_RESTORED_CONF=$_sp_val ;;
            expect) ST_EXPECT=$_sp_val ;;
            reason) ST_REASON=$_sp_val ;;
            updated) ST_UPDATED=$_sp_val ;;
        esac
    done < "$_sp_file"
    [ "$_sp_version" = 1 ] || { STATE_STATUS=corrupt; err "unsupported operation state version"; return 1; }
    case $ST_OPERATION in
        ''|install|rollback) ;;
        *) STATE_STATUS=corrupt; err "unknown operation in state"; return 1 ;;
    esac
    state_phase_known "$ST_PHASE" || { STATE_STATUS=corrupt; err "unknown phase in state"; return 1; }
    if [ -n "$ST_PHASE" ] && [ -z "$ST_OPERATION" ]; then
        STATE_STATUS=corrupt
        err "operation state has a phase without an operation"
        return 1
    fi
    case $ST_EXPECT in ''|hybrid|stock|partial) ;; *) STATE_STATUS=corrupt; err "unknown expect in state"; return 1 ;; esac
    STATE_STATUS=valid
    return 0
}

state_commit() {
    _sc_tmp="$STATE_FILE.new.$$"
    _sc_cur_boot=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || echo unknown)
    ST_BOOT_ID=$_sc_cur_boot
    ST_UPDATED=$(date '+%Y-%m-%dT%H:%M:%S' 2>/dev/null || echo unknown)
    {
        printf 'version=%s\n' "${ST_VERSION:-1}"
        [ -n "$ST_OPERATION" ] && printf 'operation=%s\n' "$ST_OPERATION"
        [ -n "$ST_OPERATION_ID" ] && printf 'operation_id=%s\n' "$ST_OPERATION_ID"
        [ -n "$ST_PHASE" ] && printf 'phase=%s\n' "$ST_PHASE"
        [ -n "$ST_BACKUP" ] && printf 'backup=%s\n' "$ST_BACKUP"
        [ -n "$ST_BOOT_ID" ] && printf 'boot_id=%s\n' "$ST_BOOT_ID"
        [ -n "$ST_MANIFEST_SHA" ] && printf 'manifest_sha256=%s\n' "$ST_MANIFEST_SHA"
        [ -n "$ST_STAGING" ] && printf 'staging=%s\n' "$ST_STAGING"
        [ -n "$ST_STAGING_NONCE" ] && printf 'staging_nonce=%s\n' "$ST_STAGING_NONCE"
        [ -n "$ST_INSTALLED_ROOTD" ] && printf 'installed_rootd=%s\n' "$ST_INSTALLED_ROOTD"
        [ -n "$ST_INSTALLED_PERM" ] && printf 'installed_permissive=%s\n' "$ST_INSTALLED_PERM"
        [ -n "$ST_INSTALLED_RC" ] && printf 'installed_rc=%s\n' "$ST_INSTALLED_RC"
        [ -n "$ST_INSTALLED_CONF" ] && printf 'installed_conf=%s\n' "$ST_INSTALLED_CONF"
        [ -n "$ST_TARGET_ROOTD" ] && printf 'target_rootd=%s\n' "$ST_TARGET_ROOTD"
        [ -n "$ST_TARGET_PERM" ] && printf 'target_permissive=%s\n' "$ST_TARGET_PERM"
        [ -n "$ST_TARGET_RC" ] && printf 'target_rc=%s\n' "$ST_TARGET_RC"
        [ -n "$ST_TARGET_CONF" ] && printf 'target_conf=%s\n' "$ST_TARGET_CONF"
        [ -n "$ST_RESTORED_ROOTD" ] && printf 'restored_rootd=%s\n' "$ST_RESTORED_ROOTD"
        [ -n "$ST_RESTORED_PERM" ] && printf 'restored_permissive=%s\n' "$ST_RESTORED_PERM"
        [ -n "$ST_RESTORED_RC" ] && printf 'restored_rc=%s\n' "$ST_RESTORED_RC"
        [ -n "$ST_RESTORED_CONF" ] && printf 'restored_conf=%s\n' "$ST_RESTORED_CONF"
        [ -n "$ST_EXPECT" ] && printf 'expect=%s\n' "$ST_EXPECT"
        [ -n "$ST_REASON" ] && printf 'reason=%s\n' "$ST_REASON"
        printf 'updated=%s\n' "$ST_UPDATED"
    } > "$_sc_tmp" || return 1
    chown 0:0 "$_sc_tmp" 2>/dev/null || true
    chmod 0644 "$_sc_tmp" 2>/dev/null || true
    sync 2>/dev/null || true
    mv -f "$_sc_tmp" "$STATE_FILE" || return 1
    sync 2>/dev/null || true
    return 0
}

state_phase() {
    printf '%s' "$ST_PHASE"
}

# --------------------------------------------------------------- lock -----

lock_acquire() {
    _la_op=$1
    _la_boot=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || echo unknown)
    if mkdir "$LOCK_DIR" 2>/dev/null; then
        printf '%s\n' "$$" > "$LOCK_DIR/pid"
        printf '%s\n' "$_la_boot" > "$LOCK_DIR/boot_id"
        printf '%s\n' "$_la_op" > "$LOCK_DIR/operation"
        chmod 0700 "$LOCK_DIR" 2>/dev/null || true
        LOCK_HELD=1
        return 0
    fi
    _la_pid=$(cat "$LOCK_DIR/pid" 2>/dev/null || echo 0)
    _la_owner_boot=$(cat "$LOCK_DIR/boot_id" 2>/dev/null || echo unknown)
    if [ -n "$_la_owner_boot" ] && [ "$_la_owner_boot" != unknown ] && [ "$_la_owner_boot" != "$_la_boot" ]; then
        [ -L "$LOCK_DIR" ] && { err "refusing symlink lock: $LOCK_DIR"; return 1; }
        rm -rf "$LOCK_DIR" 2>/dev/null || true
        if mkdir "$LOCK_DIR" 2>/dev/null; then
            printf '%s\n' "$$" > "$LOCK_DIR/pid"
            printf '%s\n' "$_la_boot" > "$LOCK_DIR/boot_id"
            printf '%s\n' "$_la_op" > "$LOCK_DIR/operation"
            chmod 0700 "$LOCK_DIR" 2>/dev/null || true
            LOCK_HELD=1
            return 0
        fi
    fi
    if [ "$_la_pid" != 0 ] && kill -0 "$_la_pid" 2>/dev/null; then
        err "another operation is in progress: pid=$_la_pid operation=$(cat "$LOCK_DIR/operation" 2>/dev/null || echo unknown)"
        return 1
    fi
    [ -L "$LOCK_DIR" ] && { err "refusing symlink lock: $LOCK_DIR"; return 1; }
    rm -rf "$LOCK_DIR" 2>/dev/null || true
    if mkdir "$LOCK_DIR" 2>/dev/null; then
        printf '%s\n' "$$" > "$LOCK_DIR/pid"
        printf '%s\n' "$_la_boot" > "$LOCK_DIR/boot_id"
        printf '%s\n' "$_la_op" > "$LOCK_DIR/operation"
        chmod 0700 "$LOCK_DIR" 2>/dev/null || true
        LOCK_HELD=1
        return 0
    fi
    err "cannot acquire operation lock $LOCK_DIR"
    return 1
}

lock_release() {
    [ "${LOCK_HELD:-0}" = 1 ] || return 0
    _lr_pid=$(cat "$LOCK_DIR/pid" 2>/dev/null || echo 0)
    if [ "$_lr_pid" = "$$" ]; then
        rm -rf "$LOCK_DIR" 2>/dev/null || true
    fi
    LOCK_HELD=0
}

# ------------------------------------------------------------- backup -----

rel_backup_name() {
    case $1 in
        bin/rootd) printf 'bin_rootd' ;;
        bin/permissive) printf 'bin_permissive' ;;
        etc/init/rootd.rc) printf 'etc_init_rootd.rc' ;;
        etc/rootd.conf) printf 'etc_rootd.conf' ;;
        *) return 1 ;;
    esac
}

rel_known_hash() {
    case $1 in
        bin/rootd) printf '%s' "$ROOTD_SHA" ;;
        bin/permissive) printf '%s' "$PERM_SHA" ;;
        etc/init/rootd.rc) printf '%s' "$RC_SHA" ;;
        *) return 1 ;;
    esac
}

# Look up one relative path in a validated manifest.  Echoes "present SHA"
# or "absent"; returns 1 when the path is absent from the manifest.
manifest_lookup() {
    _ml_bk=$1
    _ml_rel=$2
    while IFS=' ' read -r _ml_kind _ml_path _ml_sha _ml_extra || [ -n "${_ml_kind:-}" ]; do
        [ "$_ml_path" = "$_ml_rel" ] || continue
        case $_ml_kind in
            present)
                [ -n "$_ml_sha" ] && [ -z "$_ml_extra" ] || return 1
                printf 'present %s' "$_ml_sha"
                return 0
                ;;
            absent)
                [ -z "$_ml_sha" ] && [ -z "$_ml_extra" ] || return 1
                printf 'absent'
                return 0
                ;;
            *) return 1 ;;
        esac
    done < "$_ml_bk/MANIFEST" 2>/dev/null
    return 1
}

# Full manifest validation.  With ALLOW_UNMARKED=1 an old explicit backup
# without COMPLETE is accepted, but every available datum is still checked.
# Sets MV_MANIFEST_SHA on success.
manifest_validate() {
    _mv_bk=$1
    _mv_allow=${2:-0}
    MV_MANIFEST_SHA=
    dir_is_regular "$_mv_bk" || { err "backup is not a regular directory: $_mv_bk"; return 1; }
    file_is_regular "$_mv_bk/MANIFEST" || { err "missing regular MANIFEST"; return 1; }
    _mv_size=$(wc -c < "$_mv_bk/MANIFEST" 2>/dev/null | awk '{print $1}')
    [ -n "$_mv_size" ] && [ "$_mv_size" -gt 0 ] && [ "$_mv_size" -le 4096 ] || {
        err "MANIFEST is empty or oversized"
        return 1
    }
    if [ -f "$_mv_bk/COMPLETE" ]; then
        file_is_regular "$_mv_bk/COMPLETE" || { err "COMPLETE is not regular"; return 1; }
        _mv_marker_sha=$(sed -n 's/^manifest_sha256=//p' "$_mv_bk/COMPLETE" 2>/dev/null)
        [ -n "$_mv_marker_sha" ] || { err "COMPLETE has no manifest_sha256"; return 1; }
        _mv_did=$(sed -n 's/^display_id=//p' "$_mv_bk/COMPLETE" 2>/dev/null)
        _mv_fp=$(sed -n 's/^fingerprint=//p' "$_mv_bk/COMPLETE" 2>/dev/null)
        _mv_kern=$(sed -n 's/^kernel=//p' "$_mv_bk/COMPLETE" 2>/dev/null)
        [ -n "$_mv_did" ] || { err "COMPLETE has no display_id"; return 1; }
        [ -n "$_mv_fp" ] || { err "COMPLETE has no fingerprint"; return 1; }
        [ -n "$_mv_kern" ] || { err "COMPLETE has no kernel"; return 1; }
        [ "$_mv_did" = "$DISPLAY_ID" ] || { err "backup was created for a different build: $_mv_did"; return 1; }
        [ "$_mv_fp" = "$FINGERPRINT" ] || { err "backup was created for a different fingerprint"; return 1; }
        [ "$_mv_kern" = "$KERNEL_RELEASE" ] || { err "backup was created for a different kernel: $_mv_kern"; return 1; }
        _mv_op=$(sed -n 's/^operation_id=//p' "$_mv_bk/COMPLETE" 2>/dev/null)
        [ -z "$_mv_op" ] || {
            case $_mv_op in *[!A-Za-z0-9._-]*) err "bad operation_id in COMPLETE"; return 1 ;; esac
            [ ${#_mv_op} -le 64 ] || { err "oversized operation_id in COMPLETE"; return 1; }
        }
    else
        [ "$_mv_allow" = 1 ] || {
            err "backup has no COMPLETE marker; pass --allow-unmarked only for an explicitly named old backup"
            return 1
        }
        _mv_marker_sha=
    fi
    MV_MANIFEST_SHA=$(sha256_file "$_mv_bk/MANIFEST") || {
        err "cannot hash MANIFEST"
        return 1
    }
    if [ -n "$_mv_marker_sha" ] && [ "$_mv_marker_sha" != "$MV_MANIFEST_SHA" ]; then
        err "MANIFEST hash does not match COMPLETE"
        return 1
    fi
    _mv_lines=$(wc -l < "$_mv_bk/MANIFEST" 2>/dev/null | awk '{print $1}')
    [ "$_mv_lines" = 4 ] || { err "MANIFEST must have exactly 4 lines (got ${_mv_lines:-unknown})"; return 1; }
    _mv_cr=$(printf '\r')
    while IFS= read -r _mv_raw || [ -n "$_mv_raw" ]; do
        case $_mv_raw in
            ' '*|*' '|'	'*|*'	'|*"$_mv_cr"*)
                err "MANIFEST has leading/trailing whitespace or CR"
                return 1
                ;;
        esac
    done < "$_mv_bk/MANIFEST"
    _mv_seen=' '
    _mv_count=0
    while IFS=' ' read -r _mv_kind _mv_rel _mv_sha _mv_extra || [ -n "${_mv_kind:-}" ]; do
        _mv_count=$((_mv_count + 1))
        case $_mv_kind in
            present|absent) ;;
            *) err "bad MANIFEST kind: $_mv_kind"; return 1 ;;
        esac
        case " $REL_PATHS " in
            *" $_mv_rel "*) ;;
            *) err "unknown path in MANIFEST: $_mv_rel"; return 1 ;;
        esac
        case $_mv_seen in
            *" $_mv_rel "*) err "duplicate path in MANIFEST: $_mv_rel"; return 1 ;;
        esac
        case $_mv_rel in
            *..*|*' '*) err "unsafe path in MANIFEST: $_mv_rel"; return 1 ;;
        esac
        _mv_bname=$(rel_backup_name "$_mv_rel") || { err "no backup name for $_mv_rel"; return 1; }
        if [ "$_mv_kind" = present ]; then
            case $_mv_sha in
                [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
                *) err "bad SHA256 for $_mv_rel"; return 1 ;;
            esac
            [ -z "$_mv_extra" ] || { err "extra fields for present $_mv_rel"; return 1; }
            file_is_regular "$_mv_bk/$_mv_bname" || { err "missing backup file for $_mv_rel"; return 1; }
            _mv_got=$(sha256_file "$_mv_bk/$_mv_bname") || { err "cannot hash backup $_mv_rel"; return 1; }
            [ "$_mv_got" = "$_mv_sha" ] || { err "backup hash mismatch: $_mv_rel"; return 1; }
        else
            [ -z "$_mv_sha" ] && [ -z "$_mv_extra" ] || { err "extra fields for absent $_mv_rel"; return 1; }
        fi
        _mv_seen="$_mv_seen$_mv_rel "
    done < "$_mv_bk/MANIFEST"
    [ "$_mv_count" = 4 ] || { err "MANIFEST has wrong number of entries"; return 1; }
    return 0
}

# ------------------------------------------------------ operation tuples ---

# Backups published by a crashed install whose state update did not happen.
# Only a COMPLETE marker carrying the exact operation_id is considered.
find_orphan_backups() {
    _fob_op=$1
    [ -n "$_fob_op" ] || return 0
    for _fob_dir in /data/local/tmp/rootd-backup-*; do
        [ -d "$_fob_dir" ] && [ ! -L "$_fob_dir" ] || continue
        [ -f "$_fob_dir/COMPLETE" ] && [ ! -L "$_fob_dir/COMPLETE" ] || continue
        _fob_marker=$(sed -n 's/^operation_id=//p' "$_fob_dir/COMPLETE" 2>/dev/null | head -n 1)
        [ "$_fob_marker" = "$_fob_op" ] && printf '%s\n' "$_fob_dir"
    done
    return 0
}

odm_hash_or_absent() {
    _oho_path=$1
    if [ -e "$_oho_path" ] || [ -L "$_oho_path" ]; then
        [ -L "$_oho_path" ] && return 1
        [ -f "$_oho_path" ] || return 1
        sha256_file "$_oho_path"
    else
        printf 'ABSENT'
    fi
}

current_odm_tuple() {
    _cot_out=
    for _cot_rel in $REL_PATHS; do
        _cot_hash=$(odm_hash_or_absent "/odm/$_cot_rel") || return 1
        _cot_out="$_cot_out${_cot_out:+ }$_cot_hash"
    done
    printf '%s' "$_cot_out"
}

state_hash_for_rel() {
    _shr_kind=$1
    _shr_rel=$2
    case $_shr_kind:$_shr_rel in
        installed:bin/rootd) printf '%s' "$ST_INSTALLED_ROOTD" ;;
        installed:bin/permissive) printf '%s' "$ST_INSTALLED_PERM" ;;
        installed:etc/init/rootd.rc) printf '%s' "$ST_INSTALLED_RC" ;;
        installed:etc/rootd.conf) printf '%s' "$ST_INSTALLED_CONF" ;;
        target:bin/rootd) printf '%s' "$ST_TARGET_ROOTD" ;;
        target:bin/permissive) printf '%s' "$ST_TARGET_PERM" ;;
        target:etc/init/rootd.rc) printf '%s' "$ST_TARGET_RC" ;;
        target:etc/rootd.conf) printf '%s' "$ST_TARGET_CONF" ;;
        restored:bin/rootd) printf '%s' "$ST_RESTORED_ROOTD" ;;
        restored:bin/permissive) printf '%s' "$ST_RESTORED_PERM" ;;
        restored:etc/init/rootd.rc) printf '%s' "$ST_RESTORED_RC" ;;
        restored:etc/rootd.conf) printf '%s' "$ST_RESTORED_CONF" ;;
        *) return 1 ;;
    esac
}

manifest_hash_for_rel() {
    _mhr_bk=$1
    _mhr_rel=$2
    _mhr_lookup=$(manifest_lookup "$_mhr_bk" "$_mhr_rel") || return 1
    case $_mhr_lookup in
        present\ *) printf '%s' "${_mhr_lookup#present }" ;;
        absent) printf 'ABSENT' ;;
        *) return 1 ;;
    esac
}

tuple_from_kind() {
    _tfk_kind=$1
    _tfk_bk=$2
    _tfk_out=
    for _tfk_rel in $REL_PATHS; do
        case $_tfk_kind in
            backup) _tfk_hash=$(manifest_hash_for_rel "$_tfk_bk" "$_tfk_rel") || return 1 ;;
            *) _tfk_hash=$(state_hash_for_rel "$_tfk_kind" "$_tfk_rel") || return 1 ;;
        esac
        _tfk_out="$_tfk_out${_tfk_out:+ }${_tfk_hash:-ABSENT}"
    done
    printf '%s' "$_tfk_out"
}

tuple_choices_for_rel() {
    _tcr_rel=$1
    _tcr_bk=$2
    _tcr_out=
    for _tcr_kind in installed target restored; do
        _tcr_hash=$(state_hash_for_rel "$_tcr_kind" "$_tcr_rel") || return 1
        [ -n "$_tcr_hash" ] || continue
        case " $_tcr_out " in *" $_tcr_hash "*) continue ;; esac
        _tcr_out="$_tcr_out${_tcr_out:+ }$_tcr_hash"
    done
    if [ -n "$_tcr_bk" ]; then
        _tcr_hash=$(manifest_hash_for_rel "$_tcr_bk" "$_tcr_rel") || return 1
        case " $_tcr_out " in *" $_tcr_hash "*) ;; *) _tcr_out="$_tcr_out${_tcr_out:+ }$_tcr_hash" ;; esac
    fi
    printf '%s' "$_tcr_out"
}

# Build the finite set of exact four-file tuples that this operation may
# legitimately leave on disk.  Values come only from the loaded state and the
# validated backup; an arbitrary syntactically valid rootd.conf is not one of
# them.
build_known_tuples() {
    _bkt_bk=$1
    KNOWN_TUPLES=
    KNOWN_BIN_TUPLES=
    _bkt_c1=$(tuple_choices_for_rel bin/rootd "$_bkt_bk") || return 1
    _bkt_c2=$(tuple_choices_for_rel bin/permissive "$_bkt_bk") || return 1
    _bkt_c3=$(tuple_choices_for_rel etc/init/rootd.rc "$_bkt_bk") || return 1
    _bkt_c4=$(tuple_choices_for_rel etc/rootd.conf "$_bkt_bk") || return 1
    [ -n "$_bkt_c1" ] && [ -n "$_bkt_c2" ] && [ -n "$_bkt_c3" ] && [ -n "$_bkt_c4" ] || return 1
    for _bkt_a in $_bkt_c1; do
        for _bkt_b in $_bkt_c2; do
            for _bkt_c in $_bkt_c3; do
                for _bkt_d in $_bkt_c4; do
                    _bkt_t="$_bkt_a $_bkt_b $_bkt_c $_bkt_d"
                    KNOWN_TUPLES="${KNOWN_TUPLES}${KNOWN_TUPLES:+
}$_bkt_t"
                    _bkt_bin="$_bkt_a $_bkt_b $_bkt_c"
                    KNOWN_BIN_TUPLES="${KNOWN_BIN_TUPLES}${KNOWN_BIN_TUPLES:+
}$_bkt_bin"
                done
            done
        done
    done
    [ -n "$KNOWN_TUPLES" ]
}

tuple_allowed() {
    [ -n "${KNOWN_TUPLES:-}" ] || return 1
    printf '%s\n' "$KNOWN_TUPLES" | grep -Fqx "$1"
}

tuple_binaries_allowed() {
    [ -n "${KNOWN_BIN_TUPLES:-}" ] || return 1
    printf '%s\n' "$KNOWN_BIN_TUPLES" | grep -Fqx "$1"
}

tuple_binaries() {
    _tb_out=
    _tb_n=0
    for _tb_hash in $1; do
        _tb_n=$((_tb_n + 1))
        if [ "$_tb_n" -le 3 ]; then
            _tb_out="$_tb_out${_tb_out:+ }$_tb_hash"
        fi
    done
    printf '%s' "$_tb_out"
}

# Destination check used by install and rollback.  A file is acceptable when
# absent, equal to one of the allowed hashes, or (for rootd.conf only) a valid
# daemon configuration.  Anything else is reported with its path and hash.
# Arguments: PATH ALLOWED_HASHES_VAR ALLOW_VALID_CONF
destination_known() {
    _dk_path=$1
    _dk_allowed=$2
    _dk_conf=$3
    [ -e "$_dk_path" ] || return 0
    [ -L "$_dk_path" ] && { err "refusing symlink: $_dk_path"; return 1; }
    [ -f "$_dk_path" ] || { err "refusing non-regular file: $_dk_path"; return 1; }
    _dk_hash=$(sha256_file "$_dk_path") || { err "cannot read existing file: $_dk_path"; return 1; }
    for _dk_want in $_dk_allowed; do
        [ -n "$_dk_want" ] && [ "$_dk_hash" = "$_dk_want" ] && return 0
    done
    if [ "$_dk_conf" = 1 ]; then
        parse_rootd_conf "$_dk_path" && return 0
    fi
    err "unknown existing destination: $_dk_path sha256=$_dk_hash"
    err "refusing to overwrite it; move it away or use rollback first"
    return 1
}

# -------------------------------------------------------------- cleanup ---

# Remove only our own temporary names.  Paths are generated locally, never
# taken from configuration.
cleanup_temps() {
    for _ct in ${TMP_FILES:-}; do
        case $_ct in
            /odm/*.new.*|/odm/*.rollback.*|/odm/etc/.rootd.conf.new.*|/data/local/tmp/.rootd-backup-*) rm -f "$_ct" 2>/dev/null || true ;;
        esac
    done
    TMP_FILES=
}

# Best-effort return of /odm to ro.  Echoes ok or failed; never claims
# success when the kernel still reports rw.
odm_remount_ro() {
    mount -o ro,remount /odm >/dev/null 2>&1 || return 1
    sync 2>/dev/null || true
    [ "$(odm_state)" = ro ]
}

odm_remount_rw() {
    mount -o rw,remount /odm >/dev/null 2>&1 || return 1
    [ "$(odm_state)" = rw ]
}

# -------------------------------------------------------------- metadata --

stat_mode_uid_gid() {
    stat -c '%a %u %g' "$1" 2>/dev/null
}

# Echo the SELinux context of a file.  Only the exact requested path is
# inspected; callers never dump directory listings.
selinux_label() {
    ls -Z "$1" 2>/dev/null | awk '{
        for (i = 1; i <= NF; i++) {
            if ($i ~ /^u:object_r:/) { print $i; exit }
        }
    }'
}

# Verify owner/mode/label for one of the four installed files.
verify_metadata() {
    _vm_path=$1
    _vm_mode=$2
    _vm_label=$3
    _vm_meta=$(stat_mode_uid_gid "$_vm_path") || {
        err "cannot stat $_vm_path"
        return 1
    }
    _vm_m=$(printf '%s\n' "$_vm_meta" | awk '{print $1}')
    _vm_u=$(printf '%s\n' "$_vm_meta" | awk '{print $2}')
    _vm_g=$(printf '%s\n' "$_vm_meta" | awk '{print $3}')
    _vm_m=$(printf '%s' "$_vm_m" | sed 's/^0*//')
    [ -n "$_vm_m" ] || _vm_m=0
    [ "$_vm_m" = "$_vm_mode" ] || { err "mode mismatch: $_vm_path ($_vm_m != $_vm_mode)"; return 1; }
    [ "$_vm_u" = 0 ] && [ "$_vm_g" = 0 ] || { err "owner mismatch: $_vm_path ($_vm_u:$_vm_g)"; return 1; }
    _vm_ctx=$(selinux_label "$_vm_path")
    [ "$_vm_ctx" = "$_vm_label" ] || { err "label mismatch: $_vm_path ($_vm_ctx != $_vm_label)"; return 1; }
    return 0
}
