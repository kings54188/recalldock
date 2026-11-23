# Managing MCP Servers with RecallDock Gateway

When using AI coding assistants with multiple MCP servers, loading every backend server's full tool list at startup bloats prompts and mixes execution permissions. RecallDock solves this with an **on-demand MCP Gateway**.

---

## 1. Three Concepts You Must Keep Separate

1. **Gateway Interface Tools (Fixed at 7 Tools)**
   Your AI agent only sees seven built-in tools from RecallDock Gateway:
   `tools_find`, `tools_describe`, `tools_call`, `tools_release`, `work_find`, `work_read`, `work_checkpoint`.

2. **Gateway-Registered MCP Backends (`gateway.json`)**
   Your personal registry of backend MCP servers (default location: `~/.recalldock/gateway.json` on Linux, `%LOCALAPPDATA%\RecallDock\gateway.json` on Windows, or override via `RECALLDOCK_GATEWAY_REGISTRY`).
   - **Maximum capacity**: Up to **200 registered MCP servers**, up to **512 explicit tool grants per server**, and up to **1,000 cached tools per server**.
   - Backends are **never started automatically at session launch**; they are inspected only when you run `gateway discover` (or `tools_describe refresh=true`) and launched on demand when `tools_call` executes.
   - `gateway.json` stores only **references** (`path`, `format`, `keys`) pointing to the original MCP config file, not plaintext API keys or tokens.

3. **Host-Direct MCP Servers**
   MCP servers configured directly inside Codex (`~/.codex/config.toml`), Claude Code (`~/.claude.json`), or Antigravity CLI (`~/.gemini/antigravity-cli/mcp_config.json`, plugin `mcp_config.json`).
   - Adding an MCP server directly inside a host **does not automatically register it into RecallDock Gateway**, and RecallDock **never deletes or disables your host-direct MCP servers behind your back**.

---

## 2. Quick Command Reference

```sh
# 1. Read-only scan: see host-direct MCPs vs Gateway-managed backends (never leaks secrets)
recalldock gateway scan

# 2. List currently registered Gateway backends and grant counts
recalldock gateway list

# 3. Verify all registered backend source files still parse cleanly
recalldock gateway check

# 4. Check for stale strict-mode tool grants after a backend changed schema
recalldock gateway doctor
```

---

## 3. What Happens When You Add an MCP Server?

### Path A: You add an MCP server directly in your Host config (e.g. `~/.codex/config.toml` or `~/.claude.json`)
- **Host behavior**: Your host sees and calls that MCP server directly on its own next session reload.
- **Gateway behavior**: Gateway does **not** automatically register or proxy it, and RecallDock still exposes only the fixed 7 Gateway tools (`tools_find` will not list it yet).
- **Duplicate tools**: None—unless you later register the same backend in Gateway while keeping it enabled directly in the host too (which `recalldock gateway scan` clearly reports as `"both"`).

### Path B: You register an MCP server into RecallDock Gateway

You can register any MCP definition from a JSON/TOML/YAML config file either with CLI flags or an `--input` definition JSON:

```sh
# Option 1: Register an entry already in ~/.codex/config.toml or ~/.claude.json
recalldock gateway add \
  --server my-lsp \
  --source-file "$HOME/.codex/config.toml" \
  --keys mcp_servers,dsh-lsp \
  --host codex \
  --description "Local language server" \
  --enable \
  --stateful

# Option 2: Or pass a standalone server definition JSON via --input
recalldock gateway add --server my-lsp --input /abs/path/my-lsp-def.json

# Connect once to cache bounded tool metadata, then close cleanly
recalldock gateway discover --server my-lsp
```

- **`tools_find` & `tools_call` pick it up live**: You do **not** need to restart an already-running `recalldock gateway serve` process for `tools_find`, `tools_describe`, or `tools_call` to see newly registered & discovered servers (`Pool` reads `gateway.json` live).
- **No duplicate tools on the agent**: The agent's tool list stays fixed at 7 tools.

---

## 4. What Happens When You Remove an MCP Server?

- **Remove from Gateway registry**:
  ```sh
  recalldock gateway remove --server my-lsp
  ```
  1. Removes `my-lsp` from `gateway.json` atomically.
  2. Clears `my-lsp`'s cached tool definitions from `operations.db` (`RemoveServerCatalog`).
  3. Any active or already-running `gateway serve` process immediately excludes `my-lsp` from `tools_find`/`tools_describe`, rejects new `tools_call` requests (`approval_required`), and closes idle connection leases while allowing any currently in-flight call to finish without crashing.
  4. Your original host config file (`.codex/config.toml`, `.claude.json`, etc.) is **untouched**.
- **Disable temporarily instead of deleting**:
  ```sh
  recalldock gateway disable --server my-lsp
  recalldock gateway enable --server my-lsp
  ```
- **Uninstall the underlying MCP binary while leaving it registered**:
  `tools_call` / `gateway discover` returns `backend_unavailable` cleanly without crashing or falling back to another server.
- **Re-install or re-add the same server name later**:
  Old cached tool catalogs and grants are gone; run `recalldock gateway add` + `gateway discover` cleanly.

---

## 5. What Happens When Command, Args, Env, or Tool Schema Changes?

Because `gateway.json` references your source configuration rather than duplicating secrets, editing the underlying file changes its **Execution Config Hash (v2)**:

1. **Command / args / env value / transport changed**:
   - The running Gateway computes the live config hash on `tools_describe` / `tools_call` and immediately closes old leases and rejects execution with `configuration_or_schema_changed` (`ErrChanged`).
   - Fix: Run `recalldock gateway discover --server my-lsp`.
2. **Backend MCP updated and changed a tool's parameter schema (`SchemaHash` mismatch)**:
   - Even if `Command` and `Args` stayed identical, `tools_call` inspects the live `tools/list` response before executing the call and fails closed if the schema changed!
   - Fix: Run `recalldock gateway discover --server my-lsp` (and in `--trust strict` mode, inspect and re-run `gateway grant`).
3. **Paths with spaces or Chinese characters & missing env vars**:
   - Supported cleanly on both Linux and Windows when configured as absolute paths. Missing declared environment variables fail closed (`required_configuration_missing`) without falling back.

---

## 6. Trust Modes: `agent` (Default) vs. `strict`

- **`--trust agent` (default)**: Recommended for single-user local workstations. Once you register, enable, and `discover` a backend, your trusted local agent can invoke discovered tools for absolute workspace roots without per-tool grant prompts—while live config/schema hashes and lease isolation remain enforced.
- **`--trust strict`**: Intended for minimal privilege or shared setups. Every backend tool requires an explicit `recalldock gateway grant` binding tool name, exact schema hash, allowed `--root` directories (plus exact `--arguments`, `--session`, `--expires` for write/side-effecting tools). Run `recalldock gateway doctor` to find stale grants.
