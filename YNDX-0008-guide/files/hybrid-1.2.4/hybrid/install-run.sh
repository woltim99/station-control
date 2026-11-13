#!/system/bin/sh
# Device-side bootstrap wrapper: executed as stdin by
#   root-bridge esc-shell < hybrid/install-run.sh
# It only lifts SELinux to Permissive and then runs the real installer.
# Configuration is not read here; install-device.sh parses it as data.
set -eu

BASE=${BASE:-/data/local/tmp/station-max-hybrid}
case $BASE in
    /data/local/tmp/*) ;;
    *) echo "INSTALL_FAIL: unsafe BASE: $BASE" >&2; exit 1 ;;
esac
export BASE
export OPERATION_ID=${OPERATION_ID:-}
export STAGING_NONCE=${STAGING_NONCE:-}
export INFLIGHT_OWNED=${INFLIGHT_OWNED:-0}
export INFLIGHT_FILE=${INFLIGHT_FILE:-/data/local/tmp/rootd-op.inflight}
cd "$BASE" || { echo "INSTALL_FAIL: cannot enter $BASE" >&2; exit 1; }
[ -f hybrid/common.sh ] && [ ! -L hybrid/common.sh ] || {
    echo "INSTALL_FAIL: missing hybrid/common.sh" >&2
    exit 1
}
. ./hybrid/common.sh
no_symlink_components "$BASE" || { echo "INSTALL_FAIL: BASE path contains a symlink component" >&2; exit 1; }

PROG=INSTALL
require_hash ./hybrid/permissive "$PERM_SHA" hybrid/permissive || exit 1
require_hash ./hybrid/rootd "$ROOTD_SHA" hybrid/rootd || exit 1
require_hash ./hybrid/rootd.rc "$RC_SHA" hybrid/rootd.rc || exit 1
require_commands || exit 1

./hybrid/permissive esc-permissive || {
    echo "INSTALL_FAIL: permissive helper failed before installation" >&2
    exit 1
}
_enforce=$(getenforce 2>/dev/null) || {
    echo "INSTALL_FAIL: cannot read SELinux state after permissive" >&2
    exit 1
}
[ "$_enforce" = Permissive ] || {
    echo "INSTALL_FAIL: SELinux is $_enforce after permissive" >&2
    exit 1
}

exec ./hybrid/install-device.sh
