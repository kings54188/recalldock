# RecallDock Host Adapters & Compatibility Boundaries

RecallDock integrates with AI coding agents using each host's native MCP, plugin, and lifecycle Hook interfaces (`integrations/host-plugins/`). It never replaces your official host binaries, never captures full chat transcripts or prompts, and never bypasses native host security checks.

---

## Core Lifecycle & Isolation Rules

1. **Minimal SessionStart Metadata**: When a host triggers `SessionStart`, RecallDock verifies existing project bindings (`root.json` + directory identity) and outputs bounded scope metadata (`project`, `Work root`) without dumping unrelated memories into context. Unbound folders stay quietly unbound.
2. **Linked Worktree Inheritance**: A git linked worktree can reuse the already-bound main checkout's knowledge project ID (`Source=worktree_main`) while keeping `Work` progress strictly scoped per worktree directory.
3. **Host Trust Stays With the Host**: Writing MCP entries or plugin files never forces a host to silently approve hooks or commands. Hooks remain subject to each host's normal trust/approval workflow, and already-running host sessions require a restart/new session to pick up new binary paths.
4. **Protocol Checks != Live Model Validation**: Synthetic MCP/hook tests verify framing, isolation, and config escaping; they do not by themselves claim live interactive model UX across every OS.

---

## Compatibility Summary (`0.3.24`)

| Host | Linux (`amd64`) | Windows x64 (`amd64`) | Configuration Scope & Behavior |
|---|---|---|---|
| **Codex** | **Verified** (core, MCP, config, native sessions, `SubagentStart`) | **Config & Protocol Verified** (live Windows Codex TUI unverified) | Updates `[mcp_servers.recalldock_gateway]` inside `~/.codex/config.toml` (`%USERPROFILE%\.codex\config.toml` on Windows) while preserving other MCP servers and comments. Updated plugin hooks may be marked `modified` by Codex until approved in the TUI. |
| **Antigravity CLI (`agy`)** | **Verified** (plugin manifest, hooks, Gateway 7 tools, Work continuity) | **Plugin & Config Verified** (live Windows AGY session unverified) | Installed via native `agy plugin install <release>/marketplace/plugins/antigravity` after validating the manifest (`agy plugin validate --strict --json`). Uses `conversationId` + single exact `workspacePaths`; local subagent handoffs remain unverified. |
| **Claude Code** | **Partially Verified** (strict MCP config, recall, Work read/write; some native auto-promotion & subagent handoffs incomplete) | **Config & Protocol Verified** (live Windows Claude CLI unverified) | Generates isolated `claude-gateway.json` and wrapper `recalldock-claude` (`recalldock-claude.cmd` on Windows) using `--mcp-config ... --strict-mcp-config`, leaving native `claude` and `~/.claude.json` untouched. |
| **DSH** | **Verified (Core & Workflows, Host Session Limits)** (`0.2.2-rc.5` real model: 7-tool Gateway, memory recall/readback, Work read/checkpoint/CAS, `tiered-v4` candidate auto-promotion, native `spawn` subagent & parent verification, Codex review workflow) | **Config, Protocol & Install Verified** (third-party Windows 11 host load + MCP handshake; live Windows DSH Workflows unverified) | Managed block inside `~/.dsh/cordis.patch.yml` (`--host dsh`). No trusted native `SessionStart` channel (`host_session_id` unknown); parent/subagent Work writers inseparable; error-detection rate unassessed. |

---

## DeepSeek Harness (DSH) Verified Scope & Limits

- **Verified Linux Baseline**: DSH `0.2.2-rc.5` running real model workflows on Linux (`stdio` via `dsh-mcp-client` -> `recalldock gateway serve --host dsh`).
- **PASS / Verified Capabilities**:
  1. Fixed 7-tool MCP Gateway integration (`tools_find`, `tools_describe`, `tools_call`, `tools_release`, `work_find`, `work_read`, `work_checkpoint`).
  2. Long-term memory retrieval (`mem_search`, `mem_context`) and formal observation readback (`mem_get_observation`).
  3. Autonomous Work continuation: `work_find`, `work_read`, `work_checkpoint`, strict `expected` generation CAS enforcement, and post-write readback.
  4. Autonomous long-term memory candidate submission (`mem_session_start` + `mem_commit_session`) with real bound source-file evidence.
  5. Automatic admission under existing `tiered-v4` rules (without loosening policy) and subsequent retrieval/readback of the promoted observation.
  6. Native DSH `subagent` (`spawn`) execution with independent subagent session logs and real code/file execution.
  7. Parent agent independent verification of subagent outputs instead of blindly trusting subagent claims.
  8. Cross-agent independent review workflow inspecting Codex main-agent and native-subagent artifacts/evidence.
- **Explicit Host Limitations & Unverified Scope**:
  - **No trusted native `SessionStart` channel**: DSH has internal events, but exposes no unforgeable inter-process lifecycle channel that RecallDock can safely consume (`DSH_SESSION_ID` is a shell child-process env var, not a tamper-proof MCP attestation). Therefore `host_session_id` remains unknown (`writer` records only `host: "dsh"` and server-generated `gateway_session_id`).
  - **Parent/subagent Work identity not credibly separable**: While DSH parent and spawned subagents have separate DSH host sessions, RecallDock's Work writer layer shares the gateway session identity and cannot credibly distinguish parent vs. child writes; native subagent execution PASS does not imply parent/child Work writer handoff is verified.
  - **Hallucination / error detection not assessed**: No independent confirmed error sample was present in the cross-agent review set, so reliable hallucination detection is unassessed (`NOT ASSESSED`).
  - **Windows native execution**: Split verdict, do not read it as full host acceptance.
    - **Verified by a third party on Windows 11 x64 with DSH `0.2.0-rc.2`**: the `dsh-mcp-client` configuration schema, actual DSH host loading of the managed block, an MCP handshake (`protocolVersion: 2025-06-18`), exactly seven tools, and memory-backend discovery through the gateway.
    - **Verified on the `windows-2025` CI runner (isolated `USERPROFILE`, no DSH desktop host)**: installer fresh install/upgrade/idempotency/rollback, unmanaged-configuration conflict refusal, Chinese and space-containing paths, the MCP handshake against the exact command the managed block declares, the seven-tool surface, Work checkpoint CAS, and memory-backend discovery.
    - **NOT ASSESSED**: long-term memory auto-promotion in a live Windows DSH session, parent/subagent Work writer identity separation, and any DSH desktop GUI workflow. CI exercises the MCP protocol, not a running DSH application, and must not be reported as an end-to-end host acceptance.

---

## Host-Session Provenance (`operations.db`)

- Normal `recalldock gateway serve --host <HOST>` knows its gateway instance ID and selected host filter, without claiming an unauthenticated host chat ID as proof.
- Operators with a verified active host session registered via lifecycle hooks can bind an explicit gateway instance:
  ```sh
  recalldock gateway serve --host claude-code --host-session REAL_REGISTERED_ID
  ```
- Work checkpoint/event writes require a matching active root & directory identity; unknown writers remain `null` rather than being guessed. See [GATEWAY-AUTH.md](GATEWAY-AUTH.md) and [GATEWAY-MCP.md](GATEWAY-MCP.md) for Gateway details.
