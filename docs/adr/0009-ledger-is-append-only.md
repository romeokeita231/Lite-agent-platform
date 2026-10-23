# 账本只追加，等待不是一条记录

Status: accepted

原设计把「等人工」写进 `runs.status`，又给 `checkpoints.status` 只留 `success / failed / timeout` —— 同一件事记两处，靠代码手工对齐，崩溃时必然漂移。定死：`checkpoints` 只记录**已完成的 Step**，append-only、没有 UPDATE、也没有 `status` 列；Run 的当前状态只存在于 `runs`；人工等待是 `runs` 上的状态位加 `pending_step` / `human_prompt` / `deadline` 三个字段，不落任何 checkpoint。

## Consequences

- `checkpoints` 上没有 `status` 列：行存在就代表这一步成功了。少一个枚举，就少一条"两个枚举要同步"的规则。
- 恢复只读"最后一个成功 Step"，不需要判定悬挂行；崩溃不会留下永远 `pending` 的记录，因此也不需要清理逻辑。
- 失败的那次尝试不写 checkpoint。账本记"发生了什么"，不记"试过什么失败"。失败次数与原因在 `runs.attempt` 与 Span 里。**代价：从账本上查不到"第 3 步失败过两次"，只能查 Span。**
- 人工可以几小时后才回来：等待期间没有任何一行处于中间态，也没有进程为它轮询。这不是优化，是模型直接带来的性质。
