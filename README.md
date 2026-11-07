# Lite Agent Platform

A small Go runtime for durable, observable agent execution. The platform owns the
execution loop: it drives each step, checkpoints after it, lets a run stop and wait
for a human, and picks the run back up after the process that started it is gone.

Business logic lives on the other side of a policy boundary: before every step the
platform asks the business process what to do next, and the business process answers
from the ledger without remembering anything. The platform knows nothing about what an
agent is trying to accomplish.

It is all Go, and the policy is a separate Go binary talking the same HTTP contract. The
split is not about languages — it is about the platform never executing someone else's
code in its own process. See [ADR 0008](docs/adr/0008-go-for-platform-and-policy.md).

> **状态：已实现且可运行。** 本仓库包含平台、示例 Policy 和本地演示栈。决策记录在
> [`docs/adr/`](docs/adr/)，与代码冲突时以 ADR 为准；共享术语见
> [`GLOSSARY.md`](GLOSSARY.md)。

## How a run works

Six lines. If any of them ever disagree with an ADR, the ADR wins.

1. A tenant posts `POST /v1/runs` with an agent name and an input. The run is `queued`.
2. The platform asks that agent's Policy one question: what is the next step?
3. The Policy answers with an action — call a model, call a tool, or ask a human.
4. The platform performs the action, appends one Checkpoint, and writes the Span beside
   it in the same transaction.
5. It asks again. Repeat until the Policy answers *done*, or a step fails past its retries.
6. A step that needs a human parks the run at `waiting_human`. Nothing is written until
   the answer arrives, and no process waits for it. The platform dying mid-run is the same
   situation: the run resumes from the last Checkpoint.

## What it is not

- Not a business application, and not bound to any vertical.
- Not a workflow engine: there is no DSL to author, and no DAG to draw.
- Not a RAG stack: retrieval is an interface the platform exposes, not something it
  implements. See [ADR 0004](docs/adr/0004-context-without-retrieval.md).
- Not a billing system: it collects usage, it does not invoice.
- Not polyglot. There is one toolchain here on purpose; the boundary that matters is a
  process boundary, not a language boundary.

## 本地演示

`docker compose up --build` 启动 PostgreSQL、`platform` 和 `policy-example`。平台容器
在启动服务前执行 `platform seed`：创建租户 `demo-a` 和 `demo-b`，Agent `demo`、
`tool-slow`、`human`，`default` 模型路由（`demo-model`），以及 `echo`/`slow` 工具。
seed 是幂等的，并输出一行 JSON，包含两个租户 ID、各自的明文 API key 和 policy URL；
这些 key 不会在别处打印。

```sh
cp .env.example .env
docker compose up --build
```

`MODEL_BASE_URL` 默认指向 `policy-example` 自带的 OpenAI 兼容
`/v1/chat/completions` stub，因此每次演示 run 都会发起真实的 HTTP 模型调用。把它指向
其他 OpenAI 兼容服务并设置 `MODEL_API_KEY`，即可改用该服务。

三个 PowerShell 脚本演示 [ADR 0012](docs/adr/0012-one-binary-split-ready.md) 中的
隔离特性：

| 脚本 | 演示内容 |
| --- | --- |
| `scripts/demo-1-concurrency.ps1` | 租户 A 占满自己的并发上限时，租户 B 的 run 不会被排在它后面 |
| `scripts/demo-2-tool-hang.ps1` | 工具端点挂起时，一个 run 在 `STEP_TIMEOUT` 后失败；其他 run 正常完成 |
| `scripts/demo-3-crash.ps1` | 杀掉 policy 会看到重试和 `policy_unreachable`；运行中杀掉 platform，run 会从 ledger 恢复 |

每个脚本接受 `-BaseUrl`、`-ApiKeyA`、`-ApiKeyB`（与 seed 输出一致），失败时以非零
退出，例如 `powershell -File scripts/demo-1-concurrency.ps1`。

## Documents

- [`GLOSSARY.md`](GLOSSARY.md) — the vocabulary. Definitions only, no design in it.
- [`docs/adr/`](docs/adr/) — decisions that are hard to reverse, surprising without
  context, and were a real trade-off. Decisions win over any other document.
