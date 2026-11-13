param(
    [ValidateSet("video", "panel")] [string]$Target = "video",
    [string]$Device = "192.168.1.100:5555",
    [string]$Apk = ""
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$adb = Join-Path $root "platform-tools\adb.exe"

switch ($Target) {
    "video" {
        if (-not $Apk) {
            foreach ($candidate in @(
                (Join-Path $root "stage\airplay_video\airplay-receiver.apk"),
                (Join-Path $root "dist\airplay-receiver.apk"),
                (Join-Path $root "dist\airplay-receiver-upstream.apk")
            )) {
                if (Test-Path $candidate) { $Apk = $candidate; break }
            }
        }
        if (-not $Apk -or -not (Test-Path $Apk)) {
            throw "официальный APK не найден. Скачайте airplay-receiver-upstream.apk из Release и положите в dist\ или укажите -Apk путь"
        }
        Write-Host "Откат видео-приёмника на официальную сборку..."
        & $adb connect $Device 2>&1 | Out-Null
        & $adb -s $Device uninstall io.github.jqssun.airplay 2>&1 | Out-Null
        & $adb -s $Device install $Apk
        if ($LASTEXITCODE) { throw "rollback failed" }
        Write-Host "Готово: установлен официальный airplay-receiver"
    }
    "panel" {
        $candidate = Get-ChildItem (Join-Path $root "dist\previous") -Filter "station-web-*" -File -ErrorAction SilentlyContinue |
            Sort-Object LastWriteTime -Descending | Select-Object -First 1
        if (-not $candidate) { throw "нет dist\previous\station-web-*; сохраните предыдущую сборку через build-all.ps1" }
        & powershell -ExecutionPolicy Bypass -File (Join-Path $root "deploy.ps1") -Device $Device -PanelBinary $candidate.FullName
        Write-Host "Готово: панель откатана на $($candidate.Name)"
    }
}
