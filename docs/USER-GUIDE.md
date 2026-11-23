# RecallDock 使用手册

本手册面向**本机操作人**：怎么用、命令怎么写、出错怎么查、怎么升级回滚。快速安装见 [INSTALLATION.md](INSTALLATION.md)，宿主接入见 [HOST-ADAPTERS.md](HOST-ADAPTERS.md)，Gateway 代理配置见 [GATEWAY-MCP.md](GATEWAY-MCP.md)，打包与发版流程见 [RELEASE.md](RELEASE.md)。

> 实际生效版本请始终以 `recalldock version` 和新启动的宿主进程为准；进度库采用 schema v2（含 `ops` 操作审计与工作账本），降级或回滚时请使用安装器/迁移器自动创建的备份快照。

---

## 0.3.21+：Antigravity CLI

本节说明 Google Antigravity CLI（`agy`）的接入接口，实际生效版本以 `recalldock version` 及新宿主进程核对。`agy` 使用发布包内 `marketplace/plugins/antigravity/` 的原生插件，`setup --host antigravity` 仅准备共享 CLI 并提示 `agy plugin install PLUGIN_DIR`，不自动给予信任或改写原生设置。详情与单工作区/无 `SessionEnd`/认证限制见 [HOST-ADAPTERS.md](HOST-ADAPTERS.md#google-antigravity-cli-适配)。

下一版升级已有网关只改变runtime路径，保留strict、真实session、registry、env/cwd及disabled；首次SQL写失败可保护撤销新marker，写前确保含writer的L0/L1/L2可交付。开发验证全部使用隔离HOME和合成数据库，认证agy会话/模型推理尚未测试。

## 0. 这台机器上的现状

| 项 | 位置 / 值 |
| --- | --- |
| 已安装版本 | `0.3.20`（tag `v0.3.20`）；`0.3.19`/`0.3.18`/`0.3.17`/`0.3.16`/`0.3.15`/`0.3.14`/`0.3.13`/`0.3.12`/`0.3.11`/`0.3.10`/`0.3.9`/`0.3.8`/`0.3.7`/`0.3.6`/`0.3.5`/`0.3.4`/`0.3.3`/`0.3.2` 保留在 `releases/` 可回滚 |
| 启动器 | `~/.local/bin/recalldock` → `~/.local/share/recalldock/releases/0.3.20/runtime/recalldock` |
| 长期知识库 | `~/.recalldock/recalldock.db`（SQLite，唯一主数据） |
| 进度账本 | `~/.recalldock/operations.db`（只存任务进度，不是记忆库） |
| 备份目录 | `~/.recalldock/backups/`、`~/.recalldock/operations-backups/` |
| 宿主配置 | Codex：`~/.codex/config.toml`；Claude：`~/.recalldock/claude-gateway.json`；DSH：`~/.dsh/cordis.patch.yml` + `~/.dsh/profiles/web/cordis.patch.yml` |
| 网关注册表 | `~/.recalldock/gateway.json` |
| 操作界面 | `recalldock ui`（本机网页，127.0.0.1:7438）、`recalldock tui`（终端） |

先确认版本：

```sh
recalldock --version          # 期望：recalldock 0.3.20
```

> 仓库源码里的 `./recalldock` 是调用 `bin/recalldock.bin` 的启动脚本；验证源码时不要和 `PATH` 里已安装的 `recalldock` 混淆。

---

## 1. 五分钟上手

```sh
# 1) 建一个项目身份（得到 prj-... 形式的 ID）
recalldock projects create --name my-app
# 输出示例：{"id":"prj-0f29...","name":"my-app","created_at":"..."}

# 2) 把这个身份绑到工作目录（绝对路径；之后该目录及其子目录都认这个身份）
recalldock projects bind prj-0f29... --directory /path/to/my-app

# 3) 确认当前目录解析到哪个项目
recalldock projects current --json

# 4) 在项目目录里保存一条记忆
cd /path/to/my-app
recalldock save "在线备份只用快照" "运行中的 SQLite 用在线快照备份，恢复前核对完整性。" \
  --type decision --topic backup/online

# 5) 搜回来
recalldock search "在线备份"
recalldock context --mode index     # 只给索引，不含正文
```

**三条最重要的规则**

1. 未绑定的目录默认**读不到**任何历史（返回空范围），不会按目录名猜项目。
2. 模型（AI）侧不能直接写长期知识，只能提交候选，由你用 `candidates` 审核；`save` 这类写入是操作人命令。
3. 项目身份 `prj-...` 是唯一键；`my-app` 只是显示名。

---

## 2. 三层数据，别混用

| 层 | 是什么 | 命令 | 会不会进 Markdown |
| --- | --- | --- | --- |
| 长期知识 `recalldock.db` | 决策、架构、bug、偏好、流程等可复用事实 | `save/search/memory/…` | `export` 时导出 |
| 进度账本 `operations.db` | 当前任务做到哪、下一步、阻塞 | `work …` | 只在显式导出时 |
| 投影 / 交换 | Obsidian 笔记、JSON 交换文件 | `obsidian-export`、`export`、`import` | 只是投影，不是主库 |

普通进度、逐轮对话、原始日志不要写进长期知识——写 `work` 账本。

---

## 3. 长期记忆：日常命令

### 3.1 写入

```sh
# 基本写法；--project 可显式指定（未绑定时必须给）
recalldock save "标题" "正文" --type decision --topic deploy/rollback

# 可选字段
--scope project|personal       # project 默认；personal 是个人偏好类，仍受当前项目范围限制
--memory-class operational|user|agent|project
--memory-path agent/procedures/release   # 虚拟路径，根必须等于 memory_class
--abstract "一句话摘要" --overview "稍长概览"
```

- `save` 是**只创建**：同一 `topic_key` 再次 `save` 不会覆盖，要改用 `memory update`。
- 类型示例：`decision`、`architecture`、`bugfix`、`config`、`command`、`pattern`、`procedure`、`preference`、`note`。

### 3.2 读取

```sh
recalldock search "关键词" --limit 5                 # 当前绑定项目
recalldock search "关键词" --project prj-xxx --detail l1
recalldock search "关键词" --all                    # 跨所有项目（与 --project 互斥）
recalldock search "报错原文" --match-mode any        # 任一词命中，召回更宽
recalldock search "x" --profile semantic             # 排除 checkpoint/session_summary 这类工作记忆
recalldock search "x" --json | jq '.results[0].id'   # 机器可读

# 时间范围（按 created_at，UTC；--until 不含端点，纯日期会包含当天）
recalldock search "部署" --since 2026-11-01
recalldock search "部署" --since 2026-10-25 --until 2026-11-02
recalldock search "部署" --since 2026-11-01T00:00:00Z --until 2026-11-02T12:00:00Z
recalldock context --mode index                       # 有界索引（默认）
recalldock context --mode l0 --token-budget 2000
recalldock stats --json
recalldock timeline 42 --before 3 --after 3          # 看某条观察前后文
```

> `search -- --help` 可以搜字面量 `--help`。

### 3.3 查看、修订、归档

```sh
recalldock memory show 42                 # 元数据 + 快照摘要（snapshot_sha256）
recalldock memory show 42 --full          # 正文
recalldock memory history 42              # 修订历史（L0）
recalldock memory version 42 2 --full     # 看某一版

# 修订必须带当前快照摘要（CAS），防止覆盖别人/自己后来的改动。
# --input 要求是普通 JSON 文件（<=64 KiB），不能直接写内联 JSON：
cat > /tmp/patch.json <<'JSON'
{"content": "新的正文", "abstract": "一句话摘要"}
JSON
recalldock memory update 42 --expected 75532e31b974d6aa... \
  --input /tmp/patch.json --reason "补充恢复步骤"

# 归档：保留知识和历史，默认召回排除；不是删除
recalldock memory archive 42 --expected <snapshot_sha256> --reason "已过期"
recalldock memory archived --project prj-xxx
recalldock memory reactivate 42 --expected <snapshot_sha256> --reason "重新启用"

# 冷却：把长期没被召回的记忆排到复核队列（不删除，pinned/保护类跳过）
recalldock memory cooldown --project prj-xxx --min-age 30d --dry-run
recalldock memory cooldown --project prj-xxx --min-age 30d
```

### 3.4 冲突与重复（`conflicts`）

`conflicts` 在绑定目录里运行即可，默认用当前项目；未绑定会直接报错，不会静默扫 0 条。

```sh
recalldock conflicts scan --dry-run          # 预览：would_insert / already_related
recalldock conflicts scan --apply            # 写入 pending 关系（不自动改旧记忆）
recalldock conflicts list                    # 看关系、判断状态与两侧观察
recalldock conflicts show 1
recalldock conflicts stats
recalldock conflicts deferred --status deferred --limit 20   # 同步延迟重放队列
recalldock conflicts deferred --replay                       # 重放（可能有副作用，先看列表）
```

`--semantic` 在本地方向是禁用的（外部 runner 不开）；扫描只做本地词法匹配。

删除是操作人显式动作：

```sh
recalldock delete 42                 # 软删（可恢复语义上的“隐藏”，仍占历史）
recalldock delete 42 --hard          # 永久
recalldock delete session <id>       # 只有该 session 没有任何观察（含软删）才允许，不级联
recalldock delete prompt <id>
recalldock delete project <PROJECT_ID>                 # 级联软删除；当前目录必须匹配，先 backup
recalldock delete project <PROJECT_ID> --hard --confirm # 不可逆硬删除，确认对象和目录
recalldock projects prune --all --dry-run              # 全库空壳键预览；注册身份使用 --project 路径
```

---

## 3.5 网页操作界面（`recalldock ui`）

不想敲命令时，用本机网页界面浏览、搜索、审核候选、合并相似记忆、归档：

```sh
recalldock ui                     # 默认 127.0.0.1:7438，在项目目录里运行即锁定该项目
recalldock ui --port 7439         # 换端口
recalldock ui --project prj-xxx   # 显式指定项目
recalldock ui --open              # 顺便用默认浏览器打开
recalldock ui --read-only         # 只浏览；搜索也不更新 recall 等主库数据（仍可切换浏览范围）
```

启动后终端会打印一个**带一次性 token 的 URL**：

```
http://127.0.0.1:7438/?token=<随机 token>
```

- 只在运行 `recalldock ui` 的终端里打开；**不要把 URL 贴到聊天/日志里**（它相当于本机管理密码）。
- 第一次打开会写入 HttpOnly cookie 并跳转到无 token 的地址；之后直接访问 `http://127.0.0.1:7438/` 即可。
- 它只绑定 `127.0.0.1`，不接受非回环地址；和 `serve`/MCP（给 AI 用的入口）是**两个独立进程、两个信任面**，`serve` 永远不会挂载这个界面。
- 顶栏有**项目切换器**：可选「全部项目」或任一注册项目，选择写入 HttpOnly cookie 并对所有页面生效；切换只接受本机已注册的身份，重定向目标不允许外站。

页面：

| 页面 | 能做什么 |
| --- | --- |
| 概览 | 记忆/会话/候选/项目计数（按当前项目作用域统计）、上次备份时间/份数/大小、最近记忆（点标题进详情）、store 与数据库路径 |
| 搜索 | 关键词搜索；可切换“全部项目”或指定项目，填 `since/until` 时间范围（与 CLI 同语义），结果每页 20 条并带上一页/下一页，命中词高亮；点进详情看正文与历史 |
| 记忆详情 | title/类型/项目/时间、快照摘要、正文、摘要、历史版本、归档/恢复，以及带 CAS 的「编辑正文」（标题/摘要/正文 + 必填理由，保存后旧版本仍在历史里） |
| 相似 | 只读扫描当前范围词法相近的活跃记忆：显示相似度、成员与建议目标（正文最长），阈值可选 0.50–0.80；「全部项目」只聚合已注册身份。点「生成精炼草稿」进入可编辑正文，草稿只追加来源独有的句子；保存时校验快照摘要（CAS），并把来源归档到底层，仍可在「已归档」恢复 |
| 候选 | 看 AI 提交的候选（含类别/来源/到期/项目）；勾选后填一次理由即可**批量批准/拒绝**，每条仍单独校验快照摘要；批准会写正式知识并记审计。只读模式下仍显示候选表，只是不提供勾选与按钮 |
| 备份 | 列出知识库与进度库的备份文件（时间/大小），可按需计算 SHA256 与收据核对；只读，恢复仍按本文第 7 节手工流程 |
| 冲突 | 列出待处理的重复/冲突关系（来源与目标标题、状态）；只读，判定与合并仍走 `conflicts` CLI，UI 不自动改旧记忆 |
| 已归档 | 列出已归档的记忆，点标题进详情可恢复；选择「全部项目」时按项目分组（含历史项目名）；只读 |
| 项目 | 显示当前目录解析到哪个项目、是否已绑定，以及所有注册身份、记忆数、绑定目录与状态，可一键「切换到此项目」（只读）；绑定/解绑仍用 CLI |
| 诊断 | 跑 `doctor` 十项检查；默认只显示非 ok 的问题，点「显示全部」看通过项；原因与下一步保留原文 |

写操作（批准/拒绝/归档/恢复/编辑正文）都要求页面内提交的 CSRF token，并在写入前校验快照摘要（CAS），所以过期页面提交会被拒绝而不是覆盖新状态；编辑保存会生成新修订，旧版本仍在历史里。**删除、绑定/解绑项目这类不可逆或身份级操作故意不放进 UI**，仍走 CLI。

## 3.6 终端界面（`recalldock tui`）

不想开浏览器时，终端里也有一个可用的操作台（需要交互式终端；脚本里用 CLI）：

```sh
recalldock tui
```

- **概览**：固定顶栏（品牌、版本、当前作用域）+ 统计卡片 + 项目作用域行 + 动作菜单；菜单行带说明，选中项整行高亮；
- **搜索**（`s`）：按当前作用域搜索，结果三行式（元信息/标题/摘要），回车进详情，`c` 复制正文、`t` 时间线、`r` 重跑；详情页正文按终端宽度换行，长标题自动折行；
- **最近观察 / 会话浏览**：`j/k` 移动、`PgUp/PgDn` 翻页、`g/G` 跳首尾、回车进详情；
- **相似组**：菜单选「Similar memories」，`j/k` 选组、`enter` 打开建议目标、`[`/`]` 放宽/收紧阈值（0.40–0.90）、`r` 重扫；只读，精炼草稿在网页界面里生成；
- **候选审核**：菜单选「Review candidates」，`a` 批准 / `r` 拒绝、`esc` 取消；必须输入理由，提交时用候选快照摘要做 CAS，过期候选会失败而不会误改；选中候选会显示类别、项目、到期、快照摘要与完整陈述；
- **诊断**：菜单选「Run doctor」，默认只列非 ok 的检查，`a` 切换显示全部、`r` 重跑；原因与下一步按宽度换行；
- **关于/快捷键**：菜单选「About & shortcuts」，只读显示版本、数据库路径、Store ID、常用命令与快捷键（替代旧的“Setup agent plugin”——宿主插件安装只走发布包与 `integrations/`，TUI 不再写宿主配置）；
- **项目切换**（`p`）：在「全部项目 / 当前目录解析到的项目 / 其他注册项目」之间轮换，任意列表/详情页都可用，所有页面按该作用域查询；
- 终端小于约 58×16 时显示最小尺寸提示，而不是截断布局；颜色适配深色和浅色终端（设置 `NO_COLOR=1` 时自动降级为单色）；`q` 返回/退出。

## 4. 候选：Agent 提议，可信策略准入，异常人工审核

模型通过 MCP 的 `mem_commit_session` 提交有界候选；符合操作人预设策略的候选由本地内核自动正式入库，不要求逐条人工批准。模型自报字段不授予直接写入权限。

```sh
recalldock candidates policy --all --mode tiered

recalldock candidates list --all --limit 5      # 收件箱（含过期时间、快照摘要、session_origin）
recalldock candidates show cand-74130b38        # 选中候选的陈述与证据声明
recalldock candidates approve cand-74130b38 \
  --expected 75532e31b974d6aa... --reason "已核对来源"
recalldock candidates reject  cand-74130b38 \
  --expected 75532e31b974d6aa... --reason "推断，无证据"
```

四种准入模式：

| 模式 | 行为 |
| --- | --- |
| `manual` | 全部进 review，等你批 |
| `duplicates` | 仅完全相同的现存事实自动闭环，其余人审 |
| `checked` | 只核验**来源/状态**（同项目当前观察快照或已登记文件 hash）都通过才自动准入；**不证明陈述为真** |
| `tiered` | 持久类别 decision / procedure / project_constraint / failure_avoidance / verified_outcome 且达到已配置模式的资格门槛（现有实现0.85），并且**每个本地来源核验通过**才自动准入。任何高置信度都不能豁免缺失、过期、归档、跨项目或混合未验证证据；无来源、低置信、其他类别及带 topic 的更新留 review。完全相同事实去重，不静默覆盖 |

策略文件由操作人预配置，模型不能切换策略。0.3.22设置 `tiered` 会写版本4 / `tiered-v4`，不支持该模式的旧运行时保留待审；开发不会修改生产策略。现有0.85只作为显式tiered模式的资格信号，本轮19个隔离案例覆盖其边界，不能据此宣称已校准生产事实准确率或将其改成全局自动默认。

完全相同的已有记忆若已归档或来源失效，不作为可用的重复项自动晋升；只是“到期待复核”不会让记忆失效。文件证据关联的登记记忆同样只排除归档/失效，不排除复核到期。`checked` 和 `tiered` 均将已核验来源依赖和核验审计与准入结果放在同一事务中。

复核 TTL 是提醒，不是删除期限：到期候选保留正文和证据，可用 `candidates revive` 续期。候选提交仅补扫其所属项目；操作人显式 `candidates sweep --all` 才补跑全库自动准入，`candidates list --status expired|all` 可查看保留项。`review_after` 到期不等于来源失效；默认召回仍排除真正stale或归档项。关于自动准入即时结果 `results[].reason = "evidence_not_verified"` 与持久化审核状态 `rejection_reason = "operator_review_required"` 的区别，见 [GATEWAY-AUTH.md](GATEWAY-AUTH.md#候选准入即时结果与持久化审核状态)。

本地文件证据自动核验按平台区分实现与已验证边界（文件内容核验仅反映读取时的状态）：
- **Linux（`candidate_file_open_linux.go`）**：通过 `openat2`（`RESOLVE_NO_SYMLINKS` / `RESOLVE_BENEATH` / `RESOLVE_NO_XDEV`）逐段锚定绑定根，拒绝符号链接、跨挂载点及含 `.git` / `.recalldock` 声明的子目录；包含专门的目录替换与跨挂载单元测试及端到端验证。
- **Windows（`candidate_file_open_windows.go`）**：已实现原生 Win32 保护，拒绝 UNC 路径、重解析点/符号链接/junction、跨卷及含 `.git` / `.recalldock` 的子目录，并校验 `root.json` 与卷序列号/文件索引；`checked` / `tiered` 正常及非法文件证据 Go 单元测试已在 `windows-latest` CI 通过，但**Windows 专属并发目录替换压测与真实桌面 Windows 交互式 Agent 自动准入端到端流程仍未验证**。
- **其他平台（`candidate_file_open_other.go`）**：未提供受保护打开实现，所有文件证据候选统一保留人工审核。

---

## 5. 项目身份与目录绑定（最容易踩坑的地方）

### 5.1 概念

- **项目身份**：`prj-<32 hex>`，全局唯一，可绑定多个目录。
- **目录绑定**：在目录写 `.recalldock/root.json`，记录 store/project/binding 和目录身份（device+inode）。
- **历史激活**：只读选择一个历史项目来查，不绑定、不加载全部记忆。

### 5.2 常用命令

```sh
recalldock projects create --name NAME
recalldock projects adopt --name LEGACY_NAME          # 旧名称桶登记为身份；不绑目录、不猜
recalldock projects bind PROJECT_ID --directory /abs/path
recalldock projects current --directory /abs/path --json
recalldock projects registry                 # 注册身份（含无数据的空项目）；JSON stdout，口径提示 stderr
recalldock projects list --json              # 有会话/非删除观测/提示的项目；JSON 含注册名、ID和各口径计数
recalldock projects bindings PROJECT_ID      # 绑定元数据（含已退役）
recalldock projects activate PROJECT_ID --token-budget 8000   # 只读选择器
recalldock projects budget READ_BUDGET_ID
recalldock projects unbind PROJECT_ID --directory /abs/path --binding bind-xxx
recalldock projects consolidate --all --dry-run          # 合并前先看
recalldock projects consolidate --mapping map.json --apply
recalldock projects delete --project ID --confirm         # 删除从未绑定、无数据的空身份
recalldock projects prune --all --dry-run                # 全库空壳键预览（谨慎）
```

`projects adopt` 只登记已经存在的名称桶（例如历史遗留的 `ros_ws`），不创建目录绑定；之后 stats/context/search 都按新 ID 读写，不再出现字符串 key 与注册 ID 两套。没有观测或会话的名称不会创建身份。

`map.json` 必须带 `version: 1` 和当前库的 `store_id`（`recalldock store current --json` 里拿），否则拒绝执行：

```json
{
  "version": 1,
  "store_id": "store-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
  "mappings": [{"source": "old-name-key", "target": "prj-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}]
}
```

### 5.2.1 检测提示 ≠ 可写作用域（`scope_state`）

`projects current` / `store current` 的 JSON 现在带 `scope_state`，终端输出也多一行 `Scope:`：

| scope_state | 含义 | 读 | 写/模型会话 |
| --- | --- | --- | --- |
| `bound` | 命中注册绑定（或已绑定主检出的 worktree） | 有 | 允许 |
| `configured` | 目录内有 `.recalldock/config.json` | 有 | 允许 |
| `hint_only` | 只从 git 仓库名/remote 推断出项目名 | 空范围 | 拒绝，需绑定或加 config |
| `unbound` | 完全没有项目线索 | 空范围 | 拒绝 |

也就是说，`Project: child (git_remote)` 只说明“看起来像 child”，不代表能读写；这是为了避免按目录名悄悄读写另一个同名项目的历史。子模块/嵌套仓库同理：**子模块里的 git remote 优先，父仓库的绑定不会外溢**；要用子模块自己的记忆，就在子模块目录里 `projects create` + `bind`。

### 5.3 绑定规则（0.3.1 起）

- **模型侧会话必须先绑定**：目录没有绑定、没有 `.recalldock/config.json`、也不属于已绑定主检出的 worktree 时，`mem_session_start` / `POST /sessions` 返回 `project_binding_required`，不会按目录名造身份。
- **linked worktree 自动继承**：`git worktree add` 出来的目录如果没有自己的标记，会复用主检出的项目身份（`source=worktree_main`），路径仍记 worktree 本身。
- **操作人 CLI 和宿主 hook** 不受此限制：hook 首次运行会建新身份并写标记。

绑定失败时的标准修法：

```sh
recalldock projects create --name NAME
recalldock projects bind prj-... --directory /abs/path
# 再重试会话/写入
```

---

### 5.3 检索分数与空结果

- 直接词法命中使用 SQLite FTS5 `bm25()`。中文连续串编码为完整双字短语，不把碎片分别当作 OR 备选；独立概念请用空格分开。`all`（默认）要求所有有效词匹配，层级补充也不得绕过；`any` 仅在完整空格分组之间做 OR，同一混合词内的中文/ASCII 部分仍做 AND，例如 `kicker 不存在的词xyz` 是 `kicker OR (不存在的词 AND xyz)`。`query_groups` 和 `query_terms` 明示分组；无需外部分词依赖。
- 独立的 `的/了/是/在/不/和` 从有效词中排除并在 `ignored_terms` 返回；嵌入“不允许”的否定字不删除。只含停用词时返回 `query_has_no_terms`，不执行无约束召回。raw FTS 直接使用原生平面查询并报告 `raw_fts_flat_only` 回退，保留 prefix/NOT 语义。
- `score_source=fts5_bm25` 的 `score` 是当前查询候选内归一化的排序值（0.5–1），不是匹配词比例、概率或跨查询阈值；直接 FTS 命中另外返回 `raw_bm25`（SQLite 原始负值，越小越好），该值仍随查询词频/长度变化，并非统一概率阈值。未支持的 `score_threshold` 会被拒绝。精确 topic_key 命中标记 `topic_key_exact`，历史 ASCII 连接符子串回退标记 `substring_match`；两者不是 BM25。层级补充使用 `hierarchy_keyword_coverage`（0–0.25），仅节点命中使用 `node_match`（0–0.1）；直接命中始终优先。BM25 同分依次按 pinned、最近召回、创建时间倒序，最后按 id 倒序稳定决胜。
- `token_budget` 按完整序列化 `SearchResponse` 估算，包括字段名、ID、排序/预算元数据和所选文本，不再只累计正文；L1 不足会降到 L0，连最小含元数据响应也装不下时报 `budget_too_small`。CJK/其他非 ASCII rune 至少按 1 个 estimated token 计，仍不是实际模型 tokenizer，也不计量宿主完整上下文。MCP/HTTP `ReadBudget` 在传输层继续按完整包络加 128 的开销估算；有界检索和传输预算是两层检查。
- `token_budget` 装不下任何一条时返回 `budget_too_small` 错误。`detail=l1` 因预算改成 L0 时，结果包含 `detail_downgraded:{requested:"l1",applied:"l0",reason:"budget"}` 且 `truncated=true`；`selected_detail` 表示实际交付，`selected_content` 包含该层内容。profile 不会因预算扩大；strategy 的 auto 回退继续用 `fallback_reason` 明示。`mem_context` 是软预算：至少返回 1 条上下文，并带 `truncated=true, budget_limited=true`，`empty_reason` 区分 `no_data` / `budget_limited`。
- `recall_summary` 包含 `flat`、`hierarchy_added`、`budget_dropped`（token 预算丢弃）、`limit_dropped`（结果数上限）、`score_ties`（候选中参与并列的条目数）及 `tied_at_cutoff`（并列组被截断时该组总数）。统计只覆盖本次候选，不代表全库计数；CLI JSON 与 MCP 都保留这些字段。`empty_reason` 区分 `no_match`、`filtered_out`、`path_out_of_scope`。`explain_empty=true`（CLI `--explain-empty`）才额外提供 `empty_explanation`：同一项目内最多20条的明确有界采样，按 root/type/scope/profile/time/stale 给出 input/dropped/remaining，放宽建议只有实际试探找到记录才返回，不自动放宽原查询。
- 空结果可返回结构化 `did_you_mean:{query,id}`：对3–32字符 ASCII 词做一次插入/删除/替换的词汇表探针，最多处理4个有效分词、每词64个候选；保留原混合词分组，并按原项目、类型、scope、profile、root、时间、stale 验证候选。它不自动更正，不交付候选全文，不是语义检索；旧的“标题字符串/放宽 any”提示改到 `empty_explanation.relaxations`。
- 所有 MCP 工具的 inputSchema 设置 `additionalProperties:false`，在项目守卫、权限和读预算之前执行同一 schema 校验；未知键、错误类型、必填项缺失、小数/非有限/不可安全表示的整数均报 `invalid_arguments`。`mem_context` 不接受 `limit/detail/strategy/max_depth`；用 `mode/token_budget`。`mem_get_observation.id` 必须为正整数，不截断小数。合法的 operator-only action 仍按权限拒绝；拼错 action 先报参数错。
- `mem_search` 的 `match_mode/profile/strategy/detail` 均声明枚举，在项目选择之前统一报结构化 `invalid_arguments`；message 不重复相同的 error_code。CLI 同样在绑定判定前验证 scope、检索枚举、root_path 和时间；未绑定的合法查询仍返回空范围，不为验证自定义 type 隐式查询其他项目，需显式 `--project/--all`。
- `timeline`、`conflicts show/stats/deferred` 支持 `--json`，`candidates` 和 `projects registry` 的 JSON 默认输出接受显式 `--json`。timeline 的 before/after 为0..50；未知选项和非法数字不再默默忽略。dry-run 到 cap 后提示提高 `--max-insert`，不声称可以接着上次进度续跑；apply 才可基于已写关系重跑。`--max-insert` 同时限制预览的 would_insert 数量。
- root/stale/time 过滤先于候选上限；召回计数只记录实际交付的结果，空结果探针不计数，MCP 在响应预算拒绝时不计数，显式共享预算完全耗尽时直接拒绝，不执行检索。
- `review_after` 到期只是复核提示，不会把记忆从 context 排除；真正失效用 `stale_at`。
- `mode=full` 是 legacy 格式：会话行现在保留 `session_id/project/directory`，但不接受 `token_budget`；新代码优先用 `index` / `l0` / `l1`。
- 中文标题的 `topic_key` 保留 Han 字符（最多 24 个），例如 `topic/测试主题键建议`，不再全部落到 `topic/general`。
- FTS 语法默认按字面文本处理；内部引号在 all/any/候选扫描中统一转义。需要 `NEAR/OR/*/()` 时显式 `--raw-fts`（MCP `raw_fts: true`）；raw 语法/未知列错误报 `invalid_query`，真实检索故障报 `search_failed`，驱动细节只留服务端日志。`mem_search.limit` 为正整数，超出20返回 `limit_clamped`；非法数值报 `invalid_arguments`。
- `review list [--project PROJECT | --all] [--status active|stale] [--limit 1..20] [--json]` 严格拒绝未知/重复选项和缺值。`review list/dependencies/resource-dependencies` 默认当前绑定/配置项目，未绑定返回空范围，全库读取必须显式 `--all`；`review process --all` 才执行全库失效队列处理。`review mark/link` 等写操作要求当前目录匹配目标项目。`stale` 指 `stale_at` 非空，`active` 指未失效但已到复核时间的记忆，状态过滤先于 limit。模型 `mem_review(action=list)` 使用所选项目，不默认查全库；`mem_stats` 需要选择额外目录工具并同样遵循默认项目。
- `conflicts list --json` 与 `projects list --json` 返回真正 JSON。`conflicts scan --json` 报 `inspected`（成功检查数）、`failed`、`relation_errors`、`capped`；任何数据库失败显式非零退出，应用可能已部分写入，先检查报告再重试。归档记录不参与扫描；`--dry-run/--apply` 互斥，预览和应用共享 `--max-insert` 上限。`projects registry` 统计注册身份，`projects list` 统计有会话/观测/提示的项目，`stats` 的 Projects 仅统计所选范围内非删除观测（含归档），三者明确口径而非强行统一数字。

### 5.4 候选复核提示

候选过期是复核 SLA，不自动丢弃或归档。`doctor` 的 `candidate_inbox` 按所选项目统计真实 `pending_total`，分别报告 `overdue_total`、未来 `due_within_24h` 和 `oldest_review_due`；不再把全库数量或过期记录算进当前项目的未来到期量。可用 `recalldock doctor --silence-warning candidate_inbox --json`，或操作人设置逗号分隔的 `RECALLDOCK_DOCTOR_SILENCE_WARNINGS=candidate_inbox`；JSON 保留原 finding、`silenced:true` 与 `summary.silenced_warnings`，候选状态不改，error/blocked 不能被静音，未知检查名报错。默认不静音。

## 6. 进度账本：`work`

独立 `operations.db`，按**绝对 root + 精确 lane** 存。适合记录“做到哪了、下一步、卡在哪”，不写长期知识或完整对话。下面的恢复读面为 **`0.3.20`+ 接口**，实际生效版本以 `recalldock version` 为准。

```sh
# 默认最近提交在前、隐藏 archived；limit 是条数上限，仍受 L0 预算约束
recalldock work find --root /path/to/my-app --limit 20
recalldock work find --root /path/to/my-app --query 26/26 --limit 20
recalldock work find --root /path/to/my-app --order lane --status all --limit 20

# 读任务摘要（默认 L1 ≤2 KiB），必要时选择 L2 或不可变 generation
recalldock work status --root /path/to/my-app --lane my-task
recalldock work read --root /path/to/my-app --lane my-task --mode l2 --generation 1

# 新任务 expected=0；后续写入使用刚读到的 generation
recalldock work checkpoint --root /path/to/my-app --lane my-task \
  --expected 0 --input /tmp/cp.json
recalldock work append --root /path/to/my-app --lane my-task \
  --expected 1 --input /tmp/event.json

# 时间线返回 L0；事件须选择实际追加事件的 generation，并使用 L2
recalldock work history --root /path/to/my-app --lane my-task --limit 20
recalldock work history --root /path/to/my-app --lane my-task --before 2 --limit 20
recalldock work read --root /path/to/my-app --lane my-task --generation 2 --mode l2 --event
recalldock work backup
recalldock work doctor
```

- `find --query` 匹配 goal/lane、当前 checkpoint 的 summary/details/next/blockers，以及该 lane 事件的 summary/details；L0 只返回索引，不泄露命中的正文。
- `--order recent`（默认）按最新 checkpoint 的提交顺序倒序，不代表宿主仍活跃；`--order lane` 为 lane 字典序。`--status active|blocked|completed|archived|all` 可精确筛选；不传 status 只排除 archived，仍包含 blocked/completed。
- 页内 `data.has_more` 明示是否还有结果，`data.total` 是同 root/query/status 的匹配总数，不是本页条数；history 的 total 是该 lane 的历史检查点总数，不是最大 generation。`limit` 为 1..20，L0 整体 ≤2 KiB；预算缩页仍返回可续的 `data.after`（find）或 `data.before`（history）。只有 `has_more=true` 时继续，沿原 query/order/status 复用返回游标，不按 lane 或时间猜游标。
- CAS 冲突包含 `expected=N current=N`，先重读再决定写入；不存在的 lane 提示 `work_find`，缺 root 明确报路径不存在。读取与非法参数不会初始化数据库。

`cp.json`（字段有界；summary/details 与响应整体预算一起校验）：

```json
{
  "goal": "一句话目标（单行 ≤300 字节）",
  "status": "active",
  "summary": "本批做了什么、验证了什么",
  "next": ["下一步 1", "下一步 2", "下一步 3"],
  "details": "按需读取的补充细节"
}
```

`append` 的输入是一个**包装对象**（事件 + 可选 patch），不是裸事件：

```json
{
  "event": {
    "id": "evt-20261120-1",
    "input_fingerprint": "input-v1",
    "kind": "attempt",
    "status": "started",
    "summary": "开始定向验证，结果尚未确认",
    "details": "复现配置或证据路径，不粘贴原始日志"
  },
  "patch": {
    "summary": "更新后的恢复摘要",
    "next": ["检查验证结果"]
  }
}
```

同一 root/lane 下，**同 id、同 input_fingerprint、同完整 event + patch** 的重试不新增 generation，返回 `replayed:true`；不同 id 即使 fingerprint 相同也不是同一次事件。相同 id 不同 fingerprint 被拒绝；改变事件状态或 patch 是新写入，不是重放。`verified` 要求非空 evidence，但 evidence 是调用方报告的证据引用，不等于系统替它核实了事实。

MCP 使用同一读面，内置工具直接调用，不走 `tools_call`：

```text
work_find {"root":"/absolute/project","limit":20}
work_read {"root":"/absolute/project","lane":"my-task"}
work_read {"root":"/absolute/project","lane":"my-task","view":"history","limit":20}
work_read {"root":"/absolute/project","lane":"my-task","view":"event","generation":2,"mode":"l2"}
```

`work_read` 默认 `view=checkpoint`/L1；`view=history` 只用 before/limit、返回 L0，不接受 generation 或 `mode=l1|l2`；`view=event` 必须 generation≥1 且 `mode=l2`，读回 event 的 summary/details/kind 和 patch。检查点 L2 不会自动附上所有事件，先用 history 找到需要的 generation。

`0.3.20` 写入可返回 `writer.host/host_session_id/gateway_session_id`，这些是可信进程记录的来源，不是模型输入字段。普通 gateway 仅能确认自己的实例（及操作人选择的 host）；未绑定的宿主 session、旧记录缺 writer 时明确视为未知。最新 writer 也不等于“谁正在写”，不按 updated_at 猜会话；宿主绑定限制见第 9 节与 [HOST-ADAPTERS.md](HOST-ADAPTERS.md)。

---

## 7. 备份与恢复

### 7.1 备份

```sh
recalldock backup --json        # 长期知识库，在线一致快照 + integrity ok
recalldock work backup          # 进度库
```

输出示例：

```json
{"path":"~/.recalldock/backups/recalldock-backup-20261102T133208Z-1304351244.db",
 "store_id":"store-…","bytes":3641344,"sha256":"86f82cba…","integrity":"ok"}
```

### 7.2 恢复（谨慎）

1. 先停掉会写库的进程（`serve`、MCP、hook）。
2. 把当前库另存一份，再替换：

```sh
cp ~/.recalldock/recalldock.db ~/.recalldock/recalldock.db.before-restore
cp /path/to/recalldock-backup-XXXX.db ~/.recalldock/recalldock.db
rm -f ~/.recalldock/recalldock.db-wal ~/.recalldock/recalldock.db-shm
recalldock doctor --json | jq '.status'
```

3. 不要用旧 WAL 配新库；也不要在同一 store 的两个分叉副本上同时写。

---

## 8. 导出、交换与 Obsidian

```sh
recalldock export backup.json            # 全量 JSON（操作人）；只接受一个输出文件名
recalldock export --project prj-xxx one.json   # 只导出单个项目（当前已支持）
recalldock import backup.json            # 显式导入，不会自动发现/迁移旧库
# 重复导入是幂等的：相同 ID 不覆盖，输出会说明没有新增行

recalldock sync --status                 # 本地 chunks
recalldock sync --import
recalldock sync --project prj-xxx

# Obsidian 投影（单向：SQLite → Markdown；保护你的手改）
recalldock obsidian-export --vault "/path/to/obsidian-vault"
recalldock obsidian-export --vault /path/vault --project prj-xxx
recalldock obsidian-export --vault /path/vault --attachment-root /abs/dir --watch --interval 1m
# --watch 最小间隔 1m；导出会保护手改文件

# 文件资源身份（把记忆绑定到具体文件内容）
recalldock resources register --project prj-xxx --observation-id 42 --file /abs/file
recalldock resources check --file /abs/file --project prj-xxx
# 结果：unchanged | changed | missing | unregistered
```

Obsidian 投影的规矩：只生成 `vault/recalldock/...`；带 hash 的附件才复制；你手改过的文件不会被覆盖（`--force` 不是覆盖手改的授权）。

---

## 9. 给 AI / 宿主用

### 9.1 直接 stdio MCP（模型工具面）

```sh
recalldock mcp --tools=agent --project prj-xxx      # 长驻进程，推荐显式项目
recalldock mcp --tools=agent --require-project      # 未绑定直接失败
```

模型能做什么：

- 读：`mem_search`、`mem_context`、`mem_get_observation`、`mem_doctor`、`mem_current_project`、`mem_suggest_topic_key`
- 生命周期元数据：`mem_session_start` / `mem_session_end`（不能写带正文的 summary）
- 提议：`mem_commit_session`（进候选，等你批）
- `mem_review` 只有 `action=list` 可用

默认 `agent` 目录共 10 个工具；`admin` 只注册 `mem_stats/mem_timeline` 两个只读诊断工具，不是 agent 的默认组成部分；`all` 注册 23 个工具但不授予写权限，也可用 `--tools=agent,admin` 显式合并目录。`mem_get_observation/mem_timeline` 即使给出 ID，也必须有明确项目、已绑定 cwd 或匹配的会话作用域；未选项目不能按任意 ID 读取，跨项目必须显式 `all_projects:true`，且不能覆盖进程 pin。

模型**不能**做：`mem_save/update/delete/pin/save_prompt/capture_passive/judge/compare/merge`、任何审批动作。模型自报的 approval/confidence/digest 不构成权限。

### 9.2 七工具网关（本机三宿主实际在用）

```sh
recalldock gateway serve --host codex|claude-code|dsh
```

固定七个工具：`tools_find`、`tools_describe`、`tools_call`、`tools_release`、`work_find`、`work_read`、`work_checkpoint`。

**`0.3.20` 接口修订**：`tools_find` 的三个结果分类独立分页：servers 使用 `after/has_more`，后端 tools 使用 `tool_after/tool_has_more`，内置 builtin_tools 使用 `builtin_after/builtin_has_more`。沿原 query 复用对应分类返回的游标，可单独推进需要的分类，不互换或自行构造游标；每次整体仍 ≤2 KiB，预算缩页也保留续页信息；转义膨胀导致最小组合仍超预算时进一步缩短摘要并返回 `summaries_truncated:true`，名称和游标不截断，完整定义按名称 describe。内置七个短摘要由下述 describe 一屏返回，精确 schema 仍按名称选择：

```text
tools_find query=work
tools_describe server=recalldock_gateway
tools_describe server=recalldock_gateway tool=work_read
```

- 目录查询不启动后端；选定后才加载 schema。
- 默认 `--trust agent`：网关由本机宿主启动，已登记后端工具不要求逐次 grant；模型仍不能自报 approval/session，也不能绕过 catalog/schema 校验。需要最小权限时用 `--trust strict`，恢复逐工具、根目录、会话、参数摘要和过期时间校验。
- 两种模式都不是跨用户隔离；agent 是操作人对“本机宿主代理调用”的一次性委托。

```sh
recalldock gateway list                 # 查看 trust 与 grants
recalldock gateway check
recalldock gateway doctor               # 对比 grants 与 tool_catalog，列出 schema 失配
recalldock gateway discover --server ID
# strict 模式才需要的精确授权：
recalldock gateway serve --host dsh --trust strict
recalldock gateway grant --server ID --tool NAME --root /abs/path --read-only
recalldock gateway grant --server ID --tool NAME --root /abs/path --session <session> --expires 2026-12-01T00:00:00+08:00 --arguments /abs/args.json
recalldock gateway revoke --server ID --tool NAME
```

开发版中 schema 变化不会自动升级旧 read-only grant：strict 模式须先 discover/核对新定义，再由操作人显式重新 grant；模型的 confirmation/session/author 字段不能授权，agent trust 的既有委托规则不变。

宿主生命周期在开发版同步登记 `operations.db` 的 host_sessions；已有真实活动 ID 可由操作人显式绑定到新 gateway 实例：

```sh
# REAL_REGISTERED_ID 来自宿主真实生命周期，不是模型挑选的 ID
recalldock gateway serve --host claude-code --host-session REAL_REGISTERED_ID
```

它校验登记的 host/活动状态及后续 work 写入的 root。普通宿主配置只传 `--host`，尚未自动传入真实 session；不能声称三宿主已自动贯通，也不把 writer 来源标识当跨用户认证或写者锁。

### 9.3 宿主怎么接

| 宿主 | 配置文件 | 期望内容 |
| --- | --- | --- |
| 共享 CLI | `~/.local/bin/recalldock` | 指向 `releases/0.3.19/runtime/recalldock` |
| Codex | `~/.codex/config.toml` → `[mcp_servers."recalldock_gateway"]` | `command=…/0.3.19/runtime/recalldock`，`args=["gateway","serve","--host","codex"]` |
| Claude Code | 原生 `~/.local/bin/claude` symlink + RecallDock 插件 | 普通 `claude` 由原生配置/插件加载；`recalldock-claude` 显式加载 `~/.recalldock/claude-gateway.json` 并保留严格 MCP 模式 |
| DSH | `~/.dsh/cordis.patch.yml` 的 `# BEGIN/END RECALLDOCK GATEWAY` 管理块 | 同一个 gateway 命令（`--host dsh`） |

### 9.4 复用宿主，不重复开发宿主

RecallDock 的目的仍是本地选择性持久记忆、可验证项目边界与跨宿主恢复，不是另一个 MCP 客户端、版本管理器或自动压缩器。Claude 官方已提供 ToolSearch 按需 schema 加载、原生插件/MCP 与 launcher 管理；Codex 原生配置/插件和 DSH 持久 patch 负责接入；认证和自动记忆的宿主能力优先复用，不声称我们的实现更优。RecallDock 的候选准入、精确 root 绑定、有界读取与独立工作 CAS 账本才是保留职责，gateway 只作跨宿主可选适配，不再为了工具发现重做宿主。

Claude 普通入口恢复由官方 installer 管理的 symlink，不能用同名脚本覆盖：自定义入口会让 updater 保留所有旧版本。`recalldock-claude` 是显式严格入口；普通 `claude` 仍经插件接 RecallDock，但可同时加载其他 MCP，这是本轮确认的选择，不是静默扩大。既有版本不批量删除，后续清理由官方管理。Codex 已是原生 symlink；DSH 的 Node/npx 解析器属于宿主安装选择，不承担 gateway 注入，保留原有桌面/CLI行为，不在 RecallDock 重写。

### 9.5 只读打开和标识符错误

search/context/stats/doctor/MCP 等普通读取或服务启动要求所选 `recalldock.db` 已存在；打错 `RECALLDOCK_DATA_DIR` 报 `store_not_initialized`，不创建目录/空库。新库用 `RECALLDOCK_DATA_DIR=/abs/path recalldock store init` 显式初始化；操作人 save/import/projects create 与可信 host-event 的已有初始化行为保留。显式 `--project` 统一核验已知项目，未知值报 `unknown_project`，合法空身份可读取为空；不猜目录归属。scope 为 project/personal/历史 global，非法值报错；内建 type 或当前作用域已存的自定义 type 可筛选，未知 type 报错，不能按当前库的九种样例硬裁历史类型。

查询执行前用已有 x/text 的 NFKC 统一全角/半角；原有普通/原始 FTS边界仍生效。CLI查询回显和非法标识符最多100字符，JSON明示 `query_truncated`；执行原查询，不因回显截断改变检索。写锁超时明确提示数据库锁和重试，不说成迁移损坏，不重复 `recalldock:` 前缀；`work --root` 相对值先报绝对路径要求。

DSH 另外一条：`~/.dsh/profiles/web/cordis.patch.yml` 里保留 8 条 `- id: mcp-*` + `disabled: true`，这是让插件市场显示“已停用”而不是“等重启”的关键；不要只把它们放回家目录 patch。

内置进度账本工具也有权威 schema。不要把 `artifacts`/`milestones`/`paths`/`notes` 当字段；进度放在 `checkpoint` 对象里：

```text
tools_describe server=recalldock_gateway tool=work_checkpoint
```

`checkpoint` 接受 `goal`（必填，单行 ≤300 字节）、`summary`、`next`（数组 ≤3）、`details`、`status`（`active|blocked|completed|archived`）、`blockers`、`operations`；顶层只接受 `root/lane/expected/checkpoint/event/patch`。内置工具直接按名称调用，`tools_call` 只用于已登记后端。

### 3.7 合并旧记忆（`memory consolidate`）

同一类坑散成很多条时，先精炼出一条“总规则”，再把旧来源沉下去：

```sh
# 0) 先看哪些记忆可能该合并（只读，不改任何东西）
recalldock memory similar --project prj-xxx --threshold 0.6

# 也可以直接开网页：概览 →「相似」→ 选一组 →「生成精炼草稿」
# 草稿只把来源里目标没有的句子列在「【待人工精炼】」下面；
# 合并进正文后必须删掉整段标记，带着标记保存会被拒绝。
# 保存会校验生成草稿时的目标和来源快照；任一变化都要重新生成草稿。

# 1) 选定/新建一条精炼目标，并用 CAS 更新正文（正文由你或模型起草）
recalldock save "删除只用回收站" "删除一律 gio trash；禁止 rm -rf/--empty；例外：一次性临时目录。" --type decision
recalldock memory update 42 --expected <snapshot> --input /abs/patch.json --reason "合并删除类坑"

# 2) 把旧来源归档到目标（来源离开召回/搜索/复核，但保留可查）
recalldock memory consolidate 42 --source 12 --source 15 --reason "合并重复的删除教训"

# 3) 需要对比时再取来源
recalldock memory sources 42
recalldock memory show 12
```

约束：目标与来源必须同项目同 scope；pinned 来源先 unpin；重复执行幂等；**命令不改写正文**，正文改写必须显式做，避免合并时丢细节。目标继续参与 `review_after` 复核，来源只做“沉淀证据”。

网页草稿的额外规则：置顶、已归档、跨项目/作用域的来源不会进入草稿（会显示跳过原因）；正文没有变化时只校验目标和来源的摘要并归档来源，不产生空修订。正文更新、来源归档及谱系写入同事务；数据库写入失败会回滚，个别来源因保护规则不能归档时会显示原因。草稿超出正文上限时拒绝生成，请分批精炼。自动正文精炼与自动合并仍故意不做。回滚源码时按 Git diff 逐文件恢复；已发生的数据变更需使用升级前的数据库备份恢复，不应由旧版直接覆盖当前数据库。

### 9.4 会话作用域与按需记忆（0.3.22 开发候选）

默认 `SessionStart` hook 调用 `recalldock host-event <host>`；Antigravity 另由可信命令指定 `--event SessionStart`。只复用已有注册根、显式可信配置或 linked worktree 的主检出身份，登记宿主生命周期并报告 project/session/精确 Work root，不自动创建项目或注入知识。

- 普通问答、确认、闲聊和临时测试直接处理，不需要 Work 或记忆记录；没有项目或 Work 记录不阻止普通任务。
- 需要接续已有任务时才按精确 root 查询 Work；有意义的新任务可用 root/lane 与 expected=0 建立首个检查点，不要求已有长期知识项目身份。
- 需要可靠既有知识时，由 Agent 按需调用 `mem_search` / `mem_context`，先看有界索引，再取选定正文；来源失效、未决传播及预算边界沿用现有保护。
- 新持久项目身份须经明确授权的既有注册流程；`--auto-project` 只保留为显式 opt-in，不在默认插件 Hook 中使用。

正式0.3.21仍是原有默认启动索引行为；开发候选和源指引不会修改正式安装或已运行宿主，正式升级另行授权。这里的改动是按任务价值使用工具，不是按某句聊天文本做匹配。

网关也可以按需接记忆后端（agent 模式直接可用；strict 模式需下面的授权）：

```sh
recalldock gateway add --server recalldock-memory --input /abs/memory-server.json
recalldock gateway discover --server recalldock-memory
recalldock gateway grant --server recalldock-memory --tool mem_context --root /path/to/project --read-only
recalldock gateway grant --server recalldock-memory --tool mem_search  --root /path/to/project --read-only
```

agent 模式无需逐工具 grant；strict 模式下 `mem_context` / `mem_search` / `mem_get_observation` 等只读工具可预授权，`mem_commit_session` 等有副作用的调用仍要求每次显式 session + 过期时间 + 参数摘要。

已运行的宿主进程不会热换版本，**新会话/重连**才加载新二进制。

---

## 10. 安装、升级、回滚

### 10.1 从源码构建发布包

```sh
cd /path/to/recalldock
export PATH=$PATH:/usr/local/go/bin

# 正式包：要求工作树干净、版本无后缀
python3 tools/build-release.py --version 0.3.19 --output /abs/new-build-dir

# 开发候选：允许脏树，版本必须带后缀
python3 tools/build-release.py --version 0.3.19-dev.1 --development --output /abs/new-candidate-dir
```

### 10.2 安装（预览 → 应用 → 回滚）

```sh
# 0) 先备份
recalldock backup --json
recalldock work backup

# 1) 把包放到不可变目录（示例版本 0.3.19，换成你的实际版本）
V=0.3.19
mkdir ~/.local/share/recalldock/releases/$V &&
tar -xzf /abs/new-build-dir/recalldock_${V}_linux_amd64.tar.gz \
  -C ~/.local/share/recalldock/releases/$V --strip-components=1

REL=~/.local/share/recalldock/releases/$V

# 2) 预览（不写文件；给你 plan_sha256 与每个文件前后摘要）
"$REL/runtime/recalldock" setup --release "$REL" --home "$HOME" \
  --host codex --host claude-code --host dsh

# 3) 应用（备份目录必须不存在，由安装器创建）
"$REL/runtime/recalldock" setup --release "$REL" --home "$HOME" \
  --host codex --host claude-code \
  --apply --expected-sha256 "<预览里的 plan_sha256>" \
  --backup "$HOME/.local/state/recalldock/backups/$V"

# 4) 幂等检查：再预览一次，files 应为 []
"$REL/runtime/recalldock" setup --release "$REL" --home "$HOME" \
  --host codex --host claude-code

# 5) 回滚（只改回计划里的受管文件，不动数据库）
"$REL/runtime/recalldock" setup --rollback \
  --backup "$HOME/.local/state/recalldock/backups/$V"
```

注意事项：

- 日常安装也可直接使用 `python3 "$REL/tools/install-local.py" --interactive` 一键预览并确认。
- 回滚会拒绝目标文件在应用后被改过的情况；先核对，再恢复。
- 版本化候选准入策略在回滚时保留（不会退回不安全的自动准入）。

### 10.3 发布校验

```sh
grep -o 'releases/[0-9.]*' ~/.recalldock/claude-gateway.json
grep -A1 'recalldock_gateway' ~/.codex/config.toml
grep -o 'releases/[0-9.]*' ~/.dsh/cordis.patch.yml
recalldock --version
recalldock doctor --json | jq '.status'
```

---

## 11. 排错

### 11.1 常见错误码

| 报错 | 含义 | 处理 |
| --- | --- | --- |
| `project_binding_required` | 模型会话所在目录没绑定 | `projects create` + `projects bind` 后重试 |
| `binding_unverified` | 目录解析到的项目和请求的不一致（复制/移动/旧绑定） | 用正确项目重新 bind；不要复制 `.recalldock/root.json` |
| `session_scope_mismatch` | session ID 已属于别的项目/目录 | 换新 session ID；不要复用 |
| `scope_state: hint_only`（不是错误） | 项目名只是从 git remote/仓库名推断，目录未绑定 | 按 5.2.1 绑定或写 `.recalldock/config.json`；读返回空范围是预期 |
| `project_not_selected` | 读写没有显式项目 | `--project`、`projects activate`，或在项目目录内操作 |
| `read_budget_exhausted` | 读预算用尽/过期 | 等新预算、缩小查询，不要重复同样的调用 |
| `operator_review_required` | 模型调用了模型面不允许的写/审 | 改用 `candidates` 由你审批 |
| `session_not_started` | 候选提交早于会话注册 | 先 `mem_session_start` 或宿主 hook 成功后再提交 |

### 11.2 诊断

```sh
recalldock doctor                       # 10 项检查：会话、同步、分类、收件箱、来源、网络边界等
recalldock doctor --check candidate_inbox
recalldock doctor --check session_origin         # host/operator/model/unknown 分布
recalldock doctor --check network_boundary       # 出站面是否关闭
recalldock doctor --json | jq '.checks[] | select(.result != "ok")'
recalldock doctor repair --project prj-xxx --check CODE --dry-run   # 先看计划
```

### 11.3 特定场景

**搜不到东西**：确认 `pwd` 在绑定目录下，或显式 `--project prj-xxx`；`--all` 才跨项目。

**AI 说没有记忆**：宿主进程可能还是旧版本/旧绑定。先看 `recalldock doctor --check session_origin`，再重启宿主新开会话。

**插件页一直提示重启**：见第 9.3 节，确认 8 条 disable 在 **profile** patch 里。

**写不进去**：`memory update`/`archive`/`reactivate` 需要当前 `snapshot_sha256`（`memory show <id>` 里拿），别人改过就会失败——这是防覆盖，不是 bug。

**数据库被锁**：先确认没有第二个 `serve`；`doctor` 的 `sqlite_lock_contention` 会报告。

---

## 12. 环境变量

| 变量 | 作用 |
| --- | --- |
| `RECALLDOCK_DATA_DIR` | 知识/进度数据目录（默认 `~/.recalldock`）；不改变网关注册表位置 |
| `RECALLDOCK_GATEWAY_REGISTRY` | 网关注册表绝对路径；默认 `~/.recalldock/gateway.json`，显式 `gateway --registry` 优先；换临时库不会改变授权 |
| `RECALLDOCK_PROJECT` | MCP 长驻进程的可信项目覆盖 |
| `RECALLDOCK_EXPECT_STORE_ID` | 期望的 store 身份；不匹配直接失败 |
| `RECALLDOCK_READ_BUDGET_ID` | 可信宿主共享读预算；模型不能重置 |
| `RECALLDOCK_PORT` | 本地 HTTP 端口 |
| `RECALLDOCK_TIMEZONE` | 展示时区 |

旧 `ENGRAM_*`、云和更新检查相关变量在本地方向不会启用出站。

---

## 13. 安全与边界（须知）

- 模型不能直接写正式知识、不能自批候选、不能伪造来源；`evidence_refs` 存在不等于内容为真。
- 宿主身份认证**还没实现**：现在只有来源溯源（host/operator/model/unknown），项目范围检查不等于登录认证。
- 本地方向不等于操作系统级沙箱；网关可以连接你显式登记的后端。
- 不要往记忆里写凭证、密码、完整对话、原始日志；`doctor` 和候选准入都在拦这类内容。
- 本机只对 Linux 验收；Windows/macOS 仅编译通过。

---

## 14. 数据与恢复文件位置

| 内容 | 位置 |
| --- | --- |
| Linux 已安装版本目录 | `~/.local/share/recalldock/releases/` |
| Linux 安装备份目录 | `~/.local/state/recalldock/backups/` |
| Windows 安装目录与备份 | `%LOCALAPPDATA%\RecallDock\bin\`、`%LOCALAPPDATA%\RecallDock\install-backups\` |
| Work 任务进度查询 | `recalldock work find --root /path/to/your/project` |
| 数据库在线备份 | `~/.recalldock/backups/`、`~/.recalldock/operations-backups/`（Windows 位于 `%LOCALAPPDATA%\RecallDock\` 下对应目录） |

---

## 15. 一页速查

```sh
# 身份
recalldock projects create --name X
recalldock projects bind prj-... --directory /abs
recalldock projects current --json
recalldock projects registry

# 界面（本机操作人）
recalldock ui [--port 7438] [--project prj-...] [--open] [--read-only]
recalldock tui

# 记忆
recalldock save "T" "C" --type decision --topic k
recalldock search "q" [--project prj-...|--all] [--since 2026-11-01 --until 2026-11-02] [--json]
recalldock context --mode index
recalldock memory show ID [--full]
recalldock memory update ID --expected SHA --input /abs/patch.json --reason "..."
recalldock memory archive ID --expected SHA --reason "..."
recalldock memory cooldown --project prj-... --min-age 30d --dry-run

# 候选
recalldock candidates list --all
recalldock candidates approve CAND --expected SHA --reason "..."
recalldock candidates policy --all --mode checked

# 进度
recalldock work find --root /abs
recalldock work status --root /abs --lane L
recalldock work checkpoint --root /abs --lane L --expected N --input cp.json

# 维护
recalldock backup --json
recalldock work backup
recalldock doctor --json
recalldock export out.json
recalldock obsidian-export --vault "/path/to/obsidian-vault"
```

遇到本文没覆盖的情况：先跑 `recalldock doctor`，或查阅 [INSTALLATION.md](INSTALLATION.md) / [GATEWAY-MCP.md](GATEWAY-MCP.md)。
