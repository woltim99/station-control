#!/system/bin/sh
# Применяет сохранённое имя видео-приёмника. Запускается из install.ps1
# после переустановки APK: само приложение имя не помнит.
name=$(settings get global station_airplay_video_name 2>/dev/null)
if [ -z "$name" ] || [ "$name" = "null" ]; then
    exit 0
fi
am broadcast --include-stopped-packages -n io.github.jqssun.airplay/.SettingsReceiver \
    -a io.github.jqssun.airplay.SET_SETTING --es key server_name --es value "$name" \
    >/dev/null 2>&1
exit 0
