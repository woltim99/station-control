$ErrorActionPreference = "Stop"
$dir = Split-Path -Parent $MyInvocation.MyCommand.Path
$bt = Join-Path (Split-Path -Parent $dir) "bt"
foreach ($name in @("AudioProbe", "AudioViz", "TapProbe")) {
    $src = Join-Path $dir "$name.java"
    $classes = Join-Path $dir "$name-classes"
    $out = Join-Path $dir "$name-dex"
    Remove-Item $classes, $out -Recurse -Force -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force -Path $classes, $out | Out-Null
    java -jar "$bt\ecj.jar" -source 1.8 -target 1.8 -cp "$bt\android.jar" -d $classes $src
    if ($LASTEXITCODE -ne 0) { throw "ecj failed: $name" }
    $files = Get-ChildItem "$classes\*.class" | ForEach-Object { $_.FullName }
    java -cp "$bt\r8.jar" com.android.tools.r8.D8 --min-api 28 --lib "$bt\android.jar" --output $out $files
    if ($LASTEXITCODE -ne 0) { throw "d8 failed: $name" }
    Copy-Item (Join-Path $out "classes.dex") (Join-Path $dir "$name.dex") -Force
    Get-Item (Join-Path $dir "$name.dex") | Select-Object Name, Length
}
