$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$src = Join-Path $root "stage\airplay_video\src"
$jdk = if ($env:JAVA_HOME) { $env:JAVA_HOME } else { Join-Path $root "stage\corretto21\jdk21.0.13_0" }
$sdk = if ($env:ANDROID_HOME) { $env:ANDROID_HOME } else { Join-Path $root "stage\android-sdk" }
$ndk = if ($env:ANDROID_NDK_HOME) { $env:ANDROID_NDK_HOME } else { Join-Path $sdk "ndk\27.0.12077973" }
$cmake = Join-Path $sdk "cmake\3.22.1\bin"

if (!(Test-Path (Join-Path $jdk "bin\java.exe"))) { throw "JDK 21 not found: $jdk" }
if (!(Test-Path (Join-Path $sdk "platforms\android-37.0"))) { throw "Android platform 37.0 not installed" }
if (!(Test-Path $ndk)) { throw "NDK 27.0.12077973 not installed" }

$env:JAVA_HOME = $jdk
$env:ANDROID_HOME = $sdk
$env:ANDROID_SDK_ROOT = $sdk
$env:ANDROID_NDK_HOME = $ndk
# Git for Windows ships a minimal Perl; add the missing CPAN module used by
# OpenSSL's Configure (Locale::Maketext::Simple via Params::Check).
$env:PERL5LIB = Join-Path $root "stage\perl-lib"
$env:Path = @(
    (Join-Path $jdk "bin"),
    (Join-Path $sdk "platform-tools"),
    (Join-Path $sdk "build-tools\37.0.0"),
    $cmake,
    (Join-Path $ndk "prebuilt\windows-x86_64\bin"),
    (Join-Path $root "stage\winlibs\mingw64\bin"),
    "C:\Program Files\Git\usr\bin",
    "C:\Program Files\Git\cmd",
    $env:Path
) -join ";"

Push-Location $src
try {
    & .\gradlew.bat :app:assembleRelease --no-daemon --stacktrace
    if ($LASTEXITCODE -ne 0) { throw "gradle failed: $LASTEXITCODE" }
    Get-Item "app\build\outputs\apk\release\app-release.apk"
} finally {
    Pop-Location
}
