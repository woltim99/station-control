#!/system/bin/sh
# Установка «фиксации»: ADB over TCP + watchdog + OTA hosts.
# Запускать ТОЛЬКО от root, например:
#   ./station-max.sh cmd 'sh /data/local/tmp/station-max-extras/install-extras.sh'
#
# Если рядом лежат absshd и authorized_keys — дополнительно ставится root SSH
# на 2222 (см. install-ssh.sh).
set -eu

BASE=/data/local/tmp/station-max-extras
[ -d "$BASE" ] || { echo "НЕТ $BASE — сначала adb push files/extras"; exit 1; }
[ -f "$BASE/adbguard.sh" ] || { echo "нет $BASE/adbguard.sh"; exit 1; }
[ -f "$BASE/hosts-block" ] || { echo "нет $BASE/hosts-block"; exit 1; }
[ -f "$BASE/adbd_tcp.rc" ] || { echo "нет $BASE/adbd_tcp.rc"; exit 1; }

echo "== files =="
mount -o rw,remount /odm

cp "$BASE/adbguard.sh" /odm/bin/adbguard.sh
chown 0:0 /odm/bin/adbguard.sh
chmod 0755 /odm/bin/adbguard.sh
chcon u:object_r:shell_exec:s0 /odm/bin/adbguard.sh

cp "$BASE/hosts-block" /odm/etc/hosts-block
chown 0:0 /odm/etc/hosts-block
chmod 0644 /odm/etc/hosts-block
chcon u:object_r:vendor_configs_file:s0 /odm/etc/hosts-block

RC_SRC="$BASE/adbd_tcp.rc"
if [ -f "$BASE/absshd" ] && [ -f "$BASE/authorized_keys" ]; then
    cp "$BASE/absshd" /odm/bin/absshd
    chown 0:0 /odm/bin/absshd
    chmod 0755 /odm/bin/absshd
    chcon u:object_r:shell_exec:s0 /odm/bin/absshd

    mkdir -p /odm/etc/absshd
    cp "$BASE/authorized_keys" /odm/etc/absshd/authorized_keys
    chown 0:0 /odm/etc/absshd/authorized_keys
    chmod 0600 /odm/etc/absshd/authorized_keys
    chcon u:object_r:vendor_configs_file:s0 /odm/etc/absshd/authorized_keys

    RC_SRC="$BASE/adbd_ssh.rc"
    echo "SSH: включён (порт 2222)"
else
    echo "SSH: пропущен (нет absshd/authorized_keys)"
fi

cp "$RC_SRC" /odm/etc/init/adbd_tcp.rc
chown 0:0 /odm/etc/init/adbd_tcp.rc
chmod 0644 /odm/etc/init/adbd_tcp.rc
chcon u:object_r:vendor_configs_file:s0 /odm/etc/init/adbd_tcp.rc

mount -o ro,remount /odm

echo "== применяю сейчас =="
setprop service.adb.tcp.port 5555
settings put global adb_enabled 1
settings put global development_settings_enabled 1
mount --bind /odm/etc/hosts-block /system/etc/hosts 2>/dev/null || true
pm uninstall --user 0 com.yandex.launcher.updaterapp >/dev/null 2>&1 || \
    pm disable-user --user 0 com.yandex.launcher.updaterapp >/dev/null 2>&1 || true

echo "== готово =="
echo "adb_enabled=$(settings get global adb_enabled)"
echo "tcp_port=$(getprop service.adb.tcp.port)"
echo "updater=$(pm list packages --user 0 | grep updater || echo none)"
echo "Перезагрузите станцию: adb reboot"
