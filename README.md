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

> **Status: design settled, not implemented.** No code exists. Decisions are in
> [`docs/adr/`](docs/adr/); shared vocabulary in [`GLOSSARY.md`](GLOSSARY.md). What gets
> built, and when, is still open — this repository currently answers *what it would be*,
> not *what exists*.

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

## Documents

- [`GLOSSARY.md`](GLOSSARY.md) — the vocabulary. Definitions only, no design in it.
- [`docs/adr/`](docs/adr/) — decisions that are hard to reverse, surprising without
  context, and were a real trade-off. Decisions win over any other document.
