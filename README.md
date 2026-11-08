# Lite Agent Platform

> Chinese version: [README.zh.md](README.zh.md)

A small Go runtime for durable, observable agent execution. The platform owns the execution loop: it drives each step, checkpoints after it, lets a run stop and wait for a human, and picks the run back up after the process that started it is gone.

Business logic lives on the other side of a policy boundary: before every step the platform asks the business process what to do next, and the business process answers from the ledger without remembering anything. The platform knows nothing about what an agent is trying to accomplish.

It is all Go, and the policy is a separate Go binary talking the same HTTP contract. The split is not about languages — it is about the platform never executing someone else's code in its own process. See [ADR 0008](docs/adr/0008-go-for-platform-and-policy.md).

> **Status: implemented and runnable.** This repository contains the platform, an example Policy, and a local demo stack. The 25 decision records live in [`docs/adr/`](docs/adr/) and are the spec: if code and an ADR disagree, the ADR wins. Shared vocabulary is in [`GLOSSARY.md`](GLOSSARY.md).

## How a run works

Six lines. If any of them ever disagree with an ADR, the ADR wins.

1. A tenant posts `POST /v1/runs` with an `agent_id` and an input. The run is `queued`.
2. The platform asks that agent's Policy one question: what is the next step?
3. The Policy answers with an action — call a model, call a tool, or ask a human.
4. The platform performs the action, appends one Checkpoint, and writes the Span beside it in the same transaction.
5. It asks again. Repeat until the Policy answers *done*, or a step fails past its retries.
6. A step that needs a human parks the run at `waiting_human`. Nothing is appended until the answer arrives, and no process waits for it. The platform dying mid-run is the same situation: the run resumes from the last Checkpoint.

A run is only ever `queued`, `running`, `waiting_human`, `completed`, `failed`, or `cancelled`. The terminal reason is recorded separately: `step_timeout`, `policy_unreachable`, `max_retries`, `tool_schema_violation`, `policy_invalid_action`, or `cancelled_by_tenant`. A step is only ever `llm_call`, `tool_call`, or `human_review`; `done` is a closing Checkpoint, not a fourth action.

## What it is not

- Not a business application, and not bound to any vertical.
- Not a workflow engine: there is no DSL to author, and no DAG to draw.
- Not a RAG stack: retrieval is an interface the platform exposes, not something it implements. See [ADR 0004](docs/adr/0004-context-without-retrieval.md).
- Not a billing system: it collects usage, it does not invoice.
- Not polyglot. There is one toolchain here on purpose; the boundary that matters is a process boundary, not a language boundary.

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/platform` | Platform binary; `seed` and `keygen` subcommands |
| `cmd/policy-example` | Example Policy, also an OpenAI-compatible model stub and demo tool endpoints |
| `internal/runtime` | Dispatcher, step execution, retries, cancellation, context trimming |
| `internal/repository` | PostgreSQL storage and transaction boundaries |
| `internal/migrate` | Embedded migrations (`internal/migrate/sql/`) |
| `internal/httpapi` | HTTP API, authentication, evals, usage queries |
| `internal/modelgw` | Model gateway: routes, fallback chains, call records |
| `internal/toolgw` | Tool resolution, schema validation, invocation |
| `internal/contextwin` | Context window trimming |
| `internal/retrieval` | Retrieval interface (no implementation) |
| `internal/observability` | Prometheus metrics and readiness |
| `internal/domain` | Domain types and tenant context |
| `internal/config` | Environment loading |
| `scripts/` | Three PowerShell demo scripts |
| `k8s/` | Minimal Kubernetes manifests |
| `docs/adr/` | 25 architecture decision records |
| `docs/agents/` | Agent-facing operating notes |

## Quick start

### Prerequisites

- Go 1.26 (only needed to run binaries directly)
- Docker and Docker Compose (needed for the local demo stack)

### Run the local stack

```powershell
Copy-Item .env.example .env
docker compose up --build
```

Compose starts three services:

- `db`: PostgreSQL (`pgvector/pgvector:pg16`), data in the `pgdata` volume.
- `platform`: runs `platform seed` before serving on `:8080`.
- `policy-example`: the example Policy on `:9090`, also serving `/v1/chat/completions` as a model stub and `/demo/tool/echo`, `/demo/tool/slow` as tool endpoints.

`platform seed` creates tenants `demo-a` and `demo-b`, agents `demo`, `tool-slow`, `human`, `quick`, model routes `default`/`cheap`/`fast` (model `demo-model`), and tools `echo`, `slow`. Seeding is idempotent and prints one JSON line containing `policy_url`, both tenant IDs, each tenant's plaintext API key, and the agent roster; those keys are not printed anywhere else.

`MODEL_BASE_URL` defaults to the OpenAI-compatible stub shipped with `policy-example`, so every model step in the demo makes a real HTTP call. Point it at another OpenAI-compatible service and set `MODEL_API_KEY` to switch.

### First call

Agent `quick` returns `done` at step 0, which makes it a good smoke test:

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

Poll `GET /v1/runs/{id}` afterwards. Agent `demo` walks through model, tool, and human steps, so it needs `POST /v1/runs/{id}/human-input` before it reaches `completed`.

### Run the binaries directly

You need a reachable PostgreSQL. Migrations run automatically when the process starts.

```powershell
$env:DATABASE_URL = 'postgres://postgres:postgres@localhost:5433/platform?sslmode=disable'
$env:POLICY_SECRET_DEMO = 'demo-policy-secret'
$env:POLICY_SECRET_TOOL_SLOW = 'demo-policy-secret'
$env:POLICY_SECRET_HUMAN = 'demo-policy-secret'
go run ./cmd/platform seed
go run ./cmd/platform
```

If `DEMO_API_KEY_A`/`DEMO_API_KEY_B` are unset, `seed` generates a random key for each and prints them in its output. Secrets live in environment variables only and are never stored in the database.

`POLICY_URL` defaults to the compose-internal `policy-example`. When running outside Compose, either point it at a reachable Policy or use agent `quick` to exercise the platform alone.

### Build images

```powershell
docker build --target platform -t lite-agent-platform:local .
docker build --target policy-example -t lite-agent-platform-policy-example:local .
```

`keygen` prints a generated API key with its hash and prefix:

```powershell
go run ./cmd/platform keygen
```

Output: `{"api_key": "...", "api_key_hash": "...", "api_key_prefix": "..."}`.

## Demo scripts

Three PowerShell scripts demonstrate the isolation properties in [ADR 0012](docs/adr/0012-one-binary-split-ready.md). They are verified against Windows PowerShell 5.1 (`powershell.exe`, not `pwsh`).

| Script | What it demonstrates |
| --- | --- |
| `scripts/demo-1-concurrency.ps1` | While tenant A saturates its concurrency limit, tenant B's runs are not stuck behind A's |
| `scripts/demo-2-tool-hang.ps1` | A hanging tool endpoint makes one run fail after `STEP_TIMEOUT`; other runs still complete |
| `scripts/demo-3-crash.ps1` | Killing the policy shows retries and `policy_unreachable`/`max_retries`; killing the platform mid-run shows the run resuming from the ledger |

Common parameters:

- `-BaseUrl`, default `http://localhost:8080`
- `-ApiKeyA`, `-ApiKeyB`, default to `DEMO_API_KEY_A`/`DEMO_API_KEY_B`, then to `demo-a-api-key`/`demo-b-api-key`

`demo-2` also takes `-DemoRuns` (default 3); `demo-3` also takes `-PolicyHealthUrl` (default `http://localhost:9090/healthz`) and starts/stops services through `docker compose`. The scripts exit non-zero on failure:

```powershell
powershell -File scripts/demo-1-concurrency.ps1
powershell -File scripts/demo-2-tool-hang.ps1
powershell -File scripts/demo-3-crash.ps1
```

## Configuration

### Platform process

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | none, required | PostgreSQL connection string |
| `HTTP_ADDR` | `:8080` | HTTP listen address |
| `STEP_TIMEOUT` | `10s` | Per-step timeout; also the timeout basis for Policy decisions, model calls, and tool calls |
| `MAX_STEP_ATTEMPTS` | `4` | Maximum attempts for one step; backoff starts at 1s, doubles, caps at 30s |
| `TENANT_CONCURRENCY` | `4` | Maximum concurrently advancing runs per tenant; must be positive |
| `TENANT_RATE` | `0` | Run pickup rate per tenant (tokens per second); `0` disables the limiter; must be non-negative |
| `MODEL_BASE_URL` | empty | Base URL of an OpenAI-compatible service; required for model steps and `/v1/chat/completions` |
| `MODEL_API_KEY` | empty | Bearer key for the model service |
| `MODEL_CHAIN_BUDGET` | equals `STEP_TIMEOUT` | Total budget for an entire fallback chain within one model call |
| `CONTEXT_RECENT` | `8` | Number of most recent steps sent to the Policy |
| `CONTEXT_MAX_KEEP` | `16` | Upper bound on steps the Policy may retain via `keep` |
| `POLL_INTERVAL` | `100ms` | Dispatcher polling interval for runnable runs |

`docker-compose.yml` overrides some values for the demo: `STEP_TIMEOUT=5s`, `MAX_STEP_ATTEMPTS=3`, `TENANT_CONCURRENCY=1`.

An agent's HMAC secret is read by name from the platform process environment (`secret_env` stores only the variable name). The demo stack uses:

| Variable | Used by |
| --- | --- |
| `POLICY_SECRET_DEMO` | Agents `demo` and `quick` |
| `POLICY_SECRET_TOOL_SLOW` | Agent `tool-slow` |
| `POLICY_SECRET_HUMAN` | Agent `human` |

### Example Policy

| Variable | Default | Meaning |
| --- | --- | --- |
| `POLICY_ADDR` | `:9090` | Listen address |
| `POLICY_SECRET` | none, required | Verifies the platform's HMAC signature |
| `POLICY_MODEL` | `route:default` | Model route requested by agent `demo` at step 0 |

### seed

| Variable | Default | Meaning |
| --- | --- | --- |
| `POLICY_URL` | `http://policy-example:9090` | Policy address written into agents |
| `DEMO_API_KEY_A` | randomly generated | API key for tenant `demo-a` |
| `DEMO_API_KEY_B` | randomly generated | API key for tenant `demo-b` |
| `DEMO_MODEL` | `demo-model` | Model for route `default` |
| `DEMO_MODEL_CHEAP` | equals `DEMO_MODEL` | Model for route `cheap` |
| `DEMO_MODEL_FAST` | equals `DEMO_MODEL` | Model for route `fast` |

`.env.example` holds demo values only:

```dotenv
POLICY_SECRET=demo-policy-secret
DEMO_API_KEY_A=demo-a-api-key
DEMO_API_KEY_B=demo-b-api-key
MODEL_BASE_URL=http://policy-example:9090/v1
MODEL_API_KEY=
```

## HTTP API

`/healthz`, `/readyz`, and `/metrics` are public. Every other `/v1/*` route requires `Authorization: Bearer <tenant API key>` and can only see that tenant's data. Cross-tenant access to runs, agents, evals, and usage returns `404 not_found`; an unresolvable tool version returns `400 bad_request`. Errors share one body shape:

```json
{"error":"not_found","message":"run not found"}
```

| Method and path | Meaning |
| --- | --- |
| `GET /healthz` | Liveness, always `200` |
| `GET /readyz` | Readiness; `503` while the database is unavailable, migrations are incomplete, or the process is draining |
| `GET /metrics` | Prometheus metrics, with no tenant labels |
| `POST /v1/agents` | Register an agent: `name`, `policy_url`, `secret_env`; duplicate name `409` |
| `GET /v1/agents` | List this tenant's agents |
| `POST /v1/runs` | Submit a run: `agent_id`, `input`; unknown or cross-tenant agent `404`; while draining `503` |
| `GET /v1/runs/{id}` | Run detail with `status`, `output`, `terminal_reason`, `pending_step` |
| `GET /v1/runs/{id}/steps` | Checkpoints in order |
| `GET /v1/runs/{id}/trace` | Spans grouped by step |
| `POST /v1/runs/{id}/human-input` | Answer a human step: `step_index`, `input`; `409` if the run is not waiting or the index is wrong |
| `POST /v1/runs/{id}/cancel` | Request cancellation; `409` if already terminal |
| `POST /v1/tools` | Register a tool version: `tool_name`, `version`, `endpoint`, `schema` (all required), plus `description`, `is_default` |
| `GET /v1/tools` | List this tenant's tools |
| `POST /v1/tools/{tool_name}/invoke` | Invoke a tool directly: optional `version`, `args` must be an object and is validated against the schema |
| `POST /v1/chat/completions` | OpenAI-compatible entry point; `model` may be `route:<label>` or a model name; also returns a `lite` object (effective model, chain, fallback_from, token usage) |
| `GET /v1/models` | List models in routing chains and `route:<label>` entries |
| `GET /v1/tenants/{id}/usage` | Usage query; `group_by=model\|run`, `from`/`to` are RFC3339; cross-tenant `404` |
| `POST /v1/evals` | Run a set of eval cases, waiting synchronously for up to 60s, and return a report with per-case results |
| `GET /v1/evals/{id}` | Read an eval report |

An eval request takes a `cases` array whose items look like:

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

`expect.output` and `expect.tool_calls[].args` are subset matches; `tool_calls` are matched in order. The report's `passed` and `failures` name exactly which field differed.

## Policy contract

The platform posts to `POST {policy_url}/internal/policy/decide` with:

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

Headers:

- `X-Agent-Id`: agent ID
- `X-Timestamp`: RFC3339 timestamp
- `X-Signature`: `HMAC-SHA256(secret, METHOD + "\n" + PATH + "\n" + SHA256_HEX(body) + "\n" + TIMESTAMP)`

The example Policy only accepts timestamps within ±5 minutes. HMAC secrets are never stored; they are read from the environment by `secret_env`.

The response carries one of four actions:

| `action` | Fields | Platform behavior |
| --- | --- | --- |
| `llm_call` | `route`, `messages`, optional `keep` | Calls through the model gateway; `route:<label>` follows a chain, otherwise the model name is used |
| `tool_call` | `tool`, `args`, optional `version`, `keep` | Resolves the default version, validates against the schema, invokes, records |
| `human_review` | `prompt`, optional `deadline_seconds`, `keep` | Parks the run at `waiting_human` and records `pending_step` without holding a process |
| `done` | `output`, optional `keep` | Appends the closing Checkpoint and moves the run to `completed` |

The Policy has no `fail` action ([ADR 0023](docs/adr/0023-policy-has-no-fail-action.md)). The platform decides terminal reasons when a step should fail. `keep` declares which historical steps must survive trimming, capped by `CONTEXT_MAX_KEEP` ([ADR 0016](docs/adr/0016-context-is-cut-by-the-platform.md)).

Model fallback happens only on retryable errors: 429, 5xx, timeouts, and connection failures. Callers can see the effective model in `lite` or in the Checkpoint ([ADR 0003](docs/adr/0003-fallback-only-on-retryable-errors.md), [ADR 0013](docs/adr/0013-routing-by-label-and-chain.md)).

## Operations

- **Migrations** run at startup under an advisory lock, tracked in `schema_migrations`. Applied migrations are never edited; only new files are appended.
- **Idempotent seed**: rerunning it updates the same tenants, agents, routes, and tools instead of duplicating them.
- **Recovery**: at startup, leftover `running` runs are reset to `queued` and continue after the last Checkpoint. Human waits likewise depend only on the ledger, not on memory.
- **Single writer**: there is no lease or heartbeat table, and `ResetRunningRuns` takes over every `running` run. Run one replica at a time; the Kubernetes manifest uses `replicas: 1`, `maxSurge: 0`, `maxUnavailable: 1`.
- **Graceful shutdown**: on SIGTERM/interrupt, `/readyz` flips to `503` and new runs are refused; the dispatcher stops picking up runs and gives in-flight steps up to `STEP_TIMEOUT + 5s`; `ResetRunningRuns` then returns unfinished runs to `queued`; HTTP closes last. The Kubernetes manifest's `terminationGracePeriodSeconds: 30` covers this sequence.
- **Metrics**: `/metrics` exposes run status, step retries, model fallbacks and latency, tool calls, context tokens, and per-tenant inflight counts. Metrics are not the ledger and are lost on restart ([ADR 0018](docs/adr/0018-metrics-are-not-the-ledger.md)).
- **Deployment**: manifests live in [`k8s/`](k8s/), with notes in [`k8s/README.md`](k8s/README.md). The deployment goes only as far as it can explain itself; autoscaling is out of scope ([ADR 0019](docs/adr/0019-deploy-only-as-far-as-it-explains-itself.md)).

## Tests

Database-backed tests are skipped by default. Set `TEST_DATABASE_URL` to run them. The tests migrate and **truncate** the target database's `public` tables, so always point them at a dedicated test database, never at the demo or production database.

```powershell
go build ./...
go vet ./...
gofmt -l .

$env:TEST_DATABASE_URL = 'postgres://postgres:postgres@localhost:5433/platform?sslmode=disable'
go test ./... -count=1 -p 1
```

Compose's `db` does not publish a host port; start your own test database locally and make sure it is writable.

## Documents

- [`GLOSSARY.md`](GLOSSARY.md) — shared vocabulary. Definitions only, no design in it.
- [`docs/agents/`](docs/agents/) — repo conventions for issue tracking, triage labels, and domain docs.

Architecture decision records ([`docs/adr/`](docs/adr/)):

| ADR | Decision |
| --- | --- |
| [0001](docs/adr/0001-platform-owns-execution.md) | The platform owns the execution loop; clients supply step handlers |
| [0002](docs/adr/0002-tool-identity-is-name-and-version.md) | A tool's identity is (name, version), not name |
| [0003](docs/adr/0003-fallback-only-on-retryable-errors.md) | Fallback consumes only retryable errors and is visible to callers |
| [0004](docs/adr/0004-context-without-retrieval.md) | The platform provides context, not a retrieval implementation |
| [0005](docs/adr/0005-step-is-a-primitive-call.md) | A step is a primitive call, not a handler call |
| [0006](docs/adr/0006-policy-is-called-per-step.md) | The policy is asked once per step, and a timeout is a step failure |
| [0007](docs/adr/0007-model-gateway-stands-alone.md) | The model gateway works without a run; `run_id` is only an attribution axis |
| [0008](docs/adr/0008-go-for-platform-and-policy.md) | Platform and policy are both Go; the process boundary remains |
| [0009](docs/adr/0009-ledger-is-append-only.md) | The ledger is append-only; waiting is not a record |
| [0010](docs/adr/0010-span-in-the-same-transaction.md) | Spans share the checkpoint's database and transaction |
| [0011](docs/adr/0011-runtime-not-an-app-builder.md) | This is a runtime, not a smaller Dify |
| [0012](docs/adr/0012-one-binary-split-ready.md) | Ship one binary and treat the split as a deliverable |
| [0013](docs/adr/0013-routing-by-label-and-chain.md) | Routing is labels plus ordered chains, not scoring |
| [0014](docs/adr/0014-ledger-exactly-once-side-effects-at-least-once.md) | Ledger exactly-once, side effects at-least-once |
| [0015](docs/adr/0015-policy-endpoint-is-signed.md) | Platform-to-policy calls are HMAC-signed |
| [0016](docs/adr/0016-context-is-cut-by-the-platform.md) | The platform cuts context; the policy only declares what to keep |
| [0017](docs/adr/0017-eval-cases-are-code.md) | Eval cases are code assets; the platform stores snapshots only |
| [0018](docs/adr/0018-metrics-are-not-the-ledger.md) | Metrics are not the ledger |
| [0019](docs/adr/0019-deploy-only-as-far-as-it-explains-itself.md) | Deploy only as far as it explains itself; no HPA |
| [0020](docs/adr/0020-five-states-and-a-reason.md) | Five states, with the reason kept outside them |
| [0021](docs/adr/0021-isolation-in-the-app-layer.md) | Isolation lives in the app layer; only key hashes are stored |
| [0022](docs/adr/0022-two-routes-two-credentials.md) | External and internal are two routes with two credential sets |
| [0023](docs/adr/0023-policy-has-no-fail-action.md) | The policy has no `fail` action |
| [0024](docs/adr/0024-hmac-secret-is-not-stored.md) | The HMAC secret is not stored |
| [0025](docs/adr/0025-usage-is-a-query.md) | Usage is a query, not a table |
