# Changelog

本项目的所有重要变更记录在这里。提交信息遵循 [Conventional Commits](https://www.conventionalcommits.org/)。

版本以 annotated tag 与不可变平台包（Linux x64 `.tar.gz` 与 Windows x64 `.zip`）交付：每个正式版本从干净提交构建，包内附 `release.json` 与 SHA256 校验；安装、备份与回滚说明见 [docs/INSTALLATION.md](docs/INSTALLATION.md)、[docs/WINDOWS.md](docs/WINDOWS.md) 与 [docs/RELEASE.md](docs/RELEASE.md)。破坏性变更在对应版本条目中显式标注迁移路径。

## 0.3.24 — 2026-11-22（首次公开发行候选 + 原生 Windows x64 支持）

- **feat(windows):** 新增原生 Windows x64 (`windows/amd64`) 支持（纯 Go 零 CGO，无需 WSL2 或管理员权限）：默认数据目录 `%LOCALAPPDATA%\RecallDock`（与 WSL2 `~/.recalldock` 隔离）、Windows 卷序列号/文件索引目录身份与 reparse-point 防护 (`directory_identity_windows.go`、`identity_windows.go`、`candidate_file_open_windows.go`)、Windows 8.3 短路径规范化 (`NormalizeWindowsShortPath`、`expandOpsShortPath`)、进程组隔离 (`process_windows.go`)、独占锁项目识别 (`host_project_windows.go`)、路径大小写不敏感与 UNC 路径拒绝、PowerShell 交互/无交互安装与受保护回滚 (`tools/install-windows.ps1`)、Windows ZIP 打包 (`tools/build-release-windows.py`) 及 Windows 验证脚本 (`tools/verify-windows.py`)。
- **feat(gateway/install):** 新增只读 `recalldock gateway scan`（区分 `host_direct` / `gateway_managed` / `both`，绝不读取/回显密钥且不静默接管）、`gateway add` 直接命令行参数（保留 `--input`）与移除后端时的 live lease 及时关闭；Linux `install-local.py` 与 Windows `install-windows.ps1` 增加引导式 `--interactive` / `-Interactive` 安装并保留摘要 CAS 与受保护回滚。
- **docs/export:** 新增可重复的公开导出清单与校验器 (`public-export-manifest.json` + `tools/export-public.py`)，默认排除个人开发指令与内部过程文档；重写面向普通用户且澄清 7 工具与 200 后端上限的中英文 [README.md](README.md) / [README.zh-CN.md](README.zh-CN.md)、[PROVENANCE.md](PROVENANCE.md)、[docs/GATEWAY-MCP.md](docs/GATEWAY-MCP.md)、[docs/INSTALLATION.md](docs/INSTALLATION.md) 与 [docs/WINDOWS.md](docs/WINDOWS.md)。
- **compatibility:** 核心记忆准入 (`tiered-v4`)、Work CAS、Gateway 7 工具表面与 Linux 行为同 `0.3.23` 保持一致；补充 DSH `0.2.2-rc.5`（Linux）真实模型验收结论（7 工具 Gateway、长期记忆读写与 `tiered-v4` 自动晋升、Work 自主读写与 CAS、原生 `spawn` 子代理及主代理独立核验、Codex 产物跨 Agent 复核流程均已验，同时明确无可信原生 `SessionStart` 通道导致 `host_session_id` 保持未知、父子 Work writer 无法区分及未评估幻觉检出率）；Windows 原生宿主测试覆盖协议/配置/CLI 与 CI 构建验证，真实 Windows Codex/Claude/AGY 交互式模型会话标注为未验证，Windows DSH 标注为不支持/未验证。

## 0.3.23 — 2026-11-22

- **fix(gateway):** 明确 `tools_find -> tools_describe -> tools_call` 连续调用契约，后端命中结果增加 `next` 提示，并同步更新三宿主原生 `selective-memory/SKILL.md`；保持固定七工具、按需 Schema、strict 授权、lease 与宿主原生调度不变。
- **fix(store):** `CooldownArchive` 写入阶段改用单条条件 `UPDATE` 原子重检全部资格并绑定扫描到的 `revision_count` 与 `updated_at`（P2-L3）；新增并发冷却 CAS 与普通召回不改写 stale/archived/review 有效性的回归测试。
- **fix(cli):** `hashResourceFile` 使用 `%w` 保留底层错误链，`resources check` 通过 `errors.Is(err, os.ErrNotExist)` 对缺失文件返回既有 `status=missing`（P2-L4）。
- **docs/validation:** 补充四宿主真实验收边界与限制：AGY 真实核心场景、可信来源自动晋升及 Gateway 调用链已验；Claude 知识检索、Work 读写与 topic 留审/去重已验，普通自动晋升正例未验；DSH 知识/Work 只读已验，自主提交与原生可信生命周期保持未知；Codex 复用 0.3.22 验收证据。

## 0.3.22 — 2026-11-22

- **feat(codex subagent):** 原生 `SubagentStart` 从可信 `agent_id` 登记独立子会话，不把主 `session_id` 当作子身份；默认未注册根不建项目、不保存对话，也不把每轮 `SubagentStop` 冒充 SessionEnd。共享 Work generation CAS、来源/未决保留已验；未绑定宿主会话的 Work writer 仍未知，不伪造归属。
- **fix(gateway/install):** 误把内置 Work 经 tools_call 路由时明确要求直接调用，不误报需授权；目录发现只匹配工具/后端而非知识正文。Codex 只对没有显式策略的受管七工具补原生许可，保留已有审批/过滤/disabled、strict 参数及后端 grants；离线发布新增匹配 annotated tag 的 `--publish-local`，通过全包门禁后才标记 published=true，无远程上传。
- **fix(store):** 派生 `memory_nodes` 重建仅修改失去来源或聚合内容实际变化的节点；无变化的开库、迁移与在线备份保留原时间戳。
- **fix(work):** 未决提醒按既有 operations 状态聚合，显示窗口/全量 checkpoint 替换不再静默清除；当前索引准确，历史保留当时状态，明确终结后解除提醒。进度库 schema v2 不变。
- **fix(event):** 新 event+patch 在持久化前按实际 12 KiB L2 包络校验；转义或 patch 导致超限时 generation/事件/操作/检查点零副作用。旧超限历史不篡改，精确重放不生成新事件。
- **fix(identity):** 宿主生命周期与自动入口复用 `SourceWorktreeMain`，未绑定主检出不创建影子项目；知识身份继承不扩大精确 Work root/lane。
- **perf(gateway):** 固定七工具定义精简，精确缓存 Schema 单条读取，正常许可/lease 复用不变，不任意截断原生结果，不缓存过时 live Schema、不接管宿主调度或版本管理。
- **fix(admission):** 移除 tiered 高置信未验证证据豁免，所有来源必须当前/同项目/可核验；提交只补扫自身项目，显式操作人 sweep 保留全库范围。自动新增、去重、冲突保护、依赖与审计继续走既有事务。
- **breaking(policy):** tiered 设置写版本 4 / tiered-v4，安装器仅将精确已知 v3 策略先行硬化并在保护回滚时保留；不支持 v4 的旧活进程停自动准入，新引擎仍支持核验后的自动准入。
- **test/docs:** 新增 19 例真实合成 MCP 准入评测及 Gateway 成本对照，扩展未决窗口、事件预算、worktree、宿主 PATH 更新/安装保护门禁；更新当前使用说明。

## 0.3.21 — 2026-11-21

- **feat(antigravity):** 新增 Google Antigravity CLI 原生薄插件（按 PATH 解析 `agy`）：plugin.json、mcp_config.json、命名组 hooks.json 和选择性记忆技能。gateway/registry/writer 接受 antigravity host，安装器只准备共享入口并提示正常原生 plugin install，不伪造插件信任或改写原生设置。
- **fix(host lifecycle):** 原生 SessionStart 只提供 conversationId/workspacePaths，受管 hook 命令显式传 `--event SessionStart`；仅单工作区可登记，多工作区保持未绑定，忽略原始用户输入与 transcript 等字段。
- **fix(install):** 升级已有 Codex/Claude gateway 只更新 command，保留 args、strict/host-session/registry、env/cwd/disabled；既有 DSH 受管块只更新唯一 runtime command。
- **fix(work recovery):** 首次写入 SQL 失败后仅清理本事务创建且未变更、数据库确已回滚的 ops-root marker，保留既有/替换/修改文件；写前按真实 canonical root 校验 L0/L1/L2，不可回包的 checkpoint 在持久化前拒绝。
- **fix(host decoding):** 各宿主只解码自己的生命周期字段，异宿主字段保持忽略。

## 0.3.20 — 2026-11-20

- **fix(work recovery):** work_find/CLI find 搜索 goal/lane、当前 summary/details/next/blockers 与事件 summary/details；默认按最新 checkpoint 提交倒序并排除 archived。
- **fix(work pagination/readback):** find/history 返回 has_more/total，2 KiB 预算缩页仍可续页；MCP work_read 新增 history L0 与选定 generation L2 event/patch 读取。
- **feat(work origin):** 可信进程为新检查点记录可选 writer，host-event 生命周期同步 ops 登记。
- **breaking(operations schema):** 写打开进度库升级到 schema v2（新增可空 writer 列），升级前须用现有版本 `work backup` 保存快照。

## 0.3.19 — 2026-11-15

- **fix(host):** Claude 专用启动器把 `--strict-mcp-config` 放在多值 `--mcp-config` 之后，结束配置参数列表。
- **fix(tui/sync/gateway/install):** 修复 TUI 滚动及项目名映射、本地增量按内容指纹去重、内置七工具参数提前校验以及文件权限纳入安装预览与受保护回滚。

## 0.3.18 — 2026-11-15（已发布历史；审计修复随 0.3.19 安装）

- 发布提交 `2ea4db8`，本地 tag `v0.3.18`；合并前两轮 0.3.17 的 FTS 字面转义、MCP 严格输入、读取作用域、结构化错误、CLI JSON、网关注册表与项目统计修复。
- 发布构建与受保护升级回滚门禁通过。

## 0.3.17 — 2026-11-14（本地版本，不远程发布）

- **fix(read):** 普通 CLI 读取/MCP/服务启动要求已存在的所选库，SQLite `mode=rw` 防止检查后自动新建；缺库报 `store_not_initialized`。新增操作人 `store init` 显式初始化；既有明确写入/可信生命周期初始化保持兼容。
- **fix(selectors):** scope 在 search/context 各层统一校验，保留历史 global；内建 type 或作用域中已存的自定义 type 可用，未知 type 报错。显式 CLI --project 统一验证，未知身份不再被 stats/doctor/review/conflicts 假报空或健康，合法空项目保留。
- **fix(errors):** work 相对 root 先报绝对路径要求；查询用已有 x/text NFKC 归一化，全角可匹配半角；文字/JSON查询回显100字符并明示截断，执行原查询。SQLite锁超时复用已有分类，提示锁和重试，不伪装 migration，也不重复前缀。
- **host:** Claude 同名包装器移到恢复点，由官方 installer 恢复原生 symlink2.1.325，官方负责版本更新/清理；recalldock-claude保留显式严格模式，普通claude经原生插件可同时加载其他MCP，这是确认过的选择。Codex已原生管理、DSH持久patch已原生支持，不重开发宿主注入/工具发现/自动记忆/压缩。未批量删除既有版本。
- **test:** 新只读/项目/scope/type/NFKC回归与合成CLI/MCP边界验收全部通过；旧unknown=empty和read隐式建库夹具改为显式已知初始化。全量/vet/目标race/e2e及local/retrieval-v3/read-budget/write-policy smoke见维护记录。

## 0.3.16 — 2026-11-14（本地版本，不远程发布）

- **fix(search):** 中文 `all` 恢复严格 AND，层级补充不得绕过缺词；连续 Han 使用完整 bigram 短语，混合词中文/ASCII 在同一组内 AND，`any` 只在完整组间 OR，避免 `xyz` 坐标记忆挤掉 Kicker。返回 `query_groups/query_terms/ignored_terms`，独立停用词过滤、不删嵌入否定字，空有效词报 `query_has_no_terms`。**行为变更：** 原默认中文部分召回须显式选择 `any`，独立概念用空格分开；raw FTS 始终平面执行并报告回退。
- **fix(mcp):** `mem_search` schema 关闭额外属性并由服务端最外层按同一 schema 校验；拒绝未知键、错类型、小数/非有限整数。SDK 0.44.0 不自动执行输入校验，因此没有只改 schema 后宣称有效；不新增依赖。
- **feat(search):** 直接 FTS 结果增加 `raw_bm25`（原始负值、越小越好）；`score` 仍是查询内相对值，不支持 `score_threshold`。新增有界、原作用域验证的单次编辑拼写建议；`did_you_mean` 改为 `{query,id}`，不自动更正，保留混合词分组。
- **feat(diagnostics):** `explain_empty` 增加有界阶段计数与实际验证过的放宽建议，CLI 新接线 `--explain-empty`；旧标题字符串式建议移到显式放宽诊断。
- **fix(doctor):** 候选统计在项目内聚合，使用注入时钟，过期与未来24小时分开；不自动归档。新增 `--silence-warning CODE` / 操作人环境配置，保留真实 finding 与明确静音标记，不隐藏 error/blocked。
- **test:** 新增 bilingual-v3 布尔/混合词/停用词/拼写夹具，正式包必跑；修正旧部分召回用例为显式 any，覆盖 v1/v2/v3、全量 Go/vet/目标 race/e2e 与真实 MCP 测试。

## 0.3.15 — 2026-11-14（本地版本，不远程发布）

- **fix(search):** 直接检索恢复 SQLite FTS5 BM25 的 IDF、词频和长度归一化，不再用命中词比例重排；新增增量 CJK 双/三字分词投影，两字中文也能排序，SQL 在全体匹配上排序后才裁剪。投影不是新知识主库，dirty 标记与正文写入同事务，读事务同步索引后查询；旧 runtime 写入同样可被追踪。
- **fix(search):** 同分按 pinned、最近召回、创建时间倒序决胜；`recall_summary` 新增 `score_ties`、`tied_at_cutoff`、`limit_dropped`，`budget_dropped` 仅计 token 预算丢弃。`score_source=fts5_bm25` 是本次候选归一化 BM25，非跨查询概率或覆盖率；层级补充明确标记 `hierarchy_keyword_coverage` / `node_match` 且低于直接命中。
- **fix(budget):** L1 因预算降为 L0 时返回逐结果 `detail_downgraded:{requested,applied,reason:"budget"}` 并标记 `truncated`；MCP 结构化结果保留 `selected_content`，CLI JSON 不再丢掉分数来源、降级和召回统计。
- **fix(search):** root/stale/time 过滤前移到候选裁剪之前；修复中文分页忽略 offset、`中文API` 拆词漏检、raw FTS 的 prefix/单字符/去音调命中被覆盖分误删、中文 fallback 绕过 `NOT`。
- **fix(recall):** 只给交付结果计召回，候选扫描/空结果探针不计；MCP 拒绝超预算响应不增加 recall，显式共享预算已耗尽时先拒绝而不执行检索。
- **fix(mcp):** 参数错误不再附带与关键词无关的 `Try simpler keywords` 建议。
- **验证：** 新增合成回归覆盖规模、短长文档、同分与 cutoff、索引更新/回滚、raw FTS、分页、混合词、预裁剪过滤和预算拒绝；完整 store/MCP 测试、全量 Go/vet、HTTP 预算 smoke 与双语 v1/v2 检索均通过。

## 0.3.14 — 2026-11-14（本地版本，不远程发布）

补齐 0.3.13 暴露的中文 topic_key、CR 清理、FTS 语法和空结果语义。

### 中文 topic_key

- **fix(topic):** 纯中文标题不再被 `[^a-z0-9]` 清成空后落到 `topic/general`；保留最多 24 个 Han 字符，例如 `测试主题键建议` → `topic/测试主题键建议`。英文标题行为不变；截断按 rune，不再切坏 UTF-8。
- 这样中文记忆的 upsert 不再全部共享一个 key。

### 候选与文本卫生

- **fix(candidates):** `\r` 是编码污染，不是换行；改为删除 `\r`，而不是替换成 `\n`。历史 `cand-d588…` 已从迁移前备份恢复并删除 CR，版本串恢复为 `(0.1.7 起稳定)`。
- 迁移 v2 只处理含 CR 的候选，不再把所有换行压平。

### 搜索语法与空结果

- **security(search):** 默认把 `NEAR/AND/OR/NOT/*/()` 当成普通文本，CJK 子串回退和 hierarchy 都不执行；显式 `raw_fts=true` 才允许 FTS5 语法。`NEAR(测试 面板)` 默认 0 条，raw 模式才放行。
- **fix(search):** `limit=0` 返回 `invalid_limit`；`limit>20` clamp 到 20 并返回 `limit_clamped:{requested,applied}`。CLI `--limit` 统一 1..20 报错。
- **fix(search):** `explain_empty` 在有结果时返回 `explain_empty_note`，不再静默无效。
- **fix(search):** `empty_reason` 细分 `no_match` / `filtered_out` / `path_out_of_scope`；type/scope/profile 被过滤与 root_path 越界不再都叫 no_match。

### MCP

- **fix(mcp):** `mem_suggest_topic_key` 缺少 title/content 时返回 `{"error_code":"title_required"}`，不再裸文本错误。
- **docs(mcp):** `mem_current_project` 文档改为“未绑定时返回 project_not_selected；可用 all_projects 指引”，与实际一致。
- `mode=full` 继续作为 legacy 视图，文档建议新代码用 index/l0/l1。

### 验证

- 真实库：中文 topic_key、CR 修复、`NEAR(测试 面板)` 默认/raw、limit 0/999、filtered_out/path_out_of_scope、current_project 未绑定均已实测。
- 全量 `go test ./...`、`go vet ./...`、关键包 `-race` 与发布包门禁通过。

## 0.3.13 — 2026-11-14（本地版本，不远程发布）

处理 0.3.12 升级后暴露的授权迁移、上下文预算、full 会话名与 CLI 误操作问题；这一轮以真实库和真实 MCP/gateway 进程复验。

### 网关授权与 lease

- **fix(gateway):** `gateway discover` 对 **read-only 且未绑定参数摘要** 的旧 grant 自动刷新到当前 schema_hash（同工具、同 root，不扩大权限）；side-effecting/参数绑定 grant 不自动迁移。
- **feat(gateway):** 新增 `gateway doctor`：对比 registry grants 与 tool_catalog，列出失配 grant、当前哈希与是否 read-only；`gateway serve` 启动时把同样的提醒写到 stderr。
- **fix(gateway):** agent 模式不再因客户端缓存的旧 `expected_schema_hash` 拒绝调用；strict 模式仍要求匹配。`tools_release` 已释放 lease 幂等返回。

### mem_context 与 MCP

- **fix(context):** 软预算：`mem_context(token_budget=10)` 至少返回 1 条上下文并带 `truncated=true, budget_limited=true`，不再返回“没有记忆”；`empty_reason` 区分 `no_data` / `budget_limited`。
- **fix(context):** `mode=full` 的会话行改为真实 `session_id`、project 和 directory，不再把项目 ID 当会话名。
- **fix(mcp):** `mem_current_project` 未绑定时返回 `project_not_selected`（带 all_projects 指引），不再空 project 假成功；git/ambiguous 仍保留可操作的发现结果。
- **fix(mcp):** 错误结果统一走 IsError；`full + token_budget` 只返回 `context_failed`，不再包成 `result` 数组成功 envelope。
- **fix(mcp):** `mem_session_end` 连续调用保持 ended_at 不变（新增真实 MCP 回归用例）。

### CLI 与数据

- **feat(projects):** 新增 `projects delete --project ID --confirm`，删除从未绑定、无 observations/sessions/prompts/candidates 的空身份；prune 对无绑定项目给出该命令提示。
- **fix(candidates):** sweep 统计补 `skipped`，`scanned = promoted + failed + skipped`；候选 statement 的 CRLF/CR 在写入与一次迁移中清理，`
` 不再污染展示。
- **fix(projects):** `consolidate --dry-run` 在未绑定目录直接报错，不再把 `"unknown"` 当项目做相似度；`--all` 按显示名聚合，注册 ID 不会因共享旧 session 目录被误合并。

### 验证

- 真实库：`gateway discover` 迁移 mem_search grant 后 agent 旧 hash 调用成功；`gateway doctor` 从 1 条 stale 到 0；`mem_context` budget 10/200、mode=full、错误形态、current_project、consolidate /tmp、delete、sweep、`
` 清理均已实测。
- 全量 `go test ./...`、`go vet ./...`、关键包 `-race` 与发布包门禁通过。

## 0.3.12 — 2026-11-14（本地版本，不远程发布）

补一个 lease 生命周期回归：连接已死但 token 仍留在池里时，后续调用可能先拿到死连接、再被新 lease 顶替，表现为“同一 lease 失效后又活”。

- **fix(gateway):** `getLease` 拒绝 `conn.Alive()==false` 的 token；`acquire` 发现死连接时从池中删除并关闭，再创建新 lease，不再保留可复活的旧地址。
- **verification:** 网关全量测试、`-race` 与安装后 agent 调用复验；0.3.11 的检索/MCP/预算修复保持不变。

## 0.3.11 — 2026-11-14（本地版本，不远程发布）

修复检索排序、预算、过滤器与 MCP 会话默认项目这批实际可用性问题。

### 检索正确性（P0）

- **fix(search):** flat 与 hierarchy 改用同一套 0–1 observation 级词法覆盖分数；`score_source` 标明 `flat_lexical` / `hierarchy_lexical` / `node_match` / `flat_and_hierarchy`。hierarchy 节点分只用于挑选候选与轻微加权，不再与 flat 分混量级。
- **fix(search):** ASCII 词按 token 边界匹配，`test` 不再命中 `attested`；只有节点命中、正文没有查询词的层级结果标记为 `node_match` 并压低排序，避免无关记忆总排第一。
- **fix(search):** `type/scope/profile/root_path/time/stale` 在 flat 与 hierarchy 合并前统一再过一遍过滤器；`type=decision` 不会再被 hierarchy 路径绕过。
- **fix(search):** `token_budget` 装不下任何一条时返回 `budget_too_small` 错误（附最小所需 token），不再伪装成“没有记忆”；预算内至少返回一条 L0。
- **fix(search):** 空 query 在参数层返回 `query_required`，不再把 FTS5 原始 SQL 错误抛给调用方。

### MCP 可用性与错误通道（P1）

- **fix(mcp):** `mem_session_start(directory=...)` 成功后把该项目设为本 MCP 进程的默认读项目；后续 `mem_search/mem_context` 不再因为默认家目录 cwd 报 `project_not_selected`。`all_projects` 仍可显式覆盖。
- **fix(mcp):** 读预算按项目隔离；`read_budgets.project` 不再写空字符串，`all_projects` 使用独立的 `all-projects` 桶。旧空项目桶在迁移时删除。
- **fix(mcp):** 读预算包装层遇到 `IsError` 直接返回错误，不再把 `Failed to get context` 塞进成功 envelope；空 query、预算不足、上下文参数错误都走 `error_code` 错误通道。
- **fix(gateway):** `expected_schema_hash` 缺失/过期时明确提示重新 `tools_describe`，并接受 `schema_hash` 别名；`tools_release` 对已释放 lease 幂等返回 `already_released`；lease 错误说明 root/session 绑定，提示重新获取。

### 空结果与可观测性（A–D）

- `mem_search` 增加 `explain_empty`；空结果返回 `empty_reason`，并常返回精简 `recall_summary`（`flat` / `hierarchy_added` / `budget_dropped`）。
- 多词无结果时增加 `did_you_mean` 提示；完整 `trace` 中的 flat/hierarchy 分数也统一到 0–1 并带 `score_source`。

### 项目键与数据

- **feat(projects):** 新增 `projects adopt --name LEGACY_NAME`：把已有名称桶登记为身份，不创建目录绑定、不猜测；真实库的 `ros_ws`、`recalldock` 已显式 adopt 为 `prj-...`，不再和注册项目混排。
- **fix(context):** `review_after` 到期不再排除正式记忆；只有 `stale_at` 和归档排除。

### 验证

- 新增评分/层级子串误命中/类型过滤/预算错误/空 query/空解释/MCP 会话默认项目/读预算项目隔离/错误通道用例。
- 全量 `go test ./...`、`go vet ./...`、关键包 `-race` 与发布包门禁通过。

## 0.3.10 — 2026-11-14（本地版本，不远程发布）

0.3.9 安装后实测发现：网关 `discover` 只更新工具目录，不更新 registry 的 `config_hash`，而 agent 模式仍会拿旧 registry 哈希拒绝调用，于是“discover 成功但 tools_call 还是 approval_required”。

### 修复

- **fix(gateway):** `gateway discover` 成功后在同一个 registry 更新中把 `config_hash` 对齐到本次目录哈希；若配置确实变化，清空旧 grants（旧 schema 许可不能复用）。
- **fix(gateway):** agent 模式只以 live catalog + 实时 schema 为准，不再因 registry 里的陈旧 `config_hash` 拒绝；strict 模式仍执行完整 grant/配置校验。
- **test(gateway):** 增加陈旧 registry 哈希下 agent 调用、discover 后哈希对齐的回归；原有 strict 会话/许可测试保持。

### 验证

- 全量 `go test ./...`、`go vet ./...`、关键包 `-race`；网关 agent 模式真实调用 `dsh-lsp lsp_status` 通过。
- 安装、迁移与网关配置刷新回归验证通过。

## 0.3.9 — 2026-11-14（本地版本，不远程发布）

把“项目键、候选寿命、网关授权、中文检索”四个实际阻塞修到可用；原则是**不写死项目名/工具名，不把时间流逝当成知识失效**。

### 项目身份与长期可用

- **fix(store):** 新增 `project_aliases`：项目创建/导入时按唯一显示名写入别名，合并时别名跟随目标，删除空项目时一并清理。`CanonicalProjectKey` 先查别名，再查唯一 `project_identities.name`；重名时拒绝猜测，要求显式 ID。目录删除、改名、几个月不打开都不影响已登记身份。
- **fix(store):** 一次性回填 `observations/sessions/user_prompts/read_budgets/memory_candidates/prompt_tombstones/memory_dependencies/memory_nodes/cloud_upgrade_state/sync_enrolled_projects` 的历史名称键到项目 ID；无身份或重名的键保持原样，不按目录名猜。
- **fix(store):** 会话、观察、prompt、候选、搜索、stats、context 的入口统一解析别名；`prj-ceb...` 与 `xitong` 现在读同一批数据。历史会话目录被删/改名后，候选仍可晋升，不被已失效的目录绑定卡死。
- **fix(context):** `review_after` 只是复核提醒，不再让 `HostContext` 在几个月不用的项目上变空；只有 `stale_at`（来源失效）和归档继续排除注入。候选去重与文件证据也不再因为复核到期而拒绝。

### 候选准入与长期不丢

- **fix(candidates):** 取消自动 TTL 到期清理；复核到期不再擦除 statement/evidence。`ExpireMemoryCandidates` 只作为显式操作，且保留正文以便 `candidates revive`。
- **feat(candidates):** `candidates sweep` 对已有 review 按当前策略补跑自动准入；`CommitMemoryCandidates` 后也自动 sweep。高置信（≥0.95）、持久类别、无 topic 覆盖的候选即使历史引用已不可本地核验也可自动进入；低置信或弱类别仍留人工。
- **feat(candidates):** `candidates revive ID --expected SHA256 --reason TEXT` 延长复核窗口；`candidates list --status review|expired|promoted|rejected|all` 可查看保留的过期候选。doctor 文案改为“到期保留，不会静默丢弃”。

### 网关与宿主代理

- **feat(gateway):** `gateway serve` 默认 `--trust agent`：宿主是本机操作人启动的代理面，已登记后端工具无需逐次 grant 即可调用；`--trust strict` 保留旧的精确 grant/参数/会话校验。模型仍不能自报 approval 或选择网关 session；`_meta` 仍只转发 progress token。
- **fix(gateway):** agent 模式下允许尚未写入 `config_hash` 的目录项；严格模式与显式 grant 的行为不变。`gateway list` 增加 `trust` 字段。
- **docs(gateway):** 明确 agent/strict 的边界：agent 是本地宿主委托，不是跨用户隔离；需要最小权限时用 strict。

### 中文与混合检索

- **fix(search):** 含 Han 的查询走 CJK 子串回退：对连续 Han 串生成重叠 3-gram，按命中覆盖排序；`面板 kicker plasmoid 动画图标` 这类多词查询不再被 FTS5 的整串 AND 判死，可返回部分命中的有效记忆。纯 ASCII 查询仍保持严格 AND，不放大范围。
- **fix(search):** 覆盖排序取更多候选再筛选，避免“最近但只命中一个词”的结果挤掉更完整的旧结果。

### 验证

- 新增项目别名/历史键回填/删除目录后候选晋升、候选保留与 revive、sweep 高置信/不确定分流、CJK 混合短语、网关 agent 调用用例。
- 全量 `go test ./...`、`go vet ./...`、关键包 `-race` 与发布包门禁通过。

## 0.3.8 — 2026-11-07（本地版本，不远程发布）

修好进度账本“靠猜字段”的问题，并收紧 0.3.7 之后的并发、证据与注入边界。

### 网关与进度账本

- **fix(gateway):** `work_checkpoint` 的 `checkpoint`、`event`、`patch` 不再是空对象占位，改为显式 JSON Schema。`checkpoint` 字段为 `goal`（必填，单行 ≤300 字节）、`summary`、`next`（数组 ≤3）、`details`、`status`、`blockers`、`operations`；`artifacts/milestones/paths/notes` 从来不是字段，顶层只接受 `root/lane/expected/checkpoint/event/patch`。
- **feat(gateway):** `tools_describe` 支持 `server=recalldock_gateway`：不启动任何后端即可返回内置 7 工具的权威 schema 与 `schema_hash`；`tools_find` 的查询命中内置工具时返回 `builtin_tools`。内置工具直接按名称调用，`tools_call` 只用于已登记后端。
- **fix(gateway):** 未知 server 不再报含糊的 `backend_unavailable`，改为提示用 `tools_find` 或 `server=recalldock_gateway`；`work_checkpoint` 对顶层/嵌套未知字段、非法 `status` 明确列出允许字段与枚举（`active/blocked/completed/archived`）。
- **security(gateway):** 后端调用只转发 MCP progress token；其他 `_meta` 不进入已授权参数摘要之外的后端路由面。

### 记忆与候选边界

- **fix(memory):** 精炼应用与 `memory consolidate` 改为同事务：目标与每个来源的快照都绑定到草稿摘要，任一来源在生成草稿后被修改即拒绝；正文更新、来源归档、审计与谱系一起提交或一起回滚。
- **fix(memory):** 草稿超过正文上限时直接拒绝，不再静默截断；相似度阈值拒绝 `NaN`；网页 CAS 冲突提示改为“目标或来源发生变化”。
- **fix(candidates):** 完全相同的已归档/失效/待复核记忆不再作为自动去重目标；`tiered` 与 `checked` 一样在同一事务写入来源依赖和核验审计。
- **fix(candidates):** 文件证据要求登记记忆仍处于可用状态，并在 Linux 上用 `openat2` 固定根目录、拒绝符号链接/跨挂载点/嵌套 `.git` 或 `.recalldock`；不支持的内核或平台保留人工审核。
- **fix(context):** `HostContext` 先按 pinned 优先取数再应用 40 条上限，旧 pinned 记忆不会被最近条目挤出；预算不足时省略提示不再超预算。

### 验证

- 新增网关 schema/describe/错误提示用例；全量 `go test ./...`、`go vet ./...`、关键包 `-race`。
- 新增精炼/整合事务回滚、并发修改、候选归档重复、文件证据所有者状态、注入 pinned 与预算边界用例。
- 包门禁与安装后 MCP 验证在发布记录中给出。

## 0.3.7 — 2026-11-04（本地版本，不远程发布）

把“相似组”从只读命令变成可审核的精炼流程：网页和 TUI 直接看组，网页生成草稿，保存仍走快照 CAS 与显式理由。

### 界面

- **feat(ui):** 新增 `/similar` 相似记忆页：按当前范围列出词法相近的活跃记忆，显示最高相似度、成员、建议目标（正文最长），阈值可选 0.50–0.80；只读，不改知识。选择「全部项目」时按已注册身份聚合，最多 20 组。
- **feat(ui):** 新增 `/similar/draft` 精炼草稿：把来源中**未被目标覆盖的句子**追加到正文，并给出来源列表、被跳过的来源与原因；置顶、已归档、跨项目/作用域的来源不会进入草稿。
- **feat(ui):** 新增 `/similar/apply`：先按草稿摘要校验 CAS，保存精炼正文，再归档来源并写谱系；正文未改时跳过修订但仍校验摘要。部分来源失败时显示成功数与逐条原因，不假装全部完成。
- **feat(tui):** 新增「Similar memories」界面：`j/k` 选组、`enter` 打开建议目标、`[`/`]` 放宽/收紧阈值（0.40–0.90）、`r` 重扫；内容裁剪与 ESC 净化沿用现有渲染层。

### 安全边界

- 草稿中的 `【待人工精炼】` 段落必须由操作人合并并删除；带着标记保存会被拒绝，避免把工具工作底稿写进长期知识。
- 相似扫描、草稿生成都不写库；真正写入只有 `/similar/apply`（CSRF + 必填理由 + CAS），模型传输面不可达。
- 正文改写与来源归档仍是两步语义：来源归档后退出召回/搜索/复核/注入，但保留内容、可按 ID 查看、可在「已归档」恢复；不做自动合并。

### 验证

- 新增 store 用例：只提取独有句子、去重、跳过 pinned/已归档、草稿标记拒绝、过期摘要拒绝、正文未改仅归档、幂等重试、部分失败报告。
- 新增网页用例：分组页、草稿页、CAS 应用、只读拒绝、无效输入；新增 TUI 用例：加载/渲染、打开目标、阈值重扫、空范围提示、标题裁剪与 ESC 净化。
- 全量 `go test ./...`、`go vet ./...`、关键包 `-race`；合成库真机网页截图验证分组与草稿布局。

## 0.3.6 — 2026-11-04（本地版本，不远程发布）

给“精炼合并”加上只读的相似度建议，先看再动手。

### 相似度建议

- **feat(memory):** 新增 `recalldock memory similar --project PROJECT [--threshold 0.6] [--limit 1..50] [--json]`。用 CJK 双字 + 拉丁词的集合相似度（Jaccard + containment）把 active 记忆中可能重复/可精炼的分组；只读，不修改任何知识。
- **行为：** 已归档、stale、review 过期的记忆不参与；每组给出最高相似度、成员 ID/标题，以及“正文最长的一条”作为建议目标（通常信息最全）。
- **阈值实测：** 在真实 402 条记忆的库上，0.5 会把泛泛的“验证纪律”合成 10 条大组（过宽）；0.7 会漏掉 0.66–0.68 的真实近似对；0.6 得到去重对 + 相关工具坑分组，作为默认。

### 工作流

1. `memory similar` 找组；
2. `memory update` 把目标正文精炼成一条（CAS + 理由）；
3. `memory consolidate` 把来源归档并建立谱系；
4. `memory sources` / `memory show` 需要对比时再取回。

## 0.3.5 — 2026-11-04（本地版本，不远程发布）

把“太乱太杂”的旧记忆沉淀成一条可复核的坑，来源不再参与召回与遗忘提醒。

### 记忆整合

- **feat(memory):** 新增 `recalldock memory consolidate <target> --source ID [--source ID] --reason TEXT`。目标条目先由操作人/模型精炼好正文，命令只做两件事：把来源归档、写入 `memory_consolidations` 谱系。**不自动改写正文**，避免合并时静默丢细节。
- **feat(memory):** 新增 `recalldock memory sources <target> [--json]`。来源被归档后仍可按 ID 读取、可列出、可对比；默认召回、搜索、复核/遗忘提醒都不再包含它们。
- **行为：** 目标必须与来源同项目同 scope；pinned 来源拒绝（先 unpin）；重复执行幂等；部分失败会列出成功与失败 ID，不假装全部完成。
- **遗忘：** 只有精炼后的目标参与 `review_after`/stale 复核；来源沉到底层，必要时才显式取出查证。

### 验证

- 全量 `go test ./...`、`go vet ./...`；store 用例覆盖归档、谱系、幂等、pinned 拒绝、注入排除。
- CLI 在合成库实测：两条混乱来源合并为一条精炼坑，`memory sources` 可回溯，`HostContext` 不再出现来源。

## 0.3.4 — 2026-11-04（本地版本，不远程发布）

会话启动默认注入“有界项目索引”，并收口 0.3.3 后发现的界面缺陷。

### 会话启动注入

- **feat(host):** Claude Code / Codex 的 `SessionStart` hook 现在除了登记宿主会话，还会注入当前项目的有界记忆索引（默认 600 estimated tokens）。只列 `active` 观察；`archived`、`stale_at` 非空、`review_after` 已过期的待复核项一律不注入，落实“遗忘/复核机制不进提示词”。
- **feat(store):** 新增 `Store.HostContext(project, tokenBudget)` 与导出的 `EstimateTokens`；pinned 优先、最近的其次，超预算截断并提示用 `mem_search`/`mem_context` 继续；无正文、无对话、无凭证。
- **feat(host):** 注入文本明确标注 “bounded index, verify before use”，避免模型把索引当全文事实。

### 候选准入分级

- **feat(candidates):** 新增 `tiered` 准入模式（policy v3）：在 `checked` 的本地来源/状态核验之上，只有「持久类别 + confidence ≥ 0.85 + 有证据引用」才自动准入；低置信、无来源、临时类别、带 topic 的更新继续留 review。完全相同的事实仍自动去重；topic 更新按“合并/改写”处理，必须人工确认，不做静默覆盖。
- **fix(candidates):** 空/超长理由、未注册 `--project`、TUI 控制字符与详情返回等 0.3.3 后问题一并修复。

### 网页与终端

- **fix(ui):** 归档/编辑/单条审核/批量审核的空理由或超过 512 字节的理由，统一返回 400 与中文提示，不再把 store 的 409 英文内部错误抛给操作人。
- **fix(ui):** `recalldock ui --project prj-xxxx` 指向未注册身份时启动即拒绝，不再静默展示一个永远空白的库；legacy 名称键仍可用。
- **fix(tui):** 记忆正文/标题里的 ESC、OSC 52 等控制序列在渲染前净化，避免清屏、改色或写剪贴板；短 ID 与项目名按 rune 截断，不再切坏 UTF-8。
- **fix(tui):** 详情页按 `t` 看时间线后，`esc` 返回详情而不是跳回搜索结果。

### 宿主插件

- 记录并修复宿主插件 marketplace 指向旧版本目录导致插件静默不加载的问题；升级后安装器会同步更新 Claude/Codex 的 marketplace 路径。

## 0.3.3 — 2026-11-03（本地版本，不远程发布）

面向两个操作界面的缺陷修复与视觉重做；CLI 与长期知识语义未变。

### 网页操作界面

- **fix(ui):** CSP 补上 `img-src 'self'`：此前 `default-src 'none'` 会拦掉内嵌品牌图与 favicon（截图里是破图占位）。同时补 `X-Content-Type-Options: nosniff`、`Cache-Control: no-store`、`Permissions-Policy`。
- **fix(ui):** 只读模式（`--read-only`）此前把候选列表整块藏起来——页面写着“这里只展示候选”，下面却是空白。现在渲染只读候选表；切换浏览范围只写 cookie、不改知识，因此在只读模式保留。
- **fix(ui):** `/project` 的 `next` 参数接受 `/<反斜杠>evil.example`，浏览器会把反斜杠归一化成 `//`，形成开放重定向。现在拒绝反斜杠、控制字符和非本地路径。
- **fix(ui):** 搜索的项目筛选不校验：未知项目会静默返回 0 条，页头却仍显示旧范围。现在返回 400、保留查询与表单；时间格式错误也保留表单，不再整页替换。
- **fix(ui):** “下一页”改为探测额外一行（store 会把 `Limit` 截到 `MaxSearchResults`），满页时不再误判没有下一页；结果摘要显示本页条数与真实范围。
- **fix(ui):** 项目页把 `worktree_main` 绑定的目录标成“未绑定/仅配置”；现在与 CLI 作用域判断一致，并显示每个项目的记忆数与“切换到此项目”。
- **fix(ui):** “已归档”在“全部项目”范围下直接报错；现在按项目分组展示（含未注册的历史项目名）。
- **fix(ui):** 项目为 NULL 的观察不再渲染成 `<nil>`，统一显示“—”。
- **fix(ui):** 未知路径返回带导航和返回入口的 404，而不是 net/http 的纯文本。
- **fix(ui):** 候选、关系、备份超过 50 条时静默截断；页尾现在明确“只显示前 N 条”。
- **feat(ui):** 重做视觉与信息层级：品牌头 + 图标导航 + 范围选择器、大数字卡片、表格横向滚动与窄屏适配、相对时间、类别/来源/绑定状态中文化、诊断默认只显示问题（可一键看全部）、记忆详情“技术细节”折叠、编辑与归档合并进操作区。

### 终端界面

- **fix(tui):** 移除 “Setup agent plugin” 菜单。它调用旧的 `internal/setup`，会写宿主插件目录与 Claude allowlist，与当前 `setup`＝本地发布安装、宿主插件走 `integrations/host-plugins` 的边界冲突。改为只读的 “About & shortcuts”（版本、数据库路径、Store ID、常用命令、快捷键）。
- **fix(tui):** 详情页滚动没有上限：按到底后要按同样次数才能回滚。现在按实际换行行数钳制，并支持 PgUp/PgDn/Home/End。
- **fix(tui):** 时间线的 `j/k` 完全无效（视图没有使用滚动值）；整段内容现在可滚动并显示行号范围。
- **fix(tui):** 候选超过一屏时游标会走出可见区；列表窗口现在跟随游标，PgUp/PgDn/Home/End 可用。
- **fix(tui):** 列表选中行只高亮前半段（内层颜色 reset 抹掉背景）；现在整行单块渲染。CJK 按显示宽度截断，长标题/正文不再溢出到终端边缘。
- **fix(tui):** 终端小于约 58×16 时布局被截断；现在显示最小尺寸提示。
- **fix(tui):** 诊断页 9 条 `ok` 淹没唯一的警告；默认只列非 ok，`a` 切换全部，原因与下一步按宽度换行。
- **feat(tui):** 顶部固定品牌/版本/作用域栏与分隔线；统计卡片按宽度均分；项目作用域显示名称；动作行带说明与整行高亮。`p` 在任意列表/详情页切换作用域，`r` 刷新，`g/G` 跳首尾。

### 验证

- 全量 `go test ./...`、`go vet ./...` 通过。
- 真机截图：网页（概览、搜索、详情、候选、项目、诊断、备份、归档、编辑、404、窄屏）；TUI（概览、关于、候选、诊断、详情、搜索结果、会话列表）。
- 发布包构建、安装预览与受保护回滚验证通过。

## 0.3.2 — 2026-11-02（本地版本，不远程发布）

在 0.3.1 之上完善项目身份边界，并补上操作人真正会用的两个界面（网页 + 终端）。

### 操作界面

- **feat(ui):** 新增 `recalldock ui` 回环网页操作台：按作用域统计的概览、带项目切换与 `since/until` 的分页搜索、记忆详情（归档/恢复、带 CAS 的正文编辑）、候选逐条/批量批准拒绝、项目与当前目录绑定状态、诊断十项、备份列表（按需 SHA256）、冲突列表、已归档索引，以及隐藏并拒绝全部写操作的 `--read-only` 模式。打印一次性 token URL，只接受回环 Host，HttpOnly SameSite=Strict cookie，写操作要求表单 CSRF；与 `serve`/MCP 是独立进程，后者不挂载该界面。
- **feat(ui):** 界面使用随二进制内嵌的品牌 logo（`go:embed`，离线可用）、图标导航、自适应深浅色与重做的排版；搜索命中词高亮。
- **feat(tui):** 终端界面新增候选审核（必填理由、逐条快照 CAS、失败原因）、诊断页（十项检查、`r` 重跑）、项目作用域切换（`p`），详情页与区块使用图标标记，配色适配深浅终端。
- **feat(store):** `SearchOptions.Offset` 支持有界分页；`Store.ProjectStats` 返回单项目统计。
- **fix(ui):** 网页与 TUI 不再把 git remote/仓库名当作可写作用域；未绑定目录只显示提示，需显式绑定或 `.recalldock/config.json`。

### 项目身份与目录

- **fix(project):** `.git` 声明无法解析（坏 checkout、悬空 submodule/worktree 指针）时不再回退到祖先目录的绑定，检测直接 fail-closed。
- **fix(project):** 跨文件系统移动目录会改变 device+inode，旧绑定按 inode 命中但路径校验失败：检测报 stale/copied root，写入被拒绝，必须显式重绑。
- **feat(project):** `projects current` / `store current` 新增 `scope_state`（`bound` / `configured` / `hint_only` / `unbound`）与终端 `Scope:` 行，区分“检测提示”和“可写作用域”；子模块按自身 remote 识别，父仓库绑定不外溢。
- **worktree:** linked worktree 继续继承主检出的已验证绑定（`source=worktree_main`）。

### CLI 与行为修正

- **fix(cli):** 未知 observation/candidate id 返回领域错误（`observation #N not found` 等），不再泄漏 `sql: no rows in result set`；`memory version` 区分“记忆不存在”和“历史序号不存在”。
- **feat(cli):** `export --project PROJECT [file]` 支持导出单个项目。
- **feat(candidates):** 候选列表与详情带 `session_origin`（host/operator/model/unknown），让操作人看到候选来自哪种传输；它是声明，不是身份认证。
- **fix(conflicts):** `conflicts list/stats/scan` 使用 store-aware 检测解析当前目录的绑定；未绑定目录显式报错，不再静默 `unknown`。
- **fix(conflicts):** `conflicts scan --dry-run` 输出 `would_insert`，镜像关系计入 `already_related`，预览与 `--apply` 一致。
- **fix(diagnostic):** 手工会话不一致的修复命令给出精确的 `doctor repair --project <current>`；并发 `doctor repair --apply` 第二个进程在守卫更新命中 0 行时报 `noop`。
- **fix(gateway):** 固定授权过期回归：过期或不可解析的 `expires_at` 一律拒绝，活跃许可要求 session 与参数指纹精确匹配。

### 验证

- 全量 `go test ./...`、`go vet ./...`；发布包门禁（安装/回滚、Obsidian、检索 v1+v2、候选证据、项目范围、项目身份）全部为 true。
- 真机验证：跨文件系统移动/重绑、子模块隔离、并发 doctor repair、网页与 TUI 逐页截图。

## 0.3.1 — 2026-11-02（本地版本）

- **feat(sessions):** 记录会话来源并在同步载荷中保留；重复注册时保持较强的已记录来源。来源是声明，不是身份认证。
- **fix(sessions):** 模型会话要求已有项目绑定或显式项目配置；linked worktree 可继承主检出的已验证绑定。重复启动时响应实际保存的会话来源。
- **feat(diagnostic):** 增加会话来源与网络边界检查；增加记忆分类规则和第二套中英文检索夹具。
- **test(release):** HTTP 端到端测试使用隔离的项目配置，适配模型会话绑定要求。

## 0.3.0 — 2026-11-02（本地正式版，不远程发布）

首个以独立发布包形式交付的无后缀版本，收拢 0.2.x 与 `core-completion` 批次已验证的源码。

- **feat(surface):** MCP `initialize` 现在报告实际构建版本，不再硬编码 `0.1.0`；版本只能来自二进制构建（`MCPConfig.Version`），不能由工具参数提供。
- **test(surface):** 新增入口策略矩阵契约：每条 HTTP 路由和每个 MCP 工具都必须有显式审阅过的 `read / lifecycle / proposal / deny` 决策，新增入口未审阅即测试失败；未知路径与工具保持 fail-closed。
- **fix(sessions):** 固定项目的 MCP 进程不能通过 session ID、目录或候选参数跨到另一项目；底层注册在事务内核对归属，冲突返回 409，同步日志复制真实完整 session。
- **feat(project):** `projects consolidate` 同步迁移云同步所有权；退役键栅栏扩到 13 表×2；精确映射重放幂等，相近未选中键保留；复制标记/路径复用不自动继承历史。
- **feat(memory):** `checked` 准入收紧为同项目当前 observation 快照或已登记本地文件的来源/状态核验，核验/知识/依赖/审计同事务提交；不冒充语义真实性。
- **feat(bookkeeping):** 网关启动继承调用方取消，未知结果不自动重试；stateful lease 明确可见；执行配置指纹 v2 覆盖归一化/展开后的配置与 env。
- **feat(release):** 离线安装预览/CAS/私有备份/保护回滚、干净源码发布门槛、固定中英文词法评测 v1、Obsidian 生命周期/类型关系投影与附件摘要核对。
- **known limits:** 宿主握手式身份认证未实现，项目范围检查不等于认证；预算与 token 数为估算；Windows/macOS 未验收；当前仓库无 remote，发布只在本地完成。

## Unreleased

<!-- Changes that are merged but not yet released are tracked here until the next tag. -->
