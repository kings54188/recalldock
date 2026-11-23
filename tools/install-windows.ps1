<#
.SYNOPSIS
Offline reviewed local installer for RecallDock on Windows x64.

.DESCRIPTION
Provides read-only preview, digest-CAS apply, protected rollback, and safe uninstall
without administrator privileges, telemetry, network downloads, or security-policy bypasses.
#>
[CmdletBinding()]
param(
    [string]$Release,
    [string]$InstallRoot,
    [string]$DataDir,
    [string[]]$Hosts = @(),
    [switch]$Interactive,
    [switch]$Apply,
    [string]$ExpectedSha256,
    [string]$BackupDir,
    [switch]$Rollback,
    [switch]$Uninstall,
    [switch]$Help
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Utf8NoBom = New-Object System.Text.UTF8Encoding($false)
[Console]::InputEncoding = $Utf8NoBom
[Console]::OutputEncoding = $Utf8NoBom

function Show-Usage {
    @"
RecallDock Windows Local Setup

Guided Interactive Install (recommended):
  powershell.exe -NoProfile -ExecutionPolicy Bypass -File tools\install-windows.ps1 -Interactive [-Hosts codex,claude-code,antigravity]

Preview JSON (non-interactive):
  powershell.exe -NoProfile -ExecutionPolicy Bypass -File tools\install-windows.ps1 [-Release C:\path\to\release] [-Hosts codex,claude-code,antigravity]

Apply (non-interactive):
  powershell.exe -NoProfile -ExecutionPolicy Bypass -File tools\install-windows.ps1 -Release C:\path\to\release -Hosts codex,claude-code -Apply -ExpectedSha256 <SHA256> -BackupDir C:\path\to\new-backup

Rollback:
  powershell.exe -NoProfile -ExecutionPolicy Bypass -File tools\install-windows.ps1 -Rollback -BackupDir C:\path\to\backup

Uninstall (preserves recalldock.db and operations.db):
  powershell.exe -NoProfile -ExecutionPolicy Bypass -File tools\install-windows.ps1 -Uninstall [-InstallRoot C:\path\to\install] [-BackupDir C:\path\to\backup]
"@ | Write-Output
}

if ($Help) {
    Show-Usage
    exit 0
}

function Fail([string]$Message) {
    [Console]::Error.WriteLine("install refused: $Message")
    exit 1
}

function Get-BytesSha256([byte[]]$Bytes) {
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $hash = $sha.ComputeHash($Bytes)
        return ([System.BitConverter]::ToString($hash)).Replace('-', '').ToLowerInvariant()
    } finally {
        $sha.Dispose()
    }
}

function Get-FileState([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) {
        return @{ kind = 'absent'; sha256 = 'absent'; bytes = $null }
    }
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
        Fail "symlink/reparse point is not supported for managed path: $Path"
    }
    if ($item.PSIsContainer) {
        Fail "expected regular file, found directory: $Path"
    }
    $bytes = [System.IO.File]::ReadAllBytes($Path)
    return @{ kind = 'file'; sha256 = (Get-BytesSha256 $bytes); bytes = $bytes }
}

function Assert-AbsoluteLocalPath([string]$Path, [string]$Label) {
    if ([string]::IsNullOrWhiteSpace($Path)) {
        Fail "$Label must be non-empty"
    }
    if ($Path.StartsWith('\\')) {
        Fail "$Label cannot be a UNC network path: $Path"
    }
    if (-not [System.IO.Path]::IsPathRooted($Path) -or $Path -notmatch '^[A-Za-z]:\\') {
        Fail "$Label must be an absolute local drive path: $Path"
    }
}

function Assert-Platform {
    if ($env:OS -ne 'Windows_NT') {
        Fail "Windows installer requires Windows"
    }
    $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    if ($arch -ne 'X64') {
        Fail "RecallDock Windows package currently supports x64 (amd64) only; detected $arch"
    }
}

function Verify-Release([string]$ReleasePath) {
    Assert-AbsoluteLocalPath $ReleasePath 'Release'
    $resolved = (Resolve-Path -LiteralPath $ReleasePath).Path
    $manifestPath = Join-Path $resolved 'release.json'
    if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
        Fail "missing release.json in $resolved"
    }
    $rawManifest = [System.IO.File]::ReadAllBytes($manifestPath)
    $manifest = ([System.Text.Encoding]::UTF8.GetString($rawManifest)) | ConvertFrom-Json
    if ($manifest.format -ne 1 -or [string]::IsNullOrWhiteSpace($manifest.version)) {
        Fail "unsupported release manifest"
    }
    if ($manifest.platform -ne 'windows' -or $manifest.architecture -ne 'amd64') {
        Fail "release manifest is not for windows/amd64"
    }
    $exePath = Join-Path $resolved 'runtime\recalldock.exe'
    if (-not (Test-Path -LiteralPath $exePath -PathType Leaf)) {
        Fail "missing runtime\recalldock.exe"
    }
    $listed = @{}
    foreach ($prop in $manifest.files.PSObject.Properties) {
        $rel = $prop.Name
        if ($rel.Contains('..')) { Fail "unsafe manifest member: $rel" }
        $full = Join-Path $resolved ($rel -replace '/', '\')
        if (-not (Test-Path -LiteralPath $full -PathType Leaf)) {
            Fail "missing listed release member: $rel"
        }
        $actual = Get-BytesSha256 ([System.IO.File]::ReadAllBytes($full))
        if ($actual -ne $prop.Value) {
            Fail "release integrity mismatch: $rel"
        }
        $listed[$rel.ToLowerInvariant()] = $true
    }
    $actualFiles = Get-ChildItem -LiteralPath $resolved -Recurse -File -Force
    foreach ($f in $actualFiles) {
        $rel = $f.FullName.Substring($resolved.Length).TrimStart('\') -replace '\\', '/'
        if ($rel -eq 'release.json') { continue }
        if (-not $listed.ContainsKey($rel.ToLowerInvariant())) {
            Fail "release contains unlisted file: $rel"
        }
    }
    $verOut = & $exePath version 2>&1
    if ($LASTEXITCODE -ne 0) {
        Fail "runtime verification failed: $verOut"
    }
    return @{
        path = $resolved
        manifest = $manifest
        sha256 = (Get-BytesSha256 $rawManifest)
        exe = $exePath
    }
}

function Update-CodexToml([string]$ExistingText, [string]$CommandPath) {
    $escaped = ($CommandPath | ConvertTo-Json -Compress)
    $newline = "`r`n"
    if ($ExistingText -and -not $ExistingText.Contains("`r`n") -and $ExistingText.Contains("`n")) {
        $newline = "`n"
    }
    if (-not [string]::IsNullOrEmpty($ExistingText)) {
        $lines = [System.Collections.Generic.List[string]]($ExistingText -split "\r?\n", -1)
        $sectionIdx = -1
        for ($i = 0; $i -lt $lines.Count; $i++) {
            if ($lines[$i].Trim() -eq '[mcp_servers.recalldock_gateway]') {
                $sectionIdx = $i
                break
            }
        }
        if ($sectionIdx -ge 0) {
            $cmdIdx = -1
            $endIdx = $lines.Count
            for ($j = $sectionIdx + 1; $j -lt $lines.Count; $j++) {
                $t = $lines[$j].Trim()
                if ($t.StartsWith('[')) {
                    $endIdx = $j
                    break
                }
                if ($t -match '^command\s*=') {
                    $cmdIdx = $j
                    break
                }
            }
            if ($cmdIdx -ge 0) {
                $lines[$cmdIdx] = "command = $escaped"
            } else {
                $lines.Insert($endIdx, "command = $escaped")
            }
            return ($lines -join $newline)
        }
    }
    $block = @(
        '[mcp_servers.recalldock_gateway]'
        "command = $escaped"
        'args = ["gateway", "serve", "--host", "codex"]'
        'enabled = true'
        ''
        '[mcp_servers.recalldock_gateway.tools.tools_find]'
        'approval_mode = "approve"'
        '[mcp_servers.recalldock_gateway.tools.tools_describe]'
        'approval_mode = "approve"'
        '[mcp_servers.recalldock_gateway.tools.tools_call]'
        'approval_mode = "approve"'
        '[mcp_servers.recalldock_gateway.tools.tools_release]'
        'approval_mode = "approve"'
        '[mcp_servers.recalldock_gateway.tools.work_find]'
        'approval_mode = "approve"'
        '[mcp_servers.recalldock_gateway.tools.work_read]'
        'approval_mode = "approve"'
        '[mcp_servers.recalldock_gateway.tools.work_checkpoint]'
        'approval_mode = "approve"'
    ) -join $newline
    if ([string]::IsNullOrWhiteSpace($ExistingText)) {
        return $block + $newline
    }
    return $ExistingText.TrimEnd("`r", "`n") + $newline + $newline + $block + $newline
}

function Update-ClaudeGatewayJson([string]$ExistingText, [string]$CommandPath) {
    $obj = [ordered]@{}
    if (-not [string]::IsNullOrWhiteSpace($ExistingText)) {
        $parsed = $ExistingText | ConvertFrom-Json
        foreach ($p in $parsed.PSObject.Properties) {
            $obj[$p.Name] = $p.Value
        }
    }
    $servers = [ordered]@{}
    if ($obj.Contains('mcpServers') -and $null -ne $obj['mcpServers']) {
        foreach ($p in $obj['mcpServers'].PSObject.Properties) {
            $servers[$p.Name] = $p.Value
        }
    }
    if ($servers.Contains('recalldock_gateway') -and $null -ne $servers['recalldock_gateway']) {
        $gw = [ordered]@{}
        foreach ($p in $servers['recalldock_gateway'].PSObject.Properties) {
            $gw[$p.Name] = $p.Value
        }
        $gw['command'] = $CommandPath
        $servers['recalldock_gateway'] = $gw
    } else {
        $servers['recalldock_gateway'] = [ordered]@{
            command = $CommandPath
            args = @('gateway', 'serve', '--host', 'claude-code')
        }
    }
    $obj['mcpServers'] = $servers
    return ($obj | ConvertTo-Json -Depth 16) + "`n"
}

function Write-BytesAtomic([string]$TargetPath, [byte[]]$Bytes) {
    $parent = Split-Path -Parent $TargetPath
    if (-not (Test-Path -LiteralPath $parent)) {
        New-Item -ItemType Directory -Path $parent -Force | Out-Null
    }
    $tmp = Join-Path $parent (".recalldock-tmp-" + [Guid]::NewGuid().ToString('N'))
    try {
        [System.IO.File]::WriteAllBytes($tmp, $Bytes)
        Move-Item -LiteralPath $tmp -Destination $TargetPath -Force
    } finally {
        if (Test-Path -LiteralPath $tmp) {
            Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
        }
    }
}

function Invoke-ProtectedRollback([string]$BackupPath) {
    Assert-AbsoluteLocalPath $BackupPath 'BackupDir'
    $recordPath = Join-Path $BackupPath 'rollback.json'
    if (-not (Test-Path -LiteralPath $recordPath -PathType Leaf)) {
        Fail "missing rollback.json in $BackupPath"
    }
    $record = ([System.Text.Encoding]::UTF8.GetString([System.IO.File]::ReadAllBytes($recordPath))) | ConvertFrom-Json
    $pending = @()
    $retained = 0
    foreach ($entry in $record.files) {
        $state = Get-FileState $entry.path
        if ($state.sha256 -ne $entry.after_sha256) {
            Fail "refusing rollback; target was modified after install: $($entry.path)"
        }
        if ($entry.retain_on_rollback) {
            $retained++
            continue
        }
        $pending += $entry
    }
    $restored = 0
    foreach ($entry in $pending) {
        if ($entry.before_sha256 -eq 'absent') {
            if (Test-Path -LiteralPath $entry.path) {
                Remove-Item -LiteralPath $entry.path -Force
            }
        } else {
            $bakFile = Join-Path $BackupPath $entry.backup_file
            $bakBytes = [System.IO.File]::ReadAllBytes($bakFile)
            if ((Get-BytesSha256 $bakBytes) -ne $entry.before_sha256) {
                Fail "backup digest mismatch for $($entry.path)"
            }
            Write-BytesAtomic $entry.path $bakBytes
        }
        $restored++
    }
    @{ ok = $true; restored = $restored; retained = $retained } | ConvertTo-Json -Depth 6 | Write-Output
}

Assert-Platform

if ($Rollback) {
    if ($Interactive -or $Apply -or $Uninstall -or $Release -or $ExpectedSha256 -or $Hosts.Count -gt 0 -or -not $BackupDir) {
        Fail "-Rollback requires only -BackupDir"
    }
    Invoke-ProtectedRollback $BackupDir
    exit 0
}

if ($Uninstall) {
    if ($Interactive -or $Apply -or $Rollback -or $Release) {
        Fail "-Uninstall cannot be combined with -Interactive, -Apply, -Rollback, or -Release"
    }
    if (-not $InstallRoot) {
        if (-not $env:LOCALAPPDATA) { Fail "LOCALAPPDATA is unset; specify -InstallRoot" }
        $InstallRoot = Join-Path $env:LOCALAPPDATA 'RecallDock'
    }
    Assert-AbsoluteLocalPath $InstallRoot 'InstallRoot'
    if ($BackupDir) {
        Invoke-ProtectedRollback $BackupDir | Out-Null
    }
    $binDir = Join-Path $InstallRoot 'bin'
    $removed = @()
    foreach ($name in @('recalldock.exe', 'recalldock-claude.cmd')) {
        $p = Join-Path $binDir $name
        if (Test-Path -LiteralPath $p -PathType Leaf) {
            Remove-Item -LiteralPath $p -Force
            $removed += $p
        }
    }
    if ((Test-Path -LiteralPath $binDir -PathType Container) -and (@(Get-ChildItem -LiteralPath $binDir -Force).Count -eq 0)) {
        Remove-Item -LiteralPath $binDir -Force
    }
    @{
        ok = $true
        removed = $removed
        preserved_data_note = "User databases (recalldock.db, operations.db) in $InstallRoot were preserved."
    } | ConvertTo-Json -Depth 6 | Write-Output
    exit 0
}

if (-not $Release) {
    $candidateRel = Split-Path -Parent $PSScriptRoot
    if ($candidateRel -and (Test-Path -LiteralPath (Join-Path $candidateRel 'release.json') -PathType Leaf)) {
        $Release = $candidateRel
    } else {
        Fail "-Release is required (or pass -Help)"
    }
}

if ($Interactive) {
    if ($Apply -or $ExpectedSha256) {
        Fail "-Interactive cannot be combined with -Apply or -ExpectedSha256"
    }
    if ([Console]::IsInputRedirected -or -not [Environment]::UserInteractive) {
        Fail "-Interactive requires an interactive terminal session; use preview and -Apply -ExpectedSha256 in scripts"
    }
}

$rel = Verify-Release $Release
if (-not $InstallRoot) {
    if (-not $env:LOCALAPPDATA) { Fail "LOCALAPPDATA is unset; specify -InstallRoot" }
    $InstallRoot = Join-Path $env:LOCALAPPDATA 'RecallDock'
}
Assert-AbsoluteLocalPath $InstallRoot 'InstallRoot'

if (-not $DataDir) {
    $DataDir = $InstallRoot
}
Assert-AbsoluteLocalPath $DataDir 'DataDir'

$userProfile = $env:USERPROFILE
if ([string]::IsNullOrWhiteSpace($userProfile)) { Fail "USERPROFILE is unset" }
Assert-AbsoluteLocalPath $userProfile 'USERPROFILE'

$selectedHosts = @()
if ($Interactive -and $Hosts.Count -eq 0) {
    if (Test-Path -LiteralPath (Join-Path $userProfile '.codex') -PathType Container) { $selectedHosts += 'codex' }
    if ((Test-Path -LiteralPath (Join-Path $userProfile '.claude')) -or (Test-Path -LiteralPath (Join-Path $userProfile '.claude.json'))) { $selectedHosts += 'claude-code' }
    if ((Test-Path -LiteralPath (Join-Path $userProfile '.gemini') -PathType Container) -or (Get-Command agy -ErrorAction SilentlyContinue)) { $selectedHosts += 'antigravity' }
}
foreach ($h in $Hosts) {
    foreach ($part in ($h -split ',')) {
        $trimmed = $part.Trim().ToLowerInvariant()
        if ($trimmed -eq '') { continue }
        if ($trimmed -notin @('codex', 'claude-code', 'antigravity', 'dsh')) {
            Fail "unsupported host: $trimmed"
        }
        if ($selectedHosts -contains $trimmed) {
            Fail "duplicate host selection: $trimmed"
        }
        $selectedHosts += $trimmed
    }
}

$entries = @()
$notes = @(
    'No knowledge DB, registry grants, or unrelated MCP configuration changes.'
    'Host plugins/hooks/skills require the host normal trust workflow against release\marketplace.'
    'Other MCP entries remain enabled; this is not a whole-host tool security boundary.'
    'Unprivileged user-level install; RecallDock does not pin host official versions.'
)

function Add-PlannedFile([string]$Path, [byte[]]$AfterBytes, [bool]$RetainOnRollback = $false) {
    $state = Get-FileState $Path
    $afterSha = Get-BytesSha256 $AfterBytes
    if ($state.sha256 -ne $afterSha) {
        $script:entries += @{
            path = $Path
            before_sha256 = $state.sha256
            after_sha256 = $afterSha
            after_bytes = $AfterBytes
            before_bytes = $state.bytes
            retain_on_rollback = $RetainOnRollback
        }
    }
}

$installedExe = Join-Path $InstallRoot 'bin\recalldock.exe'
$runtimeBytes = [System.IO.File]::ReadAllBytes($rel.exe)
Add-PlannedFile $installedExe $runtimeBytes $false

foreach ($h in $selectedHosts) {
    switch ($h) {
        'codex' {
            $codexCfg = Join-Path $userProfile '.codex\config.toml'
            $curState = Get-FileState $codexCfg
            $curText = if ($curState.bytes) { $Utf8NoBom.GetString($curState.bytes) } else { '' }
            $nextText = Update-CodexToml $curText $installedExe
            Add-PlannedFile $codexCfg ($Utf8NoBom.GetBytes($nextText)) $false
        }
        'claude-code' {
            $claudeJson = Join-Path $DataDir 'claude-gateway.json'
            $curState = Get-FileState $claudeJson
            $curText = if ($curState.bytes) { $Utf8NoBom.GetString($curState.bytes) } else { '' }
            $nextText = Update-ClaudeGatewayJson $curText $installedExe
            Add-PlannedFile $claudeJson ($Utf8NoBom.GetBytes($nextText)) $false
            $wrapper = Join-Path $InstallRoot 'bin\recalldock-claude.cmd'
            $wrapperText = "@echo off`r`nclaude --mcp-config `"$claudeJson`" --strict-mcp-config %*`r`n"
            Add-PlannedFile $wrapper ($Utf8NoBom.GetBytes($wrapperText)) $false
        }
        'antigravity' {
            $agDir = Join-Path $rel.path 'marketplace\plugins\antigravity'
            if (-not (Test-Path -LiteralPath (Join-Path $agDir 'plugin.json') -PathType Leaf)) {
                Fail "selected release has no native Antigravity plugin"
            }
            $notes += "Antigravity uses its native plugin manager: agy plugin validate `"$agDir`" --strict --json && agy plugin install `"$agDir`". Remember to ensure installed bin dir `"$(Join-Path $InstallRoot 'bin')`" is on user PATH or configure absolute command path."
        }
        'dsh' {
            $notes += 'Windows native DSH integration is not verified in this release; skipped automated DSH config edits.'
        }
    }
}

$policyPath = Join-Path $DataDir 'candidate-admission.json'
if (Test-Path -LiteralPath $policyPath -PathType Leaf) {
    $pState = Get-FileState $policyPath
    if ($pState.bytes.Length -le 512) {
        try {
            $pObj = ($Utf8NoBom.GetString($pState.bytes)) | ConvertFrom-Json
            $keys = @($pObj.PSObject.Properties | ForEach-Object { $_.Name } | Sort-Object)
            if (($keys -join ',') -eq 'mode' -and ($pObj.mode -in @('checked', 'duplicates'))) {
                $hardened = ([ordered]@{ version = 2; mode = "$($pObj.mode)-v2" } | ConvertTo-Json -Compress) + "`n"
                Add-PlannedFile $policyPath ($Utf8NoBom.GetBytes($hardened)) $true
                $notes += 'Versioned automatic-admission policy is hardened first and retained on rollback.'
            } elseif (($keys -join ',') -eq 'mode,version' -and $pObj.version -eq 3 -and $pObj.mode -eq 'tiered-v3') {
                $hardened = ([ordered]@{ version = 4; mode = 'tiered-v4' } | ConvertTo-Json -Compress) + "`n"
                Add-PlannedFile $policyPath ($Utf8NoBom.GetBytes($hardened)) $true
                $notes += 'Tiered evidence verification is hardened to tiered-v4 and retained on rollback.'
            }
        } catch {
            # Unknown or malformed policy stays untouched (fail-closed).
        }
    }
}

# Order policy updates first
$orderedEntries = @($entries | Where-Object { $_.retain_on_rollback }) + @($entries | Where-Object { -not $_.retain_on_rollback })

$planForHash = [ordered]@{
    version = 1
    release = $rel.path
    release_sha256 = $rel.sha256
    release_version = $rel.manifest.version
    install_root = $InstallRoot
    data_dir = $DataDir
    hosts = @($selectedHosts)
    files = @($orderedEntries | ForEach-Object {
        [ordered]@{
            path = $_.path
            before_sha256 = $_.before_sha256
            after_sha256 = $_.after_sha256
            retain_on_rollback = $_.retain_on_rollback
        }
    })
}
$planSha = Get-BytesSha256 ($Utf8NoBom.GetBytes(($planForHash | ConvertTo-Json -Depth 10 -Compress)))

$preview = [ordered]@{
    plan_sha256 = $planSha
    release_version = $rel.manifest.version
    release_sha256 = $rel.sha256
    install_root = $InstallRoot
    data_dir = $DataDir
    files = @($orderedEntries | ForEach-Object {
        $item = [ordered]@{
            path = $_.path
            before = $_.before_sha256
            after = $_.after_sha256
        }
        if ($_.retain_on_rollback) { $item['retain_on_rollback'] = $true }
        $item
    })
    notes = $notes
}

if ($Interactive) {
    if (-not $BackupDir) {
        $bakRoot = Join-Path $DataDir 'install-backups'
        $stamp = "$($rel.manifest.version)-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
        $candBak = Join-Path $bakRoot $stamp
        $idx = 2
        while (Test-Path -LiteralPath $candBak) {
            $candBak = Join-Path $bakRoot "$stamp-$idx"
            $idx++
        }
        $BackupDir = $candBak
    }
    Assert-AbsoluteLocalPath $BackupDir 'BackupDir'
    if (Test-Path -LiteralPath $BackupDir) {
        Fail "BackupDir must not already exist: $BackupDir"
    }
    Write-Output "RecallDock $($rel.manifest.version) Windows install plan"
    Write-Output "  plan_sha256: $planSha"
    Write-Output "  install_root: $InstallRoot"
    Write-Output "  hosts: $(if ($selectedHosts.Count -gt 0) { $selectedHosts -join ', ' } else { '(CLI binary only)' })"
    if ($orderedEntries.Count -eq 0) {
        Write-Output "  files: none (already up to date)"
        @{ ok = $true; applied = 0; notes = @('already installed') } | ConvertTo-Json -Depth 4 | Write-Output
        exit 0
    }
    Write-Output "  backup_dir: $BackupDir"
    Write-Output "  files to update:"
    foreach ($f in $orderedEntries) {
        $kind = if ($f.before_sha256 -eq 'absent') { 'new' } else { 'update' }
        $ret = if ($f.retain_on_rollback) { ' [retain_on_rollback]' } else { '' }
        Write-Output "    - [$kind] $($f.path)$ret"
    }
    foreach ($n in $notes) {
        Write-Output "  note: $n"
    }
    $reply = Read-Host 'Apply these changes? [y/N]'
    if ($reply.Trim().ToLowerInvariant() -notin @('y', 'yes')) {
        [Console]::Error.WriteLine('Cancelled. No files were changed.')
        exit 1
    }
    $Apply = $true
    $ExpectedSha256 = $planSha
} elseif (-not $Apply) {
    if ($ExpectedSha256 -or $BackupDir) {
        Fail "ExpectedSha256 and BackupDir require -Apply"
    }
    $preview | ConvertTo-Json -Depth 10 | Write-Output
    exit 0
}

if ($ExpectedSha256 -ne $planSha) {
    Fail "plan digest mismatch (expected $planSha, got $ExpectedSha256)"
}
Assert-AbsoluteLocalPath $BackupDir 'BackupDir'
if (Test-Path -LiteralPath $BackupDir) {
    Fail "BackupDir must not already exist: $BackupDir"
}

if ($orderedEntries.Count -eq 0) {
    @{ ok = $true; applied = 0; notes = @('already installed') } | ConvertTo-Json -Depth 4 | Write-Output
    exit 0
}

New-Item -ItemType Directory -Path $BackupDir -Force | Out-Null
$recordFiles = @()
$idx = 0
foreach ($e in $orderedEntries) {
    $cur = Get-FileState $e.path
    if ($cur.sha256 -ne $e.before_sha256) {
        Fail "configuration changed since preview: $($e.path)"
    }
    $bakRel = ''
    if ($e.before_sha256 -ne 'absent') {
        $idx++
        $bakRel = "bak-$idx.dat"
        [System.IO.File]::WriteAllBytes((Join-Path $BackupDir $bakRel), $e.before_bytes)
    }
    $recordFiles += [ordered]@{
        path = $e.path
        before_sha256 = $e.before_sha256
        after_sha256 = $e.after_sha256
        backup_file = $bakRel
        retain_on_rollback = $e.retain_on_rollback
    }
}
$rollbackDoc = [ordered]@{
    format = 1
    release_version = $rel.manifest.version
    files = $recordFiles
}
[System.IO.File]::WriteAllText((Join-Path $BackupDir 'rollback.json'), ($rollbackDoc | ConvertTo-Json -Depth 10), $Utf8NoBom)

foreach ($e in $orderedEntries) {
    Write-BytesAtomic $e.path $e.after_bytes
}

@{ ok = $true; applied = $orderedEntries.Count; backup_dir = $BackupDir; plan_sha256 = $planSha } | ConvertTo-Json -Depth 6 | Write-Output
