#!/system/bin/sh
# station-audit: снимок всего, что мы изменили на станции.
# Запускать после OTA/сброса: должно совпасть с RELEASE.md.

echo "# station audit"
echo "date: $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "firmware: $(getprop ro.build.display.id)"
echo "build: $(getprop ro.build.version.incremental)"
echo "panel_pid: $(pidof station-web 2>/dev/null || echo none)"
echo "panel_bin: $(ls -l /odm/bin/station-web 2>/dev/null | awk '{print $5" "$6" "$7" "$8}')"
if [ -f /odm/etc/station-web.conf ]; then
    hash=$(grep -c '^password_hash=' /odm/etc/station-web.conf 2>/dev/null)
    plain=$(grep -c '^password=' /odm/etc/station-web.conf 2>/dev/null)
    echo "config: hash=$hash plaintext=$plain"
else
    echo "config: missing"
fi
echo "ports:"
netstat -ltn 2>/dev/null | grep LISTEN | awk '{print $4}' | sort -u | sed 's/^/  /'
echo "services:"
for s in adbd adbguard absshd rootd stationweb stationguard; do
    st=$(getprop init.svc.$s)
    if [ "$s" = "stationweb" ] && [ -n "$(pidof station-web 2>/dev/null)" ]; then
        st=running
    fi
    echo "  $s=$st"
done
echo "panel_pid: $(pidof station-web 2>/dev/null || echo none)"
echo "packages:"
for p in io.github.jqssun.airplay com.hkfuertes.shairport com.opencode.airplaytv; do
    v=$(dumpsys package "$p" 2>/dev/null | grep -m1 versionName | tr -d ' ')
    echo "  $p ${v:-missing}"
done
echo "settings:"
for k in station_accent station_airplay_name_mode station_airplay_name_custom station_airplay_video_name; do
    echo "  $k=$(settings get global "$k" 2>/dev/null)"
done
echo "init: $(ls -l /odm/etc/init/adbd_tcp.rc 2>/dev/null | awk '{print $5" "$6" "$7" "$8}')"
echo "hosts_bind: $(grep -q ' /system/etc/hosts ' /proc/mounts 2>/dev/null && echo yes || echo no)"
echo "updater: $(pm list packages --user 0 2>/dev/null | grep -c 'com.yandex.tv.updater')"
echo "guard_log_tail:"
tail -n 5 /data/local/tmp/station-guard.log 2>/dev/null | sed 's/^/  /'
