# Battlefield: Bad Company PC - one-time install / build
#
# Builds bfbc.exe on this PC from the player's own game files:
#   1. checks assets\default.xex (supplied by the player, never shipped)
#   2. installs missing build tools with winget (VS Build Tools C++, LLVM, CMake, Ninja)
#   3. finds or downloads the ReXGlue SDK
#   4. generates the recompiled code from default.xex (rexglue codegen)
#   5. compiles bfbc.exe
#
# Run through Install.cmd (double-click) or the launcher's "Install game" button.

param(
    [string]$SdkDir = "",            # existing SDK (folder that contains bin\rexglue.exe)
    [string]$SdkTag = "v0.10.0",     # ReXGlue release this project is tested with (newer SDKs break the build/boot)
    [switch]$AnyXex,                 # skip the default.xex version check (unsupported)
    [string]$Preset = "win-amd64-release",
    [int]$Jobs = 0
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Step($msg) { Write-Host ""; Write-Host "==> $msg" -ForegroundColor Cyan }
function Fail($msg) { Write-Host ""; Write-Host "ERROR: $msg" -ForegroundColor Red; exit 1 }

# --- 1. game files ------------------------------------------------------------
Step "Checking game files"
$xex = Join-Path $root 'assets\default.xex'
if (-not (Test-Path $xex)) {
    Fail "assets\default.xex not found. Put the files extracted from your own game disc into the 'assets' folder."
}
# The function list in bfbc_manifest.toml and the hooks in src/mouse_look.cpp are tied
# to one executable: the Xbox 360 USA disc default.xex with the hash below.
$supportedXex = 'AE42F7CC30AB3217AF04F1C3C6A5DDB7E640D1E7DF73B32F2877905A7D4D2670'
$xexHash = (Get-FileHash $xex -Algorithm SHA256).Hash
if ($xexHash -ne $supportedXex) {
    $msg = "Unsupported default.xex (SHA-256 $xexHash).`n       Only the Xbox 360 USA disc default.xex with SHA-256 $supportedXex is supported;`n       other versions (e.g. other regions / title updates) have different code and will not work."
    if ($AnyXex) { Write-Host "Warning: $msg" -ForegroundColor Yellow } else { Fail "$msg`n       (Run tools\install.ps1 -AnyXex to try anyway.)" }
}
if (-not (Test-Path (Join-Path $root 'assets\dist'))) {
    Write-Host "Warning: assets\dist not found, the game files may be incomplete." -ForegroundColor Yellow
}

# --- 2. build tools -------------------------------------------------------------
Step "Checking build tools"
$winget = Get-Command winget -ErrorAction SilentlyContinue
function Install-Winget($id, $extra = @()) {
    if (-not $winget) { Fail "winget is not available; please install $id manually." }
    Write-Host "Installing: $id"
    & winget install --id $id -e --accept-source-agreements --accept-package-agreements @extra
}

$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
function Find-VS {
    if (-not (Test-Path $vswhere)) { return $null }
    & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath
}
$vs = Find-VS
if (-not $vs) {
    Install-Winget 'Microsoft.VisualStudio.2022.BuildTools' @('--override',
        '--quiet --wait --norestart --add Microsoft.VisualStudio.Workload.VCTools --add Microsoft.VisualStudio.Component.Windows11SDK.22621 --add Microsoft.VisualStudio.Component.VC.CMake.Project --includeRecommended')
    $vs = Find-VS
    if (-not $vs) { Fail "Could not install the Visual Studio C++ build tools." }
}
Write-Host "Visual Studio: $vs"

# clang: prefer the copy bundled with Visual Studio ("C++ Clang tools"), then a
# standalone LLVM, then one on PATH; install LLVM only when none exists.
$llvm = @((Join-Path $vs 'VC\Tools\Llvm\x64\bin'), (Join-Path $env:ProgramFiles 'LLVM\bin')) |
    Where-Object { Test-Path (Join-Path $_ 'clang++.exe') } | Select-Object -First 1
if (-not $llvm) {
    $onPath = Get-Command clang++ -ErrorAction SilentlyContinue
    if ($onPath) { $llvm = Split-Path -Parent $onPath.Source }
}
if (-not $llvm) {
    Install-Winget 'LLVM.LLVM'
    $llvm = Join-Path $env:ProgramFiles 'LLVM\bin'
    if (-not (Test-Path (Join-Path $llvm 'clang++.exe'))) { Fail "Could not install LLVM (clang)." }
}
Write-Host "clang: $llvm"

# Load the x64 developer environment (Windows SDK, rc.exe, link.exe, bundled CMake/Ninja).
$vcvars = Join-Path $vs 'VC\Auxiliary\Build\vcvars64.bat'
cmd /c "`"$vcvars`" >nul 2>&1 && set" | ForEach-Object {
    if ($_ -match '^([^=]+)=(.*)$') { Set-Item -Path "env:$($matches[1])" -Value $matches[2] }
}
$env:PATH = "$llvm;$env:PATH"
foreach ($tool in 'cmake', 'ninja') {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        if ($tool -eq 'cmake') { Install-Winget 'Kitware.CMake'; $env:PATH = "$env:ProgramFiles\CMake\bin;$env:PATH" }
        else { Install-Winget 'Ninja-build.Ninja'; $env:PATH = "$env:LOCALAPPDATA\Microsoft\WinGet\Links;$env:PATH" }
    }
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { Fail "$tool not found." }
}

# --- 3. ReXGlue SDK -------------------------------------------------------------
Step "ReXGlue SDK"
function Get-SdkVersion($dir) {
    $f = Get-ChildItem (Join-Path $dir 'lib\cmake') -Recurse -Filter '*ConfigVersion.cmake' -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($f -and ((Get-Content $f.FullName -Raw) -match 'PACKAGE_VERSION\s+"([^"]+)"')) { return $matches[1] }
    return ''
}
$wantVersion = $SdkTag.TrimStart('v')
$candidates = @($SdkDir, $env:REXSDK, (Join-Path $root 'tools\rexglue\win-amd64'), 'C:\rexglue\win-amd64') |
    Where-Object { $_ -and (Test-Path (Join-Path $_ 'bin\rexglue.exe')) }
$sdk = $null
foreach ($c in $candidates) {
    $v = Get-SdkVersion $c
    if ($c -eq $SdkDir -or -not $v -or $v.StartsWith($wantVersion)) { $sdk = $c; break }
    Write-Host "Skipping ReXGlue SDK $v at $c (this project needs $wantVersion)." -ForegroundColor Yellow
}
if (-not $sdk -and (Test-Path (Join-Path $root 'tools\rexglue'))) {
    Remove-Item (Join-Path $root 'tools\rexglue') -Recurse -Force   # wrong version from an older install
}
if (-not $sdk) {
    $api = 'https://api.github.com/repos/rexglue/rexglue-sdk/releases'
    $rel = if ($SdkTag) { Invoke-RestMethod "$api/tags/$SdkTag" } else { (Invoke-RestMethod $api) | Select-Object -First 1 }
    $asset = $rel.assets | Where-Object { $_.name -match 'win-amd64.*\.zip$' } | Select-Object -First 1
    if (-not $asset) { Fail "No Windows SDK archive in ReXGlue release $($rel.tag_name)." }
    $zip = Join-Path $env:TEMP $asset.name
    Write-Host "Downloading: $($asset.name)"
    Invoke-WebRequest $asset.browser_download_url -OutFile $zip
    $dest = Join-Path $root 'tools\rexglue'
    Expand-Archive $zip -DestinationPath $dest -Force
    $sdk = Get-ChildItem $dest -Recurse -Filter rexglue.exe | Select-Object -First 1 | ForEach-Object { Split-Path -Parent $_.DirectoryName }
    if (-not $sdk) { Fail "Could not unpack the SDK." }
}
$rexglue = Join-Path $sdk 'bin\rexglue.exe'
Write-Host "SDK: $sdk"

# --- 4. code generation from the player's default.xex ---------------------------
Step "Generating recompiled code from default.xex (a few minutes)"
& $rexglue codegen bfbc_manifest.toml
if ($LASTEXITCODE -ne 0) { Fail "rexglue codegen failed ($LASTEXITCODE)." }
if (-not (Get-ChildItem (Join-Path $root 'generated\default') -Filter '*.cpp' -ErrorAction SilentlyContinue)) {
    Fail "rexglue codegen produced no code in generated\default. Delete the 'generated\default' folder and run the install again."
}

if (-not (Test-Path (Join-Path $root 'metadata\achievements.toml'))) {
    & $rexglue init achievements assets\default.xex metadata
    if ($LASTEXITCODE -ne 0) { Write-Host "Achievement data could not be generated (the game still works)." -ForegroundColor Yellow }
}

# --- 5. build -------------------------------------------------------------------
Step "Compiling (can take 15-30 minutes)"
& cmake --preset $Preset "-DCMAKE_PREFIX_PATH=$sdk"
if ($LASTEXITCODE -ne 0) { Fail "CMake configure failed." }
$buildArgs = @('--build', '--preset', $Preset)
if ($Jobs -gt 0) { $buildArgs += @('--parallel', $Jobs) }
& cmake @buildArgs
if ($LASTEXITCODE -ne 0) { Fail "Build failed." }

$exe = Join-Path $root "out\build\$Preset\bfbc.exe"
if (-not (Test-Path $exe)) { Fail "bfbc.exe was not produced." }

Write-Host ""
Write-Host "Install complete! Open the launcher and press PLAY." -ForegroundColor Green
