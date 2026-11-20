$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Install Go 1.26+ first.' }
if (-not (Get-Command npm -ErrorAction SilentlyContinue)) { throw 'Install Node.js/npm first.' }
if (-not (Test-Path 'frontend/node_modules')) { throw 'Run npm ci in frontend first.' }
if (-not (Test-Path 'assets/icon.ico')) { throw 'Missing assets/icon.ico.' }

npm --prefix frontend run build
if ($LASTEXITCODE -ne 0) { throw 'Frontend build failed.' }

New-Item -ItemType Directory -Force -Path 'release' | Out-Null
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
try {
  go build -trimpath -ldflags='-s -w -H=windowsgui' -o 'release/BANDIT.exe' .
  if ($LASTEXITCODE -ne 0) { throw 'Windows EXE build failed.' }

  Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class BanditIconResource {
  [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
  public static extern IntPtr BeginUpdateResource(string file, bool deleteExisting);
  [DllImport("kernel32.dll", SetLastError = true)]
  public static extern bool UpdateResource(IntPtr update, IntPtr type, IntPtr name, ushort language, byte[] data, uint size);
  [DllImport("kernel32.dll", SetLastError = true)]
  public static extern bool EndUpdateResource(IntPtr update, bool discard);
}
'@
  $icon = [IO.File]::ReadAllBytes((Join-Path $PSScriptRoot 'assets/icon.ico'))
  if ($icon.Length -lt 6 -or [BitConverter]::ToUInt16($icon, 0) -ne 0 -or [BitConverter]::ToUInt16($icon, 2) -ne 1) {
    throw 'assets/icon.ico is not a valid Windows icon file.'
  }
  $count = [BitConverter]::ToUInt16($icon, 4)
  if ($count -eq 0 -or $icon.Length -lt 6 + 16 * $count) { throw 'assets/icon.ico has an invalid image directory.' }
  $group = [byte[]]::new(6 + 14 * $count)
  [Array]::Copy($icon, 0, $group, 0, 6)
  $update = [BanditIconResource]::BeginUpdateResource((Join-Path $PSScriptRoot 'release/BANDIT.exe'), $false)
  if ($update -eq [IntPtr]::Zero) { throw "BeginUpdateResource failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())" }
  $commit = $false
  try {
    for ($i = 0; $i -lt $count; $i++) {
      $entry = 6 + 16 * $i
      $size = [BitConverter]::ToUInt32($icon, $entry + 8)
      $offset = [BitConverter]::ToUInt32($icon, $entry + 12)
      if ($size -eq 0 -or $offset -gt $icon.Length -or $size -gt $icon.Length - $offset) { throw "assets/icon.ico image $i is out of bounds." }
      $image = [byte[]]::new([int]$size)
      [Array]::Copy($icon, [int]$offset, $image, 0, [int]$size)
      $id = $i + 1
      [Array]::Copy($icon, $entry, $group, 6 + 14 * $i, 12)
      [Array]::Copy([BitConverter]::GetBytes([UInt16]$id), 0, $group, 18 + 14 * $i, 2)
      if (-not [BanditIconResource]::UpdateResource($update, [IntPtr]3, [IntPtr]$id, 0, $image, [uint32]$size)) {
        throw "UpdateResource(RT_ICON $id) failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
      }
    }
    if (-not [BanditIconResource]::UpdateResource($update, [IntPtr]14, [IntPtr]1, 0, $group, [uint32]$group.Length)) {
      throw "UpdateResource(RT_GROUP_ICON) failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
    }
    $commit = $true
  } finally {
    if (-not [BanditIconResource]::EndUpdateResource($update, -not $commit)) {
      throw "EndUpdateResource failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
    }
  }
} finally {
  Remove-Item Env:GOOS -ErrorAction SilentlyContinue
  Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
}

Get-Item 'release/BANDIT.exe' | Select-Object FullName,Length
