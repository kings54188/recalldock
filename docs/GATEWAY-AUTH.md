# 网关授权身份与宿主边界

状态：2026-11-22。安装说明与宿主边界见 [INSTALLATION.md](INSTALLATION.md) 与 [HOST-ADAPTERS.md](HOST-ADAPTERS.md)；网关的许可/指纹边界保留在 `strict` 模式，默认 `agent` 模式按宿主委托执行，并保留内置工具 schema、`tools_describe` 内置目录和 `_meta` 转发限制。执行配置指纹 v2 检测执行环境变更，授权过期回归拒绝不可解析或过期的 `expires_at`。本页不宣称可信宿主握手已经实现。

## 0.3.22 按需读取与成本边界

固定七工具和 agent/strict 信任面不变。已知工具精确读取单条缓存 Schema，不启动后端；显式 refresh 才重新发现，正常重复调用复用既有许可与自有 lease。调用仍检查 live Schema；目录变化通知不是绝对静态承诺，本版不引入可能绕过 strict 漂移检查的长期 live Schema 缓存。

隔离对照中，固定定义6,777→6,298 bytes，三个合成后端共96个直接定义59,723 bytes；精确Schema响应仍1,405 bytes，选定结果和两步Work恢复质量保持。tokens 使用内部响应估算，不是宿主账单或Google Prompt Cache节省率。后端的原生text/structuredContent与错误不擅自删除；大结果通过后端自己的分页/过滤控制。Gateway校验自身七工具输入及后端live Schema哈希，不替代任意后端的完整JSON Schema输入验证；后端仍须按官方MCP契约验证工具参数。

RecallDock仅固定自己的发布来源；宿主按当前PATH/官方扩展接口继续更新，不固定宿主版本、不重建用户插件或授权。

## 当前有哪些身份

| 身份 | 来源与用途 | 能证明什么 |
|---|---|---|
| `--host` | 网关启动参数，过滤后端目录与调用 | 启动配置选定的宿主范围；本地程序仍可自行传参 |
| gateway session | `NewServer` 随机生成，不采信工具参数中的 session | 本网关实例的许可范围与 checkpoint `writer.gateway_session_id`；不等于宿主真实聊天 ID |
| `--host-session` | 操作人在启动时显式指定已由 lifecycle 登记的真实 ID | checkpoint 历史写者；每次写入核 host/root/目录身份/未结束，未绑定时宿主 session 未知，不表示实时占用锁 |
| lease | 本进程连接池生成并绑定 server/root/session/config hash | 控制自有连接的复用与释放；不授予业务工具权限 |
| hook session ID | `host-event` 从生命周期 JSON 接收并关联项目 | 有界生命周期归属记录；输入并无独立认证签名 |
| schema/config hash | 网关本地计算并在调用前核对 | 检测被纳入指纹的配置变化；摘要本身不是操作人批准 |
| operator grant | 本地管理 CLI 保存，工具参数不能创建 | 在受保护管理面前提下的明确许可；不是同 UID 任意 shell 的隔离 |

`host-event` 当前检查事件格式、项目绑定和已有 session 的项目/目录一致性，但接受的是标准输入 JSON。具有同一用户 shell/数据库写权限的程序能调用相同管理入口，不能把 hook 输出或一个环境变量当成不可伪造的宿主证明。也不能以模型自报 approval、host 或 session 替代真实管理许可。

源码中的网关调用只允许进度令牌跨越 `_meta` 边界；其他调用元数据不会传给后端。进度令牌会在后端调用中重新生成，并在通知返回时映射回宿主的令牌。

## 两种调用信任模式（0.3.14+）

| 模式 | 启动 | 行为 | 适用 |
|---|---|---|---|
| `agent`（默认） | `gateway serve --host dsh` | 本机宿主被当作操作人的代理面：已登记后端工具无需逐次 grant，仍校验目录、配置/实时 schema、绝对 root、gateway session；模型不能自报 approval/session | agent 日常使用；授权收敛到“谁能启动这个网关” |
| `strict` | `gateway serve --trust strict` | 保留精确 grant：工具、schema、root、session、参数摘要、过期时间全部匹配才放行 | 最小权限、多人/不受信宿主、演示 |

`gateway discover` 只刷新目录中的 schema，不再自动改写任何旧 grant 的 schema_hash：即使旧工具标为 read-only，新参数也可能引入副作用。strict 模式下，schema 改变后旧 grant 保持失配，须由操作人审核新定义并显式重新授权；执行配置改变仍清除旧授权。`gateway doctor` 列出失配 grant，`gateway serve` 在 stderr 提醒。`0.3.20` 的此修复不改变默认 agent 委托模式，安装切换也不代替旧授权的显式复核。排查命令：

```sh
recalldock gateway doctor
recalldock gateway discover --server recalldock-memory
# 操作人核对新定义后，再按实际工具/root/参数运行 gateway grant
```


agent 模式不是跨用户隔离，也不把模型自报的 approval 当权限；它是操作人选择“让本机宿主代理调用”的一次性启动决定。需要恢复逐工具授权时用 strict；`gateway list` 会显示当前 `trust`。`tools_call` 仍不能由模型选择另一个 gateway session，也不能绕过 catalog/schema 校验。

## 内置工具 schema（0.3.8+）

网关固定 7 个工具都有自己的输入 schema；`work_checkpoint` 的进度对象不再用空对象占位，模型不需要猜字段：

```json
{
  "checkpoint": {
    "goal": "单行任务目标，必填，<=300 字节",
    "summary": "已验证进度摘要",
    "next": ["最多 3 条单行下一步"],
    "details": "可选细节，受 12 KiB 上限约束",
    "status": "active | blocked | completed | archived",
    "blockers": ["最多 5 条短阻塞"],
    "operations": [{"id": "op", "input_fingerprint": "fp", "status": "reported | started | applied | failed | needs_check | verified", "evidence": "可选"}]
  }
}
```

顶层只接受 `root`、`lane`、`expected`、`checkpoint`、`event`、`patch`。`artifacts`、`milestones`、`paths`、`notes` 等字段会被明确拒绝，并在错误里列出允许字段。七个内置工具都在访问账本、注册表或后端前按所声明 schema 校验类型、枚举和未知字段；`expected` / `generation` 必须是非负可安全表示整数，小数不会被 SDK getter 截断后写库。`work_find` 的 2 KiB 页面可以缩页，调用方沿 `data.after` 继续，不能把缩页当作全部结果。要取权威 schema：

```text
tools_describe server=recalldock_gateway tool=work_checkpoint
```

该调用不启动后端；内置工具直接按名称调用，`tools_call` 只用于已登记的后端工具。未知 server 会提示用 `tools_find` 或 `server=recalldock_gateway`，不再返回容易误解的 `backend_unavailable`。

## 执行配置指纹 v2（0.2.3）

0.2.2 在传输归一化与变量展开前计算指纹，遗漏了 `transport` 别名、`env_vars` 和大部分 env 值。回归反例表明：即使 schema 不变，执行目标或环境改变后也可能保留相同身份。新实现先完成传输归一化与配置展开，再计算版本为 2 的身份。

纳入摘要的输入为：原配置引用、归一化传输类型、展开后的 command/args/url/cwd、显式 env 名称和值、env_vars 的去重名称及所引用的当前值、相对命令的实际搜索 PATH、HTTP 头名称及非 Authorization 的头值，以及 bearer token 的环境变量名称。值仅参与内存中的摘要计算；目录和 registry 仍只保存引用与摘要，不复制凭证。

- 显式 env 或 env_vars 的值可能控制代码加载，不按名字猜测它是不是凭证。值变化均使旧配置许可与 lease 失效；环境变量中的凭证轮换也适用此规则。
- HTTP `Authorization` 和 bearer token 的认证值不纳入执行指纹，认证值可轮换；其他头的值可能选择租户或路由，改变时失效。自定义 API key 头也保守适用失效规则。URL、头名称或 bearer 来源名称改变需要重新确认。不同权限账号/credential 的业务作用域仍由后端和操作人约束，不能因摘要未变而推断账号权限相同。
- 等价的 `streamable-http`、`streamable_http` 和 `http` 会归一化为同一传输；宿主 autostart 开关不是业务授权，仍不计入执行身份。
- 相对命令查找依赖的继承 PATH 变化会使旧身份失效。这里没有对每个可执行文件、符号链接目标或后端依赖做内容证明，也不是操作系统执行沙箱。

## 发布时的失效与恢复

v2 不兼容旧摘要，必须拒绝沿用旧许可。升级后先对选定后端显式 `gateway discover --server ID`（或 `refresh=true`），核对 schema 和执行配置，再按需创建新许可；不得批量改写旧 grant 的 hash 来假装重新批准。原生七工具入口保持不变；默认 `agent` 模式是显式的宿主委托，需要逐工具授权时用 `--trust strict`。refresh 后，共用旧目录缓存的旧进程可能拒绝调用，应重连或开启新会话加载。

日常排障与恢复：安装升级前通过 `install-local.py` / `install-windows.ps1` 生成私有恢复备份，回滚命令使用 `recalldock setup --rollback --backup <BACKUP_DIR>`（仅恢复配置与入口，不回滚数据库）。

## 已验证与后续成功标准

本批测试覆盖：执行配置变化导致摘要变化；旧 live lease 在 env 值、传输别名或展开后的参数改变时被拒绝并关闭；lease 不能跨网关/根目录；host 范围不匹配拒绝；模型自报 session/host/approval 不能选择授权 session。现有 root/session/args/expiry/schema 检查继续执行。

真正的宿主身份认证还需要管理权限在模型可任意调用的 shell/工具面之外：由宿主或受保护 broker 持有发行与撤销权限，凭证通过受控通道交给网关，绑定真实 session、canonical root、host 和有效期，并能抵抗复制与重放。尚未确认各宿主都提供这样的通道；因此本版不生成签名密钥、不部署 broker，也不以同 UID 文件权限冒充完成该目标。

## 已核对的宿主能力范围

- Codex 官方说明区分本地 stdio 的命令/环境变量配置，以及 HTTP 的 bearer/OAuth 和可信第一方会话认证。后者不能据此推断为第三方本地网关提供聊天身份凭证；这是基于文档范围的限制判断，并非对所有未公开通道的否定。[官方 MCP 文档](https://learn.chatgpt.com/docs/extend/mcp)
- Claude hooks 通过 JSON 提供 session 等生命周期元数据；其自托管环境文档明确说明 shell、hooks 和 stdio MCP 处于相同用户环境。结合本项目直接读取 hook JSON 的实现，不能把字段存在视为独立认证。[Hooks 输入契约](https://code.claude.com/docs/en/hooks)，[自托管环境边界](https://code.claude.com/docs/en/self-hosted-environments-configuration)
- DSH 的 stdio 传输工厂只传递 command/args/env/cwd（env 合并清理后的父环境与显式配置），未提供独立会话证明发行或校验通道；仅作已检查行为说明，不外推全部桌面实现。

当前可交付的是配置身份失效与本地操作人授权边界。真正的跨宿主可信认证仍需要受保护的宿主/broker 通道、权限分离和隔离夹具，不能用可由模型启动的同 UID 文件或环境变量替代。后续在实际通道确定后再实现发行、撤销、重放与并发反例；本批不为了填补文档空白而部署伪认证。

## 候选准入即时结果与持久化审核状态

在 `checked` 或 `tiered` 模式下调用 `mem_commit_session` 提交候选后，可能会同时观察到两个原因字段：

- **`results[].reason = "evidence_not_verified"`**（`mem_commit_session` / `CommitMemoryCandidates` 返回）：**即时自动准入尝试结果**。表示在本次提交事务的自动准入评估阶段，由于来源证据未通过核验（如摘要不匹配、不可读取或类型未核验），本次未能自动晋升。
- **`rejection_reason = "operator_review_required"`**（`memory_candidates` 表及 `candidates list` 持久化字段）：**候选在数据库中的持久审核状态**。当候选处于 `status = "review"` 待审队列时，写入该状态表明需要操作人人工复核。

两者属于不同语义层（即时准入尝试评估 vs. 数据库持久审核状态），同一条候选同时出现这两个字段是正常且一致的，不构成状态冲突。
