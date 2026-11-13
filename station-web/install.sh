#!/system/bin/sh
# Метка установки: сторож не должен поднимать панель, пока идёт деплой.
mkdir -p /data/local/tmp/station-web
date +%s > /data/local/tmp/station-web/install.stamp
pid=$(pidof station-web)
[ -n "$pid" ] && kill -9 $pid
sleep 1
# Убираем старый резидентный помощник ввода, чтобы он поднялся из свежего dex.
for hp in $(ps -A -o PID,ARGS 2>/dev/null | grep 'InputHelper' | grep -v grep | awk '{print $1}'); do
    kill -9 $hp 2>/dev/null
done
# И старый захват звука для визуализации.
for hp in $(ps -A -o PID,ARGS 2>/dev/null | grep 'AudioViz' | grep -v grep | awk '{print $1}'); do
    kill -9 $hp 2>/dev/null
done
mkdir -p /data/local/tmp/minicap
if [ -f /data/local/tmp/station-web/minicap ]; then
    cp /data/local/tmp/station-web/minicap /data/local/tmp/minicap/minicap
    cp /data/local/tmp/station-web/minicap.so /data/local/tmp/minicap/minicap.so
    chmod 0755 /data/local/tmp/minicap/minicap /data/local/tmp/minicap/minicap.so
    chown 0:0 /data/local/tmp/minicap/minicap /data/local/tmp/minicap/minicap.so
fi
mount -o rw,remount /odm
cp /data/local/tmp/station-web/station-web /odm/bin/station-web
chmod 0755 /odm/bin/station-web
chown 0:0 /odm/bin/station-web
chcon u:object_r:shell_exec:s0 /odm/bin/station-web
for f in station-guard.sh station-audit.sh apply-video-name.sh adbguard.sh; do
    src=/data/local/tmp/station-web/$f
    [ -f "$src" ] || continue
    cp "$src" /odm/bin/$f
    chmod 0755 /odm/bin/$f
    chown 0:0 /odm/bin/$f
    chcon u:object_r:shell_exec:s0 /odm/bin/$f
done
if [ ! -f /odm/etc/station-web.conf ]; then
    cp /data/local/tmp/station-web/station-web.conf /odm/etc/station-web.conf
    chmod 0600 /odm/etc/station-web.conf
    chown 0:0 /odm/etc/station-web.conf
    chcon u:object_r:vendor_configs_file:s0 /odm/etc/station-web.conf
fi
cp /data/local/tmp/station-web/adbd_tcp.rc /odm/etc/init/adbd_tcp.rc
chmod 0644 /odm/etc/init/adbd_tcp.rc
chown 0:0 /odm/etc/init/adbd_tcp.rc
chcon u:object_r:vendor_configs_file:s0 /odm/etc/init/adbd_tcp.rc
mount -o ro,remount /odm
ls -lZ /odm/bin/station-web /odm/etc/station-web.conf /odm/etc/init/adbd_tcp.rc
# Перезапуск сторожа: init поднимет его заново с новым скриптом.
for gp in $(ps -A -o PID,ARGS 2>/dev/null | grep '[s]tation-guard.sh' | awk '{print $1}'); do
    kill "$gp" 2>/dev/null
done
# То же для adbguard: подхватит новый переключатель ADB.
for ap in $(ps -A -o PID,ARGS 2>/dev/null | grep '[a]dbguard.sh' | awk '{print $1}'); do
    kill "$ap" 2>/dev/null
done
setprop ctl.start stationguard 2>/dev/null
/system/bin/setsid /odm/bin/station-web -config /odm/etc/station-web.conf </dev/null >/dev/null 2>&1 &
sleep 2
netstat -ltn 2>/dev/null | grep 8080
