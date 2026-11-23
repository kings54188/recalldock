---
name: recalldock-memory
description: "Use RecallDock for bounded, selective project memory and task recovery when resuming work, checking prior verified decisions, or recording a long-running task's progress."
---

# Selective memory and task recovery

Read prior knowledge only when relevant. Ordinary task progress belongs in independent `operations.db`, not the long-term knowledge database. Do not save raw prompts, transcripts, logs, or automatic session summaries.

This legacy source path is not the packaged host skill: releases copy `integrations/host-plugins/plugins/claude-code/skills/selective-memory/SKILL.md`. The recovery extensions below describe `0.3.20` development; inspect the live schema before using new parameters with an older installed runtime.

## Recover a task

1. Check current files/processes, then call `work_find` with the explicit absolute root. Default recent order follows latest checkpoint submission, hides archived tasks, and searches current progress plus event summaries/details.
2. Reuse `after` only while `has_more=true`, retaining query/order/status. `total` is the matching count, not page size; `limit=1..20` still fits a 2 KiB L0 page and may shrink for the byte budget.
3. Select one exact lane and read `work_read` L1. Use L2 only for needed details. History is a separate L0 view; select an event generation before reading its details.

```text
work_find {"root":"/absolute/project","limit":20}
work_read {"root":"/absolute/project","lane":"task"}
work_read {"root":"/absolute/project","lane":"task","view":"history","limit":20}
work_read {"root":"/absolute/project","lane":"task","view":"event","generation":2,"mode":"l2"}
```

History accepts before/limit, not generation or mode=l1/l2. Event requires generation>=1 and mode=l2, and returns event summary/details/kind plus patch. A checkpoint L2 does not expand the whole event stream.

CLI fallback uses the same root/lane boundary. Use the configured local MCP or CLI fallback; independent setup remains disabled.

```sh
recalldock work find --root /absolute/project --limit 20
recalldock work status --root /absolute/project --lane task
recalldock work history --root /absolute/project --lane task --limit 20
recalldock work read --root /absolute/project --lane task --generation 2 --mode l2 --event
```

## Record bounded progress

Use `work_checkpoint` with an explicit expected generation and either `checkpoint` or `event` plus optional `patch`. Put goal/summary/next/details/status/blockers/operations inside checkpoint, not invented top-level fields. Mark completed only after validation; verified operations require evidence references.

CAS conflicts include expected/current: reread before deciding how to write. An event retry is idempotent only for the same root/lane/id/fingerprint/full event+patch; `replayed:true` distinguishes replay. A different id with the same fingerprint is not the same event. Check actual state before retrying an unknown outcome.

Writer metadata is process-supplied, not a session selector. Missing writer or host_session_id means unknown; a gateway instance is not a host session, and the latest writer is not proof of an active writer. Only trusted host lifecycle events register real IDs; do not invent IDs or claim another session.

## Discover tools and preserve boundaries

The gateway has seven tools: tools_find/tools_describe/tools_call/tools_release/work_find/work_read/work_checkpoint. Call built-in work tools directly. `tools_find` does not start backends; servers use after/has_more, backend tools use tool_after/tool_has_more, and built-ins use builtin_after/builtin_has_more. Advance the needed category independently with its returned cursor.

Use `tools_describe server=recalldock_gateway` for the seven short built-in summaries and add `tool=NAME` for the exact schema. For registered backends, describe the selected schema before tools_call. Strict schema changes need an explicit operator re-grant, even for read-only tools; model approval/session/author fields grant no permission. Preserve gateway-owned lease controls and release owned connections after use.

Long-term knowledge is selective and separate: propose only verified, reusable findings through the allowed candidate flow after a real host registration. Formal admission/review is operator-controlled; zero candidates is normal. Never turn memory recovery into automatic capture, session-end writing, host compression, or configuration repair.
