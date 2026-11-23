<#
.SYNOPSIS
  Install a previously downloaded JikuNode release for the current Windows user.

.DESCRIPTION
  Copies jikunode.exe and jikunode-tray.exe to %LOCALAPPDATA%\Programs\jikunode,
  adds that folder to the user's PATH, registers the tray app to start at logon
  (HKCU Run key, no administrator rights needed) and starts it. The tray app
  runs the node without a console window and restarts it when it exits.

  Verify the archive against the signed checksums.txt before installing, or
  pass -Checksums to have this script check the SHA-256 for you.

.PARAMETER Archive
  The release zip (jikunode_<version>_windows_<arch>.zip) or a folder that
  holds jikunode.exe and jikunode-tray.exe.

.PARAMETER Checksums
  Optional checksums.txt from the same release; the archive's SHA-256 must
  match its entry.

.PARAMETER Port
  Loopback status port, if the default 16888 is taken on this computer.

.PARAMETER NoStart
  Install and register autostart, but do not start the tray app now.

.EXAMPLE
  .\install-user.ps1 .\jikunode_1.4.0_windows_amd64.zip -Checksums .\checksums.txt
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory = $true, Position = 0)][string]$Archive,
  [string]$Checksums,
  [ValidateRange(1, 65535)][int]$Port = 16888,
  [switch]$NoStart
)
Set-StrictMode -Version 3
$ErrorActionPreference = 'Stop'

$nodeExe = 'jikunode.exe'
$trayExe = 'jikunode-tray.exe'
$installDir = Join-Path $env:LOCALAPPDATA 'Programs\jikunode'
$runKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'

$source = (Resolve-Path -LiteralPath $Archive).Path
$staging = $null
try {
  if (Test-Path -LiteralPath $source -PathType Leaf) {
    if ($Checksums) {
      $name = Split-Path -Leaf $source
      $line = Get-Content -LiteralPath $Checksums | Where-Object { ($_ -split '\s+')[1] -replace '^\*', '' -eq $name }
      if (@($line).Count -ne 1) { throw "checksums.txt has no single entry for $name" }
      $want = ($line -split '\s+')[0].ToLowerInvariant()
      $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $source).Hash.ToLowerInvariant()
      if ($want -ne $got) { throw "checksum mismatch for $name" }
      Write-Host "Checksum OK for $name"
    }
    $staging = Join-Path ([IO.Path]::GetTempPath()) ("jikunode-install-" + [guid]::NewGuid())
    Expand-Archive -LiteralPath $source -DestinationPath $staging
    $source = $staging
  } elseif ($Checksums) {
    throw '-Checksums needs the release zip, not a folder'
  }
  foreach ($exe in $nodeExe, $trayExe) {
    if (-not (Test-Path -LiteralPath (Join-Path $source $exe) -PathType Leaf)) { throw "$exe not found in $Archive" }
  }

  # Stop a running tray app (it stops its node) so the files can be replaced.
  try {
    [Threading.EventWaitHandle]::OpenExisting('Local\JikuNodeTrayQuit').Set() | Out-Null
    for ($i = 0; $i -lt 50; $i++) {
      $mutex = $null
      if (-not [Threading.Mutex]::TryOpenExisting('Local\JikuNodeTray', [ref]$mutex)) { break }
      $mutex.Dispose()
      Start-Sleep -Milliseconds 200
    }
  } catch [Threading.WaitHandleCannotBeOpenedException] { }
  Get-Process -Name 'jikunode', 'jikunode-tray' -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -and ((Split-Path -Parent $_.Path) -eq $installDir) } |
    Stop-Process -Force

  New-Item -ItemType Directory -Force -Path $installDir | Out-Null
  foreach ($exe in $nodeExe, $trayExe) {
    Copy-Item -LiteralPath (Join-Path $source $exe) -Destination (Join-Path $installDir $exe) -Force
  }
  Unblock-File -LiteralPath (Join-Path $installDir $nodeExe), (Join-Path $installDir $trayExe)
  Write-Host "Installed to $installDir"

  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  $entries = @($userPath -split ';' | Where-Object { $_ })
  if ($entries -notcontains $installDir) {
    [Environment]::SetEnvironmentVariable('Path', (($entries + $installDir) -join ';'), 'User')
    Write-Host 'Added to your PATH (open a new terminal to use the jikunode command)'
  }
  $env:Path = "$env:Path;$installDir"

  $trayPath = Join-Path $installDir $trayExe
  $trayArgs = @()
  if ($Port -ne 16888) { $trayArgs = @('--port', "$Port") }
  $command = ('"' + $trayPath + '" ' + ($trayArgs -join ' ')).Trim()
  New-Item -Path $runKey -Force -ErrorAction SilentlyContinue | Out-Null
  Set-ItemProperty -Path $runKey -Name 'JikuNode' -Value $command
  Write-Host 'JikuNode will start when you sign in to Windows'

  # Start menu entry, to start the tray app again after Quit.
  $shortcut = Join-Path ([Environment]::GetFolderPath('Programs')) 'JikuNode.lnk'
  $link = (New-Object -ComObject WScript.Shell).CreateShortcut($shortcut)
  $link.TargetPath = $trayPath
  $link.Arguments = ($trayArgs -join ' ')
  $link.WorkingDirectory = $installDir
  $link.Description = 'JikuNode supplier egress node'
  $link.Save()

  if (-not $NoStart) {
    if ($trayArgs.Count) { Start-Process -FilePath $trayPath -ArgumentList $trayArgs } else { Start-Process -FilePath $trayPath }
    Write-Host 'JikuNode started; look for its icon in the notification area'
  }
  $portArg = ''
  if ($Port -ne 16888) { $portArg = " --port $Port" }
  Write-Host ''
  Write-Host 'Next: bind this node with the code from the provider console:'
  Write-Host '  jikunode bind <code> --name <node-name>'
  Write-Host "Check it with: jikunode status$portArg"
} finally {
  if ($staging) { Remove-Item -LiteralPath $staging -Recurse -Force -ErrorAction SilentlyContinue }
}
