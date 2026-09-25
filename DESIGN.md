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

`PROFILE.md` and `SKILLS.md` are loaded into the system prompt for every advisor
call. The user authors them; **Apex also appends to them as it learns.**

### Learned observations

When a conversation reveals something durable about the user — "I like pixel art",
"I never want to touch Kubernetes again", "I'm learning Zig" — Apex records it
rather than losing it when the session ends. The user should not have to teach Apex
the same fact twice.

The mechanics are the marker discipline already proven on `PROJECTS.md`, because
the hazard is identical: a file the user hand-edits and Apex also writes.

**Apex owns one section and nothing else** — a trailing `## Observed` block. Every
entry carries a marker and a date; everything outside the block is the user's and is
never touched. Writes are atomic (temp file, rename), so an interrupted write cannot
truncate an identity file.

```markdown
## Observed
<!-- Apex maintains the entries below. Everything else in this file is yours. -->
<!--apex 2026-09-25--> Prefers pixel art for UI work — source: said while reviewing a mockup.
<!--apex 2026-09-21--> Builds CLIs in Go or Rust by default — source: asked for a stack recommendation.
```

> **M6 changed the separator from the semicolon this example first used.** The
> source has to be read back — `apex profile` lists observations "with dates and
> sources" — and a semicolon cannot be parsed out of a claim that contains one.
> `" — source: "` is explicit and survives the round trip. It stays visible prose
> rather than a hidden comment attribute on purpose: this text is re-sent to the
> model on every advisor call, and "said while reviewing one mockup" is exactly
> the qualifier that stops a one-off remark being read as a standing preference.
>
> The block is found by its **heading**, not by being last in the file, so a user
> who adds a section below it keeps it. Apex's block simply ends where the next
> heading begins.

**Extraction is gated, not per-turn.** An extraction call after every chat turn
doubles the cost of conversation. Apex screens cheaply first — first person plus a
preference or capability verb ("I like / prefer / always / never / I use / I'm
learning") — and only then spends a call. A turn revealing nothing costs nothing.

The gate errs wide deliberately: a false positive costs one cheap call that
returns nothing, a false negative loses a fact silently. **The call goes to the
`digest` route**, not the `chat` one — extraction is a classification, which §8
names the digest slot as the cost lever for. The gate bounds frequency; the route
bounds unit cost, and both are needed.

**Observations are superseded, not stacked.** "I'm learning Zig", followed months
later by "I know Zig well now", must replace the earlier line rather than sit beside
it. Contradiction handling belongs in extraction, not in a later cleanup pass.

**The block is capped at 12 entries per file.** Identity context is re-sent on
*every* advisor call, and §8 established that prompt caching does not work on the
subscription transport, so an unbounded block is a compounding cost forever.

**Consolidation belongs to extraction, because nothing else pays for it.** An
earlier draft said to "consolidate when it fills" without naming an owner — and
every other model call in Apex is attached to a user action (`sync`, `review`, `do`,
a chat turn). "The block is full" is not a user action, so no invocation was funding
it. Consolidation therefore rides along with an extraction call that was going to
happen anyway: the extractor may replace two narrow claims with one broader line. A
block that fills and then goes quiet truncates oldest-first without consolidating,
which is the honest behaviour rather than a silent debt.

> The general form is worth remembering: **a spec describing ongoing upkeep must
> name the invocation that pays for it**, or the upkeep quietly becomes some other
> command's opportunistic side effect — or never happens at all.

**`apex profile` reports the block's approximate per-call token cost.** The cap
above is a number chosen by argument, and arguments about token cost are easy to be
wrong about by a factor of five. Apex already measures per-call usage everywhere
else; a claim about compounding cost that ships no instrument is an assertion, not a
budget.

**The user stays in control.** `apex profile` lists observations with dates and
sources; `apex profile --forget <n>` removes one. An observation the user promotes
into their own prose above the block should then be dropped from it.

> **The risk worth naming:** a one-off remark becoming a standing preference. "I
> like pixel art" said about one specific mockup is not a general aesthetic, and an
> over-general extraction quietly skews every future suggestion with no visible
> cause. Recording the *source* alongside the claim is what makes a wrong inference
> debuggable rather than mysterious.

**Extraction must never be shown its own prior conclusions as authority.** This is
the sharper version of the risk above, and it is structural rather than a matter of
care. Everything else Apex writes is *data* — digests, registry summaries. An
observation is different: it enters the system prompt of every advisor call,
**including the next extraction call**, which has to see existing observations in
order to supersede them. So a wrong observation biases the very model deciding
whether to record the next one, and a standing preference becomes
**self-confirming** rather than merely wrong.

Recording the source makes a bad inference debuggable *by a human*; it does nothing
to break the loop. Prior observations must therefore be presented to the extractor
as provisional claims open to revision — explicitly not as established facts about
the user — and an extraction that merely re-confirms an existing entry adds nothing
and must be discarded rather than counted as corroboration.

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

-- Extended by migration 003: session_id (§9 requires a resumable dispatch) and
-- project_slug (§14's `apex start` dispatch has no action item, so nothing else
-- records what it worked on). Both were derivable by reading this schema against
-- §9 and §14 before M5 began — a cross-section consistency pass over the schema is
-- cheap at design time and a migration afterwards.
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

**1. Additive only — but additive protects old _readers_, not old _writers_.**
`CREATE TABLE`, `ADD COLUMN`, `CREATE INDEX`. Never `DROP`, never rename, never
change a column type, never add a `NOT NULL` column without a default. A column an
old binary does not know about is harmless; a column it expects and cannot find is
a crash.

The subtlety that "additive" hides: a `CREATE UNIQUE INDEX` or any new constraint
**narrows the set of writes the schema accepts**. It is additive in DDL terms and
still breaks an older binary that was happily performing a write the new constraint
forbids. Since DDL forwards to the primary and is immediately global, one machine
running `apex sync` can break another machine's writes without either user doing
anything wrong.

So: additive DDL needs no review for readers. **A constraint that narrows the
accepted write set needs the same one-way-door review as a destructive change**,
and should ship only once every machine runs a binary that already upholds the
invariant in application code. Migration 002 (`UNIQUE` on `projects.path`) is safe
precisely because M2 already matched registry entries by path — but the rule as
originally written would not have told you to check that.

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
    Effort    string // low|medium|high|xhigh|max. Applies to models that support
                     // output_config.effort; ignored by providers and models that
                     // do not. This is a per-MODEL axis, not per-vendor:
                     // claude-haiku-4-5 — the model this spec recommends for the
                     // digest slot — rejects both effort and adaptive thinking
                     // with a 400. Taking the spec's own cost advice must not
                     // produce an error.
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
    CacheWriteTokens int    // see below
    Model            string // which model actually served this request
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

**Schemas must target the intersection of both vendors' accepted subsets**, not
arbitrary JSON Schema. In practice that means an object root, every property
required, and `additionalProperties: false`. A schema that only one vendor accepts
turns a config change into a runtime 400, so write to the intersection from the
start rather than discovering the boundary after a provider switch.

**`CacheWriteTokens` is not redundant with `CacheReadTokens`.** Watching reads alone
cannot distinguish caching that works from a prefix just unstable enough to
re-write the cache on every call — in that failure mode reads stay at zero and
writes stay high, which is the expensive case, and it looks identical to "not
cached yet" if only reads are recorded. Cache writes are billed at a premium, so
this is the number that catches the mistake.

**`Model` closes a gap in §7's schema.** `digests.model` and
`action_items.generated_by` both record which model produced a row. Without the
serving model on the response, Apex can only record what it *asked* for, which
diverges the moment a fallback or an alias resolves to something else.

> **Both fields landed in M4**, in all three adapters. Anthropic reads
> `cache_creation_input_tokens` and the accumulated message's `model`; OpenAI
> reports no cache-write count (its caching is automatic and unbilled, so zero is
> the honest value there) and takes the serving model from the chunk; claude-cli
> reads the CLI's `cache_creation_input_tokens` and stamps the model the run
> reported, falling back to the single key of the result event's `modelUsage`.
>
> **The first thing they measured is a finding.** Every live `apex sync` through
> `claude-cli` reports `cache r=0 w≈1150` — the prompt cache is written on every
> call and never read. That is the exact expensive pattern this section warns
> about, and reads alone could not have distinguished it from "not cached yet".
> The cause is structural rather than a bug in the prompt: each call is a fresh
> subprocess with `--no-session-persistence`, and the CLI picks its own cache
> breakpoints, so Apex's stable-prefix ordering buys nothing on this transport.
> It still buys everything on the API adapters, where `CacheSystem` places a real
> breakpoint. **Prompt-cache savings are therefore an API-key benefit, not a
> subscription one** — which is a second, independent reason to route the bulk
> `digest` slot at a key when one exists.
>
> The same runs expose a second limitation: the CLI reports `input_tokens: 2` for
> a prompt carrying a full PROJECT.md, README and git log. Its input accounting
> counts only the uncached remainder, so `Usage.InputTokens` from `claude-cli` is
> not comparable with the same field from the API adapters and must not be summed
> across them.

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

### Subscription-backed inference (`claude-cli`)

A user with a Claude Pro or Max subscription can run the advisor loop through it
instead of an API key. This needs **no interface change**: `internal/provider/claudecli`
is a third `Provider` implementation that shells out to the `claude` CLI, exactly as
`ClaudeCodeExecutor` does for the builder loop. It is the same pattern applied one
layer up — Apex owns the judgment, an existing tool owns the transport.

```
claude -p \
  --output-format stream-json --include-partial-messages --verbose \
  --system-prompt <identity + digests> \
  --model opus --effort high \
  --restricted --strict-mcp-config --disable-slash-commands \
  --tools "" --no-session-persistence \
  [--json-schema <schema>]        # Structured() only
```

Verified against `claude` 2.1.278 on 2026-09-21. Three flags were added to this
block during M3.5, each for a reason worth keeping:

- **`--verbose` is mandatory.** Without it the CLI refuses outright — "When
  using `--print`, `--output-format=stream-json` requires `--verbose`" — before
  any inference happens. It does not make the stream chattier in this mode.
- **`--tools ""` disables every built-in tool.** `--restricted` removes the
  code-running tools but leaves Read, Write and Edit available, and an advisor
  call has no business editing files in whatever directory `apex` was launched
  from. v1 has no tool use at all, so the honest setting is none.
- **`--no-session-persistence`** keeps advisor prompts — which carry
  `PROFILE.md`, `SKILLS.md` and every digest — out of `~/.claude/projects`.
  Apex never resumes these sessions, so a transcript would only be a second,
  unmanaged copy of the user's context.

`--json-schema` covers `Structured`; `stream-json` covers `Stream`. Auth state is
readable via `claude auth status --json`, which is what `doctor` checks: it
reports `loggedIn`, `authMethod` (`claude.ai` for a subscription) and
`subscriptionType`, and never the credential itself.

> **Never pass `--bare`.** It reads as the obvious flag for using Claude Code as a
> raw inference engine — skip hooks, CLAUDE.md discovery, plugins — but its own help
> states that Anthropic auth then becomes "strictly `ANTHROPIC_API_KEY` or
> `apiKeyHelper`; OAuth and keychain are never read." It disables the exact
> mechanism this provider exists to use. `--restricted` is the correct isolation
> flag: it strips the code-running tools and ignores user/project settings files
> while leaving auth working normally.

**The binding constraint is quota contention, not cost.** A subscription session
limit is shared with the user's real Claude Code usage. If Apex exhausts it
refreshing digests, the user cannot use Claude Code for actual work until it
resets. An API key has no such coupling. Per-slot routing makes this a config
decision:

```toml
[models.advisor]          # judgment-heavy, low volume
provider = "claude-cli"   # subscription
model    = "opus"         # CLI aliases (opus/sonnet) or a full model id
effort   = "high"

[models.digest]           # bulk, mechanical, high volume
provider = "anthropic"    # API key — do not spend session quota here
model    = "claude-opus-5"
effort   = "low"
```

**Every route requires a `model`**, including `claude-cli` ones; `Config.Validate`
rejects a route without it. Earlier drafts of this section showed a `claude-cli`
route with no model, which would not have loaded.

`apex doctor --probe` was run against a live Pro subscription on 2026-09-21 and
returned `claude-cli probe  sonnet answered (2 input, 4 output tokens)`, confirming
the full production argv above — `--tools ""` and `--no-session-persistence`
included — actually executes rather than merely parsing.

Two consequences follow from it being a subprocess. Each call pays process startup,
so parallel digest refresh needs a **bounded worker pool**, not one process per
project. And its exhaustion mode is a *session* limit, which is not an API 429 and
must not be reported as one.

**Session-limit detection is only half-verified, by construction.** The CLI emits a
structured `rate_limit_event` carrying `status`, `resetsAt` and window utilisation
— but only its `"allowed"` form has ever been observed, because seeing the
exhausted form requires exhausting the window. The implementation therefore reads a
non-permissive status *only on a run that already failed*, so an unobserved
soft-warning value cannot turn a working call into a false session limit, and falls
back to matching the CLI's error text otherwise. Those text patterns are an
educated guess. The failure mode is "reported as unknown with the CLI's own message
quoted" rather than "silently wrong" — acceptable, but not the same as verified.
**If this limit is ever hit in practice, capture the `result` event's exact text.**
It is the single most valuable missing fixture in the codebase — and as of M5 the
same unverified phrase list exists in two packages, `provider/claudecli` and
`executor/claudecode`, so one capture fixes both.

`KindSessionLimit` is deliberately **not** retryable. The request would succeed
eventually, but hours later, and a digest loop treating it as retryable would spin.

M3.5 added two `provider.Kind` values for failures no HTTP-backed adapter can
produce: `KindSessionLimit` and `KindUnavailable` (the CLI is not installed).
`KindSessionLimit` is deliberately **not** `Retryable()`: the request would
succeed eventually, but the window resets in hours, and a caller that treats it
as retryable would spin. The reset time, when the CLI reports one, is on
`Error.ResetsAt`.

Two limitations of the subprocess transport are real and recorded rather than
hidden. `Request.MaxTokens` has no CLI flag and is ignored — the CLI decides the
output limit from the model. And multi-turn history is flattened into one
prompt string; the CLI's `--input-format stream-json` would be faithful, but it
turns a one-shot subprocess into a protocol, and only M6's chat view needs it.

OpenAI has no supported equivalent. ChatGPT Plus exposes no programmatic API, and
driving the web API directly violates its terms. OpenAI remains API-key-only.

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

**But what changed on disk comes from `git status`, not from the event stream.**
Counting file edits by tool name is a second, weaker answer to a question the
working tree answers exactly — and it is wrong whenever the agent writes via a shell
heredoc rather than the `Write` tool, which a real M5 run did, reporting "no file
changes" over a tree with two new files. Where the agent's own count disagrees with
the working tree, report both and say which one counts. The general rule: **when a
fact is observable locally, never accept the subprocess's account of it.**

> **The corollary, learned the hard way across M5 and M6.** M5 concluded that a
> dispatch could not run commands, and mitigated it by *telling the agent so in the
> brief*. An M6 dispatch then ran a Bash command and, in the same reply, reported
> that it could not run commands — because the brief had told it that. The
> mitigation had made the original finding unfalsifiable for an entire milestone,
> and it only surfaced because a test disagreed with a log.
>
> So: **a mitigation that tells the agent what it cannot do must never be the same
> channel used to observe what it can do.** Anywhere Apex asserts a capability limit
> inside a prompt, that limit needs an independent check — the same rule stated
> above for file changes, applied to capabilities.

**Apex generates the session UUID** and stores it on the `exec_runs` row. A stalled
or failed dispatch is then resumable with `claude --resume <uuid>`, and the run is
traceable after the fact.

**`--permission-prompts none` is required for unattended dispatch.** Nothing is
watching the TTY, so anything that would prompt must deny rather than hang.
`--permission-mode acceptEdits` allows file edits without confirmation while still
gating riskier operations; `bypassPermissions` is deliberately not the default and
stays a per-run opt-in (`apex do --bypass-permissions`).

> **The permission mode and the acceptance criteria were specified independently,
> and they contradict each other.** Every brief asks that the change build and the
> tests still pass — but under `acceptEdits` + `--permission-prompts none`, `Bash`
> is *denied*, so the run cannot compile or test anything. The first real dispatch
> burned a turn discovering this and then honestly reported work it could not
> verify. Neither decision is wrong alone; together they guaranteed that every
> unattended run reports unverified work.
>
> **Measured in M6, and the truth is narrower than either guess.** Under
> `acceptEdits` + `--permission-prompts none` + `--safe-mode` on 2.1.282:
> `git status --short` **ran**, `mkdir -p .probe && rmdir .probe` **ran**, and
> `go version` was **DENIED** ("This command requires approval"). **Approval is per
> command, not per mode.** So the original conclusion holds exactly where it
> matters — `go version` is about as harmless as a toolchain call gets, so
> `go build` and `go test` certainly are — while the brief's blanket claim that the
> agent cannot run commands was simply false. The brief now says it cannot run
> commands *that build or test this project*, which is the load-bearing clause.
>
> This **strengthens** the case for a narrow `--allowed-tools` allowlist rather than
> dissolving it: the CLI is already doing per-command allowlisting, it just does not
> know that `go build ./...` is this project's verification step. Still open.
>
> M6 established that the denial is **per command, not per mode**: `git status`
> and `mkdir` run, `go version` is denied. §18 has the measurement. The brief was
> corrected to claim only what is true, and the allowlist remains the real fix.
>
> The general lesson is worth carrying into later sections: **wherever a spec
> states both a capability limit and a success criterion, check that the limit
> permits the criterion.** They are usually written in different paragraphs, by
> different reasoning, and never compared.

Everything is teed to `~/.apex/logs/exec/<run-id>.log`.

Because the agent runs inside the project directory, it picks up `PROJECT.md` and
any `CLAUDE.md` from the working tree without Apex passing them explicitly. The
brief therefore carries *intent* — what to do and why it matters — rather than
project background.

**The digest is loaded but deliberately kept out of the brief.** §14's flow says to
load it, which reads as an instruction to include it; it exists only to warn that
the portfolio has moved since the item was generated. Putting it in the brief would
be exactly the project background this section forbids.

**Decided: a dispatch inherits the _project_ `CLAUDE.md` and not the user's
global one.** Project inheritance is the design, and it is why the brief can stay
short. Global inheritance was a side effect nobody chose — both real M5 dispatches
volunteered that they were departing from a convention in `~/.claude/CLAUDE.md`,
meaning a user's personal Claude Code conventions silently shape every Apex
dispatch. Some of them are actively wrong here: "delegate code generation to a
sub-agent" is redundant advice for a process that *is* the delegation layer.

**The mechanism was settled empirically in M6.** What was checked against
`claude` 2.1.278, and re-checked against 2.1.282:

| Candidate | Why it does not work |
|---|---|
| `--restricted` | Strips the code-running tools the executor exists to use |
| `--bare` | Skips `CLAUDE.md` discovery but forces `ANTHROPIC_API_KEY` and never reads OAuth — kills subscription auth |
| `--setting-sources project,local` | Governs `settings.json` sources, not `CLAUDE.md` discovery |
| `CLAUDE_CONFIG_DIR=<scratch>` | Does relocate discovery — and relocates `.claude.json` with it, so the logged-in account goes too. `claude auth status --json` reports `loggedIn: false, authMethod: "none"`; seeding the scoped directory with the real `oauthAccount` does not change it. The `--bare` failure by another route |
| `--safe-mode` **alone** | Keeps auth working, but disables *all* customizations including the **project** `CLAUDE.md`, skills and hooks |

**Shipped: `--safe-mode`, plus Apex re-supplying the project `CLAUDE.md` verbatim
in the appended system prompt.** Verified by real dispatch against 2.1.282 in a
fixture project whose `CLAUDE.md` required a specific first line in every new
file: the run authenticated, kept Bash, Read, Write and Edit, wrote the file with
that line and cited the convention by name, and no rule from the user's global
file appeared anywhere in it. A second probe asked the run directly whether it had
been told anything about delegating work to a sub-agent — the global file's actual
content — and it answered no.

> **`claude auth status --json` is the cheap oracle.** The config-directory
> approach was ruled out for zero tokens, because auth resolution can be
> questioned without inference. Reach for it before spending a call on any
> future flag.

**What a dispatch inherits, stated rather than assumed.** `--safe-mode` disables
every customization at *both* user and project scope, so the honest accounting is:

- **Inherited:** the project's root `CLAUDE.md`, re-supplied by Apex under a
  heading that says where it came from; and the working tree itself, because the
  agent is standing in it.
- **Not inherited:** the user's global `CLAUDE.md` — the point of the exercise;
  `CLAUDE.md` files in subdirectories and `@`-imports from any of them; skills,
  hooks, plugins, MCP servers and `settings.json` at either scope.

Resolving nested files and imports would mean reimplementing the CLI's discovery
rules against a moving target, and getting that subtly wrong is exactly the "half
a config" outcome this section rules out. The limitation is documented instead.

`executor.inherit_user_config = true` drops `--safe-mode` and restores the 2.1.x
default, global file included. The field is named for what it turns *on* so that
its absence from an older `config.toml` means the safe value.

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
apex profile               show what apex has learned about you; --forget <n>
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

**The update invariant: a message belongs to the component that requested it, not
the one in focus.** This is the part of the Elm architecture that actually breaks,
and it broke here — `Init` loads all three views at once, chat is the default, so
the items and projects results arrived while chat held focus and were dropped. Both
views read "loading…" forever with no error anywhere. Every driven-`Update` test
passed, because such tests set the view first; only launching the application showed
it.

Two related invariants: a stream is identified by handle, so a reply arriving after
a cancel is discarded rather than appended to the next question; and markdown is
rendered when a turn *completes*, not per token, because running a parser over a
half-written document is both expensive and visibly wrong.

> **A spec that names an architecture should state that architecture's invariants,
> not only its screens.** Naming three views said nothing about the one rule whose
> violation is invisible to unit tests.

`tui/` imports downward only. Nothing in `advisor/`, `provider/`, or `executor/`
may import it — that boundary is what keeps a v2 non-terminal frontend possible.
`TestTUIImportsDownwardOnly` walks the module and enforces it, the same way
`TestNoSelectStar` enforces §7's rule: a guarantee is only worth something if
breaking it fails the build.

Dispatch would be the natural place for that boundary to break, because a
dispatch is more than a subprocess — it takes the project lock, opens the
`exec_runs` row and moves the item's status, all of which lives in `cmd/apex`.
The TUI therefore takes a `Dispatcher` callback and streams whatever it writes,
over an `io.Pipe` drained by the same re-issuing `tea.Cmd` as a model stream.

**`enter` asks before it dispatches.** This is an addition to the line above, and
it is deliberate: a dispatch takes an exclusive lock, spends subscription quota
from the same window as the user's own sessions, and lets an agent edit a real
project. Every other irreversible act in Apex is opt-in by name; a single
keystroke starting one from a list the cursor is already moving through would be
the exception.

**A message goes to the view that owns it, not the view that is focused.** This
was the first real bug in the package and no unit test found it, because every
unit test sets the view before sending the message. `Init` loads all three
surfaces at once and chat is the default, so the items and projects loads arrived
while chat was focused and were dropped — both views then read "loading…" forever
with nothing anywhere saying why. Only launching the application showed it.

---

## 13. Config and secrets

`~/.apex/config.toml` holds model routing, executor choice, and preferences.

**Apex resolves _credentials_, not only keys.** A model route is satisfied either
by an API key or by a subscription-backed CLI, and `doctor` must report which.

For key-based providers (`anthropic`, `openai`), keys are never stored in config:

1. macOS Keychain (`apex:anthropic`, `apex:openai`) via `go-keyring`
2. `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` environment variables
3. Fail with an explicit message naming what is missing and how to set it

A Keychain **error** (locked, access denied) is not the same as a secret being
absent — fall through to the environment rather than failing outright.

For `claude-cli`, there is no key to resolve. Its credential check is the binary
being present and `claude auth status` reporting a logged-in account. Apex never
reads, stores, or forwards the underlying OAuth credential; it only observes that
the CLI has one. A route configured for `claude-cli` must therefore never be
reported as "missing API key" — that message would send the user to fix the wrong
thing.

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

```toml
[sync]
digest_workers = 2      # how many digests to generate at once
```

- **`explicit`** — only `apex sync` refreshes. Most predictable, no surprise spend.
- **`on_start`** — refresh stale digests when the TUI launches.
- **`scheduled`** — refresh if `last_synced_at` is older than the interval, checked
  before any advisor command.

Default is `explicit`. Commands that depend on digests report their age when they
run against stale data, so the predictable default never silently misleads.

`digest_workers` bounds the errgroup in §14 step 4. It is small by default
because a `claude-cli` route is one subprocess per project (§8): an unbounded
fan-out over a twelve-project portfolio starts twelve `claude` processes and
spends a session window shared with the user's own Claude Code work as fast as
the machine allows. A user on an API key has no such coupling and can raise it.

`apex sync --dry-run` reports which digests are stale and sends nothing to a
model, which is the cheap way to find out what a refresh would cost.

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

Versions in use as of M6 are pinned in `go.mod`. The Charm set arrived with M6
and nothing before it needed them:

```
github.com/charmbracelet/bubbletea    v1.3.10   (M6)
github.com/charmbracelet/bubbles      v1.0.0    (M6: textarea, viewport)
github.com/charmbracelet/glamour      v1.0.0    (M6: TUI scrollback only)
github.com/charmbracelet/lipgloss     v1.1.1-…  (M6, pulled by glamour)
github.com/anthropics/anthropic-sdk-go  v1.74.0
github.com/openai/openai-go             v1.12.0
turso.tech/database/tursogo             (not yet used; TursoBackend is unbuilt)
github.com/spf13/cobra                  v1.10.2
github.com/pelletier/go-toml/v2         v2.4.3
github.com/zalando/go-keyring           v0.2.8
golang.org/x/sync/errgroup              v0.22.0
modernc.org/sqlite                      v1.59.0
```

`gopkg.in/yaml.v3` parses `PROJECT.md` front matter. Glamour brings a real
transitive tail — goldmark, chroma, bluemonday, termenv — which is the price of
not hand-rolling a markdown renderer, and it is confined to `internal/tui`.

Verified present: Go 1.27.1 (arm64), git 2.54.0, sqlite3, `claude` at
`~/.local/bin/claude` (2.1.282 as of M6). Not present: `codex`, `aider`.

---

## 16. Build order

Each milestone is independently verifiable.

| # | Milestone | Contents |
|---|---|---|
| M1 | Skeleton | Cobra CLI, config, keychain, `store.Backend`, migrations (§7), `lock` pkg (§10), `apex doctor` |
| M2 | Context | Markdown + frontmatter parsing, registry sync, git introspection |
| M3 | Providers | Anthropic and OpenAI adapters, streaming, structured output |
| M3.5 | Subscription provider | `claudecli` provider, credential model, `doctor` auth check |
| M4 | Advisor | Digest generation, `apex review`, `apex ideas`, `apex items`, `apex show` |
| M5 | Executor | `Executor` interface, `ClaudeCodeExecutor`, `apex do`, `apex start` |
| M6 | TUI | Bubble Tea chat, items, and projects views |

M1–M4 produce a genuinely useful tool on their own. M5 closes the loop. M6 makes
it pleasant — and adds the one thing none of the earlier milestones could: a
conversation, which is where learned observations (§6) have something to draw on.

All seven are complete.

---

## 17. Deliberately deferred

Not in v1, but the design should not foreclose them:

- **Native builder loop** — drops in behind `Executor` with no other changes.
- **Multi-user** — all state under one root; schema takes a `user_id` migration.
- **Non-terminal frontend** — enforced by `tui/` importing downward only.
- **Retrieval** — unnecessary while digests fit comfortably in context.
- ~~Apex writing to `PROFILE.md` / `SKILLS.md`~~ — **reversed, and built in M6.**
  Apex maintains a marked `## Observed` block in each file; see §6 and §18.

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

7. **`SELECT *` enforcement** — closed in M1 as `TestNoSelectStar`, which walks
   every `.go` and `.sql` file and fails on a match outside comments. A test rather
   than a convention, which is what the guarantee needed.

8. **`apex doctor` mutation** — closed in M4. `doctor` is read-only: it reports
   `N applied, N pending` as a **warning** whose fix is `run: apex sync`, and never
   calls `Migrate`. Every other read-only command (`items`, `show`, `review`,
   `ideas`) refuses against a pending schema with the same message rather than
   quietly migrating; only `sync` applies. This was not hypothetical — during M3,
   `apex doctor` applied migration 002 to the real database with no prompt and no
   mention in its own output, which is a verification command taking a one-way
   door.

9. **`Usage.CacheWriteTokens` and `Usage.Model`** — closed in M4 across all three
   adapters. See the blockquote in §8: the first thing they measured was that
   `claude-cli` writes the prompt cache on every call and never reads it, which
   reads alone could not have detected.

10. **Quota contention warning** — added in M4, with the condition **narrowed from
    what this section originally proposed**. The original text asked for a warn
    whenever `models.digest.provider == "claude-cli"`. That is wrong on the machine
    this feature shipped to: every route there is `claude-cli` with no API key
    anywhere, deliberately, so the warning would fire on every run about a choice
    with no alternative — and a `doctor` that nags about the only possible
    configuration teaches the user to stop reading its warnings.

    The implemented condition is the one that is actually actionable: **digests
    routed at `claude-cli` while a key for a key-based provider resolves.** The
    advice — "spend the bulk slot on the key you already have" — only makes sense
    when there is a key to spend. §8's second finding reinforces it: prompt-cache
    savings do not exist on the subscription transport at all.

11. **`Provider.Structured` reports `Usage`** — closed in M5. The signature is
    now `Structured(...) (Usage, error)` across all three adapters, and usage is
    returned even on a failed call, so a request that was billed and then failed
    to decode still accounts for itself. `review` and `ideas` print what they
    cost, and `action_items.generated_by` records the **serving** model rather
    than the route's alias — a live run now writes `claude-sonnet-5` in both
    `digests.model` and `generated_by`, where M4 wrote `claude-sonnet-5` and
    `sonnet` for the same run.

12. **One `claude` lookup** — closed in M5. `internal/claudecmd` now owns finding
    the binary, killing a process group, and decoding the `stream-json` wire
    format; the provider and the executor both use it, and `doctor` reports the
    executor's availability by calling `Executor.Available()` rather than by
    re-implementing the question. The command lines stay separate and must: the
    provider's `--tools "" --restricted` would make a dispatch unable to edit
    anything, and a test asserts neither argv grows the other's flags.

13. **`doctor` checks the identity files** — closed in M5. `PROFILE.md` and
    `SKILLS.md` are reported present, absent, or present-but-empty, as a warning
    with the cost named: an empty identity context still produces output, it is
    just generic advice about code rather than advice for this user, and that
    failure is invisible from the outside.

    **M6 added a fourth state, because M6 broke this check.** Once Apex writes a
    `## Observed` block into these files (§6), "the file has content" stops
    answering "the user has written identity context" — a `PROFILE.md` holding
    nothing but two observations Apex inferred would have passed, silencing the
    warning at exactly the moment it started mattering. `doctor` and
    `Context.MissingIdentity` now ask `AuthoredEmpty`, which parses out Apex's
    block, and a file holding only Apex's own inferences is reported as such.
    `Document.Empty` keeps its old meaning because the *prompt* wants the whole
    file, observations included.

    This is the §9 lesson in a different costume: **§6 and §18-13 were written
    independently, and their interaction was a regression neither one could see.**
    Whenever a section grants Apex write access to something another section
    reads as a signal, check that the signal still means what it meant.

14. **Excluding the user's global `CLAUDE.md`** — closed in M6, empirically. See
    the table in §9. The config-directory approach was ruled out for zero tokens
    with `claude auth status --json`; `--safe-mode` plus re-supplying the project
    `CLAUDE.md` was verified by a real dispatch. What a dispatch does and does not
    inherit is enumerated in §9 rather than left to be discovered, because §9's own
    standard was that documenting the inheritance beats half-inheriting it.

15. **Glamour** — closed in M6, in the direction §18 item 12 of the previous round
    anticipated. It is a dependency now, and it renders the TUI's chat scrollback
    and nothing else. `cmd/apex/render.go` keeps its plain renderer so `apex show
    AI-003 > note.md` still produces a file rather than escape sequences, and the
    two renderers are not a duplication to be consolidated: one exists precisely
    because the other emits ANSI.

16. **Learned observations** — built in M6 against §6, reusing `contextfs`'s
    marker discipline and atomic writes rather than growing a second copy. A real
    conversation turn — "I always build my CLIs in Go, and I have been learning
    Zig on the side" — produced, in one gated call:

    ```
    PROFILE.md  <!--apex 2026-09-25--> Builds CLIs in Go by default. — source: said 'I always build my CLIs in Go'
    SKILLS.md   <!--apex 2026-09-25--> Is learning Zig on the side. — source: said they have been learning Zig on the side
    ```

    Note the split across the two files: a capability went to `SKILLS.md` and a
    preference to `PROFILE.md`, which is the same distinction the cheap gate
    screens on.

### When Apex gains write access to something another section reads

§18 had `doctor` warn when identity context is missing, because an empty
`PROFILE.md` still produces output — just generic advice — and that failure is
invisible from outside. §6 then made Apex a *writer* of those files. The two were
written independently, and their interaction is a regression neither could see: after
two extractions, a `PROFILE.md` containing nothing but Apex's own inferences passes
the not-empty check. **The warning goes quiet at exactly the moment it starts
mattering**, and Apex reasons from a persona it invented while `doctor` reports
everything is fine.

Fixed by teaching `doctor` a third state — "holds only Apex's own observations,
nothing you wrote" — which requires parsing Apex's block out before judging emptiness,
while the prompt still receives the whole file.

> **The general rule, and the last one this design earned: whenever a section grants
> Apex write access to something another section reads as a signal, re-check that the
> signal still means what it meant.** §7 already has this instinct for schemas, in the
> one-way-door review for constraints that narrow the accepted write set. This is the
> same hazard expressed in prose instead of DDL, and prose has no compiler.

### Still open

- **Turso offline-writes maturity in `tursogo`** — would remove the network
  dependency on writes and let `TursoBackend` become the sensible default. Verify
  against the driver before relying on it.
- **`--permission-mode acceptEdits` cannot verify its own work.** Every brief
  asks for "the change builds, and the project's existing tests still pass", and
  under the default mode a dispatched agent may write files and may not run
  commands — `--permission-prompts none` denies rather than asks. M5's first real
  dispatch hit exactly this: the agent's build-and-test command was denied, and it
  correctly reported a change it had not been able to verify. Two half-fixes
  shipped: the brief now states up front what the run may do, and
  `--bypass-permissions` is the per-run opt-in §9 always intended. The real
  question is left open — whether the right default is a narrow allowlist
  (`--allowed-tools`) covering build and test for the project's stack, which would
  let an unattended run meet its own acceptance criteria without granting
  everything.

  **M6 measured what `acceptEdits` actually permits, and the answer changes the
  shape of the fix.** A real dispatch against 2.1.282 under these exact flags ran
  `Bash ls -a; cat PROJECT.md` and got the file's contents back — with
  `permission_denials: []` — while the brief was telling it that it could not run
  commands at all. A follow-up probe under the same flags:

  | Command | Outcome |
  |---|---|
  | `git status --short` | ran |
  | `mkdir -p .probe && rmdir .probe` | ran |
  | `go version` | **denied** — "This command requires approval" |

  **Approval is per command, not per mode.** The CLI auto-approves what it judges
  safe and refers everything else to a prompt that `--permission-prompts none`
  turns into an automatic denial. So M5's conclusion holds exactly where it
  matters — `go version` is about as harmless as a toolchain invocation gets and
  was still denied, so `go build` and `go test` certainly are — while the blanket
  claim in the brief was false, and false in the direction that makes the agent
  stop trying. The brief now says "you CANNOT run commands **that build or test
  this project**", which is accurate and is the sentence the acceptance criteria
  need.

  The exact denial text is worth keeping, because it is unusually good and Apex
  does not have to write its own:

  > Permission for this tool use was denied. It requires approval, and this
  > session has no approval surface — nobody can answer a permission prompt here
  > — so it was denied automatically. The action was NOT performed; do not claim
  > it succeeded, and do not retry it […] What required approval: This command
  > requires approval

  This **strengthens** the `--allowed-tools` case rather than dissolving it. The
  CLI is already doing per-command allowlisting; it simply does not know that
  `go build ./...` is this project's verification step. A narrow allowlist would
  be telling it one true fact, not handing over the machine.

- **What changed is read from git, not from the agent.** M5's second real
  dispatch wrote both its files with shell heredocs rather than the edit tools,
  so the tool-derived file list was empty while the working tree held two new
  files. `apex do` now reports `git status --short` and says so when the agent's
  own count disagrees. The `Result.FilesChanged` field remains a claim, and is
  useful only as the thing to compare against.

- **`--probe` is cheap, not free, for `claude-cli`.** The API adapters bound the
  probe with `max_tokens: 16`; the CLI has no equivalent flag, so a subscription
  probe generates a full short reply against the user's window.
- **Deduplication is exact-title only.** M4 compares normalised titles within a
  project (lowercase, punctuation dropped, whitespace collapsed). It catches the
  common case — the model re-proposing what it proposed last week — and misses any
  paraphrase. Anything better needs embeddings, which §1 rules out of v1, and the
  failure mode was chosen deliberately: a duplicate the user can dismiss is better
  than a silently dropped item they never see.
- **Observation consolidation is a cap, not a consolidation.** §6 asks for
  related observations to be "consolidated into single lines when it fills". What
  ships is the cap (twelve per file) plus a prompt that invites the extractor to
  replace two narrower claims with one — which only happens on a turn that
  qualifies for extraction anyway. A block that fills and then goes quiet is
  truncated from the end, oldest first, with no consolidation pass. A real
  consolidation would be a second call on a schedule, and it is not obviously
  worth one.

- **Observation quality has a sample size of one.** The extraction prompt insists
  that most turns yield nothing, and the one real turn it has seen produced two
  correct, well-sourced observations. Whether it stays that disciplined over a
  hundred turns — or slowly fills the cap with near-duplicates — is unmeasured,
  and the cost of finding out late is that identity context quietly gets worse
  while looking like it is working.
- **`apex review <project>` narrows the digest set to one project**, which is
  cheaper and strictly worse: the advisor's whole value is the cross-portfolio
  comparison. It is offered because a user working on one thing will ask for it,
  but the flag is a cost lever, not a quality one, and the help text says so.
