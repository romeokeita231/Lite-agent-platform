# Step 是一次原语调用，不是一次 Handler 调用

Status: accepted（修订 ADR 0001）

ADR 0001 说"平台拥有执行循环、客户端提供步骤执行体"，但没回答 Handler 跑到一半崩溃时从哪接。定死粒度：Step 只有三种——`llm_call`、`tool_call`、`human_review`，都是平台执行的**原语**。业务侧不再提供"执行体"，而是在每个 Step 之前被问一次的无状态 **Policy**：给定账本，回答下一步做什么。平台执行该动作、落盘、再问一次。

## Considered Options

- **粗粒度（Step = 一次 Handler 调用）**：Handler 内部自己调模型和工具。代价是平台看不见工具调用边界，超时、Schema 校验、权限校验全部落空，工具网关（模块③）退化成 HTTP 代理。而且 Handler 崩在中途时，除了整个重跑没有别的选择。
- **细粒度 yield（协程式：Python generator、Go goroutine）**：每次让出控制权就是一次模型/工具调用，平台在让出点落盘。恢复要重放到断点并回放已记录的调用结果，也就是 SDK 必须强制确定性——这正是 ADR 0001 否掉的、最贵的那条路。

## Consequences

- 原文档内部本来就自相矛盾：§4.1 的 `checkpoints.step_type` 是 `llm_call / tool_call / human_review`（本 ADR 的模型），§1.1 的客户端 checkpoint API 是另一个模型。本 ADR 采信数据模型，废弃接口。
- Policy 无状态，所以"恢复"不需要重放：拿账本重新问一次即可，已执行的动作记在账上、不会重做。
- 代价：每走一步多一次平台 → Policy 的往返。这是 ADR 0006 处理的事。
- ADR 0001 的"客户端提供步骤执行体"这句被本 ADR 取代；其余结论（平台拥有循环、可观测边界由平台定、降级目标是声明式工作流）不变。
