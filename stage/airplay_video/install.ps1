param(
    [string]$Device = "192.168.1.100:5555",
    [string]$Apk = ""
)
$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$adb = Join-Path $root "platform-tools\adb.exe"
$dev = $Device
if (-not $Apk) {
    $built = Join-Path $root "stage\airplay_video\src\app\build\outputs\apk\release\app-release.apk"
    if (Test-Path $built) {
        $Apk = $built
    } else {
        $asset = Get-ChildItem (Join-Path $root "dist") -Filter "airplay-video-*.apk" -ErrorAction SilentlyContinue |
            Sort-Object LastWriteTime -Descending | Select-Object -First 1
        if ($asset) { $Apk = $asset.FullName }
    }
}
$apk = $Apk

if (!(Test-Path $apk)) { throw "APK not found: $apk (build it or pass -Apk)" }
& $adb connect $dev 2>&1 | Out-Null

& $adb -s $dev uninstall io.github.jqssun.airplay 2>&1 | Out-Null
& $adb -s $dev install -r $apk
if ($LASTEXITCODE -ne 0) { throw "install failed" }
Start-Sleep -Seconds 2

$settings = @(
    @("server_port", "7001"),
    @("advertise_video", "true"),
    @("advertise_mirroring", "true"),
    @("advertise_audio", "true"),
    @("force_sw_aac", "true"),
    @("auto_start", "true"),
    @("boot_auto_start", "true"),
    @("run_in_background", "true"),
    @("launch_on_connect", "true"),
    @("keep_screen_on", "true"),
    @("debug_enabled", "false")
)
foreach ($s in $settings) {
    & $adb -s $dev shell "am broadcast -n io.github.jqssun.airplay/.SettingsReceiver -a io.github.jqssun.airplay.SET_SETTING --es key '$($s[0])' --es value '$($s[1])'" 2>&1 | Out-Null
}

# Имя задаёт панель: применяем сохранённое значение на самой станции
# (UTF-8 не проходит через консоль PowerShell).
& $adb -s $dev shell "sh /odm/bin/apply-video-name.sh" 2>&1 | Out-Null

& $adb -s $dev shell "am force-stop io.github.jqssun.airplay; sleep 1; am start -n io.github.jqssun.airplay/.MainActivity" | Out-Null
Start-Sleep -Seconds 3

# Приёмник открывает порт не мгновенно; на «спящем» экране первый запуск
# иногда остаётся пустым процессом. Ждём 7001 и повторяем запуск.
$portOk = $false
for ($i = 0; $i -lt 10; $i++) {
    Start-Sleep -Seconds 2
    $n = (& $adb -s $dev shell "netstat -ltn 2>/dev/null | grep -c 7001") -join ""
    if ($n -match '^\d+$' -and [int]$n -gt 0) { $portOk = $true; break }
}
if (-not $portOk) {
    Write-Warning "порт 7001 не поднялся, повторяю запуск приложения"
    & $adb -s $dev shell "am start -n io.github.jqssun.airplay/.MainActivity" | Out-Null
    Start-Sleep -Seconds 5
}
& $adb -s $dev shell "dumpsys window 2>/dev/null | grep mCurrentFocus | head -n 1"
