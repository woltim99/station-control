#!/system/bin/sh
# Проверка фиксации ADB/OTA (можно запускать из обычного adb shell, без root).
#   adb shell sh /data/local/tmp/station-max-extras/verify-extras.sh
echo "model=$(getprop ro.product.model)"
echo "build=$(getprop ro.build.version.incremental)"
echo "adb_enabled=$(settings get global adb_enabled)"
echo "adbd=$(getprop init.svc.adbd)"
echo "adbguard=$(getprop init.svc.adbguard)"
echo "tcp_port=$(getprop service.adb.tcp.port)"
echo "updater=$(pm list packages --user 0 | grep updater || echo none)"
if grep -q ' /system/etc/hosts ' /proc/mounts 2>/dev/null; then
    echo "hosts=bind"
else
    echo "hosts=stock"
fi
echo "--- files ---"
ls -l /odm/bin/adbguard.sh /odm/etc/hosts-block /odm/etc/init/adbd_tcp.rc 2>/dev/null || true
ls -l /odm/bin/absshd /odm/etc/absshd/authorized_keys 2>/dev/null || true
