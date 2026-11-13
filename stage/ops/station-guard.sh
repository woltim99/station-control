#!/system/bin/sh
# station-guard: держит панель и оба AirPlay-приёмника живыми после OTA,
# падений и ручных остановок. Ничего не перезапускает чаще, чем раз в минуту.

LOG=/data/local/tmp/station-guard.log
PANEL_CONF=/odm/etc/station-web.conf
SHAIRPORT=com.hkfuertes.shairport
VIDEO=io.github.jqssun.airplay

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >>"$LOG" 2>/dev/null
}
rotate() {
    [ -f "$LOG" ] || return
    size=$(wc -c <"$LOG" 2>/dev/null)
    [ -n "$size" ] && [ "$size" -gt 262144 ] && mv -f "$LOG" "$LOG.1"
}

start_panel() {
    [ -x /odm/bin/station-web ] || return
    /system/bin/setsid /odm/bin/station-web -config "$PANEL_CONF" </dev/null >>/data/local/tmp/station-web.log 2>&1 &
    log "restart: station-web"
}

start_audio() {
    am broadcast --include-stopped-packages -n "$SHAIRPORT/.SettingsReceiver" \
        -a "$SHAIRPORT.CONFIGURE_SETTINGS" --es key receiver_enabled --ez value true \
        >/dev/null 2>&1
    sleep 2
    if ! pidof "$SHAIRPORT" >/dev/null 2>&1; then
        am start-foreground-service -n "$SHAIRPORT/.ReceiverService" >/dev/null 2>&1
    fi
    log "restart: shairport"
}

start_video() {
    am start -n "$VIDEO/.MainActivity" >/dev/null 2>&1
    log "restart: airplay video"
}

# ждём завершения загрузки: вендорные приёмники стартуют сами
while [ "$(getprop sys.boot_completed)" != "1" ]; do
    sleep 5
done
sleep 30
log "guard started"

last_panel=0
last_audio=0
last_video=0

while true; do
    rotate
    now=$(date +%s)

    if ! pidof station-web >/dev/null 2>&1; then
        stamp=$(cat /data/local/tmp/station-web/install.stamp 2>/dev/null || echo 0)
        case "$stamp" in ''|*[!0-9]*) stamp=0 ;; esac
        if [ $((now - stamp)) -gt 120 ] && [ $((now - last_panel)) -gt 60 ]; then
            last_panel=$now
            start_panel
        fi
    fi

    if ! pidof "$SHAIRPORT" >/dev/null 2>&1; then
        if [ $((now - last_audio)) -gt 120 ]; then
            last_audio=$now
            start_audio
        fi
    fi

    if ! pidof "$VIDEO" >/dev/null 2>&1; then
        if [ $((now - last_video)) -gt 120 ]; then
            last_video=$now
            start_video
        fi
    fi

    sleep 20
done
