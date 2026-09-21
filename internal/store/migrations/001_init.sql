-- 001_init: the v1 schema (DESIGN.md §7).
--
-- Migrations are forward-only and additive-only. Never DROP, never rename,
-- never change a column type, never add a NOT NULL column without a default:
-- an older binary must keep working against a newer schema.
--
-- schema_migrations and migration_lock are also created by the migrator's
-- bootstrap before this file runs, so both are IF NOT EXISTS here. They are
-- listed for completeness: this file is the whole schema.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    applied_at  DATETIME NOT NULL,
    applied_by  TEXT NOT NULL   -- hostname, for debugging a bad migration
);

CREATE TABLE IF NOT EXISTS migration_lock (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    holder      TEXT NOT NULL,
    acquired_at DATETIME NOT NULL
);

CREATE TABLE projects (
    slug            TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    path            TEXT NOT NULL,
    status          TEXT,
    registered_at   DATETIME NOT NULL,
    last_synced_at  DATETIME
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

CREATE INDEX idx_action_items_status ON action_items(status);
CREATE INDEX idx_action_items_project ON action_items(project_slug);

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

CREATE INDEX idx_ideas_status ON ideas(status);

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

CREATE INDEX idx_messages_session ON messages(session_id, id);

CREATE TABLE exec_runs (
    id             TEXT PRIMARY KEY,
    action_item_id TEXT REFERENCES action_items(id) ON DELETE SET NULL,
    executor       TEXT NOT NULL,
    started_at     DATETIME NOT NULL,
    finished_at    DATETIME,
    exit_status    TEXT,                     -- success | failed | cancelled
    log_path       TEXT
);

CREATE INDEX idx_exec_runs_action_item ON exec_runs(action_item_id);
