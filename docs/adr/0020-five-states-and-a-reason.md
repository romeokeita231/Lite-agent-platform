# 状态只有五个，原因不在状态里

Status: accepted

Run 的状态集：`queued` / `running` / `waiting_human` / `completed` / `failed` / `cancelled`。终态三个。超时**不是**状态，是 `failed` 加 `terminal_reason=step_timeout`。

## Consequences

- 状态机不按原因分支。每多一个"原因型状态"，所有 `switch` 都要多一个 case，而这些 case 的行为完全一样——都不能再推进一步。
- `queued` 必须是真的。ADR 0012 的每租户并发上限会让 Run 真的排队；如果状态里没有 `queued`，那段时间 Run 会显示"运行中"却什么都没干——这是状态机撒谎的典型方式。
- `terminal_reason` 枚举固定：`step_timeout` / `policy_unreachable` / `max_retries` / `tool_schema_violation` / `policy_invalid_action` / `cancelled_by_tenant`。
- 展示与统计按 `terminal_reason` 分组，不按状态分组——五个状态分不出信息量。
