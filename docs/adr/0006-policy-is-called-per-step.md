# 每步向 Policy 要一次决策，超时算 Step 的失败

Status: accepted

平台每走一步都要问业务侧一次"下一步做什么"，于是平台变成了一个依赖业务进程的编排者。定三条边界：回调是**推**（平台 HTTP 调 Policy 的 endpoint），不是拉（不做队列和 lease）；决策超时算 **Step 失败**，走指数退避重试，重试耗尽才判 Run 失败；重试期间 Run 留在 `running`，只在 `runs` 上记 `attempt` 和 `next_attempt_at`，不新增状态。

## Considered Options

- **拉（worker 轮询 + lease）**：能自动发现 worker 崩溃、不需要平台知道 worker 地址。代价是 lease 表、心跳、租约过期判定三件套，而架构里刚把 worker 去掉。若将来出现"平台不知道有多少个 worker"的需求再引入。

## Consequences

- 杀掉 Policy 进程、平台照样能演示超时 → 重试 → 结构化错误。这正好是原设计要求"示例 B"验证的东西，而且由平台演示，不是由 Agent 演示。
- 退避重试只有一处实现（Step 重试），模型网关不自己退避——与 ADR 0003 一致。
- 平台把"业务侧不可用"当成一次普通的 Step 失败，不为它设专门状态机分量。少一个状态，少一处不一致。
