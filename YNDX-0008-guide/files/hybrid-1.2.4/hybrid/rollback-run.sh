#!/system/bin/sh
# Device-side rollback bootstrap wrapper.  Used only when a healthy rootd is
# not available and the operator has explicitly chosen the bootstrap recovery
# path:
#   BACKUP='...' [ALLOW_UNMARKED=1] root-bridge esc-shell < rollback-run.sh
set -eu

BASE=${BASE:-/data/local/tmp/station-max-hybrid}
case $BASE in
    /data/local/tmp/*) ;;
    *) echo "ROLLBACK_FAIL: unsafe BASE: $BASE" >&2; exit 1 ;;
esac
export BASE
export OPERATION_ID=${OPERATION_ID:-}
export STAGING_NONCE=${STAGING_NONCE:-}
export INFLIGHT_OWNED=${INFLIGHT_OWNED:-0}
export INFLIGHT_FILE=${INFLIGHT_FILE:-/data/local/tmp/rootd-op.inflight}
cd "$BASE" || { echo "ROLLBACK_FAIL: cannot enter $BASE" >&2; exit 1; }
[ -f hybrid/common.sh ] && [ ! -L hybrid/common.sh ] || {
    echo "ROLLBACK_FAIL: missing hybrid/common.sh" >&2
    exit 1
}
. ./hybrid/common.sh
no_symlink_components "$BASE" || { echo "ROLLBACK_FAIL: BASE path contains a symlink component" >&2; exit 1; }

PROG=ROLLBACK
[ -n "${BACKUP:-}" ] || { echo "ROLLBACK_FAIL: BACKUP is not set" >&2; exit 1; }
require_hash ./hybrid/permissive "$PERM_SHA" hybrid/permissive || exit 1
require_commands || exit 1

./hybrid/permissive esc-permissive || {
    echo "ROLLBACK_FAIL: permissive helper failed before rollback" >&2
    exit 1
}
_enforce=$(getenforce 2>/dev/null) || {
    echo "ROLLBACK_FAIL: cannot read SELinux state after permissive" >&2
    exit 1
}
[ "$_enforce" = Permissive ] || {
    echo "ROLLBACK_FAIL: SELinux is $_enforce after permissive" >&2
    exit 1
}

set -- "$BACKUP"
[ "${ALLOW_UNMARKED:-0}" = 1 ] && set -- --allow-unmarked "$@"
[ "${ALLOW_REPLACE_CONFIG:-0}" = 1 ] && set -- --replace-config "$@"
exec ./hybrid/rollback-device.sh "$@"
