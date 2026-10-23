# 账本 exactly-once，副作用 at-least-once

Status: accepted

"你的恢复是 exactly-once 吗"必须有唯一答案：不是。已经落 Checkpoint 的 Step 绝不重跑，这个由账本保证；但"执行完成、Checkpoint 尚未提交"之间崩溃时，该 Step 会再执行一次，所以副作用是**至少一次**。平台不假装成 exactly-once，改为给每个 Step 发幂等键。

## Consequences

- 幂等键 = `(run_id, step_index)`，**不含 attempt**。重试必须复用同一个键，否则去重直接失效——这是这套机制里最容易写错的一处，要写进注释。
- 键透传给工具端点（`Idempotency-Key` 头），由工具侧去重。平台不替外部系统做去重，它只保证键稳定。
- `llm_call` 不去重：重复调用没有副作用，只重复花钱，所以它只进计量。这个不对称是有意的——对有副作用的步骤要求幂等，对只有成本的步骤只要求计费。
- 任何地方都不许出现裸的 "exactly-once"，除非当场补一句"账本层面"。
