$ErrorActionPreference = "Stop"
$dir = Split-Path -Parent $MyInvocation.MyCommand.Path
Remove-Item "$dir\classes", "$dir\dexout" -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path "$dir\classes", "$dir\dexout" | Out-Null
java -jar "$dir\ecj.jar" -source 1.8 -target 1.8 -cp "$dir\android.jar" -d "$dir\classes" "$dir\BtHelper.java"
if ($LASTEXITCODE -ne 0) { throw "ecj failed" }
$classes = Get-ChildItem "$dir\classes\*.class" | ForEach-Object { $_.FullName }
java -cp "$dir\r8.jar" com.android.tools.r8.D8 --min-api 28 --lib "$dir\android.jar" --output "$dir\dexout" $classes
if ($LASTEXITCODE -ne 0) { throw "d8 failed" }
Get-ChildItem "$dir\dexout" | Select-Object Name, Length
