$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$version = (Get-Content (Join-Path $root "VERSION") -Raw).Trim()
$build = (Get-Date).ToString("yyyy-MM-ddTHH:mm")

$go = (Get-Command go -ErrorAction SilentlyContinue).Source
if (-not $go) { $go = "C:\Program Files\Go\bin\go.exe" }

$env:GOOS = "linux"
$env:GOARCH = "arm64"
$env:CGO_ENABLED = "0"

Push-Location $root
try {
    & $go build -trimpath -ldflags "-s -w -X main.buildVersion=$version -X main.buildTime=$build" -o station-web_linux_arm64 .
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
    Write-Host "built v$version ($build)"
} finally {
    Pop-Location
}
