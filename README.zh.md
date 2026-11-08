# Lite Agent Platform

> English version: [README.md](README.md)

一个用 Go 写的轻量 Agent 运行平台。平台自己拥有执行循环：逐步推进、每步落盘、允许中途停下等人、进程消失后从账本接着跑。

业务逻辑放在 Policy 的另一侧。平台在每个 Step 之前问业务进程"下一步做什么"，业务进程只看账本回答，不记得上一次说过什么。平台不知道一个 Agent 想完成什么。

平台和示例 Policy 都是 Go，通过同一套 HTTP 契约通信。这个拆分不是因为语言，而是因为平台不在自己的进程里执行别人的代码。见 [ADR 0008](docs/adr/0008-go-for-platform-and-policy.md)。

> **状态：已实现且可运行。** 本仓库包含平台、示例 Policy 和本地演示栈，25 条决策记录在 [`docs/adr/`](docs/adr/)。ADR 是规格，与代码冲突时以 ADR 为准；共享术语见 [`GLOSSARY.md`](GLOSSARY.md)。

## 一次 Run 怎么跑

六句话。任何一句和 ADR 冲突，以 ADR 为准。

1. 租户 `POST /v1/runs`，带上 `agent_id` 和 `input`。Run 进入 `queued`。
2. 平台向该 Agent 的 Policy 问一个问题：下一步做什么。
3. Policy 返回一个动作：调模型、调工具，或者移交人工。
4. 平台执行动作，追加一条 Checkpoint，并在同一事务里写下对应的 Span。
5. 平台再问一次，直到 Policy 回答 `done`，或者某个 Step 超过重试次数失败。
6. 需要人工的 Step 把 Run 停在 `waiting_human`。答案到达之前不追加 Checkpoint，也没有进程在等它。进程在 Run 中途挂掉是同一回事：Run 从最后一条 Checkpoint 之后恢复。

Run 的状态只有 `queued`、`running`、`waiting_human`、`completed`、`failed`、`cancelled`；终态原因单独记录：`step_timeout`、`policy_unreachable`、`max_retries`、`tool_schema_violation`、`policy_invalid_action`、`cancelled_by_tenant`。Step 只有三种动作：`llm_call`、`tool_call`、`human_review`；`done` 是收尾 Checkpoint，不是第四种动作。

## 它不做什么

- 不是业务应用，也不绑定任何垂直领域。
- 不是工作流引擎：没有 DSL 要写，也没有 DAG 要画。
- 不是 RAG 栈：检索是平台暴露的接口，不是平台实现的。见 [ADR 0004](docs/adr/0004-context-without-retrieval.md)。
- 不是计费系统：它收集用量，不开账单。
- 不是多语言运行时。这里只有一个工具链是故意的；真正重要的边界是进程边界，不是语言边界。

## 仓库结构

| 路径 | 内容 |
| --- | --- |
| `cmd/platform` | 平台二进制；子命令 `seed`、`keygen` |
| `cmd/policy-example` | 示例 Policy，兼作 OpenAI 兼容模型 stub 和 demo 工具端点 |
| `internal/runtime` | 调度、Step 执行、重试、取消、上下文裁剪 |
| `internal/repository` | PostgreSQL 存储与事务边界 |
| `internal/migrate` | 内嵌迁移（`internal/migrate/sql/`） |
| `internal/httpapi` | HTTP API、认证、评测、用量查询 |
| `internal/modelgw` | 模型网关：路由、fallback 链、调用记录 |
| `internal/toolgw` | 工具解析、schema 校验、调用 |
| `internal/contextwin` | 上下文窗口裁剪 |
| `internal/retrieval` | 检索接口（无实现） |
| `internal/observability` | Prometheus 指标与 readiness |
| `internal/domain` | 领域类型与租户上下文 |
| `internal/config` | 环境变量装载 |
| `scripts/` | 三个 PowerShell 演示脚本 |
| `k8s/` | 最小 Kubernetes 清单 |
| `docs/adr/` | 25 条架构决策记录 |
| `docs/agents/` | 给 agent 的仓库操作说明 |

## 快速开始

### 前置

- Go 1.26（只用二进制时需要）
- Docker 与 Docker Compose（本地演示栈需要）

### 一键启动本地栈

```powershell
Copy-Item .env.example .env
docker compose up --build
```

Compose 启动三个服务：

- `db`：PostgreSQL（`pgvector/pgvector:pg16`），数据在 `pgdata` 卷里。
- `platform`：启动服务前先执行 `platform seed`，监听 `:8080`。
- `policy-example`：示例 Policy，监听 `:9090`，同时提供 `/v1/chat/completions` 模型 stub 和 `/demo/tool/echo`、`/demo/tool/slow` 工具端点。

`platform seed` 创建租户 `demo-a`、`demo-b`，Agent `demo`、`tool-slow`、`human`、`quick`，模型路由 `default`/`cheap`/`fast`（默认模型 `demo-model`），以及工具 `echo`、`slow`。seed 幂等，并输出一行 JSON，包含 `policy_url`、两个租户 ID、各自的明文 API key 和 agent 名单；这些 key 不会在别处打印。

`MODEL_BASE_URL` 默认指向 `policy-example` 自带的 OpenAI 兼容 stub，因此每次演示的模型 Step 都会发出真实 HTTP 调用。要改用别的 OpenAI 兼容服务，设置 `MODEL_BASE_URL` 和 `MODEL_API_KEY` 即可。

### 第一次调用

`quick` Agent 在第一步就返回 `done`，适合冒烟验证：

```powershell
$key = 'demo-a-api-key'
$headers = @{ Authorization = "Bearer $key" }
$agent = Invoke-RestMethod http://localhost:8080/v1/agents -Headers $headers |
    Where-Object { $_.name -eq 'quick' } | Select-Object -First 1
$body = @{ agent_id = $agent.id; input = @{ question = 'hi' } } | ConvertTo-Json -Depth 4
$run = Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/runs `
    -Headers $headers -ContentType 'application/json' -Body $body
$run
```

之后用 `GET /v1/runs/{id}` 轮询状态。`demo` Agent 会依次走模型、工具、人工，需要配合 `POST /v1/runs/{id}/human-input` 才能走到 `completed`。

### 直接跑二进制

需要一个可用的 PostgreSQL。仓库内的迁移在进程启动时自动执行。

```powershell
$env:DATABASE_URL = 'postgres://postgres:postgres@localhost:5433/platform?sslmode=disable'
$env:POLICY_SECRET_DEMO = 'demo-policy-secret'
$env:POLICY_SECRET_TOOL_SLOW = 'demo-policy-secret'
$env:POLICY_SECRET_HUMAN = 'demo-policy-secret'
go run ./cmd/platform seed
go run ./cmd/platform
```

`seed` 在 `DEMO_API_KEY_A`/`DEMO_API_KEY_B` 未设置时会各生成一个随机 key 并随 seed 输出打印。密钥只走环境变量，不落库。

`POLICY_URL` 默认指向 compose 内的 `policy-example`；脱离 compose 单跑时，要么把它改成实际可达的 Policy 地址，要么只用 `quick` Agent 验证平台自身。

### 构建镜像

```powershell
docker build --target platform -t lite-agent-platform:local .
docker build --target policy-example -t lite-agent-platform-policy-example:local .
```

`keygen` 可生成 API key 及其哈希、前缀：

```powershell
go run ./cmd/platform keygen
```

输出 `{"api_key": "...", "api_key_hash": "...", "api_key_prefix": "..."}`。

## 演示脚本

三个 PowerShell 脚本演示 [ADR 0012](docs/adr/0012-one-binary-split-ready.md) 中的隔离特性，按 Windows PowerShell 5.1（`powershell.exe`，不是 `pwsh`）验证：

| 脚本 | 演示内容 |
| --- | --- |
| `scripts/demo-1-concurrency.ps1` | 租户 A 占满自己的并发上限时，租户 B 的 Run 不会被排在它后面 |
| `scripts/demo-2-tool-hang.ps1` | 工具端点挂起时，一个 Run 在 `STEP_TIMEOUT` 后失败；其他 Run 正常完成 |
| `scripts/demo-3-crash.ps1` | 杀掉 policy 会看到重试和 `policy_unreachable`/`max_retries`；运行中杀掉 platform，Run 会从账本恢复 |

共同参数：

- `-BaseUrl`，默认 `http://localhost:8080`
- `-ApiKeyA`、`-ApiKeyB`，默认读 `DEMO_API_KEY_A`/`DEMO_API_KEY_B`，再退回 `demo-a-api-key`/`demo-b-api-key`

`demo-2` 额外有 `-DemoRuns`（默认 3）；`demo-3` 额外有 `-PolicyHealthUrl`（默认 `http://localhost:9090/healthz`），并且会调用 `docker compose` 启停服务。脚本失败时以非零退出：

```powershell
powershell -File scripts/demo-1-concurrency.ps1
powershell -File scripts/demo-2-tool-hang.ps1
powershell -File scripts/demo-3-crash.ps1
```

## 配置

### 平台进程

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `DATABASE_URL` | 无，必填 | PostgreSQL 连接串 |
| `HTTP_ADDR` | `:8080` | HTTP 监听地址 |
| `STEP_TIMEOUT` | `10s` | 单个 Step 的超时；也是 Policy 决策、模型调用、工具调用的超时基准 |
| `MAX_STEP_ATTEMPTS` | `4` | 同一个 Step 的最大尝试次数；退避从 1s 起翻倍，封顶 30s |
| `TENANT_CONCURRENCY` | `4` | 每个租户同时推进的 Run 上限，必须为正 |
| `TENANT_RATE` | `0` | 每个租户的 Run 领取速率（每秒令牌数），`0` 表示不限速；必须非负 |
| `MODEL_BASE_URL` | 空 | OpenAI 兼容服务的基地址；做模型 Step 或调 `/v1/chat/completions` 时必须配置 |
| `MODEL_API_KEY` | 空 | 模型服务的 Bearer key |
| `MODEL_CHAIN_BUDGET` | 等于 `STEP_TIMEOUT` | 一次模型调用里整条 fallback 链的总预算 |
| `CONTEXT_RECENT` | `8` | 发给 Policy 的最近 Step 数 |
| `CONTEXT_MAX_KEEP` | `16` | Policy 用 `keep` 声明保留的 Step 数上限 |
| `POLL_INTERVAL` | `100ms` | Dispatcher 轮询可运行 Run 的间隔 |

`docker-compose.yml` 为演示覆盖了部分值：`STEP_TIMEOUT=5s`、`MAX_STEP_ATTEMPTS=3`、`TENANT_CONCURRENCY=1`。

Agent 的 HMAC 密钥按名字从平台进程的环境变量读取（`secret_env` 只存变量名）。演示栈使用：

| 变量 | 用途 |
| --- | --- |
| `POLICY_SECRET_DEMO` | `demo`、`quick` Agent 的 Policy 签名密钥 |
| `POLICY_SECRET_TOOL_SLOW` | `tool-slow` Agent |
| `POLICY_SECRET_HUMAN` | `human` Agent |

### 示例 Policy

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `POLICY_ADDR` | `:9090` | 监听地址 |
| `POLICY_SECRET` | 无，必填 | 校验平台 HMAC 签名 |
| `POLICY_MODEL` | `route:default` | `demo` Agent 第一步请求的模型路由 |

### seed

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `POLICY_URL` | `http://policy-example:9090` | 写进 Agent 的 Policy 地址 |
| `DEMO_API_KEY_A` | 随机生成 | 租户 `demo-a` 的 API key |
| `DEMO_API_KEY_B` | 随机生成 | 租户 `demo-b` 的 API key |
| `DEMO_MODEL` | `demo-model` | `default` 路由的模型 |
| `DEMO_MODEL_CHEAP` | 等于 `DEMO_MODEL` | `cheap` 路由的模型 |
| `DEMO_MODEL_FAST` | 等于 `DEMO_MODEL` | `fast` 路由的模型 |

`.env.example` 只放演示值：

```dotenv
POLICY_SECRET=demo-policy-secret
DEMO_API_KEY_A=demo-a-api-key
DEMO_API_KEY_B=demo-b-api-key
MODEL_BASE_URL=http://policy-example:9090/v1
MODEL_API_KEY=
```

## HTTP API

`/healthz`、`/readyz`、`/metrics` 公开；其余 `/v1/*` 都要求 `Authorization: Bearer <租户 API key>`，并只能访问该租户的数据。Run、Agent、评测、用量等资源跨租户访问表现为 `404 not_found`；工具解析不到版本时是 `400 bad_request`。错误体统一为：

```json
{"error":"not_found","message":"run not found"}
```

| 方法与路径 | 说明 |
| --- | --- |
| `GET /healthz` | 存活检查，恒 `200` |
| `GET /readyz` | 就绪检查；数据库不可用、迁移未完成或正在停机时 `503` |
| `GET /metrics` | Prometheus 指标，不带租户标签 |
| `POST /v1/agents` | 注册 Agent：`name`、`policy_url`、`secret_env`；重名 `409` |
| `GET /v1/agents` | 列出本租户 Agent |
| `POST /v1/runs` | 提交 Run：`agent_id`、`input`；Agent 不存在或跨租户 `404`；停机中 `503` |
| `GET /v1/runs/{id}` | Run 详情，含 `status`、`output`、`terminal_reason`、`pending_step` |
| `GET /v1/runs/{id}/steps` | 按顺序列出 Checkpoint |
| `GET /v1/runs/{id}/trace` | Span 按 Step 分组 |
| `POST /v1/runs/{id}/human-input` | 回答人工 Step：`step_index`、`input`；Run 不在等待或步号不符 `409` |
| `POST /v1/runs/{id}/cancel` | 请求取消；已到终态 `409` |
| `POST /v1/tools` | 注册工具版本：`tool_name`、`version`、`endpoint`、`schema`（必填），`description`、`is_default` |
| `GET /v1/tools` | 列出本租户工具 |
| `POST /v1/tools/{tool_name}/invoke` | 直接调用工具：`version` 可选，`args` 必须是对象，按 schema 校验 |
| `POST /v1/chat/completions` | OpenAI 兼容入口；`model` 可以是 `route:<label>` 或模型名；额外返回 `lite` 字段（effective model、chain、fallback_from、token 用量） |
| `GET /v1/models` | 列出路由链中的模型与 `route:<label>` |
| `GET /v1/tenants/{id}/usage` | 用量查询；`group_by=model\|run`，`from`/`to` 为 RFC3339；跨租户 `404` |
| `POST /v1/evals` | 跑一组评测用例，同步等待最多 60s，返回报告与逐例结果 |
| `GET /v1/evals/{id}` | 读取评测报告 |

评测请求的 `cases` 是数组，每项形如：

```json
{
  "agent": "quick",
  "input": {"question": "hi"},
  "expect": {
    "status": "completed",
    "tool_calls": [{"tool": "echo", "args": {"message": "hello"}}],
    "output": {"answer": "quick"}
  }
}
```

`expect.output` 与 `expect.tool_calls[].args` 是子集匹配；`tool_calls` 按顺序匹配。结果中的 `passed` 与 `failures` 会明确说明哪个字段不符。

## Policy 契约

平台向 `POST {policy_url}/internal/policy/decide` 发请求，请求体：

```json
{
  "run_id": "...",
  "agent": "demo",
  "step_index": 0,
  "attempt": 0,
  "input": {"question": "hi"},
  "ledger": {
    "recent": [{"step_index": 0, "step_type": "llm_call", "result": {}}],
    "summary": "...",
    "dropped": [1, 2]
  }
}
```

请求头：

- `X-Agent-Id`：Agent ID
- `X-Timestamp`：RFC3339 时间
- `X-Signature`：`HMAC-SHA256(secret, METHOD + "\n" + PATH + "\n" + SHA256_HEX(body) + "\n" + TIMESTAMP)`

示例 Policy 只接受时间戳在 ±5 分钟内的请求。HMAC 密钥不落库，按 `secret_env` 从环境变量读取。

响应是四种动作之一：

| `action` | 字段 | 平台行为 |
| --- | --- | --- |
| `llm_call` | `route`、`messages`，可选 `keep` | 经模型网关调用；`route:<label>` 走路由链，否则按模型名 |
| `tool_call` | `tool`、`args`，可选 `version`、`keep` | 解析默认版本、按 schema 校验、调用并记录 |
| `human_review` | `prompt`，可选 `deadline_seconds`、`keep` | Run 进 `waiting_human`，写入 `pending_step`，不占进程 |
| `done` | `output`，可选 `keep` | 写终态 Checkpoint，Run 变 `completed` |

Policy 没有 `fail` 动作（[ADR 0023](docs/adr/0023-policy-has-no-fail-action.md)）。Step 该失败时由平台判定终态原因。`keep` 用于声明哪些历史 Step 必须保留，裁剪上限是 `CONTEXT_MAX_KEEP`（[ADR 0016](docs/adr/0016-context-is-cut-by-the-platform.md)）。

模型侧的 fallback 只在可重试错误时发生：429、5xx、超时、连接失败；调用方能在 `lite` 或 Checkpoint 里看到实际使用的模型（[ADR 0003](docs/adr/0003-fallback-only-on-retryable-errors.md)、[ADR 0013](docs/adr/0013-routing-by-label-and-chain.md)）。

## 运行与运维

- **迁移**：在进程启动时自动执行，使用 advisory lock 和 `schema_migrations` 表。已应用的迁移不再修改，只追加新文件。
- **seed 幂等**：可以重复执行；同一个租户、Agent、路由、工具会被更新而不是重复插入。
- **恢复**：启动时把遗留的 `running` Run 重置为 `queued`，从最后一条 Checkpoint 之后继续。人工等待同样只靠账本，不靠内存。
- **单写者**：平台没有 lease/心跳表，`ResetRunningRuns` 会接管所有 `running`。因此同一时间只应运行一个副本；Kubernetes 清单用 `replicas: 1`、`maxSurge: 0`、`maxUnavailable: 1`。
- **优雅停机**：收到 SIGTERM/中断后，`/readyz` 先转 `503`，不再接受新 Run；Dispatcher 停止领取新 Run 并给在飞 Step 最多 `STEP_TIMEOUT + 5s` 收尾；随后 `ResetRunningRuns` 把未完成的 Run 归零为 `queued`，最后关闭 HTTP。Kubernetes 清单的 `terminationGracePeriodSeconds: 30` 覆盖这段过程。
- **指标**：`/metrics` 暴露 Run 状态、Step 重试、模型 fallback 与耗时、工具调用、上下文 token、租户在飞数量。指标不是账本，重启会丢（[ADR 0018](docs/adr/0018-metrics-are-not-the-ledger.md)）。
- **看运行情况**：没有 Dashboard 界面，这是设计决定——所有交互走 API/CLI。`scripts/status.ps1` 一次打印一屏快照：容器状态、`/healthz` 与 `/readyz`、按状态汇总的 Run、仍在飞的 Run（`queued`/`running`/`waiting_human`）、最近五次 Run，以及从账本本身读出的 Checkpoint/Span 条数。

  ```powershell
  powershell -File scripts/status.ps1
  ```
- **部署**：清单见 [`k8s/`](k8s/)，说明见 [`k8s/README.md`](k8s/README.md)。部署只做到能讲清楚，不追求自动扩缩容（[ADR 0019](docs/adr/0019-deploy-only-as-far-as-it-explains-itself.md)）。

## 测试

数据库测试默认跳过；设置 `TEST_DATABASE_URL` 后运行。测试会迁移并 **truncate** 目标库的 `public` 表，务必指向专用测试库，不要指向演示或生产库。

```powershell
go build ./...
go vet ./...
gofmt -l .

$env:TEST_DATABASE_URL = 'postgres://postgres:postgres@localhost:5433/platform?sslmode=disable'
go test ./... -count=1 -p 1
```

Compose 的 `db` 不映射宿主机端口；本机测试库需要自行启动并保证可写。

## 文档

- [`GLOSSARY.md`](GLOSSARY.md) — 共享术语。只有定义，没有设计。
- [`docs/agents/`](docs/agents/) — Issue 跟踪、triage 标签、领域文档的仓库约定。

架构决策记录（[`docs/adr/`](docs/adr/)）：

| ADR | 决策 |
| --- | --- |
| [0001](docs/adr/0001-platform-owns-execution.md) | 平台拥有执行循环，客户端只提供步骤处理器 |
| [0002](docs/adr/0002-tool-identity-is-name-and-version.md) | 工具的身份是 (name, version)，不是 name |
| [0003](docs/adr/0003-fallback-only-on-retryable-errors.md) | Fallback 只吃可重试的错误，且对调用方可见 |
| [0004](docs/adr/0004-context-without-retrieval.md) | 平台提供上下文，不提供检索实现 |
| [0005](docs/adr/0005-step-is-a-primitive-call.md) | Step 是一次原语调用，不是一次 Handler 调用 |
| [0006](docs/adr/0006-policy-is-called-per-step.md) | 每步向 Policy 要一次决策，超时算 Step 的失败 |
| [0007](docs/adr/0007-model-gateway-stands-alone.md) | 模型网关可以脱离 Run 使用，run_id 只是分摊维度 |
| [0008](docs/adr/0008-go-for-platform-and-policy.md) | 平台与策略都是 Go，进程边界保留 |
| [0009](docs/adr/0009-ledger-is-append-only.md) | 账本只追加，等待不是一条记录 |
| [0010](docs/adr/0010-span-in-the-same-transaction.md) | Span 与 Checkpoint 同库同事务 |
| [0011](docs/adr/0011-runtime-not-an-app-builder.md) | 这是一套运行时，不是一个小号的 Dify |
| [0012](docs/adr/0012-one-binary-split-ready.md) | 交付一个二进制，把可拆分性当交付物 |
| [0013](docs/adr/0013-routing-by-label-and-chain.md) | 路由是标签加有序链，不是评分 |
| [0014](docs/adr/0014-ledger-exactly-once-side-effects-at-least-once.md) | 账本 exactly-once，副作用 at-least-once |
| [0015](docs/adr/0015-policy-endpoint-is-signed.md) | 平台调 Policy 用 HMAC 签名 |
| [0016](docs/adr/0016-context-is-cut-by-the-platform.md) | 上下文由平台裁剪，Policy 只声明保留 |
| [0017](docs/adr/0017-eval-cases-are-code.md) | 评测用例是代码资产，平台只存快照 |
| [0018](docs/adr/0018-metrics-are-not-the-ledger.md) | 指标不是账本 |
| [0019](docs/adr/0019-deploy-only-as-far-as-it-explains-itself.md) | 部署只做到能讲清楚，不抄 HPA |
| [0020](docs/adr/0020-five-states-and-a-reason.md) | 状态只有五个，原因不在状态里 |
| [0021](docs/adr/0021-isolation-in-the-app-layer.md) | 隔离靠应用层，密钥只存哈希 |
| [0022](docs/adr/0022-two-routes-two-credentials.md) | 对外与内部是两条路由、两套凭证 |
| [0023](docs/adr/0023-policy-has-no-fail-action.md) | Policy 没有 `fail` action |
| [0024](docs/adr/0024-hmac-secret-is-not-stored.md) | HMAC 密钥不落库 |
| [0025](docs/adr/0025-usage-is-a-query.md) | 用量是查询，不是表 |
