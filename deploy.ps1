param(
    [string]$Device = "",
    [string]$HostName = "",
    [int]$SshPort = 2222,
    [string]$Key = "stage\ssh\client_ed25519",
    [string]$User = "",
    [string]$Password = $env:STATION_PASSWORD,
    [string]$PanelBinary = ""
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$adb = Join-Path $root "platform-tools\adb.exe"

if (-not $HostName) {
    $HostName = if ($env:STATION_HOST) { $env:STATION_HOST } else { "192.168.1.100" }
}
if (-not $Device) {
    $Device = if ($env:STATION_DEVICE) { $env:STATION_DEVICE } else { "${HostName}:5555" }
}
if (-not $User) {
    $User = if ($env:STATION_USER) { $env:STATION_USER } else { "admin" }
}

$keyPath = Join-Path $root $Key
if (-not (Test-Path $keyPath)) {
    throw "SSH key not found: $keyPath. Pass -Key <path> or create the client key (see RELEASE.md)."
}
if (-not (Test-Path $adb)) {
    $adbCmd = Get-Command adb -ErrorAction SilentlyContinue
    if ($adbCmd) { $adb = $adbCmd.Source } else {
        throw "adb not found: put platform-tools into platform-tools\ or install Android Platform Tools and add it to PATH."
    }
}

if (-not $PanelBinary) {
    $ver = (Get-Content (Join-Path $root "station-web\VERSION") -Raw).Trim()
    $PanelBinary = Join-Path $root "dist\station-web-$ver"
}
if (-not (Test-Path $PanelBinary)) {
    throw "panel binary not found: $PanelBinary (сначала build-all.ps1)"
}

# После переключателя ADB клиент не знает о станции, пока не выполнит connect.
& $adb connect $Device 2>&1 | Out-Null

& $adb -s $Device push $PanelBinary /data/local/tmp/station-web/station-web
& $adb -s $Device push (Join-Path $root "station-web\install.sh") /data/local/tmp/station-web/install.sh
& $adb -s $Device push (Join-Path $root "stage\ops\station-guard.sh") /data/local/tmp/station-web/station-guard.sh
& $adb -s $Device push (Join-Path $root "stage\ops\station-audit.sh") /data/local/tmp/station-web/station-audit.sh
& $adb -s $Device push (Join-Path $root "stage\ops\apply-video-name.sh") /data/local/tmp/station-web/apply-video-name.sh
& $adb -s $Device push (Join-Path $root "stage\adbd_tcp.rc") /data/local/tmp/station-web/adbd_tcp.rc
& $adb -s $Device push (Join-Path $root "stage\adbguard.sh") /data/local/tmp/station-web/adbguard.sh
$wifiDex = Join-Path $root "stage\wifi\dexout\classes.dex"
if (Test-Path $wifiDex) {
    & $adb -s $Device push $wifiDex /data/local/tmp/station-web/wifi.dex
}

& ssh -p $SshPort -i $keyPath -o StrictHostKeyChecking=no -o UserKnownHostsFile=NUL -o IdentitiesOnly=yes `
    root@$HostName "cd /data/local/tmp/station-web && sh install.sh >/dev/null 2>&1; sleep 3; pidof station-web"
if ($LASTEXITCODE) { throw "install failed" }

if (-not $Password) {
    Write-Warning "Пароль не задан: проверка версии пропущена. Укажите -Password или переменную STATION_PASSWORD."
    exit 0
}

$pair = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes("${User}:${Password}"))
$status = Invoke-RestMethod -Uri "http://${HostName}:8080/api/status" -Headers @{ Authorization = "Basic $pair" }
Write-Host "panel=$($status.panel_version) build=$($status.panel_build)"
