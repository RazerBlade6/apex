-- 003_exec_run_session: record what a dispatch actually was.
--
-- Two columns, both nullable, both additive (DESIGN.md §7): no DROP, no
-- rename, no type change, no NOT NULL without a default, and — the subtler
-- rule — no new constraint that narrows the set of writes the schema accepts,
-- so a binary compiled before this migration keeps working unchanged.
--
-- session_id is required by §9: "Apex generates the session UUID and stores it
-- on the exec_runs row. A stalled or failed dispatch is then resumable with
-- `claude --resume <uuid>`." An id Apex generated and did not store is an id
-- nobody can resume, which makes the whole resumability argument moot. M1
-- wrote the table from §7's schema, which predates that requirement.
--
-- project_slug exists because M5 added a second kind of dispatch. `apex do`
-- has an action item, and the item names its project; `apex start` scaffolds a
-- project from an idea and has no action item at all, so without this column
-- its run row records an executor, a timestamp and nothing that says what was
-- worked on. It is deliberately NOT a foreign key: an exec run is a historical
-- record of something that happened, and it should outlive the project row the
-- way action_item_id already outlives its item (ON DELETE SET NULL).
ALTER TABLE exec_runs ADD COLUMN session_id TEXT;
ALTER TABLE exec_runs ADD COLUMN project_slug TEXT;

CREATE INDEX idx_exec_runs_project ON exec_runs(project_slug);
