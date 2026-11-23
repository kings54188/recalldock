# Privacy & Network Behaviour

RecallDock is a local-first tool. **This program does not transfer any information to other networked systems unless specifically requested by the user or the person installing or operating it.**

This document describes exactly what the program does and does not do over the network, so the statement above can be verified against the source.

---

## 1. Default State: No Outbound Connections

A default RecallDock installation performs **no automatic outbound network requests**. There is no telemetry, no analytics, no crash reporting, and no usage tracking.

All persistent data lives in local SQLite databases inside the data directory described in the README (`~/.recalldock` on Linux, `%LOCALAPPDATA%\RecallDock` on Windows).

### Component-by-component

| Component | Network behaviour | Default |
|---|---|---|
| Memory store (`recalldock.db`) | Local SQLite only; no network code path | Offline |
| Work/task store (`operations.db`) | Local SQLite only; no network code path | Offline |
| MCP Gateway (`recalldock gateway serve`) | **stdio only**. Speaks JSON-RPC over stdin/stdout with the host agent; it does not listen on a port and does not dial out | Offline |
| Backend MCP servers | The gateway starts backends that the operator registered in `gateway.json`. Those backends are third-party programs with their own network behaviour, which RecallDock neither controls nor mediates | Operator-defined |
| HTTP server (`recalldock serve`) | Binds loopback `127.0.0.1` only | Loopback |
| Operator UI (`recalldock ui`) | Binds loopback `127.0.0.1` only, with a one-time terminal token | Loopback |
| Candidate hook (`recalldock candidate-hook`) | Delivers to a **loopback endpoint only**; non-local endpoints are rejected by `validateLocalEndpoint` | Loopback |
| Update check | **Disabled by default.** The upstream release endpoint is intentionally empty for independent builds. A request is only made if an operator explicitly sets `RECALLDOCK_RELEASE_API_URL`; there is no fallback endpoint | Offline |
| Obsidian export | Writes Markdown files to a local vault path you supply | Offline |
| Cloud sync / cloud server | **Disabled in this build.** `recalldock cloud` exits with `cloud commands are disabled`; the legacy cloud source is retained but not reachable from the CLI | Disabled |

---

## 2. Connections That Only Happen Because You Asked

These paths exist but require explicit operator action. Each is off until you configure it.

- **Update check** — only when `RECALLDOCK_RELEASE_API_URL` is set. An `Authorization: Bearer` header is attached only if `RECALLDOCK_RELEASE_TOKEN` is present, and a custom endpoint never inherits unrelated account credentials.
- **Candidate hook delivery** — only when an endpoint is configured, and only to a loopback address.
- **Gateway backends** — only the backends you register and the tools you grant. Backend behaviour, including any network access it performs on its own, is outside RecallDock's boundary.
- **Cloud commands** — not reachable in this build.

---

## 3. What Is Never Collected

RecallDock does not collect, and has no facility to collect, any of the following:

- Your memory contents, prompts, sessions, or Work checkpoints
- File paths, project names, or repository identifiers
- Host configuration, credentials, or environment variables
- Machine identifiers, hardware fingerprints, or installation IDs
- Crash dumps, stack traces, or usage statistics

Nothing is uploaded anywhere as a side effect of normal operation.

---

## 4. Third-Party Components

RecallDock bundles the following, none of which perform network calls on their own behalf:

- **Go standard library and Go modules** listed in `go.mod` — used for local SQLite, HTTP-over-loopback, and CLI work. See [PROVENANCE.md](../PROVENANCE.md) for the dependency license breakdown.
- **`modernc.org/sqlite`** — pure-Go SQLite, no CGO, no network.
- **Bundled host plugin assets** (`marketplace/`) — static JSON and Markdown manifests describing how to launch RecallDock from Codex, Claude Code and Antigravity. They describe local program invocations and do not phone home.

Third-party MCP backends you choose to register are separate programs. Review their own privacy policies before granting them tools.

---

## 5. Verifying These Claims

Everything above is checkable in the public source:

```sh
# Confirm there is no upstream release endpoint compiled in
grep -n "githubLatestReleaseURL" internal/version/check.go

# Confirm the candidate hook rejects non-loopback endpoints
grep -rn "validateLocalEndpoint" internal/candidatehook/

# Confirm cloud commands are disabled in this build
recalldock cloud

# Confirm the gateway uses stdio, not a socket
recalldock gateway serve --help
```

---

## 6. Reporting a Concern

If you find a code path that contradicts this document, please report it privately via **GitHub Security Advisories** (`Security → Advisories → Report a vulnerability`) as described in [SECURITY.md](../SECURITY.md). Please do not open a public issue for a privacy or security defect.

---

## 7. Changes to This Policy

Material changes to network behaviour will be described in [CHANGELOG.md](../CHANGELOG.md) under the affected release. Because this is a local-first tool with no telemetry, no data-retention question arises: no server-side data exists to retain.
