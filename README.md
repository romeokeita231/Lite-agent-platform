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

> **Status: implemented and runnable.** The platform, the example policy and a local
> demo stack exist in this repository. Decisions live in
> [`docs/adr/`](docs/adr/) and win wherever they disagree with the code; shared
> vocabulary in [`GLOSSARY.md`](GLOSSARY.md).

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

## Local demo

`docker compose up --build` starts PostgreSQL, `platform` and `policy-example`. The
platform container runs `platform seed` before the server: it creates tenants `demo-a`
and `demo-b`, the agents `demo`, `tool-slow` and `human`, the `default` model route
(`demo-model`) and the `echo`/`slow` tools. Seeding is idempotent and prints one JSON
line with both tenant IDs, their plaintext API keys and the policy URL; those keys are
printed nowhere else.

```sh
cp .env.example .env
docker compose up --build
```

`MODEL_BASE_URL` defaults to `policy-example`'s own OpenAI-compatible
`/v1/chat/completions` stub, so every demo run performs a real HTTP model call. Point it
at another OpenAI-compatible provider and set `MODEL_API_KEY` to use that provider
instead.

Three PowerShell scripts demonstrate the isolation properties in
[ADR 0012](docs/adr/0012-one-binary-split-ready.md):

| script | shows |
| --- | --- |
| `scripts/demo-1-concurrency.ps1` | tenant A saturates its concurrency limit; tenant B's run is not queued behind it |
| `scripts/demo-2-tool-hang.ps1` | a hung tool endpoint fails one run after `STEP_TIMEOUT`; the other runs complete |
| `scripts/demo-3-crash.ps1` | killing the policy shows retries and `policy_unreachable`; killing the platform mid-run resumes from the ledger |

Each accepts `-BaseUrl`, `-ApiKeyA` and `-ApiKeyB` (matching the seed output), and exits
non-zero on failure, e.g. `powershell -File scripts/demo-1-concurrency.ps1`.

## Documents

- [`GLOSSARY.md`](GLOSSARY.md) — the vocabulary. Definitions only, no design in it.
- [`docs/adr/`](docs/adr/) — decisions that are hard to reverse, surprising without
  context, and were a real trade-off. Decisions win over any other document.
