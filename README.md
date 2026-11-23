# RecallDock

**RecallDock — Local memory and work continuity for AI coding agents.**

[English](README.md) | [简体中文](README.zh-CN.md) | [Installation](docs/INSTALLATION.md) | [Windows Guide](docs/WINDOWS.md) | [MCP Gateway Guide](docs/GATEWAY-MCP.md) | [Provenance](PROVENANCE.md)

RecallDock helps AI coding assistants remember verified project knowledge across sessions and resume active multi-step tasks when you start a new chat or switch between agents—all stored locally in SQLite without cloud accounts, raw transcript capture, or modifying your agent versions.

---

## Why Use RecallDock?

AI coding agents usually start every session with a blank slate:

1. They forget past architecture decisions, project conventions, and fixed bugs—so you repeat the same explanations.
2. They mix rough work-in-progress notes with durable facts, polluting future searches.
3. They dump every configured MCP tool schema into context at startup, consuming tokens before real work starts.
4. Switching from one agent (such as Codex) to another (such as Claude Code or Antigravity CLI) leaves your in-progress task behind.

RecallDock solves this locally in one Go binary backed by two independent SQLite databases.

---

## Core Capabilities

- **Long-Term Memory (`recalldock.db`)**: Stores filtered project decisions, conventions, and verified bugfixes so agents retrieve relevant facts on demand instead of guessing from raw chat history.
- **Work Continuity (`operations.db`)**: Tracks your active goal, verified summary, next steps, and blockers per project directory and task lane, letting a later session or another host resume from the exact checkpoint.
- **Multi-Agent Support**: Works with **Codex**, **Claude Code**, **Antigravity CLI (`agy`)**, and **DSH (DeepSeek Harness)** through each tool's native MCP and plugin/hook configuration, without locking you into a specific LLM provider or altering official agent updates.
- **On-Demand MCP Gateway (`recalldock gateway serve`)**:
  - Exposes a **fixed set of 7 Gateway tools** (`tools_find`, `tools_describe`, `tools_call`, `tools_release`, `work_find`, `work_read`, `work_checkpoint`) so backend MCP tool definitions are loaded only when needed.
  - **7 is the Gateway interface tool count, NOT a limit on your MCP servers.** Up to **200 MCP backends** can be registered in the Gateway, alongside any host-direct MCP servers you keep enabled.
  - **Explicit control, no silent takeover**: Host-configured MCP servers remain separate from Gateway-registered backends until you inspect them (`recalldock gateway scan`) and register what you want (`recalldock gateway add`). See [docs/GATEWAY-MCP.md](docs/GATEWAY-MCP.md) for adding, removing, updating, and permissions.
- **Local & Safe**: Everything lives on your machine (`~/.recalldock` on Linux, `%LOCALAPPDATA%\RecallDock` on Windows). Projects require explicit directory binding, models cannot silently overwrite approved long-term knowledge, and installs always preview changes and save backups first.

---

## Supported Platforms & Agents

Release packages are built for **Linux (`x86_64` / `amd64`)** and **Windows (`x64` / `amd64` native — no WSL2 or admin rights required)**. macOS and ARM64 packages are not yet provided or verified.

| Agent Host | Linux (`amd64`) | Windows x64 (`amd64`) | Notes & Current Boundaries |
|---|---|---|---|
| **Codex** | **Verified** (core, MCP, config, real native sessions & subagent hooks) | **Config & Protocol Verified** (live interactive Windows session not yet run) | Updates `[mcp_servers.recalldock_gateway]` in `.codex/config.toml`; keeps existing MCP servers and comments. |
| **Antigravity CLI (`agy`)** | **Verified** (core, plugin manifest, hooks, Gateway 7 tools, Work continuity) | **Plugin & Config Verified** (live interactive Windows session not yet run) | Uses `agy plugin install` against the bundled plugin directory; local subagent handoffs unverified. |
| **Claude Code** | **Partially Verified** (strict MCP launcher, recall, Work read/write; some native auto-promotion & subagent handoffs incomplete) | **Config & Protocol Verified** (live interactive Windows session not yet run) | Writes `claude-gateway.json` + `recalldock-claude` wrapper; original `claude` and `~/.claude.json` stay untouched. |
| **DSH** | **Verified (Core & Workflows, Host Session Limits)** (`0.2.2-rc.5` real model: 7-tool MCP Gateway, memory recall & readback, Work read/checkpoint/CAS/readback, evidence-backed candidate commit & `tiered-v4` auto-promotion + readback, native `spawn` subagent execution & parent verification, cross-agent Codex review workflow) | **Unsupported / Unverified on Windows** | Managed block in `~/.dsh/cordis.patch.yml` (`--host dsh`). Host limits: no trusted native `SessionStart` channel (`host_session_id` remains unknown), parent/subagent Work writers cannot be credibly separated, and hallucination/error detection is unassessed (no confirmed error sample). |

See [docs/HOST-ADAPTERS.md](docs/HOST-ADAPTERS.md) for details on what each host reads and preserves.

---

## Download & Quick Install

> **Release Status**: `0.3.24` is the current public release. Prebuilt Linux and Windows packages are available from [GitHub Releases](https://github.com/kings54188/recalldock/releases/tag/v0.3.24); you can also build directly from source below.

Download the release archive for your OS and `checksums.txt` from [GitHub Releases](https://github.com/kings54188/recalldock/releases).

### Linux (`x86_64` / `amd64`)

1. Download `recalldock_0.3.24_linux_amd64.tar.gz` and `checksums.txt`, then verify:
   ```sh
   sha256sum -c checksums.txt --ignore-missing
   ```
2. Extract to a permanent versioned directory (do not delete this directory later):
   ```sh
   mkdir -p ~/.local/share/recalldock/releases
   tar -xzf recalldock_0.3.24_linux_amd64.tar.gz -C ~/.local/share/recalldock/releases
   mv ~/.local/share/recalldock/releases/recalldock_0.3.24_linux_amd64 ~/.local/share/recalldock/releases/0.3.24
   ```
3. Run the guided installer (auto-detects your release path, home directory, and installed hosts, shows every change and automatic backup path, and asks `[y/N]` before modifying anything):
   ```sh
   python3 ~/.local/share/recalldock/releases/0.3.24/tools/install-local.py --interactive
   ```
   Verify with `recalldock version`. For scripted non-interactive install, custom `--host` flags, upgrade, and rollback options, see [docs/INSTALLATION.md](docs/INSTALLATION.md).

### Windows (`x64` / `amd64` Native — No WSL2 or Admin Required)

1. Download `recalldock_0.3.24_windows_amd64.zip` and `checksums.txt`, then verify the SHA256 against `checksums.txt` in PowerShell:
   ```powershell
   $zip = "recalldock_0.3.24_windows_amd64.zip"
   $actual = (Get-FileHash .\$zip -Algorithm SHA256).Hash.ToLower()
   $expected = ((Select-String -Path .\checksums.txt -Pattern "\b$([regex]::Escape($zip))$").Line -split '\s+')[0].ToLower()
   if ($actual -eq $expected) { "OK: SHA256 matches ($actual)" } else { throw "SHA256 mismatch: $actual != $expected" }
   ```
2. Extract the ZIP to a local folder, then run the guided user-level installer:
   ```powershell
   powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tools\install-windows.ps1 -Interactive
   ```
   It previews changes, saves a private backup under `%LOCALAPPDATA%\RecallDock\install-backups\`, installs `%LOCALAPPDATA%\RecallDock\bin\recalldock.exe`, and updates only RecallDock entries after your confirmation.

> **Unsigned Binary Notice**: `recalldock.exe` is currently unsigned, so Windows SmartScreen may show a warning on first launch. Verify the SHA256 hash against official `checksums.txt` rather than disabling Windows security protections. See [docs/WINDOWS.md](docs/WINDOWS.md) for PowerShell 5.1/7 notes and PATH setup.

### Build from Source (Go 1.25.10+)

```sh
git clone https://github.com/kings54188/recalldock.git
cd recalldock
go test -count=1 ./...
go build -ldflags='-X main.version=dev' -o /tmp/recalldock ./cmd/recalldock
```

---

## Simplest Quickstart

### Linux (Bash)

```sh
# 1. Create a folder, register a project, capture its real ID, and bind the folder
PROJECT_DIR="$HOME/recalldock-demo"
mkdir -p "$PROJECT_DIR"
PROJECT_ID=$(recalldock projects create --name "my-app" --json | python3 -c "import json, sys; print(json.load(sys.stdin)['id'])")
recalldock projects bind "$PROJECT_ID" --directory "$PROJECT_DIR"

# 2. Save and search project knowledge from your terminal
cd "$PROJECT_DIR"
recalldock save "Backup before schema changes" "Always run recalldock backup --json before upgrading." \
  --type decision --topic ops/backup
recalldock search "schema changes"

# 3. Inspect active Work tasks for this directory (stored separately in operations.db)
recalldock work find --root "$PROJECT_DIR"

# 4. Inspect host MCP configs vs Gateway-registered MCPs (read-only, never prints secrets)
recalldock gateway scan
recalldock gateway list
```

### Windows (PowerShell)

```powershell
# 1. Create a folder, register a project, capture its real ID, and bind the folder
$ProjectDir = Join-Path $env:USERPROFILE "recalldock-demo"
New-Item -ItemType Directory -Force -Path $ProjectDir | Out-Null
$ProjectId = (recalldock projects create --name "my-app" --json | ConvertFrom-Json).id
recalldock projects bind $ProjectId --directory $ProjectDir

# 2. Save and search project knowledge from PowerShell
Set-Location $ProjectDir
recalldock save "Backup before schema changes" "Always run recalldock backup --json before upgrading." `
  --type decision --topic ops/backup
recalldock search "schema changes"

# 3. Inspect active Work tasks for this directory (stored separately in operations.db)
recalldock work find --root $ProjectDir

# 4. Inspect host MCP configs vs Gateway-registered MCPs (read-only, never prints secrets)
recalldock gateway scan
recalldock gateway list
```

---

## Managing MCP Servers (Gateway vs. Host Direct)

RecallDock distinguishes three layers cleanly:
1. **The 7 Gateway Tools**: fixed tool interface exposed to your agent (`tools_find`, `tools_describe`, `tools_call`, `tools_release`, `work_find`, `work_read`, `work_checkpoint`).
2. **Gateway-Registered MCP Backends** (`gateway.json`): up to **200** external MCP servers you explicitly register with `recalldock gateway add`, inspect with `recalldock gateway list`, discover with `recalldock gateway discover`, or remove with `recalldock gateway remove`.
3. **Host-Direct MCP Servers**: MCP entries configured directly in Codex, Claude Code, or Antigravity.

Adding or removing a server in `gateway.json` takes effect live on `tools_find` / `tools_call` without restarting an active Gateway process, while changes to host-direct MCP configs still follow each host's normal session reload rules. For full add/remove/modify instructions and permission modes (`agent` vs `strict`), read [docs/GATEWAY-MCP.md](docs/GATEWAY-MCP.md).

---

## Local Data, Backups, Upgrade & Uninstall

| Platform | Default Data Directory | Main Files | Override Env Vars |
|---|---|---|---|
| **Linux** | `~/.recalldock` | `recalldock.db`, `operations.db`, `gateway.json`, `backups/`, `operations-backups/` | `RECALLDOCK_DATA_DIR`, `RECALLDOCK_GATEWAY_REGISTRY` |
| **Windows** | `%LOCALAPPDATA%\RecallDock` | `recalldock.db`, `operations.db`, `gateway.json`, `backups/`, `operations-backups/`, `bin\recalldock.exe` | `RECALLDOCK_DATA_DIR`, `RECALLDOCK_GATEWAY_REGISTRY` |

> **WSL2 Warning**: Do not point native Windows `recalldock.exe` and WSL2 Linux `recalldock` at the same SQLite database file concurrently. Keep `%LOCALAPPDATA%\RecallDock` and `~/.recalldock` separate.

- **Back up both databases before upgrading**: `recalldock backup --json` and `recalldock work backup`.
- **Protected rollback & safe uninstall**: Rolling back or uninstalling only reverts RecallDock binaries/managed config entries and **always preserves** your `recalldock.db` and `operations.db` databases unless you delete them manually. Rollback refuses to overwrite files you edited after installation. See [docs/INSTALLATION.md](docs/INSTALLATION.md) and [docs/WINDOWS.md](docs/WINDOWS.md).

---

## Code Signing Policy

RecallDock intends to submit its Windows release builds for free code signing once the project meets the programme's reputation criteria. **As of `0.3.24`, Windows builds are not yet signed** — see the unsigned-binary notice above.

### Signing programme

RecallDock has applied-toward / intends to use free code signing for its Windows release builds. Once an application is approved and the first signed release ships, the arrangement will be:

> Free code signing provided by [SignPath.io](https://about.signpath.io), certificate by [SignPath Foundation](https://signpath.org).

**That arrangement is not active yet.** No RecallDock binary has been signed to date, and this notice will be replaced with an unconditional statement only after a genuinely signed release is published. Until then, verify downloads against `checksums.txt` as described above.

### Roles

| Role | Responsibility | Member |
|---|---|---|
| Author | Trusted to modify source without additional review | [@kings54188](https://github.com/kings54188) |
| Reviewer | Reviews every change proposed by non-committers | [@kings54188](https://github.com/kings54188) |
| Approver | Approves each individual signing request | [@kings54188](https://github.com/kings54188) |

RecallDock is currently maintained by a single person, so all three roles are held by the same member. This is disclosed rather than presented as a separation of duties that does not exist.

### Signed artefacts

If signing is approved, only the Windows release packages (`recalldock_<version>_windows_amd64.zip`) produced by the GitHub Actions release workflow from a tagged commit will be submitted. Binaries built locally or by third parties are never signed.

### Privacy

See [Privacy & Network Behaviour](docs/PRIVACY.md). In short: this program does not transfer any information to other networked systems unless specifically requested by the user or the person installing or operating it.

---

## Documentation

- [Installation, Upgrade & Rollback (Linux)](docs/INSTALLATION.md)
- [Windows Native Guide](docs/WINDOWS.md)
- [Managing MCP Servers with the Gateway](docs/GATEWAY-MCP.md)
- [CLI Quickstart & Daily Usage](docs/LOCAL-QUICKSTART.md)
- [Host Adapters & Compatibility Boundaries](docs/HOST-ADAPTERS.md)
- [Security Boundaries](SECURITY.md) | [Gateway Security Model](docs/GATEWAY-AUTH.md)
- [Privacy & Network Behaviour](docs/PRIVACY.md)
- [Contributing](CONTRIBUTING.md)

---

## License

RecallDock is open-source software licensed under the MIT License. See [LICENSE](LICENSE).
