# AGENTS.md

Operating notes for agents working in this repository.

## Agent skills

### Issue tracker

Issues and specs live as GitHub issues, managed with the `gh` CLI.
See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage roles use their default label names.
See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` and `docs/adr/` at the repo root.
See `docs/agents/domain.md`.

## Status

Design settled, implementation not started. The 22 ADRs under `docs/adr/` are the spec and
`README.md` is the entry point. Read the ADR covering the area before proposing a change to
it, and say which ADR a change contradicts.

## Language

Design docs, ADRs, `README.md` and `GLOSSARY.md` are written in Chinese. Code, comments,
identifiers, test names and commit messages are English.

## Local environment (Windows)

- Go 1.26 toolchain. Docker for Postgres.
- This repo sits inside `project-for-jobs/`, which is **not** a git repo and holds a second,
  unrelated project. Never `git add` anything from the parent directory.
- `origin` is `github.com/romeokeita231/Lite-agent-platform`. A global
  `url.https://gh-proxy.com/…insteadOf` rewrite routes HTTPS git traffic through the proxy;
  `gh` talks to `api.github.com` directly.
- Secrets live in environment variables only. Nothing credential-shaped gets committed;
  `.env*` is ignored.
