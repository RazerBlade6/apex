# Apex

**A personal project management, design, and development hub that lives in your terminal.**

Apex keeps a persistent model of **you** — your skills, preferences and goals — and of
**your portfolio** — every project you have, its state, and where it's stuck. It uses both
to generate action items on existing projects, propose new projects that actually fit you,
and implement that work at your direction.

It runs on your own API keys, or on a Claude Pro/Max subscription with no API key at all.

[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

---

## How it works

Apex separates two loops, and the separation is the whole design.

**The advisor loop** reads your context, reasons across your entire portfolio at once, and
produces text — action items, project ideas, critiques, plans. This is where Apex's value
lives: nothing else knows both you and all of your projects simultaneously.

**The builder loop** writes code. Apex does not implement it. It assembles context and a
task brief, then dispatches to [Claude Code](https://claude.com/claude-code) running in the
project directory. Apex owns the judgment; the agent owns the edits.

Rather than reading whole repositories, Apex maintains a short **digest** per project — a
few hundred tokens synthesised from your notes, the README and recent git history, cached
against a content-and-git-state hash and regenerated only when something actually changes.
That's what makes cross-portfolio reasoning tractable: ten digests fit comfortably in one
context window, ten repositories do not.

---

## Building

### Prerequisites

| Requirement | Why |
|---|---|
| **macOS or Linux** | `flock(2)` for locking; Windows is unsupported |
| **Go 1.27+** | Building Apex |
| **git** | Project introspection |
| **[Claude Code](https://claude.com/claude-code)** | The builder loop (`apex do`, `apex start`) |
| **An Anthropic or OpenAI API key, _or_ a Claude Pro/Max subscription** | The advisor loop |

Apex has **no C dependencies** and builds with `CGO_ENABLED=0`, so it cross-compiles to a
single static binary with no runtime to install.

### Build from source

```sh
git clone https://github.com/RazerBlade6/apex.git
cd apex
go build -o apex ./cmd/apex
```

That's it. Verify it:

```sh
./apex doctor
```

### Install onto your PATH

```sh
go install ./cmd/apex
```

This puts `apex` in `$(go env GOPATH)/bin`. Add that to your `PATH` if it isn't already:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

### Build with a version stamp

```sh
go build -ldflags "-X main.version=1.2.0" -o apex ./cmd/apex
```

### Cross-compile

Because there's no cgo, any target works from any host:

```sh
GOOS=darwin  GOARCH=arm64 go build -o apex-darwin-arm64  ./cmd/apex
GOOS=darwin  GOARCH=amd64 go build -o apex-darwin-amd64  ./cmd/apex
GOOS=linux   GOARCH=amd64 go build -o apex-linux-amd64   ./cmd/apex
GOOS=linux   GOARCH=arm64 go build -o apex-linux-arm64   ./cmd/apex
```

**macOS and Linux only.** Apex uses `flock(2)` for its per-project and replica locks,
which has no Windows equivalent in the standard library, so Windows is not a supported
target. See [known limitations](#status-and-known-limitations).

### Run the tests

```sh
go test ./...              # 238 tests, entirely offline — no API calls, no cost
go test -race ./...        # concurrency checks
```

The suite never contacts a provider and never spawns the real `claude` binary, so it costs
nothing to run and won't break when you're offline.

---

## Getting started

**1. Check your environment.**

```sh
apex doctor
```

`doctor` verifies every assumption Apex makes — toolchain, credentials, the `claude` binary,
config validity, migration state, and whether your identity files exist. It's read-only and
tells you specifically what's wrong and how to fix it.

**2. Add credentials.** Either a key:

```sh
security add-generic-password -s apex:anthropic -a default -w   # macOS Keychain
# or
export ANTHROPIC_API_KEY=...
```

…or run on a Claude subscription with no key at all — see [Configuration](#configuration).

**3. Tell Apex who you are.** Create `~/.apex/context/PROFILE.md` and `SKILLS.md`. These go
into the system prompt on every advisor call, so keep them tight and high-signal.

**4. Register a project.** Add an entry to `~/.apex/context/PROJECTS.md`:

```markdown
## ScholarRAG
path: ~/Development/ScholarRAG
```

…and drop a `PROJECT.md` in that repo describing what it is and where you're stuck.

**5. Run the loop.**

```sh
apex sync      # read the registry, generate digests
apex review    # propose action items across the portfolio
apex ideas     # propose new projects that fit you
apex do AI-003 # implement one
apex           # or just launch the TUI
```

---

## Commands

| Command | What it does |
|---|---|
| `apex` | Launch the TUI (chat, items, projects) |
| `apex doctor` | Verify toolchain, credentials, config, migrations, identity files |
| `apex doctor --probe` | Also make one minimal real call per provider to prove credentials work |
| `apex config` | Show resolved configuration and where each credential came from |
| `apex sync [project]` | Refresh the registry and regenerate stale digests |
| `apex sync --dry-run` | Report what would be regenerated, without spending anything |
| `apex review [project]` | Generate action items |
| `apex ideas` | Propose new projects |
| `apex items [--status s]` | List action items, grouped by project |
| `apex show <id>` | Detail for an action item or idea |
| `apex do <id>` | Dispatch an action item to the coding agent |
| `apex start <idea-id>` | Scaffold a new project from an idea |
| `apex profile` | List what Apex has learned about you |
| `apex profile --forget <n>` | Remove one learned observation |

The non-interactive commands are cron-friendly — "every Monday, refresh my action items" is
a one-line crontab entry.

---

## The TUI

`apex` with no arguments opens a [Bubble Tea](https://github.com/charmbracelet/bubbletea)
interface with three views on `tab`:

- **Chat** — your prompts and the input field on the left, Apex's replies on the right,
  streamed and rendered as markdown
- **Items** — action items as cards grouped by project, with the selected item's project on
  the left and its full detail on the right; filtered with `f`, dispatched with `enter`
- **Projects** — the registry with digest previews and staleness indicators

Chat splits the two speakers into their own columns, so a one-line question does not get
the same measure as the answer to it:

```
                              Chat       Items       Projects

  3 digest(s) · claude-cli/opus
╭─────────────────────────────╮ ╭──────────────────────────────────────────────────────────╮
│ what should I pick up this  │ │   ScholarRAG's chunking is the cheapest win: the table-  │
│ evening?                    │ │   aware split is about forty lines and the ingestion     │
│                             │ │   tests already cover it.                                │
│                             │ │                                                          │
│ ╭─────────────────────────╮ │ │                                                          │
│ │ › Ask Apex…             │ │ │                                                          │
│ │ ›                       │ │ │                                                          │
│ ╰─────────────────────────╯ │ │                                                          │
╰─────────────────────────────╯ ╰──────────────────────────────────────────────────────────╯
  tab views · enter send · esc stop · ctrl+c quit                               apex 1.2.0
```

Items is three boxes: the selected item's project on the left, the items as cards grouped by
project in the middle, and the item in full on the right. It opens with nothing selected and
the side boxes empty; `↑↓` picks an item and `esc` puts it back:

```
                              Chat       Items       Projects

  2 item(s) · filter: all · pgup/pgdn scroll the detail
╭─────────────────────────╮ ╭──────────────────────────────────╮ ╭─────────────────────────╮
│ ScholarRAG              │ │ ScholarRAG                       │ │ Make chunking           │
│ ~/Development/ScholarR… │ │ ╭──────────────────────────────╮ │ │ table-aware             │
│                         │ │ │ Make chunking table-aware    │ │ │ AI-001 · proposed ·     │
│ status  active          │ │ │ AI-001 · 2d ago     proposed │ │ │ medium effort           │
│ branch  main · 4f2c9e1… │ │ ╰──────────────────────────────╯ │ │ created 2d ago          │
│ commit  3h ago          │ │ ╭──────────────────────────────╮ │ │                         │
│ Add ingestion tests fo… │ │ │ Cache embeddings between ru… │ │ │ Changes                 │
│ digest  1d ago          │ │ │ AI-002 · 5h ago     accepted │ │ │ Split ingested PDFs on  │
│ items   2 open · 0 done │ │ ╰──────────────────────────────╯ │ │ table boundaries        │
│                         │ │                                  │ │ instead of fixed token  │
│ Digest                  │ │                                  │ │ windows, so a table is  │
│ A retrieval pipeline    │ │                                  │ │ never cut in half.      │
│ over research papers.   │ │                                  │ │                         │
│ …                       │ │                                  │ │ …                       │
╰─────────────────────────╯ ╰──────────────────────────────────╯ ╰─────────────────────────╯
  tab views · ↑↓ move · f filter · enter dispatch · r reload · ctrl+c quit      apex 1.2.0
```

Projects keeps a single pane. Below 60 columns of content Chat falls back to one pane, and
below 76 Items falls back to a single list, rather than rendering unreadably narrow ones.

The palette is [gruvbox](https://github.com/morhetz/gruvbox) dark, written as truecolor hex
and downsampled by termenv on terminals with a smaller palette. [`UI.md`](UI.md) is the
full visual contract.

---

## Configuration

`~/.apex/config.toml`, created with defaults on first run. Set `APEX_HOME` to relocate it.

```toml
[models.advisor]           # cross-portfolio reasoning: review, ideas
provider = "claude-cli"    # anthropic | openai | claude-cli
model    = "opus"
effort   = "high"

[models.chat]
provider = "claude-cli"
model    = "opus"
effort   = "medium"

[models.digest]            # bulk summarisation
provider = "claude-cli"
model    = "opus"
effort   = "low"

[executor]
default = "claudecode"
model   = "opus"
effort  = "high"

[sync]
mode           = "explicit"  # explicit | on_start | scheduled
digest_workers = 2           # bounded — each digest is a subprocess

[store]
backend = "local"            # local | turso
```

### Credentials

**Keys are never stored in config.** They resolve from the macOS Keychain
(`apex:anthropic`, `apex:openai`), then the environment (`ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`), then fail with a message naming exactly what's missing.

### Running on a Claude subscription

Set any route's `provider` to `claude-cli` and Apex runs it through the `claude` CLI on your
Pro/Max subscription — no API key needed. `apex doctor` checks `claude auth status` instead
of looking for a key.

Two tradeoffs worth knowing:

- **Quota is shared** with your own Claude Code usage. If Apex exhausts your session window
  generating digests, you can't use Claude Code for real work until it resets.
- **Prompt caching doesn't apply.** Each call is a fresh subprocess, so the stable prefix is
  re-sent every time. On an API key that prefix caches at roughly a tenth of the cost.

Mixing is supported and is usually the best of both — judgment on the subscription, bulk
work on a key:

```toml
[models.advisor]
provider = "claude-cli"    # subscription

[models.digest]
provider = "anthropic"     # API key — don't spend session quota on summarisation
model    = "claude-opus-5"
```

---

## Context files

Apex reads two kinds of file, in two places.

### `~/.apex/context/`

- **`PROFILE.md`** — who you are, how you work, what makes a project die for you
- **`SKILLS.md`** — what you know, what you're learning
- **`PROJECTS.md`** — the registry: a name and a path per project, with Apex-generated
  summary lines beneath

### `<project>/PROJECT.md`

Each project carries its own detail file, committed to that project's repo:

```markdown
---
name: ScholarRAG
status: active
stack: [python, fastapi, postgres]
---

## What it is
Retrieval system over a personal corpus of academic papers.

## Where I'm stuck
Chunking strategy loses table context, so numeric questions fail.
```

This lives in the project rather than in `~/.apex` deliberately: it's version controlled
alongside the code, survives Apex being reinstalled, moves when the project moves, and is
already in the working tree when a coding agent is dispatched there — so project context
reaches the builder without costing a single token of prompt.

### What Apex writes back

Apex maintains a trailing `## Observed` block in `PROFILE.md` and `SKILLS.md`. When a
conversation reveals something durable — "I always build CLIs in Go", "I'm learning Zig" —
it's recorded with its source so you never have to say it twice:

```markdown
## Observed
<!--apex 2026-09-25--> Builds CLIs in Go by default. — source: said 'I always build my CLIs in Go'
```

Everything outside that block is yours and is never touched. `apex profile` lists what's
been learned; `apex profile --forget <n>` removes an entry.

---

## Status and known limitations

**v1 is complete** — advisor, executor, TUI, and self-maintaining context. Being honest
about the rough edges:

- **`apex do` can't verify its own work unattended.** Under the default permission mode the
  dispatched agent may edit files but not run build or test commands, so it reports changes
  it couldn't check. Pass `--bypass-permissions` to let it verify. A narrow allowlist of
  just build-and-test commands is the better fix and isn't built yet.
- **Deduplication is exact-title only**, so `review` will occasionally propose a paraphrase
  of an item you already have. A duplicate you can dismiss beats a silently dropped one.
- **Remote persistence is unimplemented.** `store.backend = "turso"` is designed but the Go
  driver was prerelease at the time of writing; `local` is the default and works fully.
- **Windows is unsupported.** Locking is built on `flock(2)`, which POSIX provides and
  Windows does not. A `LockFileEx` implementation behind a build tag would close this;
  the rest of the codebase is already platform-clean and cgo-free.
- **`review <project>` is a cost lever, not a quality one.** Narrowing to one project makes
  the advisor worse at exactly the cross-portfolio reasoning that justifies it.

---

## Design

[`DESIGN.md`](DESIGN.md) is the full specification — architecture, data model, interfaces,
concurrency, migration discipline, and the reasoning behind every decision, including the
ones that turned out to be wrong and why. It's the authoritative document; this README is
the summary.

[`UI.md`](UI.md) is the visual contract for the terminal interface: the frame and its
arithmetic, the two-box chat layout, and the palette. It is separate because appearance
changes on a different clock from architecture.

## License

[MIT](LICENSE)
