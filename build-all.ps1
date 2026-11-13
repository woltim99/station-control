param(
    [switch]$SkipPanel,
    [switch]$SkipVideo,
    [switch]$SkipTV
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$dist = Join-Path $root "dist"
$prev = Join-Path $dist "previous"

function Get-Version($path, $pattern) {
    $text = Get-Content $path -Raw
    $m = [regex]::Match($text, $pattern)
    if (-not $m.Success) { throw "version not found in $path" }
    return $m.Groups[1].Value
}

$panelVer = (Get-Content (Join-Path $root "station-web\VERSION") -Raw).Trim()
$videoVer = Get-Version (Join-Path $root "stage\airplay_video\src\app\build.gradle.kts") 'versionName\s*=\s*"([^"]+)"'
$tvVer = Get-Version (Join-Path $root "stage\tvapp\AndroidManifest.xml") 'android:versionName="([^"]+)"'

New-Item -ItemType Directory -Force -Path $dist, $prev | Out-Null
Get-ChildItem $dist -File | Where-Object { $_.Name -notin @("SHA256SUMS.txt", "BUILD.txt") } |
    Move-Item -Destination $prev -Force

if (-not $SkipPanel) {
    Write-Host "== panel $panelVer =="
    & powershell -ExecutionPolicy Bypass -File (Join-Path $root "station-web\build.ps1")
    if ($LASTEXITCODE) { throw "panel build failed" }
    Copy-Item (Join-Path $root "station-web\station-web_linux_arm64") (Join-Path $dist "station-web-$panelVer") -Force
    Copy-Item (Join-Path $root "station-web\install.sh") (Join-Path $dist "install.sh") -Force
    Copy-Item (Join-Path $root "stage\ops\station-guard.sh") $dist -Force
    Copy-Item (Join-Path $root "stage\ops\station-audit.sh") $dist -Force
    Copy-Item (Join-Path $root "stage\ops\apply-video-name.sh") $dist -Force
    Copy-Item (Join-Path $root "stage\adbguard.sh") $dist -Force
    $wifiDex = Join-Path $root "stage\wifi\dexout\classes.dex"
    if (Test-Path $wifiDex) { Copy-Item $wifiDex (Join-Path $dist "wifi.dex") -Force }
    Copy-Item (Join-Path $root "stage\adbd_tcp.rc") $dist -Force
}

if (-not $SkipVideo) {
    Write-Host "== airplay video $videoVer =="
    & powershell -ExecutionPolicy Bypass -File (Join-Path $root "stage\airplay_video\build.ps1")
    if ($LASTEXITCODE) { throw "video build failed" }
    Copy-Item (Join-Path $root "stage\airplay_video\src\app\build\outputs\apk\release\app-release.apk") `
        (Join-Path $dist "airplay-video-$videoVer.apk") -Force
}

if (-not $SkipTV) {
    Write-Host "== tv app $tvVer =="
    & powershell -ExecutionPolicy Bypass -File (Join-Path $root "stage\tvapp\build.ps1")
    if ($LASTEXITCODE) { throw "tv build failed" }
    Copy-Item (Join-Path $root "stage\tvapp\airplaytv.apk") (Join-Path $dist "airplaytv-$tvVer.apk") -Force
}

$sums = Get-ChildItem $dist -File |
    Where-Object { $_.Name -notin @("SHA256SUMS.txt", "BUILD.txt") } |
    ForEach-Object { "$((Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower())  $($_.Name)" }
$sums | Set-Content (Join-Path $dist "SHA256SUMS.txt") -Encoding ASCII

@(
    "panel=$panelVer"
    "video=$videoVer"
    "tv=$tvVer"
    "built=$(Get-Date -Format o)"
    "host=$env:COMPUTERNAME"
) | Set-Content (Join-Path $dist "BUILD.txt") -Encoding ASCII

Get-Content (Join-Path $dist "BUILD.txt")
Get-ChildItem $dist -File | Select-Object Name, Length | Format-Table -AutoSize
