# Installing, Upgrading & Rolling Back RecallDock (Linux)

This guide covers standard Linux (`x86_64` / `amd64`) user-level installation without `root` permissions. For native Windows x64, see [WINDOWS.md](WINDOWS.md).

---

## 1. Verify & Unpack the Release

> **Release Status**: `0.3.24` is the current public release; download the packages below from GitHub Releases.

1. Download `recalldock_0.3.24_linux_amd64.tar.gz` and `checksums.txt` from [GitHub Releases](https://github.com/kings54188/recalldock/releases).
2. Verify the archive hash:
   ```sh
   sha256sum -c checksums.txt --ignore-missing
   ```
3. Unpack to a **permanent** directory (for example `~/.local/share/recalldock/releases/0.3.24`):
   ```sh
   mkdir -p ~/.local/share/recalldock/releases
   tar -xzf recalldock_0.3.24_linux_amd64.tar.gz -C ~/.local/share/recalldock/releases
   mv ~/.local/share/recalldock/releases/recalldock_0.3.24_linux_amd64 ~/.local/share/recalldock/releases/0.3.24
   ```
   > **Important**: `install-local.py` configures `~/.local/bin/recalldock` and host MCP entries to point to `runtime/recalldock` inside this extracted release directory. Keep the release folder in place until you upgrade or uninstall.

---

## 2. Guided Interactive Install (Recommended)

Run the included installer interactively (requires Python 3.11+):

```sh
python3 ~/.local/share/recalldock/releases/0.3.24/tools/install-local.py --interactive
```

What this does safely under the hood:
- Auto-detects the extracted release directory, your `$HOME`, and installed AI agent directories (`.codex`, `.claude`/`.claude.json`, `.gemini`/Antigravity, `.dsh`).
- You can still limit hosts explicitly with `--host codex --host claude-code`.
- Displays every planned file change, `plan_sha256`, and the automatic private backup directory (`~/.local/state/recalldock/backups/VERSION-TIMESTAMP`).
- Stops cleanly if you answer anything other than `y` / `yes` (or if invoked in a non-interactive script).
- Applies changes using digest CAS (Compare-And-Swap) so concurrent edits to your host configs are never silently overwritten.
- Prints the exact `--rollback` command when finished.

Verify installation:
```sh
recalldock version
```
*(Ensure `~/.local/bin` is on your `PATH`.)*

---

## 3. Non-Interactive / Scripted Install (Preview + Digest CAS Apply)

For reproducible automation or reviewing exact JSON diffs first:

```sh
RELEASE="$HOME/.local/share/recalldock/releases/0.3.24"

# 1. Preview changes (read-only) and copy the returned plan_sha256
"$RELEASE/runtime/recalldock" setup \
  --release "$RELEASE" \
  --home "$HOME" \
  --host codex --host claude-code --host antigravity

# 2. Apply using that plan_sha256 and a new non-existent backup folder
PLAN_SHA256="replace-with-plan-sha256"
"$RELEASE/runtime/recalldock" setup \
  --release "$RELEASE" \
  --home "$HOME" \
  --host codex --host claude-code --host antigravity \
  --apply --expected-sha256 "$PLAN_SHA256" \
  --backup "$HOME/.local/state/recalldock/backups/0.3.24"
```

---

## 4. What Changes for Each Host

- **`codex`**: Updates `[mcp_servers.recalldock_gateway]` inside `~/.codex/config.toml`. Preserves your comments, unrelated MCP servers, and custom `[mcp_servers.recalldock_gateway]` approval policy if already configured.
- **`claude-code`**: Writes `~/.recalldock/claude-gateway.json` and installs a wrapper `~/.local/bin/recalldock-claude` (`claude --mcp-config ~/.recalldock/claude-gateway.json --strict-mcp-config "$@"`) while leaving your native `claude` binary and `~/.claude.json` untouched.
- **`antigravity`**: Installs the shared CLI and prints the exact native command to register the bundled plugin:
  ```sh
  agy plugin validate "$HOME/.local/share/recalldock/releases/0.3.24/marketplace/plugins/antigravity" --strict --json
  agy plugin install "$HOME/.local/share/recalldock/releases/0.3.24/marketplace/plugins/antigravity"
  ```
- Existing databases (`recalldock.db`, `operations.db`), `gateway.json` backend grants, and official host versions are never overwritten or pinned.

---

## 5. Upgrading

1. Back up both SQLite databases first:
   ```sh
   recalldock backup --json
   recalldock work backup
   ```
2. Set `NEW_VERSION="0.3.25"` (or target release tag) and extract the new release to `"$HOME/.local/share/recalldock/releases/$NEW_VERSION"`.
3. Run `install-local.py --interactive` from the new release directory (or `--apply` with a fresh `--backup` directory).
4. Start a fresh session in your AI agent host so it reloads the updated runtime binary.

---

## 6. Protected Rollback & Uninstalling

### Protected Rollback
```sh
BACKUP_NAME="replace-with-backup-dir-name"
recalldock setup --rollback --backup "$HOME/.local/state/recalldock/backups/$BACKUP_NAME"
```
- **Manual-edit protection**: If you modified a managed configuration file after installation, rollback **refuses to overwrite** your edits.
- **Security policy retention**: If an older `candidate-admission.json` policy was hardened to `checked-v2` / `duplicates-v2` / `tiered-v4`, rollback retains the hardened security policy while restoring configs and launchers.

### Uninstalling (Preserves Your Memory & Work Databases)
1. Run rollback for the backup created when you installed (or remove `~/.local/bin/recalldock`, `~/.local/bin/recalldock-claude`, and the `recalldock_gateway` block manually if you installed custom edits afterwards).
2. Remove `~/.local/share/recalldock/releases/0.3.24`.
3. Your SQLite databases in `~/.recalldock` (`recalldock.db`, `operations.db`, `gateway.json`, `backups/`) remain intact unless you delete `~/.recalldock` explicitly.
