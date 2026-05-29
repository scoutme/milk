# milk Installation Script (Windows native)
# Usage: irm https://raw.githubusercontent.com/scoutme/milk/main/install.ps1 | iex
#
# Environment variables:
#   $env:MILK_VERSION - Release tag to install, e.g. v0.2.0 (default: latest)

$ErrorActionPreference = 'Stop'

$Repo   = 'scoutme/milk'
$BinDir = "$env:LOCALAPPDATA\milk\bin"

function Write-Info    { Write-Host "==> $_" -ForegroundColor Cyan }
function Write-Success { Write-Host "==> $_" -ForegroundColor Green }
function Write-Warn    { Write-Host "Warning: $_" -ForegroundColor Yellow }

# ── resolve version ───────────────────────────────────────────────────────────

$Version = $env:MILK_VERSION
if (-not $Version) {
    "Resolving latest release..." | Write-Info
    try {
        $release = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
        $Version = $release.tag_name
    } catch { }
}
if (-not $Version) {
    throw "No published releases found. Set `$env:MILK_VERSION to a specific tag (e.g. v0.1.0), or build from source via WSL2: curl -fsSL https://raw.githubusercontent.com/$Repo/main/install-from-source.sh | sh"
}

# ── detect arch ───────────────────────────────────────────────────────────────

$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

# ── download ──────────────────────────────────────────────────────────────────

$BinaryName  = "milk-windows-$Arch.exe"
$BaseUrl     = "https://github.com/$Repo/releases/download/$Version"
$BinaryUrl   = "$BaseUrl/$BinaryName"
$ChecksumUrl = "$BaseUrl/$BinaryName.sha256"

"Downloading milk $Version (windows/$Arch)..." | Write-Info

New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
$TmpBin = Join-Path $env:TEMP "milk-install.exe"
$TmpSha = Join-Path $env:TEMP "milk-install.sha256"

Invoke-WebRequest -Uri $BinaryUrl   -OutFile $TmpBin  -UseBasicParsing
try { Invoke-WebRequest -Uri $ChecksumUrl -OutFile $TmpSha -UseBasicParsing } catch { }

# ── verify checksum ───────────────────────────────────────────────────────────

if (Test-Path $TmpSha) {
    $Expected = (Get-Content $TmpSha -Raw).Split(' ', [StringSplitOptions]::RemoveEmptyEntries)[0].Trim()
    $Actual   = (Get-FileHash $TmpBin -Algorithm SHA256).Hash.ToLower()
    if ($Actual -ne $Expected.ToLower()) {
        Remove-Item $TmpBin, $TmpSha -ErrorAction SilentlyContinue
        throw "Checksum mismatch (expected $Expected, got $Actual)."
    }
    "Checksum verified" | Write-Success
}

# ── install ───────────────────────────────────────────────────────────────────

$Dest = Join-Path $BinDir 'milk.exe'
Move-Item -Force $TmpBin $Dest
Remove-Item $TmpSha -ErrorAction SilentlyContinue

"Installed milk $Version to $Dest" | Write-Success

# ── PATH hint ─────────────────────────────────────────────────────────────────

$UserPath = [Environment]::GetEnvironmentVariable('PATH', 'User')
if ($UserPath -notlike "*$BinDir*") {
    [Environment]::SetEnvironmentVariable('PATH', "$UserPath;$BinDir", 'User')
    Write-Host ""
    Write-Host "Added $BinDir to your PATH (user scope)." -ForegroundColor Green
    Write-Host "Restart your terminal for it to take effect."
}

Write-Host ""
Write-Host "============================================" -ForegroundColor Green
Write-Host "  milk installed successfully!" -ForegroundColor Green
Write-Host "============================================" -ForegroundColor Green
Write-Host ""
Write-Host "Binary: $Dest"
Write-Host ""
Write-Host "Next steps:" -ForegroundColor White
Write-Host "  milk              # interactive mode"
Write-Host "  milk 'your prompt' # single-shot mode"
Write-Host "  milk --help       # all options"
Write-Host ""
Write-Host "Requirements: milk's local agent uses Unix shell tools (sh, find, grep)."
Write-Host "Run milk from Git Bash or WSL2 — cmd.exe / PowerShell are not supported."
Write-Host ""
Write-Host "Config file: $env:USERPROFILE\.milk\config.json (created on first run)"
Write-Host ""
Write-Host "For more information: https://github.com/$Repo"
Write-Host ""
