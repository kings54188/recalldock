# RecallDock on Windows (x64 Native)

RecallDock provides native Windows x64 (`windows/amd64`) support without requiring WSL2 or administrator privileges.

## Supported Platform & Boundaries

- **Supported**: Windows 10 / 11 x64 (`amd64`), verified in CI on `windows-latest`.
- **Not provided in this release**: Windows ARM64, macOS, MSI installers, Windows services, or commercially signed `.exe` binaries.
- **SmartScreen notice**: Because `recalldock.exe` is currently **unsigned**, Windows SmartScreen may show an "Unknown publisher" prompt on first launch. Always download from official GitHub Releases and verify the ZIP SHA256 against `checksums.txt` before running. Do not disable Windows Defender or SmartScreen globally.
- **PowerShell compatibility**: Tested for PowerShell 7 and written to support PowerShell 5.1 (`[Console]::OutputEncoding = UTF8NoBOM` enabled cleanly in the installer).

---

## Data Storage & Directory Layout

By default on native Windows, RecallDock keeps its binary, databases, and host configs cleanly under `%LOCALAPPDATA%\RecallDock`:

| Item | Default Native Windows Location | Notes |
|---|---|---|
| Installed binaries | `%LOCALAPPDATA%\RecallDock\bin\recalldock.exe` | User-level install; no admin required |
| Claude strict launcher | `%LOCALAPPDATA%\RecallDock\bin\recalldock-claude.cmd` | Created only if `claude-code` is selected |
| Long-term memory DB | `%LOCALAPPDATA%\RecallDock\recalldock.db` (`-wal`/`-shm`) | Override root via `RECALLDOCK_DATA_DIR` |
| Work ledger & catalog DB | `%LOCALAPPDATA%\RecallDock\operations.db` (`-wal`/`-shm`) | Independent from `recalldock.db` |
| Gateway registry | `%LOCALAPPDATA%\RecallDock\gateway.json` | Override via `RECALLDOCK_GATEWAY_REGISTRY` |
| SQLite online backups | `%LOCALAPPDATA%\RecallDock\backups\`, `operations-backups\` | Created via `VACUUM INTO` snapshots |
| Claude strict MCP config | `%LOCALAPPDATA%\RecallDock\claude-gateway.json` | Preserves other MCP entries |
| Codex MCP config | `%USERPROFILE%\.codex\config.toml` | Updates `[mcp_servers.recalldock_gateway]` only |

> **Do NOT share one SQLite file between native Windows and WSL2**:
> Native Windows (`%LOCALAPPDATA%\RecallDock`) and WSL2 (`~/.recalldock`) intentionally use separate default directories. Pointing both native Windows `recalldock.exe` and WSL2 Linux `recalldock` at the same SQLite file simultaneously risks WAL/locking corruption.

---

## Path Support & Restrictions

- **Supported**: Drive-letter paths (`C:\...`), spaces, Chinese / Unicode usernames and project folders, Windows backslashes (`\`), and case-insensitive NTFS lookups.
- **Rejected fail-closed**:
  - Relative paths where an absolute path is required (`RECALLDOCK_DATA_DIR`, project bindings, Gateway roots, Work roots)
  - UNC network paths (`\\server\share\...`)
  - Symlinks / NTFS reparse points / junctions in verified project bindings, backup directories, and candidate file evidence paths (`internal/store/candidate_file_open_windows.go` verifies `FILE_FLAG_OPEN_REPARSE_POINT`, volume serial number, `root.json`, and sub-directory `.git`/`.recalldock` boundaries; valid & invalid `checked`/`tiered` file-evidence unit tests pass on `windows-latest` CI, while Windows-specific concurrent directory-swap stress tests and live Windows interactive agent sessions remain unverified)
  - Classic `MAX_PATH` (260 chars) limits can apply in some OS contexts unless Windows Long Paths are enabled; Work roots are bounded to 1024 bytes.

---

## Guided Interactive Install (Recommended)

> **Release Status**: `0.3.24` is the upcoming initial public release (download links activate once `v0.3.24` is tagged and published on GitHub Releases).

1. Download `recalldock_0.3.24_windows_amd64.zip` and `checksums.txt` from [GitHub Releases](https://github.com/kings54188/recalldock/releases).
2. Verify SHA256 against `checksums.txt` in PowerShell:
   ```powershell
   $zip = "recalldock_0.3.24_windows_amd64.zip"
   $actual = (Get-FileHash .\$zip -Algorithm SHA256).Hash.ToLower()
   $expected = ((Select-String -Path .\checksums.txt -Pattern "\b$([regex]::Escape($zip))$").Line -split '\s+')[0].ToLower()
   if ($actual -eq $expected) { "OK: SHA256 matches ($actual)" } else { throw "SHA256 mismatch: $actual != $expected" }
   ```
3. Extract the ZIP to a local folder and run:
   ```powershell
   powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tools\install-windows.ps1 -Interactive
   ```
   What this does:
   - Auto-detects the extracted release folder and installed hosts (Codex / Claude Code / Antigravity CLI).
   - Displays all files to be created or modified, `plan_sha256`, and the automatic backup path under `%LOCALAPPDATA%\RecallDock\install-backups\`.
   - Applies only after you confirm `y` (`-Interactive` refuses non-interactive scripts).
   - Installs `%LOCALAPPDATA%\RecallDock\bin\recalldock.exe` (and optionally `recalldock-claude.cmd`).
   - Optionally add `%LOCALAPPDATA%\RecallDock\bin` to your **User** `PATH` so `recalldock` works in every terminal.

---

## Scripted Install (Preview JSON + Digest CAS Apply)

For unattended or audited installs:

```powershell
# 1. Preview changes (read-only) and copy the returned plan_sha256
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tools\install-windows.ps1 `
  -Hosts codex,claude-code

# 2. Apply using that plan_sha256 and a new non-existent backup folder
$PlanSha256 = "replace-with-plan-sha256"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tools\install-windows.ps1 `
  -Hosts codex,claude-code `
  -Apply -ExpectedSha256 $PlanSha256 `
  -BackupDir "$env:LOCALAPPDATA\RecallDock\install-backups\0.3.24"
```

---

## Windows Native Host Compatibility Status

| Host | Windows Native Config Path | Verified Scope |
|---|---|---|
| **Codex** | `%USERPROFILE%\.codex\config.toml` (`[mcp_servers.recalldock_gateway]`) | Config generation, TOML path escaping, and rollback verified; live interactive Windows Codex TUI session unverified. |
| **Claude Code** | `%LOCALAPPDATA%\RecallDock\claude-gateway.json` + `recalldock-claude.cmd` | Config generation, JSON escaping, and rollback verified; live interactive Windows Claude session unverified. |
| **Antigravity CLI (`agy`)** | Bundled `marketplace\plugins\antigravity` (`recalldock.exe` entrypoint) | Plugin manifest & MCP config verified; live interactive Windows AGY model session unverified. |
| **DSH** | — | Not verified / unsupported on native Windows in this release. |

---

## Upgrading, Protected Rollback & Uninstalling

1. Back up both SQLite databases before upgrading:
   ```powershell
   recalldock backup --json
   recalldock work backup
   ```
2. Extract the new release ZIP and re-run `.\tools\install-windows.ps1 -Interactive` (or scripted preview + `-Apply` with a new `-BackupDir`).
3. **Protected Rollback** (refuses to overwrite files modified after install; retains hardened security policies):
   ```powershell
   $BackupName = "replace-with-backup-dir-name"
   powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tools\install-windows.ps1 `
     -Rollback -BackupDir "$env:LOCALAPPDATA\RecallDock\install-backups\$BackupName"
   ```
4. **Uninstall (preserves user memory & Work databases)**:
   ```powershell
   $BackupName = "replace-with-backup-dir-name"
   powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tools\install-windows.ps1 `
     -Uninstall -BackupDir "$env:LOCALAPPDATA\RecallDock\install-backups\$BackupName"
   ```
   `recalldock.db` and `operations.db` in `%LOCALAPPDATA%\RecallDock` remain intact unless you delete that folder explicitly.
