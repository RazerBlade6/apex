# Apex — v1 Design Spec

**Status:** draft · **Date:** 2026-09-21 · **Target:** single-user local CLI

---

## 1. What Apex is

Apex is a personal project-management, design, and development hub that runs as a
terminal application. It holds a persistent model of **you** (profile, skills,
preferences) and of **your portfolio** (every project, its state, its goals), and
uses both to do three things:

1. **Generate action items** on existing projects.
2. **Propose new projects** that fit your skills and interests.
3. **Execute** — implement an action item, or scaffold a new project — at your direction.

It also takes free-form requests through a chat interface, with the same context
loaded.

### Goals (v1)

- Single-user, local-first, runs on one machine.
- Bring-your-own API keys (OpenAI and Anthropic).
- Useful without any cloud component.
- Both an interactive TUI and scriptable non-interactive commands.

### Non-goals (v1)

- Multi-user, accounts, or hosting (remote *persistence* is in scope — see §7).
- Web or GUI frontend.
- Retrieval / embeddings / vector search.
- Automatic project discovery by scanning the filesystem.

---

## 2. Core architecture: two loops

The central design principle is that Apex contains two distinct loops that must
stay separate.

### The advisor loop

Reads context, reasons across the whole portfolio, and emits **text**: action
items, project ideas, critiques, plans. Mostly read-only, relatively cheap, and
this is where all of Apex's novelty lives — nothing else knows both you and your
full project set at once.

Apex owns this loop completely.

### The builder loop

Writes code, runs commands, makes commits. Expensive, risky, and already solved
by existing tools.

**Apex does not implement this loop.** It assembles context and a task brief,
then dispatches to an external coding agent (`claude`) running in the project
directory. Apex owns the judgment; the agent owns the edits.

This is deliberate. Rebuilding tool-use plumbing, permission prompts, diff
review, and context management would consume most of the effort available and
produce something worse than what already exists. It also keeps Apex's own
provider interface tiny — it needs streaming text and structured output, nothing
more.

The dispatch target sits behind a Go interface so a native loop can be added later
without reworking anything around it.

---

## 3. Stack

| Concern | Choice |
|---|---|
| Language | Go 1.27 |
| TUI | Bubble Tea + Lipgloss + Bubbles |
| Markdown rendering | Glamour |
| CLI | Cobra |
| Storage | `modernc.org/sqlite` (pure Go) for `LocalBackend`; `tursogo` arrives with `TursoBackend` |
| Config | TOML |
| Secrets | macOS Keychain, env var fallback |
| Anthropic | `github.com/anthropics/anthropic-sdk-go` |
| OpenAI | `github.com/openai/openai-go` |

**Why Go:** single static binary (the distribution story that makes a public
release viable), the Charm ecosystem — Glamour matters specifically because
everything Apex emits is markdown — goroutines mapping cleanly onto parallel
digest refresh plus streaming plus subprocess supervision, ~5ms startup, and
existing fluency.

The known cost is that provider content blocks are sum types, which Go models
awkwardly. Dispatching the builder loop keeps that pain contained, because the
tool-use plumbing lives inside the dispatched agent rather than inside Apex.

> **Never use a CGO SQLite driver.** `mattn/go-sqlite3` and `go-libsql` both need
> CGO, which forfeits static linking and cross-compilation — the main reason Go was
> chosen. `tursogo` avoids CGO by calling prebuilt native libraries through `purego`.
> That is a step down from fully pure Go (`modernc.org/sqlite`) but preserves
> cross-compilation. See §7 for the backend interface that keeps this reversible.

---

## 4. Package layout

```
apex/
  cmd/apex/main.go
  internal/
    config/      config.toml loading, keychain access, model routing
    contextfs/   markdown parsing: PROFILE, SKILLS, PROJECTS, PROJECT
    project/     registry, git introspection, digest source assembly
    store/       SQLite: schema, migrations, queries
    provider/    Provider interface
      anthropic/ Anthropic adapter
      openai/    OpenAI adapter
    advisor/     digests, review, ideas, chat — composes context + provider
    executor/    Executor interface
      claudecode/ dispatch to the claude CLI
    tui/         Bubble Tea application
  DESIGN.md
  go.mod
```

`internal/advisor` is the core. Everything else exists to feed it or to act on
its output.

**`internal/` matters for the v2 story:** it keeps the package boundaries private
until they're stable, so extracting a reusable library later is a deliberate
choice rather than an accident of import paths. `tui/` must not be imported by
anything below it.

---

## 5. State layout on disk

Apex state lives in `~/.apex/`, entirely separate from the Apex source repo.

```
~/.apex/
  config.toml              model routing, executor choice, preferences
  apex.db                  SQLite: items, ideas, digests, sessions, runs
  context/
    PROFILE.md             who you are, how you work, what you care about
    SKILLS.md              what you know, what you're learning
    PROJECTS.md            the registry (see §6)
  logs/
    exec/<run-id>.log      captured output from dispatched runs
```

Keys are **never** written here. They live in the Keychain.

Isolating all state under one root is what makes the eventual multi-user story
tractable — that directory becomes a per-user scope rather than something to
untangle.

---

## 6. Context model

### Two-tier project context

**`~/.apex/context/PROJECTS.md`** — the registry. You hand-register projects
with a name and a path; Apex generates the summary line beneath each entry on
`apex sync`. This is the only file that answers "which projects exist and where
do they live," which is a job per-project files genuinely cannot do.

The generated summary carries an explicit marker. Identifying it *positionally* —
"the line after `path:`" — works but makes a hand-edit that shifts a line silently
destructive. A marker makes the round trip safe by construction, and the format has
no users yet, so this costs nothing now and is unfixable later.

```markdown
# Projects
<!-- Summary lines are generated by `apex sync`. Edit each project's PROJECT.md instead. -->

## ScholarRAG
path: ~/Development/ScholarRAG
<!--apex-->Retrieval over academic papers. Active; blocked on chunking.

## Apex
path: ~/Development/Apex
Personal project management and development hub. Building v1 skeleton.
```

**`<project>/PROJECT.md`** — the detail, living inside each project's own
directory and committed to its repo.

```markdown
---
name: ScholarRAG
status: active
stack: [python, fastapi, postgres]
started: 2026-03-14
---

## What it is
Retrieval system over a personal corpus of academic papers.

## Current state
Ingest and embedding pipeline work. Query API returns results.

## Where I'm stuck
Chunking strategy loses table context, so numeric questions fail.

## Goals
- Table-aware chunking
- Citation spans in responses
```

**Why the detail file lives in the project, not in `~/.apex`:**

1. It travels with the project — version controlled, survives an Apex reinstall,
   moves when the project moves, visible to anyone who clones the repo.
2. **Dispatched agents pick it up for free.** When Apex runs `claude` in that
   directory, `PROJECT.md` is already in the working tree. Project context reaches
   the builder without occupying a single token of Apex's own prompt.
3. You edit it where you already are when you notice something worth recording.

Note that this split is **not** a context-size optimization. Context savings come
from selective loading, which either layout supports. The justification is
portability and agent-visibility.

### Project identity and renaming

**`path` is a project's identity, not its name.** The slug derives from the name, so
renaming `## Apex` to `## Apex CLI` would otherwise create a second row and orphan
the first along with its cached digest — silent, and increasingly expensive as
digests accumulate.

`apex sync` therefore matches registry entries to existing rows **by path first**. A
matched row whose name changed is re-keyed, carrying its digest and history forward.
An entry whose path no longer appears in the registry is reported as orphaned and
pruned only on explicit confirmation, never silently.

### Identity context

`PROFILE.md` and `SKILLS.md` are hand-written and read-only to Apex. They are
loaded into the system prompt for every advisor call. Apex never writes to them
in v1 — it may *suggest* additions in chat, but the user applies them.

### Digests

A **digest** is a ~200–300 token synthesis of one project's current state,
generated by Apex and cached in SQLite. This is the unit the advisor loop reasons
over.

Sources, in priority order:
- `PROJECT.md` (authoritative — the user's own words)
- `README.md` if present
- `git log --oneline -20`
- `git status --short`

Digests are cached against a `source_hash`. `apex sync` regenerates only projects
whose hash changed, which keeps a full sync cheap once warm.

**The hash covers every source, and the dirty state is a digest of `git status
--short`, never a boolean.** A boolean is broken in the case that matters most: once
a tree goes dirty it stays dirty, so the hash stops moving and the cached digest
freezes for exactly the projects being actively worked on. `README.md` must be
hashed for the same reason — in a non-git project (which §6 explicitly allows) a
README edit would otherwise be invisible to the cache forever.

```
apex-source-hash-v2\n
project.md:absent\n           | project.md:<N>\n<N bytes>\n
readme.md:absent\n            | readme.md:<N>\n<N bytes>\n
head:<sha>\n                  empty when unborn or not a repository
status:<sha256 of `git status --short` output>\n
```

SHA-256, lowercase hex. Bodies are length-prefixed so no content can forge the
fields that follow it. Line endings normalise CRLF→LF. The project path is
deliberately **not** hashed: the same commit checked out at two paths is the same
state.

**Why digests rather than reading repos directly:** the advisor's most valuable
work is cross-portfolio reasoning, which requires holding every project in context
at once. Ten digests is ~3K tokens; ten repos is not tractable. Apex drills into
actual source only when a task is scoped to one project.

---

## 7. Data model

SQLite, migrated forward on startup. Schema is single-user; adding a `user_id`
column later is a trivial migration and should not be pre-empted now.

```sql
CREATE TABLE projects (
    slug            TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    path            TEXT NOT NULL,
    status          TEXT,
    registered_at   DATETIME NOT NULL,
    last_synced_at  DATETIME   -- when DIGESTS were last refreshed, not when the
                               -- registry was last read; §13 `scheduled` mode
                               -- reads this to decide whether to regenerate
);

CREATE TABLE digests (
    project_slug  TEXT PRIMARY KEY REFERENCES projects(slug) ON DELETE CASCADE,
    body          TEXT NOT NULL,
    source_hash   TEXT NOT NULL,
    generated_at  DATETIME NOT NULL,
    model         TEXT NOT NULL
);

CREATE TABLE action_items (
    id            TEXT PRIMARY KEY,          -- AI-014
    project_slug  TEXT NOT NULL REFERENCES projects(slug) ON DELETE CASCADE,
    title         TEXT NOT NULL,
    body          TEXT,
    rationale     TEXT,                      -- why Apex thinks this matters now
    effort        TEXT,                      -- small | medium | large
    status        TEXT NOT NULL,             -- proposed | accepted | in_progress
                                             -- | in_review | done | dismissed
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL,
    generated_by  TEXT,                      -- model id
    context_hash  TEXT                       -- provenance: digest set that produced it
);

CREATE TABLE ideas (
    id                   TEXT PRIMARY KEY,   -- IDEA-007
    title                TEXT NOT NULL,
    pitch                TEXT,
    rationale            TEXT,               -- why this fits *you* specifically
    status               TEXT NOT NULL,      -- proposed | saved | started | dismissed
    started_project_slug TEXT REFERENCES projects(slug),
    created_at           DATETIME NOT NULL,
    generated_by         TEXT
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    title      TEXT,
    started_at DATETIME NOT NULL
);

CREATE TABLE messages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role          TEXT NOT NULL,             -- user | assistant | system
    content       TEXT NOT NULL,
    created_at    DATETIME NOT NULL,
    model         TEXT,
    input_tokens  INTEGER,
    output_tokens INTEGER
);

CREATE TABLE exec_runs (
    id             TEXT PRIMARY KEY,
    action_item_id TEXT REFERENCES action_items(id) ON DELETE SET NULL,
    executor       TEXT NOT NULL,
    started_at     DATETIME NOT NULL,
    finished_at    DATETIME,
    exit_status    TEXT,                     -- success | failed | cancelled
    log_path       TEXT
);
```

`context_hash` on action items is worth keeping: it lets Apex tell you an item was
generated against a state of the project that has since moved on.

### Action item IDs

Globally sequential (`AI-014`), not per-project. Short to type, stable when a
project is renamed, and unambiguous in a cross-portfolio list — which is the view
that matters most. Ideas use the same scheme (`IDEA-007`).

### Remote persistence

State replicates to **Turso Cloud** so it survives a machine change, using
`tursogo` embedded replicas.

**Read the write semantics before building on this.** An embedded replica serves
reads from a local file (fast, works offline) but **forwards every write to the
remote primary**. Writes therefore require connectivity. An earlier draft of this
spec claimed Apex stays "fully functional offline" — that is true for reads only.

The consequence for a CLI you invoke twenty times a day is significant: on a plane,
`apex review` could not record a single action item. So:

- **`LocalBackend` is the default.** A plain local file, no network, no degradation.
- **`TursoBackend` is opt-in**, for when cross-machine persistence is worth a
  network dependency on writes.
- Turso's newer **offline-writes** mode is the eventual right answer — genuinely
  local-first with deferred reconciliation. Its maturity in `tursogo` is
  **unverified**; check before relying on it, do not assume.

```go
type Backend interface {
    Open(ctx context.Context) (*sql.DB, error)
    Sync(ctx context.Context) error  // no-op for local-only
    Online() bool                    // whether writes can currently succeed
    Describe() string                // shown by `apex doctor`
}
```

```toml
[store]
backend       = "local"         # local | turso
database      = "apex-vedant"   # Turso database name; auth token in Keychain
sync_interval = "5m"
```

Commands that mutate state check `Online()` first and fail with a clear message
rather than a driver-level error from deep in a transaction.

> **A note on what this is buying.** Most of this state is *derived* — action items
> regenerate from `PROJECT.md` files already committed to your repos. The genuinely
> irreplaceable state is narrow: which items you accepted or dismissed, and the
> ideas backlog. If the Turso dependency stops earning its keep, `LocalBackend` plus
> a git-backed JSON export of those two tables covers the same need.

### Migrations

Turso provides no migration machinery worth using — no versioning beyond filename
convention, no rollback, no per-environment tracking. Apex owns this.

Two facts drive the entire design. **DDL is a write, so it forwards to the primary
and is immediately global** — a migration run on your laptop changes the schema for
every machine sharing that database, including ones running an older Apex binary.
And **an older binary must keep working afterwards**, because you will not upgrade
every machine simultaneously.

That yields four rules:

**1. Additive only.** `CREATE TABLE`, `ADD COLUMN`, `CREATE INDEX`. Never `DROP`,
never rename, never change a column type, never add a `NOT NULL` column without a
default. A column an old binary does not know about is harmless; a column it
expects and cannot find is a crash.

**2. Never `SELECT *`.** Always name columns explicitly. This single rule is what
makes additive migrations safe across binary versions — it is a lint-enforceable
invariant, not a style preference.

**3. Forward-only, numbered, embedded.** Migration files live in
`internal/store/migrations/NNN_name.sql`, compiled into the binary with `embed.FS`
so a deployed Apex can never disagree with its own schema. Applied versions are
recorded in `schema_migrations`, which itself syncs — so every machine agrees on
what has been applied.

**4. Migrating requires connectivity, and says so.** Each migration is atomic —
SQLite has transactional DDL, and the `schema_migrations` insert belongs in the
same transaction, so a single migration never half-applies. A *run* of several can
still stop partway: three pending, the second fails, one stays applied. That is
correct for forward-only migrations and the error must name which one failed.

```go
func (s *Store) Migrate(ctx context.Context) error {
    if !s.backend.Online() {
        return ErrOfflineMigration // explicit, not a driver error
    }
    if err := s.backend.Sync(ctx); err != nil { return err }   // pull latest first

    rel, err := acquireMigrationLock(ctx, s.db)                // cross-machine
    if err != nil { return err }
    defer rel()

    applied, err := appliedVersions(ctx, s.db)
    if err != nil { return err }
    for _, m := range pending(embedded, applied) {
        if err := applyOne(ctx, s.db, m); err != nil {
            return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
        }
    }
    return nil
}
```

The migration lock is **cross-machine**, so it lives in the database rather than on
disk — two machines upgrading at once must not race:

```sql
CREATE TABLE schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    applied_at  DATETIME NOT NULL,
    applied_by  TEXT NOT NULL   -- hostname, for debugging a bad migration
);

CREATE TABLE migration_lock (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    holder      TEXT NOT NULL,
    acquired_at DATETIME NOT NULL
);
```

Acquire with `INSERT ... ON CONFLICT DO NOTHING`, then verify you hold it. A
crashed process cannot release a database row the way the kernel releases a file
lock, so a stale row must be reclaimable — but **reclaim must be a
compare-and-swap, not a read-then-write**:

```sql
UPDATE migration_lock
   SET holder = ?, acquired_at = ?
 WHERE id = 1
   AND holder = ?              -- the exact row that was read
   AND acquired_at = ?;
```

Check `RowsAffected() == 1` to confirm you won. Two machines that both observe the
same stale row would otherwise both reclaim it and migrate concurrently. This is
the failure mode that passes every test and corrupts a schema once, in production,
on the one day two machines upgrade together.

Because rollback does not exist, **every migration is reviewed as a one-way door.**
Test against a Turso branch before applying to the real database.

## 8. Provider abstraction

Because the builder loop is dispatched, Apex needs only two capabilities from a
provider: **streaming text** and **structured output**. No tool use in v1. This
keeps the interface small enough that Go's weak sum types never become a problem.

```go
package provider

type Role string

const (
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
)

type Message struct {
    Role    Role
    Content string
}

type Request struct {
    System    string
    Messages  []Message
    Model     string
    MaxTokens int
    Effort    string // low|medium|high|xhigh|max; Anthropic only, ignored elsewhere
}

type EventType int

const (
    EventTextDelta EventType = iota
    EventDone
    EventError
)

type Usage struct {
    InputTokens      int
    OutputTokens     int
    CacheReadTokens  int
}

type Event struct {
    Type  EventType
    Text  string
    Usage *Usage // populated on EventDone
    Err   error  // populated on EventError
}

type Provider interface {
    Name() string

    // Stream returns a channel closed when the response completes or errors.
    Stream(ctx context.Context, req Request) (<-chan Event, error)

    // Structured constrains output to a JSON schema and unmarshals into out.
    Structured(ctx context.Context, req Request, schema json.RawMessage, out any) error
}
```

`Structured` is what generates action items and ideas — both are lists of typed
records, not prose, and constraining them to a schema removes an entire class of
parsing failure.

### Implementation notes

- **Do not write the adapters from memory.** Both SDKs move quickly. Consult the
  `claude-api` skill for Anthropic and the `openai-go` repo for OpenAI before
  writing either adapter.
- Anthropic model IDs carry no date suffix (`claude-opus-5`).
- Anthropic: use adaptive thinking (`thinking: {type: "adaptive"}`); `budget_tokens`
  is rejected on Opus 5. Depth is controlled by `output_config.effort`.
- Anthropic structured output goes in `output_config.format`, not the deprecated
  `output_format`.
- Use streaming for anything with a large `max_tokens` — non-streaming requests hit
  HTTP timeouts.
- Go error handling: `errors.As` into `*anthropic.Error`, then switch on
  `StatusCode`. Distinguish retryable (429, 5xx, connection) from non-retryable
  (400, 404) rather than catching one broad class.

### Prompt caching

Project digests and identity context are stable between calls and form a natural
cache prefix. Order the prompt **stable first**: identity context → digests →
volatile request. Place the cache breakpoint after the digests. This is the real
answer to input cost at scale, and it costs nothing in design terms if the prompt
is assembled in the right order from the start.

### Model routing

Configured per task type, so a single knob changes cost/quality per surface.

```toml
[models.advisor]          # review, ideas — the reasoning that matters
provider = "anthropic"
model    = "claude-opus-5"
effort   = "high"

[models.chat]
provider = "anthropic"
model    = "claude-opus-5"
effort   = "medium"

[models.digest]           # bulk summarization; the obvious cost lever
provider = "anthropic"
model    = "claude-opus-5"
effort   = "low"
```

All three default to `claude-opus-5`. The `digest` slot is the intended place to
downgrade — `claude-haiku-4-5` is the natural choice there, since summarizing a
README and a git log is not intelligence-bound — but that is a user decision, not
a default.

---

## 9. Executor abstraction

```go
package executor

type TaskBrief struct {
    ActionItemID string
    ProjectPath  string   // cwd for the dispatched agent
    Title        string
    Instruction  string   // assembled brief
    ContextPaths []string // files the agent should read first
}

type EventType int

const (
    EventOutput EventType = iota
    EventDone
    EventError
)

type Event struct {
    Type EventType
    Text string
    Err  error
}

type Executor interface {
    Name() string

    // Available reports whether this executor can actually run on this machine.
    // Checked before every dispatch and by `apex doctor`.
    Available() error

    Run(ctx context.Context, brief TaskBrief) (<-chan Event, error)
}
```

`Available()` is load-bearing, not decoration. Apex must never assume a binary is
present — it checks, and on failure reports exactly what is missing and stops,
rather than proceeding on a broken assumption.

### ClaudeCodeExecutor

Verified against `claude` 2.1.278 on 2026-09-21.

```
claude -p \
  --output-format stream-json \
  --include-partial-messages \
  --session-id <uuid> \
  --permission-mode acceptEdits \
  --permission-prompts none \
  --append-system-prompt <brief> \
  --model opus \
  --effort high
```

Run with `cmd.Dir` set to the project path. Three details matter:

**Parse `stream-json`, never scrape stdout.** The CLI emits structured JSON events,
so the executor can distinguish tool calls from file edits from the final result and
map them onto typed `Event` values. Text scraping would throw that away.

**Apex generates the session UUID** and stores it on the `exec_runs` row. A stalled
or failed dispatch is then resumable with `claude --resume <uuid>`, and the run is
traceable after the fact.

**`--permission-prompts none` is required for unattended dispatch.** Nothing is
watching the TTY, so anything that would prompt must deny rather than hang.
`--permission-mode acceptEdits` allows file edits without confirmation while still
gating riskier operations; `bypassPermissions` is deliberately not the default and
should stay a per-run opt-in.

Everything is teed to `~/.apex/logs/exec/<run-id>.log`.

Because the agent runs inside the project directory, it picks up `PROJECT.md` and
any `CLAUDE.md` from the working tree without Apex passing them explicitly. The
brief therefore carries *intent* — what to do and why it matters — rather than
project background.

> **The builder loop runs on the Claude Code subscription, not on an API key.**
> Apex is bring-your-own-key for the advisor loop only. Worth remembering for the
> public v2 story, where users may have one without the other.

> **Background dispatch** (`--bg`, with `claude attach|logs|stop`) is available and
> would let the TUI stay responsive during long runs. Not in v1 — foreground with a
> cancellable context is simpler and the interface already supports adding it.

---

## 10. Concurrency and locking

Apex has three resources that need protection, and they split cleanly on one
question: **is this resource shared across machines, or local to one?** The answer
determines where the lock lives.

| Resource | Scope | Mechanism | Why |
|---|---|---|---|
| Schema | Cross-machine | Row in `migration_lock` | DDL forwards to the primary; every machine shares one schema |
| Project working tree | Machine-local | `flock` on `~/.apex/locks/<slug>.lock` | Each machine has its own checkout |
| Local replica file | Machine-local | `flock` on `~/.apex/locks/db.lock` | The replica file belongs to one machine |

The project lock **must not** live in the synced database. Two machines dispatching
against the same project are editing two independent working trees — that is
ordinary git divergence, not a conflict. A synced lock would falsely block it.

### Per-project lock

Every dispatch (`apex do`, `apex start`) takes an **exclusive** lock on its project
for the full duration of the run. Two agents editing one working tree concurrently
produces interleaved edits and conflicting writes with no way to attribute either.

`flock(2)` is the right primitive because **the kernel releases it when the process
dies.** No stale-lock detection, no timeout heuristics, no cleanup on crash — the
failure mode that makes hand-rolled lockfiles unreliable simply does not arise.

```go
package lock

type Mode int

const (
    Exclusive Mode = iota
    Shared
)

type Lock struct {
    path string
    f    *os.File
}

// AcquireAs is non-blocking. A held lock returns *BusyError naming the holder,
// so the message can say what is running rather than just "locked".
//
// runID is a parameter so one dispatch shares a single identifier across its
// lock file, its exec_runs row, and its log file. Acquire() wraps this with a
// generated ID for callers that do not need correlation.
func AcquireAs(path string, mode Mode, runID string) (*Lock, error) {
    f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
    if err != nil {
        return nil, err
    }
    how := unix.LOCK_EX | unix.LOCK_NB
    if mode == Shared {
        how = unix.LOCK_SH | unix.LOCK_NB
    }
    if err := unix.Flock(int(f.Fd()), how); err != nil {
        holder, _ := io.ReadAll(f) // best effort; empty is fine
        f.Close()
        return nil, &BusyError{Path: path, Holder: string(holder)}
    }
    // Reading above moved the offset; rewind before rewriting. Harmless on a
    // fresh descriptor, wrong the moment this is refactored.
    if _, err := f.Seek(0, io.SeekStart); err != nil {
        f.Close()
        return nil, err
    }
    if err := f.Truncate(0); err != nil {
        f.Close()
        return nil, err
    }
    fmt.Fprintf(f, "run=%s pid=%d started=%s", runID, os.Getpid(), time.Now().Format(time.RFC3339))
    return &Lock{path: path, f: f}, nil
}

func (l *Lock) Release() error {
    unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
    return l.f.Close()
}
```

Writing the holder into the file is what turns an unhelpful error into a useful one:

```
$ apex do AI-022
error: ScholarRAG is already being worked on
       run 7f3a91 (pid 8821), started 4m ago
       apex logs 7f3a91   to watch it
       apex stop 7f3a91   to cancel it
```

**`apex sync` takes a shared lock**, not an exclusive one. It only reads git state
and `PROJECT.md`, so several syncs may run at once — but it must not read a tree
mid-dispatch, or it would digest a half-written state and cache the result.

### Local replica lock

`tursogo` documents that opening the local database while a sync is in progress can
corrupt it. Since a TUI session and a `apex review` in another terminal can easily
overlap, this is not hypothetical.

Same primitive: `Sync()` takes an exclusive lock on `~/.apex/locks/db.lock`; normal
database access takes a shared one. `LocalBackend` skips this entirely — with no
sync, there is nothing to serialize against.

### What is deliberately not locked

Advisory commands (`apex review`, `apex ideas`, `apex items`) take no project lock.
They only read digests from the database, never the working tree, so they stay
responsive during a long dispatch.

## 11. Command surface

```
apex                       launch the TUI
apex doctor                verify toolchain, credentials, executor availability
apex sync [project]        refresh registry and regenerate stale digests
apex review [project]      generate action items
apex ideas                 propose new projects
apex items [--status s]    list action items
apex show <id>             detail for an item or idea
apex do <id>               dispatch an action item to the executor
apex start <idea-id>       scaffold a new project from an idea
apex config                show resolved config and key sources
```

The non-interactive commands are what make Apex cron-able — "every Monday, refresh
my action items" — which is likely where a large share of the real value lands.

`apex doctor` exists because silently assuming the environment is the failure mode
this project most wants to avoid.

---

## 12. TUI

Bubble Tea, Elm architecture. Three views, switched by tab:

- **Chat** (default) — input at the bottom, scrollback rendered through Glamour.
- **Items** — action items grouped by project, filterable by status; `enter`
  dispatches.
- **Projects** — the registry with digest previews and staleness indicators.

Streaming is a natural fit: the provider's `Event` channel is drained by a
`tea.Cmd` that returns one `streamDeltaMsg` per event and re-issues itself, so
tokens arrive as ordinary messages into `Update`.

`tui/` imports downward only. Nothing in `advisor/`, `provider/`, or `executor/`
may import it — that boundary is what keeps a v2 non-terminal frontend possible.

---

## 13. Config and secrets

`~/.apex/config.toml` holds model routing, executor choice, and preferences.

**Keys are never stored in config.** Resolution order:

1. macOS Keychain (`apex:anthropic`, `apex:openai`) via `go-keyring`
2. `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` environment variables
3. Fail with an explicit message naming what is missing and how to set it

A Keychain **error** (locked, access denied) is not the same as a secret being
**absent**. On error, fall through to the environment variable rather than failing
outright — a locked Keychain must never mask a perfectly good env var — and surface
the underlying error only if every source comes up empty.

`apex config` reports which source each key resolved from, without printing the
key itself.

### Sync policy

When digests refresh is a setting, not a hardcoded behavior:

```toml
[sync]
mode     = "explicit"   # explicit | on_start | scheduled
schedule = "daily"      # daily | weekly — only read when mode = "scheduled"
```

- **`explicit`** — only `apex sync` refreshes. Most predictable, no surprise spend.
- **`on_start`** — refresh stale digests when the TUI launches.
- **`scheduled`** — refresh if `last_synced_at` is older than the interval, checked
  before any advisor command.

Default is `explicit`. Commands that depend on digests report their age when they
run against stale data, so the predictable default never silently misleads.

---

## 14. Key flows

### `apex sync`

1. Parse `PROJECTS.md`, upsert registry rows.
2. For each project, verify the path exists — report and skip if not.
3. Compute `source_hash` from `PROJECT.md` + git HEAD + dirty flag.
4. For changed projects only, assemble sources and generate a digest (parallel,
   bounded by an errgroup).
5. Write digests back; regenerate the summary lines in `PROJECTS.md`.

### `apex review`

1. Load `PROFILE.md`, `SKILLS.md`, and all digests.
2. One `Structured` call returning a typed list of action items with title, body,
   rationale, effort, and project.
3. Deduplicate against existing non-dismissed items.
4. Insert as `proposed`, stamped with `context_hash`.

### `apex do <id>`

1. Load the item, its project, and its digest.
2. `executor.Available()` — stop with a clear message if not.
3. Assemble a `TaskBrief`: what to do, why, acceptance criteria. Not project
   background, which the agent reads from the working tree.
4. Set status `in_progress`, open an `exec_runs` row.
5. Stream output to the TUI and the log file.
6. On exit, set `in_review` (success) or back to `accepted` (failure) and record
   the exit status. **Apex never marks an item `done` on its own** — that is the
   user's judgment after reviewing the diff.

### `apex start <idea-id>`

Three tiers, escalating from deterministic to generative. The principle is that a
model should only do the part that genuinely varies.

**Tier 1 — Apex, deterministic.** Create the directory, `git init`, write
`PROJECT.md` from the idea, add a `.gitignore`, register in `PROJECTS.md`. Identical
for every project and must be correct, so no model is involved.

**Tier 2 — the ecosystem's own scaffolder,** where one fits the stack: `go mod init`,
`cargo new`, `uv init`, `npm create vite`. These are maintained by the ecosystem and
always current, which beats both a hand-written template and a model's recollection
of idiomatic layout. Apex keeps a small stack → command mapping, and skips this tier
when no scaffolder matches.

**Tier 3 — dispatch to the executor** for the structure this specific idea needs.
Same path as `apex do`, different brief.

This is why Apex maintains no project templates: tier 1 covers what is universal,
tier 2 borrows from people who do it better, tier 3 handles what is bespoke.

---

## 15. Dependencies

**None of these are installed.** The module cache is empty on this machine; the
first `go get` will fetch everything fresh, and versions below are unpinned
because they have not been verified.

```
github.com/charmbracelet/bubbletea
github.com/charmbracelet/lipgloss
github.com/charmbracelet/bubbles
github.com/charmbracelet/glamour
github.com/anthropics/anthropic-sdk-go
github.com/openai/openai-go
turso.tech/database/tursogo
github.com/spf13/cobra
github.com/pelletier/go-toml/v2
gopkg.in/yaml.v3
github.com/zalando/go-keyring
golang.org/x/sync/errgroup
```

Verified present: Go 1.27.1 (arm64), git 2.54.0, sqlite3, `claude` at
`~/.local/bin/claude`. Not present: `codex`, `aider`.

---

## 16. Build order

Each milestone is independently verifiable.

| # | Milestone | Contents |
|---|---|---|
| M1 | Skeleton | Cobra CLI, config, keychain, `store.Backend`, migrations (§7), `lock` pkg (§10), `apex doctor` |
| M2 | Context | Markdown + frontmatter parsing, registry sync, git introspection |
| M3 | Providers | Anthropic and OpenAI adapters, streaming, structured output |
| M4 | Advisor | Digest generation, `apex review`, `apex ideas`, `apex items` |
| M5 | Executor | `Executor` interface, `ClaudeCodeExecutor`, `apex do`, `apex start` |
| M6 | TUI | Bubble Tea chat, items, and projects views |

M1–M4 produce a genuinely useful tool on their own. M5 closes the loop. M6 makes
it pleasant.

---

## 17. Deliberately deferred

Not in v1, but the design should not foreclose them:

- **Native builder loop** — drops in behind `Executor` with no other changes.
- **Multi-user** — all state under one root; schema takes a `user_id` migration.
- **Non-terminal frontend** — enforced by `tui/` importing downward only.
- **Retrieval** — unnecessary while digests fit comfortably in context.
- **Apex writing to `PROFILE.md` / `SKILLS.md`** — it may suggest; the user applies.

---

## 18. Resolved decisions

Settled 2026-09-21, previously open:

1. **`claude` CLI surface** — verified against 2.1.278. See §9.
2. **Action item IDs** — global (`AI-014`). See §7.
3. **Digest freshness** — a user setting, not a hardcoded policy. See §13.
4. **Scaffolding** — three tiers, no templates maintained. See §14.

5. **Migration pattern** — forward-only, additive-only, embedded, cross-machine
   locked. See §7. Established before M1 as intended.
6. **Dispatch concurrency** — enforced per-project exclusive lock. See §10.

### Still open

- **Turso offline-writes maturity in `tursogo`** — would remove the network
  dependency on writes and let `TursoBackend` become the sensible default. Verify
  against the driver before relying on it.
7. **`SELECT *` enforcement** — closed in M1 as `TestNoSelectStar`, which walks
   every `.go` and `.sql` file and fails on a match outside comments. A test rather
   than a convention, which is what the guarantee needed.

8. **`apex doctor` mutation** — closed. `apex sync` now carries the migration, so
   `doctor` can become read-only and merely report pending migrations. Change it
   when M3 next touches `cmd/apex`.

### Still open

- **`store.UpsertProject` clears `last_synced_at`.** Its `ON CONFLICT` sets the
  column from `excluded`, so any caller that does not pre-read the row silently
  wipes it. `project.Upsert` works around this; fix it at the source in M3 or M4
  rather than requiring every caller to remember.
