#!/system/bin/sh
# Снять фиксацию ADB/OTA/SSH. Root (rootd) остаётся.
# Запускать от root:
#   ./station-max.sh cmd 'sh /data/local/tmp/station-max-extras/uninstall-extras.sh'
set -eu

mount -o rw,remount /odm
rm -f /odm/etc/init/adbd_tcp.rc
rm -f /odm/bin/adbguard.sh
rm -f /odm/bin/absshd
rm -rf /odm/etc/absshd
rm -f /odm/etc/hosts-block
mount -o ro,remount /odm

umount /system/etc/hosts 2>/dev/null || true
pm install-existing --user 0 com.yandex.launcher.updaterapp >/dev/null 2>&1 || true

echo "extras удалены. Перезагрузите станцию."
echo "ADB over TCP больше не будет подниматься сам; updater восстановлен (если был удалён)."
