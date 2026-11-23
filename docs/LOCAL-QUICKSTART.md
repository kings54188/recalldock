# RecallDock CLI Quickstart

This guide shows daily operator workflows using the `recalldock` CLI (`~/.local/bin/recalldock` on Linux, `%LOCALAPPDATA%\RecallDock\bin\recalldock.exe` on Windows). For Windows paths and PowerShell details, see [WINDOWS.md](WINDOWS.md).

---

## 1. Verify Binary & Storage Paths

```sh
recalldock version
recalldock --help
```

Default storage locations:
- **Linux**: `~/.recalldock/` (`recalldock.db` for long-term memory, `operations.db` for Work & MCP catalog)
- **Windows**: `%LOCALAPPDATA%\RecallDock\`
- Override directory via `RECALLDOCK_DATA_DIR=/absolute/path` (must not share one SQLite file concurrently between Windows and WSL2).

---

## 2. Create & Bind a Project

RecallDock isolates memory by explicit project bindings (`prj-...`) tied to real directory identity rather than guessing from folder names:

### Linux (Bash)
```sh
PROJECT_DIR="$HOME/recalldock-demo"
mkdir -p "$PROJECT_DIR"
PROJECT_ID=$(recalldock projects create --name "my-app" --json | python3 -c "import json, sys; print(json.load(sys.stdin)['id'])")
recalldock projects bind "$PROJECT_ID" --directory "$PROJECT_DIR"
cd "$PROJECT_DIR"
recalldock projects current --json
```

### Windows (PowerShell)
```powershell
$ProjectDir = Join-Path $env:USERPROFILE "recalldock-demo"
New-Item -ItemType Directory -Force -Path $ProjectDir | Out-Null
$ProjectId = (recalldock projects create --name "my-app" --json | ConvertFrom-Json).id
recalldock projects bind $ProjectId --directory $ProjectDir
Set-Location $ProjectDir
recalldock projects current --json
```

Nested subdirectories inherit the bound root unless a deeper folder has its own binding. Linked git worktrees reuse an already-bound main checkout's project ID automatically while keeping their `Work` roots separate.

---

## 3. Save, Search & Revise Long-Term Memory

Inside a bound project directory:

```sh
# Save a verified decision or fix
recalldock save "Use WAL online snapshots" "Back up running SQLite files via recalldock backup --json." \
  --type decision --topic ops/backup

# Search & retrieve bounded context
recalldock search "WAL" --json
recalldock context --mode index
recalldock stats
```

Inspect or safely update an observation with snapshot CAS:

```sh
OBS_ID=1
recalldock memory show "$OBS_ID" --full
recalldock memory history "$OBS_ID" --limit 5
# Prepare /tmp/patch.json with {"title":"...","content":"..."} and set SNAPSHOT_SHA256 from `memory show`
SNAPSHOT_SHA256="replace-with-snapshot-sha256"
recalldock memory update "$OBS_ID" --expected "$SNAPSHOT_SHA256" --input /tmp/patch.json --reason "verified update"
```

---

## 4. Manage Work Continuity (`operations.db`)

Work checkpoints live separately from long-term knowledge, keyed by exact `--root` and `--lane` (Bash uses `"$PROJECT_DIR"`, PowerShell uses `$ProjectDir`):

```sh
recalldock work find --root "$PROJECT_DIR"
recalldock work status --root "$PROJECT_DIR" --lane release-prep
recalldock work read --root "$PROJECT_DIR" --lane release-prep --mode l2 --generation 1
recalldock work doctor
```

---

## 5. Backups, Diagnostics & Obsidian Export

```sh
# Online WAL-safe SQLite backups (both independent databases)
recalldock backup --json
recalldock work backup

# Read-only health checks
recalldock doctor --json

# Protected one-way Markdown projection into an Obsidian vault
VAULT_DIR="$HOME/obsidian-vault"
mkdir -p "$VAULT_DIR"
recalldock obsidian-export --vault "$VAULT_DIR" --project "$PROJECT_ID"
```

---

## 6. Managing Gateway MCP Backends

See [GATEWAY-MCP.md](GATEWAY-MCP.md) for adding, removing, scanning, discovering, and configuring permissions (`agent` vs `strict`) across up to 200 MCP backends behind the 7 fixed Gateway tools.
