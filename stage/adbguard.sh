#!/system/bin/sh
# adbguard: держит ADB-over-TCP включённым, но уважает переключатель панели
# (settings global station_adb_enabled: 1 — включён, 0 — выключен).
# Также blackhole OTA/firmware domains через bind-mounted hosts.

while true; do
    if ! grep -q ' /system/etc/hosts ' /proc/mounts 2>/dev/null; then
        mount --bind /odm/etc/hosts-block /system/etc/hosts 2>/dev/null
    fi

    want=$(settings get global station_adb_enabled 2>/dev/null)
    [ "$want" = "0" ] || want=1

    if [ "$want" = "0" ]; then
        en=$(settings get global adb_enabled 2>/dev/null)
        if [ "$en" != "0" ]; then
            settings put global adb_enabled 0
        fi
    else
        en=$(settings get global adb_enabled 2>/dev/null)
        svc=$(getprop init.svc.adbd 2>/dev/null)
        if [ "$en" != "1" ]; then
            settings put global adb_enabled 1
            settings put global development_settings_enabled 1
        elif [ "$svc" != "running" ]; then
            settings put global adb_enabled 0
            sleep 1
            settings put global adb_enabled 1
            settings put global development_settings_enabled 1
        fi
        port=$(getprop service.adb.tcp.port 2>/dev/null)
        if [ "$port" != "5555" ]; then
            setprop service.adb.tcp.port 5555
        fi
    fi

    sleep 10
done
