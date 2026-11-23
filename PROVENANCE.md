# RecallDock Provenance & Attribution

> 中文概览：RecallDock 在开发最初曾参考过 [Gentleman-Programming/engram](https://github.com/Gentleman-Programming/engram)（MIT License）的早期工作树，随后在公开调研、比较多项记忆系统与 Agent 协议思路后，依据独立目标重新设计和实现了核心架构。本文档依据可核查的代码、依赖清单与 Git 基线记录，如实区分源码继承、大幅改写、独立新建、概念借鉴及无法逐行还原的历史边界。

---

## 1. Development Trajectory & Initial Reference Baseline

RecallDock was not developed simply as an "Engram rename" or an incrementally patched fork. Its evolution followed three distinct phases:

1. **Initial Reference Baseline**: Local development started from a recovered early "Engram Next" working tree imported as local baseline commit `9a5cc0b` (510 files; 198 Go files / 122,953 LOC), originating from **https://github.com/Gentleman-Programming/engram** under the **MIT License** (`Copyright (c) 2026 Alan Buscaglia`). That upstream repository also defines naming expectations in `TRADEMARKS.md`, permitting derivative or distinct distributions under separate primary names provided copyright notices remain intact and official endorsement is not implied.
2. **Broader Research & Design Exploration**: Subsequent architecture decisions were shaped by comparing external memory and agent designs (including `LangMem`, `Generative Agents` concepts, official SQLite documentation, Obsidian link conventions, and native Codex / Claude Code / Antigravity MCP & plugin specifications) alongside independent requirements for fail-closed project isolation and small context footprints.
3. **Extensive Independent Implementation**: Between baseline `9a5cc0b` and the `v0.3.23` feature freeze (before adding native Windows support in `0.3.24`), the codebase expanded to **802 files** (**436 Go files / 164,815 LOC**): **305 files added**, **467 modified**, **25 renamed**, **13 deleted**, with only **5 files** remaining byte-for-byte unchanged relative to `9a5cc0b`.

---

## 2. Classification of Codebase Origins

### Category 1: Directly Inherited Unchanged Baseline Files

At the `v0.3.23` feature freeze, only 5 files from baseline `9a5cc0b` remained byte-for-byte unchanged (configuration/build scaffolding and unchanged baseline helpers). This is a historical baseline comparison, not a claim that removed documents are present in the current release. Where upstream MIT files remain, copyright notices are preserved under [LICENSE](LICENSE) and [NOTICE.md](NOTICE.md).

### Category 2: Inherited and Heavily Modified / Rewritten Subsystems

These subsystems existed in `9a5cc0b` and retain structural lineage, while undergoing substantial security, project-scope, storage, and CLI behavioral rewrites:

- `internal/store/` core files (`store.go`, `candidates.go`, `retrieval.go`, `relations.go`, `dependencies.go`, `resource_dependencies.go`, `diagnostic.go`)
- `internal/mcp/`, `internal/server/`, `internal/obsidian/`, `internal/project/`, `internal/setup/`, `internal/sync/`, `internal/tui/`, `internal/diagnostic/`, `internal/version/`, `internal/timeutil/`, `internal/candidatehook/`
- `internal/cloud/*` and `internal/llm/*` (retained for source/test continuity and structural reference; cloud autosync and external semantic runners are **disabled** in production entrypoints)

### Category 3: Removed Historical Documents & Retained Legacy Plugin Assets

The historical technical manuals `DOCS.md`, `docs/AGENT-SETUP.md`, and `docs/PLUGINS.md` have been removed from the current source tree and are excluded from public exports; they are not part of the RecallDock release documentation. Their removal does not remove upstream attribution, which remains in [LICENSE](LICENSE), [NOTICE.md](NOTICE.md), and this provenance record.

Legacy assets under `plugin/` remain for source lineage and existing test coverage, not as the current supported installer or host-adapter workflow. The root `.claude-plugin/` contains the current marketplace metadata. Current host adapters live in `integrations/host-plugins/`, and current installation and release instructions are maintained in [docs/INSTALLATION.md](docs/INSTALLATION.md), [docs/WINDOWS.md](docs/WINDOWS.md), and [docs/RELEASE.md](docs/RELEASE.md).

### Category 4: Independently Designed and Implemented in RecallDock

The following packages and subsystems do not exist in upstream `Gentleman-Programming/engram` and were designed and built specifically for RecallDock:

1. **On-Demand MCP Gateway (`internal/gateway/`)**:
   - Fixed 7-tool MCP surface (`tools_find`, `tools_describe`, `tools_call`, `tools_release`, `work_find`, `work_read`, `work_checkpoint`) decoupled from up to 200 registered backend servers
   - Execution config fingerprinting v2, live schema hash verification, `agent` vs. `strict` trust modes, per-grant root/session/args/expiry bounds, stateful lease lifecycle, idle TTL reaping, and read-only host config inspection (`cmd/recalldock/gateway_scan.go`)
2. **Independent Work Continuity Engine (`internal/opsstore/`, `internal/worklog/`)**:
   - Separate `operations.db` (`schema v2`) for bounded L0/L1/L2 checkpoints, operation events/patches, optimistic generation CAS, trusted host-session provenance, and cached MCP tool catalogs
3. **Long-Term Memory Isolation & Security Subsystems (`internal/store/` — 87 new Go files added post-`9a5cc0b`)**:
   - Opaque canonical project identities (`prj-...`), physical directory identity binding (`device+inode` on Linux; `VolumeSerialNumber+FileIndex` on Windows) + `.recalldock/root.json`, and linked-worktree inheritance checks
   - Verifiable candidate admission (`candidate_auto.go`, `candidate_evidence.go`, `candidate_file_evidence.go`, `candidate_file_open_*.go`, `candidate_sweep.go`, `candidate_approval.go`)
   - Optimistic CAS observation revision history (`observation_cas.go`, `observation_history.go`, `observation_create.go`)
   - Lifecycle cooldown archiving, recall reinforcement, similarity grouping, and bounded refinement (`memory_archive.go`, `memory_recall.go`, `similarity.go`, `consolidation.go`, `refine.go`)
   - CJK bi/tri-gram indexing, bounded read budgets, and WAL-safe `VACUUM INTO` online backups (`search_cjk_index.go`, `read_budget.go`, `read_selectors.go`, `backup.go`)
4. **Input & Lifecycle Policy Guards (`internal/memorypolicy/`, `internal/toolargs/`)**
5. **Local Loopback Operator Web UI (`internal/operatorui/`)**
6. **CLI Lifecycle Entrypoints (`cmd/recalldock/` — 41+ new Go files post-`9a5cc0b`)**:
   - `gateway`, `work`, `projects`, `memory`, `resources`, `backup`, `host-event`, `ui`, `setup`
7. **Native Host Adapters & Release Tooling (`integrations/host-plugins/`, `tools/`)**:
   - Thin host adapters for Codex, Claude Code, and Antigravity CLI
   - Offline release packaging, digest-CAS + protected rollback installers (`install-local.py`, `install-windows.ps1`), Windows verifier (`verify-windows.py`), and repeatable public snapshot exporter (`export-public.py`)
8. **Native Windows x64 Platform Layer (`*_windows.go`)**:
   - Win32 directory handle / reparse-point checks, `%LOCALAPPDATA%\RecallDock` storage isolation, Windows 8.3 short-path canonicalization, and process-group isolation

### Category 5: Source Code Inherited from Other External Projects

**None.** No third-party source code was copied into RecallDock outside standard Go module dependencies declared in `go.mod`.

### Category 6: Conceptual References & Official Interface Specifications

The following external articles, papers, and specifications served strictly as **design ideas or protocol specifications** (zero source code copied):

| Source | Role | Reference Focus |
|---|---|---|
| **LangMem Conceptual Guide** | Conceptual | Functional separation between semantic, episodic, and procedural memory |
| **Generative Agents (`arXiv:2304.03442`)** | Conceptual inspiration | Structured reflection ideas (explicitly rejecting raw transcript ingestion and heuristic decay scoring) |
| **Obsidian Graph & Wikilinks Spec** | Interoperability target | One-way Markdown vault projection format (`internal/obsidian`) |
| **SQLite Documentation** | Technical reference | `VACUUM INTO`, WAL snapshot semantics, FTS5 tokenization |
| **MCP / Codex / Claude Code / Antigravity Docs** | Protocol target | MCP stdio JSON-RPC, host plugin manifests, and hook schemas |

### Category 7: Items That Cannot Be Independently Verified

- Local baseline commit `9a5cc0b` cites upstream commit `5162366` alongside pre-import local modifications in a recovered working tree.
- Upstream commit `5162366` returned HTTP `404` on the `Gentleman-Programming/engram` GitHub API during this audit, so line-by-line attribution between upstream `5162366` and pre-`9a5cc0b` local changes cannot be reconstructed mechanically.
- Consequently, RecallDock retains full MIT copyright attribution (`Copyright (c) 2026 Alan Buscaglia` alongside `Copyright (c) 2026 RecallDock Contributors`) as a conservative legal boundary.

---

## 3. Dependency Licenses (`go.mod`)

Direct dependencies inspected from Go module cache license files:
- **MIT**: `github.com/a-h/templ`, `github.com/charmbracelet/bubbles`, `github.com/charmbracelet/bubbletea`, `github.com/charmbracelet/lipgloss`, `github.com/jackc/pgx/v5`, `github.com/mark3labs/mcp-go`
- **BSD-3-Clause**: `golang.org/x/net`, `golang.org/x/sys`, `modernc.org/sqlite`
- **Copyleft (GPL/AGPL)**: None.

---

## 4. Public Repository Export Policy

To keep local development history, personal email metadata, machine paths, local workbooks, and operator databases separate from public source releases, the public repository is produced from a manifest-verified clean export (`public-export-manifest.json` + `tools/export-public.py`) without any local developer instructions or private state in Git history.
