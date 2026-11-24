<#
.SYNOPSIS
Builds jikunode.exe and jikunode-tray.exe from this checkout, the same way the
release workflow does.

.DESCRIPTION
Run it from anywhere in the repository. It needs Go (the version in go.mod)
and nothing else: no C compiler, no cgo. The programs go to
dist\jikunode_<version>_windows_<arch>\ (with -Portable, a folder ending in
_portable that also holds the jikunode.portable marker). -Zip also makes a
.zip next to the folder, which install-user.ps1 accepts.

.EXAMPLE
.\deploy\windows\build.ps1
.\deploy\windows\build.ps1 -Version 0.6.0 -Zip
.\deploy\windows\build.ps1 -Portable
.\deploy\windows\build.ps1 -Arch arm64
#>
[CmdletBinding()]
param(
  # Version stamped into jikunode.exe (shown by `jikunode version` and the
  # status page). Without -Version the default in cmd/jikunode/main.go stays.
  [string]$Version,
  # amd64 or arm64; defaults to this computer's architecture.
  [ValidateSet('', 'amd64', 'arm64')][string]$Arch = '',
  # Output folder, relative to the repository root.
  [string]$Out = 'dist',
  # Add the jikunode.portable marker (data stays next to the programs).
  [switch]$Portable,
  # Also zip the folder.
  [switch]$Zip
)
$ErrorActionPreference = 'Stop'

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  throw 'Go was not found. Install it from https://go.dev/dl/ (the version in go.mod or newer) and open a new terminal.'
}
$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if (-not $Arch) {
  $Arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
}
$label = if ($Version) { $Version } else { 'dev' }
$name = "jikunode_${label}_windows_${Arch}"
if ($Portable) { $name += '_portable' }
$dir = [System.IO.Path]::GetFullPath((Join-Path (Join-Path $root $Out) $name))
New-Item -ItemType Directory -Force $dir | Out-Null

$ldflags = '-s -w'
if ($Version) { $ldflags += " -X main.Version=$Version" }

$saved = @{ CGO_ENABLED = $env:CGO_ENABLED; GOOS = $env:GOOS; GOARCH = $env:GOARCH }
Push-Location $root
try {
  $env:CGO_ENABLED = '0'; $env:GOOS = 'windows'; $env:GOARCH = $Arch
  Write-Host "Building jikunode.exe ($Arch)..."
  go build -trimpath -ldflags $ldflags -o (Join-Path $dir 'jikunode.exe') ./cmd/jikunode
  if ($LASTEXITCODE -ne 0) { throw 'go build ./cmd/jikunode failed' }
  Write-Host "Building jikunode-tray.exe ($Arch)..."
  # -H=windowsgui: the tray app opens no console window.
  go build -trimpath -ldflags '-s -w -H=windowsgui' -o (Join-Path $dir 'jikunode-tray.exe') ./cmd/jikunode-tray
  if ($LASTEXITCODE -ne 0) { throw 'go build ./cmd/jikunode-tray failed' }
} finally {
  Pop-Location
  foreach ($k in $saved.Keys) {
    if ($null -eq $saved[$k]) { Remove-Item "env:$k" -ErrorAction SilentlyContinue }
    else { Set-Item "env:$k" $saved[$k] }
  }
}

$files = @('jikunode.exe', 'jikunode-tray.exe')
if ($Portable) {
  $marker = Join-Path $dir 'jikunode.portable'
  if (-not (Test-Path $marker)) { New-Item -ItemType File $marker | Out-Null }
  $files += 'jikunode.portable'
}
if ($Zip) {
  $zipPath = "$dir.zip"
  Remove-Item $zipPath -ErrorAction SilentlyContinue
  Compress-Archive -Path ($files | ForEach-Object { Join-Path $dir $_ }) -DestinationPath $zipPath
  Write-Host "Zip:    $zipPath"
}
Write-Host "Folder: $dir"
