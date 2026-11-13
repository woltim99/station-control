#!/system/bin/sh
# Убираем отладочные хвосты и старые секреты.
rm -f /data/local/tmp/station-web/station-web.conf
rm -f /data/local/tmp/station-web-events.jsonl
rm -f /data/local/tmp/capture_fb4.sh /data/local/tmp/capture_fb5.sh
rm -f /data/local/tmp/fb4.tar /data/local/tmp/fb5.tar /data/local/tmp/gifcache.tar
rm -f /data/local/tmp/stationweb.test
rm -f /data/local/tmp/fb4_*.bin /data/local/tmp/fb5_*.bin
rm -f /data/local/tmp/probe_led.sh /data/local/tmp/probe_glagol.sh
rm -f /data/local/tmp/probe_strings.sh /data/local/tmp/scan_all_apks.sh
rm -f /data/local/tmp/find_gifs.sh /data/local/tmp/wifi_diag.sh
rm -f /data/local/tmp/wifi_1337.sh /data/local/tmp/leftovers.sh
rm -f /data/misc/wifi/WifiConfigStore.xml.stationweb.bak
echo "cleaned"
echo "--- station-web ---"
du -sh /data/local/tmp/station-web 2>/dev/null
echo "--- legacy gone? ---"
ls /data/local/tmp/station-web-events.jsonl /data/local/tmp/station-web/station-web.conf 2>/dev/null || echo yes
echo "--- wifi saved ---"
CLASSPATH=/data/local/tmp/station-web/wifi.dex app_process /system/bin WifiHelper list 2>/dev/null
