# Apex

A personal project management, design, and development hub that lives in your terminal.

Apex holds a persistent model of **you** — your profile, skills, and preferences —
and of **your portfolio** — every project you have, its state, and where it's stuck.
It uses both to generate action items on existing projects, propose new ones that
actually fit you, and implement work at your direction.

> **Status: early.** Milestone 1 of 6 is complete. The foundation (config, storage,
> locking, environment checks) works; the parts that make it useful do not exist yet.
> See [Roadmap](#roadmap). Not yet usable for its actual purpose.

## How it works

Apex separates two loops that are usually conflated, and the separation is the
whole design.

**The advisor loop** reads your context, reasons across your entire portfolio at
once, and produces text — action items, project ideas, critiques, plans. This is
what Apex is for, and it's the part nothing else does, because nothing else knows
both you and all of your projects simultaneously.

**The builder loop** writes code. Apex does not implement it. It assembles context
and a task brief, then dispatches to an external coding agent running in the
project directory. Apex owns the judgment; the agent owns the edits.

Rather than reading whole repositories, Apex maintains a short **digest** per
project — a few hundred tokens synthesized from your notes, the README, and recent
git history. That's what makes cross-portfolio reasoning tractable: ten digests fit
comfortably in context, ten repositories do not.

## Requirements

- Go 1.27+
- git
- An Anthropic and/or OpenAI API key (for the advisor loop)
- [Claude Code](https://claude.com/claude-code) (for the builder loop, once M5 lands)

## Install

```sh
git clone https://github.com/RazerBlade6/apex.git
cd apex
go build ./cmd/apex
```

Apex builds with `CGO_ENABLED=0` and has no C dependencies, so it cross-compiles to
a single static binary.

## Usage

What works today:

```sh
apex doctor    # verify toolchain, keys, config, and migration state
apex config    # show resolved configuration and where each key came from
```

`apex doctor` is the place to start. It checks every assumption Apex makes about
your machine and tells you specifically what's wrong and how to fix it, rather than
failing later with something cryptic.

## Configuration

Config lives at `~/.apex/config.toml` and is created with defaults on first run.
Set `APEX_HOME` to relocate it.

```toml
[models.advisor]           # cross-portfolio reasoning: review, ideas
provider = "anthropic"
model    = "claude-opus-5"
effort   = "high"

[models.chat]
provider = "anthropic"
model    = "claude-opus-5"
effort   = "medium"

[models.digest]            # bulk summarization; the natural cost lever
provider = "anthropic"
model    = "claude-opus-5"
effort   = "low"

[store]
backend = "local"          # local | turso

[sync]
mode = "explicit"          # explicit | on_start | scheduled
```

**API keys are never stored in config.** They resolve from the macOS Keychain
first, then environment variables:

```sh
security add-generic-password -s apex:anthropic -a default -w
# or
export ANTHROPIC_API_KEY=...
```

## Project context

Apex reads two kinds of file, in two places.

`~/.apex/context/` holds who you are — `PROFILE.md` and `SKILLS.md`, both
hand-written — and `PROJECTS.md`, the registry. You register a project there with a
name and a path; Apex generates the summary line beneath it.

Each project then carries its own `PROJECT.md`, committed to that project's repo.
This is deliberate: the file travels with the project, survives Apex being
reinstalled, and is already in the working tree when a coding agent is dispatched
there — so project context reaches the builder without costing a token.

```markdown
---
name: ScholarRAG
status: active
stack: [python, fastapi]
---

## What it is
Retrieval system over a personal corpus of academic papers.

## Where I'm stuck
Chunking strategy loses table context, so numeric questions fail.
```

## Roadmap

| | Milestone | Status |
|---|---|---|
| M1 | Skeleton — config, storage, migrations, locking, `doctor` | done |
| M2 | Context — markdown parsing, registry, git introspection | next |
| M3 | Providers — Anthropic and OpenAI, streaming and structured output | |
| M4 | Advisor — digests, `review`, `ideas`, `items` | |
| M5 | Executor — dispatch, `do`, `start` | |
| M6 | TUI — chat, items, and projects views | |

M1–M4 produce a genuinely useful tool. M5 closes the loop. M6 makes it pleasant.

## Design

[`DESIGN.md`](DESIGN.md) is the full v1 specification — architecture, data model,
interfaces, concurrency, and the reasoning behind each decision. It is the
authoritative document; this README is the summary.

## License

[MIT](LICENSE)
