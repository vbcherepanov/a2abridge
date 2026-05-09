# install.ps1 — one-line installer for Windows (PowerShell 5.1+ or 7).
#
# Typical usage:
#   iwr -useb https://raw.githubusercontent.com/<owner>/a2abridge/main/install.ps1 | iex
#
# With flags:
#   $env:A2A_VERSION = "v0.2.0"; iwr -useb ... | iex
#
# Env overrides:
#   A2A_REPO         GitHub repo in owner/name (default: vbcherepanov/a2abridge)
#   A2A_VERSION      tag (default: latest release)
#   A2A_PREFIX       install prefix (default: $HOME\.a2abridge)
#   A2A_NO_SERVICE   "1" to skip Windows Service install
#   A2A_NO_IDE       "1" to skip writing IDE configs

[CmdletBinding()]
param(
    [string] $Version = $env:A2A_VERSION,
    [string] $Repo    = $(if ($env:A2A_REPO) { $env:A2A_REPO } else { "vbcherepanov/a2abridge" }),
    [string] $Prefix  = $(if ($env:A2A_PREFIX) { $env:A2A_PREFIX } else { Join-Path $HOME ".a2abridge" }),
    [switch] $DryRun
)

$ErrorActionPreference = "Stop"

# --- detect platform --------------------------------------------------
$arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { "386" }
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64" -or $env:PROCESSOR_ARCHITEW6432 -eq "ARM64") {
    $arch = "arm64"
}

# --- resolve version --------------------------------------------------
if (-not $Version) {
    Write-Host "→ resolving latest release for $Repo"
    $Version = (Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest").tag_name
}
if (-not $Version) {
    throw "could not resolve latest version"
}
Write-Host "→ installing a2abridge $Version (windows/$arch) into $Prefix"

# --- download + unzip -------------------------------------------------
$VersionStripped = $Version -replace "^v",""
$Asset = "a2abridge_${VersionStripped}_windows_${arch}.zip"
$Url   = "https://github.com/$Repo/releases/download/$Version/$Asset"
$Tmp   = New-Item -ItemType Directory -Path (Join-Path ([IO.Path]::GetTempPath()) ("a2abridge-install-" + [guid]::NewGuid().ToString("N").Substring(0,8)))
try {
    Write-Host "→ downloading $Url"
    Invoke-WebRequest -Uri $Url -OutFile (Join-Path $Tmp "a2abridge.zip")
    Expand-Archive -Path (Join-Path $Tmp "a2abridge.zip") -DestinationPath $Tmp -Force

    $BinDir = Join-Path $Prefix "bin"
    New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
    Move-Item -Force (Join-Path $Tmp "a2abridge.exe") (Join-Path $BinDir "a2abridge.exe")
} finally {
    Remove-Item -Recurse -Force $Tmp
}

$Bin = Join-Path $Prefix "bin\a2abridge.exe"

# --- register IDEs + skill + hook -------------------------------------
if ($env:A2A_NO_IDE -ne "1") {
    Write-Host "→ registering MCP server in detected IDEs"
    $applyArgs = @("install")
    if (-not $DryRun) { $applyArgs += "--apply" }
    & $Bin @applyArgs
}

# --- service supervisor ----------------------------------------------
if (-not $DryRun -and $env:A2A_NO_SERVICE -ne "1") {
    Write-Host "→ installing directory daemon"
    try { & $Bin service install }
    catch { Write-Warning "service install failed — retry manually: $Bin service install" }
}

# --- summary ---------------------------------------------------------
Write-Host ""
Write-Host "a2abridge $Version installed."
Write-Host ""
Write-Host "  binary:  $Bin"
Write-Host "  doctor:  $Bin doctor"
Write-Host "  service: $Bin service status"
Write-Host ""
Write-Host "Add to PATH so a2abridge is reachable:"
Write-Host "  setx PATH `"`$env:PATH;$Prefix\bin`""
Write-Host ""
Write-Host "Restart your IDEs (Claude Code, Codex, Cursor, ...) to pick up the new MCP server."
