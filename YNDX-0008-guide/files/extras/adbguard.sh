#!/system/bin/sh
# adbguard — держит ADB over TCP включённым, даже если облачный конфиг
# Яндекса выставил adb_enabled=0. Заодно поддерживает bind-mount hosts
# с заблокированными OTA-доменами.
#
# Запускается init-сервисом adbguard (см. adbd_tcp.rc).

while true; do
    # OTA hosts: /odm/etc/hosts-block -> /system/etc/hosts (сам /system не меняем)
    if ! grep -q ' /system/etc/hosts ' /proc/mounts 2>/dev/null; then
        mount --bind /odm/etc/hosts-block /system/etc/hosts 2>/dev/null
    fi

    en=$(settings get global adb_enabled 2>/dev/null)
    svc=$(getprop init.svc.adbd 2>/dev/null)
    if [ "$en" != "1" ]; then
        settings put global adb_enabled 1
        settings put global development_settings_enabled 1
    elif [ "$svc" != "running" ]; then
        # framework перезапускает adbd только на переходе 0 -> 1
        settings put global adb_enabled 0
        sleep 1
        settings put global adb_enabled 1
        settings put global development_settings_enabled 1
    fi

    port=$(getprop service.adb.tcp.port 2>/dev/null)
    if [ "$port" != "5555" ]; then
        setprop service.adb.tcp.port 5555
    fi

    sleep 10
done
