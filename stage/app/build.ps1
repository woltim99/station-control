$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$dir = Join-Path $root "stage\app"
$bt = Join-Path $root "stage\airplay_apk\bt\android-14"
$jar = Join-Path $root "stage\airplay_apk\platform\android-9\android.jar"
$ecj = Join-Path $root "stage\bt\ecj.jar"
$r8 = Join-Path $root "stage\bt\r8.jar"
$ks = Join-Path $dir "station.jks"

if (!(Test-Path $ecj) -or !(Test-Path $r8) -or !(Test-Path (Join-Path $bt "aapt2.exe"))) {
    throw "Android build tools not found. Use a release APK or see RELEASE.md for the toolchain layout."
}

if (-not (Test-Path $ks)) {
    & keytool -genkeypair -keystore $ks -alias station -keyalg RSA -keysize 2048 -validity 10000 `
        -storepass station123 -keypass station123 -dname "CN=Station Viz" 2>&1 | Out-Null
}
Remove-Item "$dir\classes", "$dir\dex", "$dir\base.apk", "$dir\aligned.apk", "$dir\stationviz.apk" -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path "$dir\classes", "$dir\dex" | Out-Null

$src = Get-ChildItem "$dir\src" -Recurse -Filter *.java | ForEach-Object { $_.FullName }
java -jar $ecj -source 1.8 -target 1.8 -cp $jar -d "$dir\classes" $src
if ($LASTEXITCODE -ne 0) { throw "ecj failed" }

$classes = Get-ChildItem "$dir\classes" -Recurse -Filter *.class | ForEach-Object { $_.FullName }
java -cp $r8 com.android.tools.r8.D8 --min-api 28 --lib $jar --output "$dir\dex" $classes
if ($LASTEXITCODE -ne 0) { throw "d8 failed" }

& "$bt\aapt2.exe" link -o "$dir\base.apk" --manifest "$dir\AndroidManifest.xml" -I $jar
if ($LASTEXITCODE -ne 0) { throw "aapt2 link failed" }

$py = @"
import zipfile
p = r'$dir\base.apk'
with zipfile.ZipFile(p, 'a') as z:
    z.write(r'$dir\dex\classes.dex', 'classes.dex')
print('dex added')
"@
$py | py -
if ($LASTEXITCODE -ne 0) { throw "dex add failed" }

& "$bt\zipalign.exe" -f 4 "$dir\base.apk" "$dir\aligned.apk"
if ($LASTEXITCODE -ne 0) { throw "zipalign failed" }

& "$bt\apksigner.bat" sign --ks $ks --ks-pass pass:station123 --key-pass pass:station123 --out "$dir\stationviz.apk" "$dir\aligned.apk"
if ($LASTEXITCODE -ne 0) { throw "apksigner failed" }

Get-Item "$dir\stationviz.apk" | Select-Object Name, Length
