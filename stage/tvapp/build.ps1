$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$dir = Join-Path $root "stage\tvapp"
$bt = Join-Path $root "stage\airplay_apk\bt\android-14"
$jar = Join-Path $root "stage\airplay_apk\platform\android-9\android.jar"
$ecj = Join-Path $root "stage\bt\ecj.jar"
$r8 = Join-Path $root "stage\bt\r8.jar"
$ks = Join-Path $root "stage\app\station.jks"

if (!(Test-Path $ecj) -or !(Test-Path $r8) -or !(Test-Path (Join-Path $bt "aapt2.exe"))) {
    throw "Android build tools not found. Use a release APK or see RELEASE.md for the toolchain layout."
}
if (!(Test-Path $ks)) {
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $ks) | Out-Null
    & keytool -genkeypair -keystore $ks -alias station -keyalg RSA -keysize 2048 -validity 10000 `
        -storepass station123 -keypass station123 -dname "CN=Station TV" 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "keytool failed" }
}

Remove-Item "$dir\classes", "$dir\dex", "$dir\base.apk", "$dir\aligned.apk", "$dir\airplaytv.apk" -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path "$dir\classes", "$dir\dex" | Out-Null

$src = Get-ChildItem "$dir\src" -Recurse -Filter *.java | ForEach-Object { $_.FullName }
java -jar $ecj -source 1.8 -target 1.8 -cp $jar -d "$dir\classes" $src
if ($LASTEXITCODE -ne 0) { throw "ecj failed" }

$classes = Get-ChildItem "$dir\classes" -Recurse -Filter *.class | ForEach-Object { $_.FullName }
java -cp $r8 com.android.tools.r8.D8 --min-api 28 --lib $jar --output "$dir\dex" $classes
if ($LASTEXITCODE -ne 0) { throw "d8 failed" }

$resZip = Join-Path $dir "res.zip"
Remove-Item $resZip -Force -ErrorAction SilentlyContinue
$resDir = Join-Path $dir "res"
$resArgs = @()
if (Test-Path $resDir) {
    & "$bt\aapt2.exe" compile --dir $resDir -o $resZip
    if ($LASTEXITCODE -ne 0) { throw "aapt2 compile failed" }
    $resArgs = @($resZip)
}

& "$bt\aapt2.exe" link -o "$dir\base.apk" --manifest "$dir\AndroidManifest.xml" -I $jar @resArgs
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

& "$bt\apksigner.bat" sign --ks $ks --ks-pass pass:station123 --key-pass pass:station123 --out "$dir\airplaytv.apk" "$dir\aligned.apk"
if ($LASTEXITCODE -ne 0) { throw "apksigner failed" }

Get-Item "$dir\airplaytv.apk" | Select-Object Name, Length
