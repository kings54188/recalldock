# 记忆分类与写入规则

状态：分类规则自 `0.3.0` 起保持稳定；`0.3.14+` 扩展了进度账本 schema 与并发边界。本页固定四类长期记忆的用途、允许内容与排除内容，避免同一事实在多套分类里重复或冲突。数据主库仍只有 `recalldock.db`；`operations.db` 只存有界进度，不是第二记忆库。

## 四类记忆

| `memory_class` | 对应记忆类型 | 用途 | 典型内容 |
| --- | --- | --- | --- |
| `operational` | 工作记忆（working）/ 会话情景 | 当前任务的进度、检查点、会话摘要；默认不参与语义召回 | `checkpoint`、`session_summary`、`summary` |
| `project` | 语义记忆（项目事实） | 可直接复用的项目级事实与决策 | `decision`、`architecture`、`bugfix`、`config`、`command`、`policy`、`file_change`、`reference`、`note` |
| `user` | 个人事实 / 偏好 | 只对当前用户成立、跨项目复用的偏好与事实 | `preference`、`user_fact` |
| `agent` | 程序记忆（可复用方法） | 可迁移的做法、模式与经验 | `procedure`、`learning`、`pattern`、`experience` |

`MemoryClassForType` 是确定性的类型映射；未知遗留类型落到 `project`，并只由 doctor 的 `memory_classification` 检查报告，不做模型驱动的重写。显式传入的 `memory_class` 只接受上表四个值，其他值直接报错。

## 写入规则

1. 模型侧不直接写长期知识：只能通过 `mem_commit_session` 提交候选，准入由操作人策略（`manual|duplicates|checked`）决定；`checked` 只核验来源/状态，不证明陈述正确。
2. 工作记忆（`operational`）与有界进度优先写入 `operations.db`（`work checkpoint`），避免把过程噪音留在长期知识里；会话结束的摘要正文由操作人决定，模型工具不能提交带正文的 summary。
3. 同一主题重复写入按 `topic_key` 去重/复活；`duplicates` 策略只闭环完全相同的现存事实，`manual` 一律人审。
4. 正式知识修订必须带快照摘要（CAS）；编辑保留归档态与 `review_after`，只有显式 `memory reactivate` 或审核标记才改变生命周期，读取不算验证性复用。
5. 归档不是删除：`memory archive` 保留知识与历史、排除默认召回；受保护类（`user`/`operational`）默认不被冷却降权。

## 排除内容（任何分类都不写入）

- 普通进度流水、逐轮对话、完整聊天记录、原始日志与命令输出；
- 猜测、未验证的推断、模型自报的置信度或“已确认”标签；
- 凭证、密钥、令牌、密码与可复用认证材料；
- 他人隐私数据或与当前项目无关的个人信息。

发现上述内容时，候选应被拒绝或留在 review；操作人也不应手工绕过准入。

## 召回行为

- `mem_search`/`search` 默认项目范围，`profile=semantic` 排除 `operational`，`profile=operational` 只取检查点与会话摘要；`scope=personal` 只取 `user` 类且不扩大项目范围。
- 召回强化与冷却并存：近期复用加权，长期未用可被 `memory cooldown` 排期复核；pinned 与 `user`/`operational` 豁免降权。
- 预算与 token 数是估算值，不冒充宿主真实 tokenizer 或总上下文计量。

## 相关检查

`recalldock doctor --check memory_classification` 报告各类数量与未识别类型；`session_origin` 报告会话来源（host/operator/model/unknown，属于溯源性不是认证）；`network_boundary` 报告出站面保持关闭。

分类规则变更需同时更新本页、`CHANGELOG.md` 与对应测试；不得新增并行的第二套分类主数据。
