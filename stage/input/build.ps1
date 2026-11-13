$ErrorActionPreference = "Stop"
$dir = Split-Path -Parent $MyInvocation.MyCommand.Path
$bt = Join-Path (Split-Path -Parent $dir) "bt"
Remove-Item "$dir\classes", "$dir\dexout" -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path "$dir\classes", "$dir\dexout" | Out-Null
java -jar "$bt\ecj.jar" -source 1.8 -target 1.8 -cp "$bt\android.jar" -d "$dir\classes" "$dir\InputHelper.java"
if ($LASTEXITCODE -ne 0) { throw "ecj failed" }
$classes = Get-ChildItem "$dir\classes\*.class" | ForEach-Object { $_.FullName }
java -cp "$bt\r8.jar" com.android.tools.r8.D8 --min-api 28 --lib "$bt\android.jar" --output "$dir\dexout" $classes
if ($LASTEXITCODE -ne 0) { throw "d8 failed" }
Get-ChildItem "$dir\dexout" | Select-Object Name, Length
