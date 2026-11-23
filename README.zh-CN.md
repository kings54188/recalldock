# RecallDock（忆栈）

**RecallDock（忆栈）—— 为 AI 编程助手提供本地长期记忆与跨会话、跨宿主任务接续。**

[English](README.md) | [简体中文](README.zh-CN.md) | [安装指南](docs/INSTALLATION.md) | [Windows 使用说明](docs/WINDOWS.md) | [MCP Gateway 管理](docs/GATEWAY-MCP.md) | [来源说明](PROVENANCE.md)

RecallDock 是一个本地优先的 Go 工具，帮助 AI 编程助手记住项目里真正重要的决策与经验，并在你新建会话或切换不同 Agent 工具时安全接续未完成任务。全部数据默认保存在你的电脑本地，不依赖云端账号，不采集原始聊天记录，也不会锁定或篡改各宿主官方版本。

---

## 为什么需要它？

AI 编程助手通常每次开新对话都会从零开始：

1. 之前的架构约束、问题排查结论很快被遗忘，用户只能反复重复解释。
2. 零碎进度、临时猜测如果混进长期知识，会让后续搜索越来越不准。
3. 电脑上配了多个 MCP 服务器时，启动会话就把全部工具说明塞给模型，非常占上下文。
4. 从 Codex 切换到 Claude Code 或 Antigravity CLI 时，手头进行到一半的任务难以无缝衔接。

RecallDock 用一个单二进制程序配合两个独立 SQLite 数据库在本地解决这些问题。

---

## 核心功能

- **长期记忆（`recalldock.db`）**：保存经过筛选的项目知识、稳定决策与可复用修复方案，由 Agent 按当前任务需要精准检索。
- **任务接续（Work，`operations.db`）**：按项目目录与任务线保存目标、已验证摘要、下一步与阻塞点；新会话或另一款宿主工具可以从准确检查点继续推进，且普通进度不污染长期记忆。
- **多 Agent 支持**：通过 Codex、Claude Code、Antigravity CLI（`agy`）与 DSH（DeepSeek Harness）各自官方支持的 MCP 及插件/Hook 接口接入，不绑定任何特定模型服务。
- **按需 MCP Gateway（`recalldock gateway serve`）**：
  - 向模型暴露**固定 7 个 Gateway 工具**（`tools_find`、`tools_describe`、`tools_call`、`tools_release`、`work_find`、`work_read`、`work_checkpoint`），只有用到后端工具时才读取对应定义，避免启动即加载所有后端 Schema。
  - **7 是 Gateway 自身暴露的接口数量，不是你可使用的 MCP 服务器数量上限。** Gateway 登记表支持**最多 200 个 MCP 后端**，同时不影响宿主自己直连的 MCP。
  - **不静默接管，权责清晰**：宿主原有直连 MCP 与 Gateway 托管后端严格区分，可通过只读命令 `recalldock gateway scan` 对比查看，再决定是否显式加入 Gateway。详细增删改与权限机制见 [docs/GATEWAY-MCP.md](docs/GATEWAY-MCP.md)。
- **本地安全与隔离**：默认保存在 `~/.recalldock`（Linux）或 `%LOCALAPPDATA%\RecallDock`（Windows）。项目目录需显式绑定才会互通，安装前先预览并保存私有备份，保护既有配置不被擅自覆盖。

---

## 支持的平台与 Agent 兼容现状

当前提供 **Linux (`x86_64` / `amd64`)** 与 **Windows (`x64` / `amd64` 原生，无需 WSL2、无需管理员权限)** 安装包；尚未提供或验证 macOS 与 ARM64 包。

| AI Agent 宿主 | Linux (`amd64`) 实测状态 | Windows x64 (`amd64`) 实测状态 | 说明与当前边界 |
|---|---|---|---|
| **Codex** | **已验证**（核心、MCP、配置、真实原生会话与子代理 Hook） | **配置与协议已验证**（真实 Windows 交互会话尚未实测） | 仅更新 `.codex/config.toml` 的 `[mcp_servers.recalldock_gateway]`，保留其他 MCP 与注释。 |
| **Antigravity CLI (`agy`)** | **已验证**（插件清单、Hook 身份、Gateway 七工具与 Work 接续） | **插件清单与 MCP 配置已核验**（真实 Windows 会话尚未实测） | 通过原生 `agy plugin install` 安装随包插件；本地原生子代理交接尚未验证。 |
| **Claude Code** | **部分已验证**（严格 MCP 启动器、记忆检索、Work 读写；部分原生自动准入与子代理交接未完整验证） | **配置与协议已验证**（真实 Windows 会话尚未实测） | 生成独立 `claude-gateway.json` 与 `recalldock-claude` 启动脚本，保留原有 `claude` 与 `~/.claude.json`。 |
| **DSH** | **核心与工作流已验证（存在宿主会话限制）**（`0.2.2-rc.5` 真实模型已验：原生 7 工具 Gateway、长期记忆检索与读回、Work 自主读写/CAS/写后读回、带真实证据候选提交与 `tiered-v4` 自动晋升及读回、原生 `subagent` (`spawn`) 执行与主代理独立核验、Codex 产物跨 Agent 复核流程） | **Windows 原生不支持 / 未验证** | 经 `~/.dsh/cordis.patch.yml` 受管块接入（`--host dsh`）。宿主限制：无可信原生 `SessionStart` 通道（`host_session_id` 保持未知）、父子代理在 Work writer 层无法可信区分，且无独立错误样本（不能宣称已证明可靠幻觉检出能力）。 |

详见 [docs/HOST-ADAPTERS.md](docs/HOST-ADAPTERS.md)。

---

## 下载与快速安装

> **发布状态说明**：`0.3.24` 为当前正式公开发行版本，Linux 与 Windows 预编译安装包已发布于 [GitHub Releases](https://github.com/kings54188/recalldock/releases/tag/v0.3.24)；也可以按下方步骤从源码直接编译使用。

前往 [GitHub Releases](https://github.com/kings54188/recalldock/releases) 下载对应平台安装包及 `checksums.txt`。

### Linux (`x86_64` / `amd64`)

1. 下载 `recalldock_0.3.24_linux_amd64.tar.gz` 与 `checksums.txt`，校验文件完整性：
   ```sh
   sha256sum -c checksums.txt --ignore-missing
   ```
2. 解压到固定的版本目录（安装后会引用其中运行时，请勿放在临时目录后再删）：
   ```sh
   mkdir -p ~/.local/share/recalldock/releases
   tar -xzf recalldock_0.3.24_linux_amd64.tar.gz -C ~/.local/share/recalldock/releases
   mv ~/.local/share/recalldock/releases/recalldock_0.3.24_linux_amd64 ~/.local/share/recalldock/releases/0.3.24
   ```
3. 运行引导安装命令（自动识别当前发布包路径、HOME 和已安装宿主，展示完整变更摘要和自动备份目录，由你确认 `[y/N]` 后才执行受保护安装）：
   ```sh
   python3 ~/.local/share/recalldock/releases/0.3.24/tools/install-local.py --interactive
   ```
   完成后运行 `recalldock version` 验证。脚本化无交互参数、自定义 `--host`、升级与回滚方式见 [docs/INSTALLATION.md](docs/INSTALLATION.md)。

### Windows (`x64` / `amd64` 原生 —— 无需 WSL2、无需管理员权限)

1. 下载 `recalldock_0.3.24_windows_amd64.zip` 与 `checksums.txt`，在 PowerShell 中与 `checksums.txt` 自动比对 SHA256：
   ```powershell
   $zip = "recalldock_0.3.24_windows_amd64.zip"
   $actual = (Get-FileHash .\$zip -Algorithm SHA256).Hash.ToLower()
   $expected = ((Select-String -Path .\checksums.txt -Pattern "\b$([regex]::Escape($zip))$").Line -split '\s+')[0].ToLower()
   if ($actual -eq $expected) { "OK: SHA256 校验通过 ($actual)" } else { throw "SHA256 不匹配: $actual != $expected" }
   ```
2. 解压到本地目录后运行引导安装命令：
   ```powershell
   powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\tools\install-windows.ps1 -Interactive
   ```
   安装器会先列出要改动的文件与自动备份位置，经你确认 `[y/N]` 后安装 `%LOCALAPPDATA%\RecallDock\bin\recalldock.exe` 并仅更新 RecallDock 受管配置。

> **未签名程序提示**：`recalldock.exe` 目前尚未进行商业代码签名，首次运行可能触发 Windows SmartScreen 提示。请务必只从官方 Release 下载并用 `checksums.txt` 核对 SHA256，切勿为此全局关闭 Defender 或 SmartScreen。PowerShell 5.1/7 及环境变量配置见 [docs/WINDOWS.md](docs/WINDOWS.md)。

### 从源码编译（需要 Go 1.25.10+）

```sh
git clone https://github.com/kings54188/recalldock.git
cd recalldock
go test -count=1 ./...
go build -ldflags='-X main.version=dev' -o /tmp/recalldock ./cmd/recalldock
```

---

## 最简使用示例

### Linux (Bash)

```sh
# 1. 创建演示目录，创建项目并读取真实项目 ID 后完成绑定
PROJECT_DIR="$HOME/recalldock-demo"
mkdir -p "$PROJECT_DIR"
PROJECT_ID=$(recalldock projects create --name "my-app" --json | python3 -c "import json, sys; print(json.load(sys.stdin)['id'])")
recalldock projects bind "$PROJECT_ID" --directory "$PROJECT_DIR"

# 2. 保存并检索长期记忆
cd "$PROJECT_DIR"
recalldock save "变更 Schema 前先备份" "升级前先运行 recalldock backup --json 保存一致快照。" \
  --type decision --topic ops/backup
recalldock search "Schema"

# 3. 查看当前目录对应的 Work 任务进度（存于独立 operations.db）
recalldock work find --root "$PROJECT_DIR"

# 4. 只读扫描宿主已有 MCP 与 Gateway 托管后端的区分状态（绝不输出密钥）
recalldock gateway scan
recalldock gateway list
```

### Windows (PowerShell)

```powershell
# 1. 创建演示目录，创建项目并读取真实项目 ID 后完成绑定
$ProjectDir = Join-Path $env:USERPROFILE "recalldock-demo"
New-Item -ItemType Directory -Force -Path $ProjectDir | Out-Null
$ProjectId = (recalldock projects create --name "my-app" --json | ConvertFrom-Json).id
recalldock projects bind $ProjectId --directory $ProjectDir

# 2. 在 PowerShell 中保存并检索长期记忆
Set-Location $ProjectDir
recalldock save "变更 Schema 前先备份" "升级前先运行 recalldock backup --json 保存一致快照。" `
  --type decision --topic ops/backup
recalldock search "Schema"

# 3. 查看当前目录对应的 Work 任务进度（存于独立 operations.db）
recalldock work find --root $ProjectDir

# 4. 只读扫描宿主已有 MCP 与 Gateway 托管后端的区分状态（绝不输出密钥）
recalldock gateway scan
recalldock gateway list
```

---

## 新增、删除与管理 MCP 服务器

使用 RecallDock 时请区分三个层次：
1. **7 个 Gateway 工具**：面向模型暴露的固定网关接口（`tools_find`、`tools_describe`、`tools_call`、`tools_release`、`work_find`、`work_read`、`work_checkpoint`）。
2. **Gateway 已登记后端（`gateway.json`）**：最多支持 **200 个** MCP 后端，由你通过 `recalldock gateway add` / `discover` / `list` / `remove` 管理。
3. **宿主直连 MCP**：写在 Codex、Claude Code 或 Antigravity 自身配置里的 MCP，保持独立运行，不会未经允许自动并入 Gateway。

向 `gateway.json` 新增或移除后端后，正在运行的 Gateway 进程在 `tools_find` / `tools_call` 时会即时感知并立即生效；而宿主直连 MCP 的改动仍需按各宿主自身机制重开会话加载。完整操作、配置修改失效规则及 `agent` / `strict` 权限区别见 [docs/GATEWAY-MCP.md](docs/GATEWAY-MCP.md)。

---

## 本地存储、备份、升级与卸载

| 平台 | 默认数据目录 | 包含主要文件 | 环境变量覆盖 |
|---|---|---|---|
| **Linux** | `~/.recalldock` | `recalldock.db`、`operations.db`、`gateway.json`、`backups/`、`operations-backups/` | `RECALLDOCK_DATA_DIR`、`RECALLDOCK_GATEWAY_REGISTRY` |
| **Windows** | `%LOCALAPPDATA%\RecallDock` | `recalldock.db`、`operations.db`、`gateway.json`、`backups/`、`operations-backups/`、`bin\recalldock.exe` | `RECALLDOCK_DATA_DIR`、`RECALLDOCK_GATEWAY_REGISTRY` |

> **WSL2 注意事项**：请勿让原生 Windows `recalldock.exe` 与 WSL2 Linux 版 `recalldock` 同时读写同一个 SQLite 数据库文件，两个平台默认目录分离正是为了避免文件锁冲突。

- **升级前先备份双库**：`recalldock backup --json` 与 `recalldock work backup`。
- **受保护回滚与保留数据卸载**：回滚与卸载只清理 RecallDock 程序及受管入口配置，**默认完整保留** `recalldock.db` 与 `operations.db`。若你安装后手工改过配置文件，回滚会先拒绝覆盖以防误删你的修改。详见 [docs/INSTALLATION.md](docs/INSTALLATION.md) 与 [docs/WINDOWS.md](docs/WINDOWS.md)。

---

## 详细文档

- [Linux 安装、升级与回滚指南](docs/INSTALLATION.md)
- [Windows 原生使用说明](docs/WINDOWS.md)
- [MCP Gateway 后端管理指南（增删改、扫描与权限）](docs/GATEWAY-MCP.md)
- [CLI 快速上手与日常操作](docs/LOCAL-QUICKSTART.md)
- [AI Agent 宿主适配与兼容边界](docs/HOST-ADAPTERS.md)
- [安全模型与边界说明](SECURITY.md) | [Gateway 授权机制](docs/GATEWAY-AUTH.md)
- [参与贡献](CONTRIBUTING.md)

---

## 许可证

RecallDock 采用 MIT License 开源，详见 [LICENSE](LICENSE)。
