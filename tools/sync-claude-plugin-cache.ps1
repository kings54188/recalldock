<#
.SYNOPSIS
    Mirror this fork's plugin trees into the installed plugin caches.
.DESCRIPTION
    Both hosts install a directory-marketplace plugin as a COPY, so editing the
    fork's plugin/ tree does not change an already-installed plugin:

      plugin\claude-code -> %USERPROFILE%\.claude\plugins\cache\engram\engram\0.1.1
      plugin\codex       -> %CODEX_HOME%\plugins\cache\engram\engram\0.1.1

    Run this after changing plugin scripts or skills, or reinstall the plugin
    through the host CLI instead. Targets that are not installed are skipped.
.PARAMETER Pair
    Explicit "source|cache" pairs, replacing the defaults.
.PARAMETER Report
    Report per-file drift only; write nothing.
.EXAMPLE
    pwsh -NoProfile -File tools/sync-claude-plugin-cache.ps1 -Report
    pwsh -NoProfile -File tools/sync-claude-plugin-cache.ps1
#>
[CmdletBinding()]
param(
    [string[]]$Pair,
    [switch]$Report
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path $PSScriptRoot -Parent
$codexHome = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $env:USERPROFILE '.codex' }

$pairs = if ($Pair) {
    foreach ($entry in $Pair) {
        $parts = $entry -split '\|', 2
        if ($parts.Count -ne 2) { throw "Pair must be 'source|cache': $entry" }
        [pscustomobject]@{ Source = $parts[0]; Cache = $parts[1] }
    }
} else {
    [pscustomobject]@{ Source = (Join-Path $repo 'plugin\claude-code'); Cache = (Join-Path $env:USERPROFILE '.claude\plugins\cache\engram\engram\0.1.1') },
    [pscustomobject]@{ Source = (Join-Path $repo 'plugin\codex');       Cache = (Join-Path $codexHome 'plugins\cache\engram\engram\0.1.1') }
}

foreach ($p in $pairs) {
    if (-not (Test-Path -LiteralPath $p.Source)) { throw "Source is missing: $($p.Source)" }
    if (-not (Test-Path -LiteralPath $p.Cache)) { Write-Output "skip (target not installed): $($p.Cache)"; continue }

    $same = 0
    $written = 0
    foreach ($file in Get-ChildItem -LiteralPath $p.Source -Recurse -File) {
        $rel = $file.FullName.Substring($p.Source.Length).TrimStart('\')
        $target = Join-Path $p.Cache $rel
        $identical = (Test-Path -LiteralPath $target) -and
            ((Get-FileHash -LiteralPath $file.FullName -Algorithm SHA256).Hash -eq (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash)
        if ($identical) { $same++; continue }
        if ($Report) { Write-Output "  drift: $rel"; continue }
        $dir = Split-Path -Parent $target
        if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
        Copy-Item -LiteralPath $file.FullName -Destination $target -Force
        Write-Output "  synced: $rel"
        $written++
    }
    Write-Output ("{0} -> {1}: identical={2} synced={3}" -f $p.Source, $p.Cache, $same, $written)
}
if ($Report) { Write-Output 'report only - nothing written' }