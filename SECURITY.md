# Security Policy

## Supported Versions

Security fixes will apply to the **latest published public release** once released on GitHub Releases (pre-release / local dev snapshots and unreleased `0.3.24` preview builds are unsupported):

| Version | Status | Supported |
|---|---|---|
| `0.3.24` | Upcoming initial public release candidate (currently unpublished preview) | Upon official `v0.3.24` release |
| Older / local dev builds | Pre-public or development snapshots | No |

## Reporting a Vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Report vulnerabilities privately via **GitHub Security Advisories** in this repository (`Security -> Advisories -> Report a vulnerability`).

Please include:
- A clear description of the issue and affected component (CLI, store, gateway, installer, or host plugin)
- Steps to reproduce with synthetic data
- Potential security impact (e.g., cross-project access, path traversal, privilege bypass, stdio leakage)

## Security Model & Scope

RecallDock is a local-first tool writing to local SQLite databases (`recalldock.db` and `operations.db`):

- **In scope**:
  - Cross-project leakage across explicit bindings or worktree boundaries
  - Symlink / reparse-point / path-traversal bypasses in candidate file evidence, Obsidian export, backups, or installers
  - Unapproved gateway tool execution or schema-drift bypasses in `strict` mode
  - Bypassing candidate admission (`tiered-v4` / `manual` / `checked-v2`) via model-supplied fields
  - Unintended data corruption or silent overwrite during protected config rollback
- **Explicit current boundaries (out of scope / known limits)**:
  - RecallDock does **not** isolate arbitrary commands executed under the same OS user account (`UID`); a process running as the same OS user can access the same files or operator CLI unless restricted by the OS/sandbox.
  - Host lifecycle hooks receive JSON metadata from the host without cryptographic host-to-gateway handshake authentication (`host_session_id` provenance is observability, not tamper-proof authentication).
  - Model-supplied approval/confidence/author fields never grant permission, and source-evidence verification checks byte integrity—not semantic truth.
  - Windows `recalldock.exe` binaries in initial releases are unsigned; see [docs/WINDOWS.md](docs/WINDOWS.md) for SHA256 verification and SmartScreen notes.
