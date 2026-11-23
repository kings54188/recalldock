# Contributing to RecallDock

Thank you for your interest in contributing to RecallDock!

## Development Prerequisites

- Go **1.25.10** or compatible toolchain per [go.mod](go.mod)
- Python **3.11+** (for release packaging and installer test suites; the RecallDock runtime binary itself has zero Python dependency)
- Git

## Building & Testing Locally

Run commands from the repository root containing `go.mod`:

```sh
# Full unit tests + coverage
go test -count=1 -cover ./...

# Static checks
go vet ./...

# Concurrency race detector (core packages)
go test -race -count=1 ./internal/opsstore ./internal/gateway ./internal/store ./internal/mcp ./internal/server ./internal/project ./cmd/recalldock

# HTTP end-to-end tests
go test -count=1 -tags e2e ./internal/server/...

# Python installer & migration unit tests (Linux)
python3 -I -B tools/test_install_local.py
python3 -I -B tools/test_gateway_migrate.py
```

On Windows (PowerShell):

```powershell
go vet ./...
go test -count=1 ./...
go test -count=1 -tags e2e ./internal/server/...
```

Cross-compile check from Linux to Windows x64:

```sh
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/recalldock
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go vet ./...
```

## Core Architecture & Contribution Guardrails

1. **Test isolation**: All tests that write databases or configuration must use `t.TempDir()`, an isolated `HOME` / `USERPROFILE` / `LOCALAPPDATA`, and explicit `RECALLDOCK_DATA_DIR`. Never read or mutate real user databases in `~/.recalldock` or `%LOCALAPPDATA%\RecallDock`.
2. **Gateway & admission boundaries**:
   - Keep the MCP Gateway surface fixed at 7 tools (`tools_find`, `tools_describe`, `tools_call`, `tools_release`, `work_find`, `work_read`, `work_checkpoint`).
   - Do not weaken `strict` grant checks, live schema verification, `tiered-v4` source evidence checks, or Work CAS generation locking.
   - Do not add automatic background conversation capture or outbound update polling.
3. **Host neutrality**: Never pin, downgrade, or overwrite host official binaries/versions (`codex`, `claude`, `agy`, `dsh`) or strip unrelated user MCP entries.
4. **Cross-platform safety**: Avoid hardcoded POSIX-only paths or Unix permission assumptions in shared code; keep Linux and Windows platform behaviors explicit via build tags (`*_linux.go`, `*_windows.go`).
5. **Commit style**: Use [Conventional Commits](https://www.conventionalcommits.org/) (e.g., `fix(gateway): ...`, `feat(store): ...`, `docs: ...`). Include tests for any behavioral fix.

## Release Packaging

See [docs/RELEASE.md](docs/RELEASE.md) for Linux `.tar.gz` (`tools/build-release.py`) and Windows `.zip` (`tools/build-release-windows.py`) immutable release builds and verification gates.
