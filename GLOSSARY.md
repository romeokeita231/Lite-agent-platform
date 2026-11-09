# Lite Agent Platform

一个通用的 AI Agent 运行平台：它自己拥有一次执行的循环，逐步落盘，允许人在中间插一手，并在进程消失之后接着跑。平台不承载任何业务逻辑，业务以 Policy 的形式接进来。

## Language

### 租户

**Tenant**:
调用平台的独立组织单位。Run、调用记录与用量都按它归属，跨租户互不可见。
_Avoid_: 用户、账号、project

### 执行

**Run**:
一次完整的 Agent 执行，从提交到终态。平台里唯一有生命周期的东西。
_Avoid_: task、job、execution、会话

**Step**:
Run 里由**平台**发起的一次动作，只有三种：调一次模型、调一次工具、移交一次人工。Policy 返回 `done` 时平台仍会写入 `step_type=done` 的终态 Checkpoint，那只是收尾记录，不是第四种动作。步骤边界归平台，不归业务侧。
_Avoid_: 阶段、node、super-step、iteration

**Checkpoint**:
一个 Step 成功结束后落盘的状态快照。恢复只从 Checkpoint 出发，不从内存出发。
_Avoid_: 快照、存档、save point

**Policy**:
业务侧给出的决策函数。每个 Step 之前平台问它一次"下一步做什么"，它按账本回答。无状态——它不记得上一次被问过什么，所以随时可以从头再问。
_Avoid_: Handler、处理器、Agent 代码、策略引擎

**Human Review**:
需要人给出输入才能继续的 Step。它进行期间 Run 停在原地，不占任何进程、不耗任何轮询。
_Avoid_: 审批、interrupt、人工确认

**Resume**:
从最后一个成功的 Checkpoint 之后继续执行。它发生在 Step 之间。
_Avoid_: 重试、replay、恢复执行

**Retry**:
同一个 Step 失败后再次执行它。它发生在同一个 Step 上，与 Resume 不是一回事。
_Avoid_: 重跑、重放

### 模型

**Fallback Chain**:
一次模型调用失败时按顺序接替的有序模型列表。
_Avoid_: 重试链、备用模型、backup

**Effective Model**:
一次调用最终真正使用的模型。它可能与调用方请求的不一致。
_Avoid_: selected model、实际模型

### 工具

**Tool**:
注册在案、可被 Step 调用的能力。
_Avoid_: skill、function、plugin

**Tool Version**:
Tool 的一个不可变实现版本。同一个 Tool 可以并存多个版本。
_Avoid_: 工具迭代、工具快照

**Default Version**:
调用方未指定版本时解析到的那个 Tool Version。它不必是最新的那个。
_Avoid_: latest、当前版本

**Invocation**:
一次带参数的 Tool 调用。参数由平台按该版本的 schema 校验后才放行。
_Avoid_: call、执行、请求

### 可观测

**Trace**:
一次 Run 产生的完整调用记录。
_Avoid_: 链路、日志、audit log

**Span**:
Trace 里一段有起止时间的记录。一个 Step 至少对应一个 Span；同一次模型调用的每次 fallback 尝试各自成一个 Span，所以 Span 比 Step 密。
_Avoid_: 埋点、阶段

**Console**:
内嵌在 platform 二进制里的只读观察台（`/console`），凭租户 API key 只观察本租户的 Run、Checkpoint 与 Span，并可对 `waiting_human` 的 Run 提交人工应答。它不创建、不编排、不发布，因此不属于 [ADR 0011](docs/adr/0011-runtime-not-an-app-builder.md) 排除的"应用构建"（[ADR 0026](docs/adr/0026-console-observes-not-builds.md)）。
_Avoid_: Dashboard、管理界面、控制台
