package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned by the Get* helpers when no row matches.
var ErrNotFound = errors.New("store: not found")

// Every query below names its columns explicitly. Never SELECT *: that single
// rule is what makes an additive migration safe for a binary compiled before
// the new column existed (DESIGN.md §7).

const (
	projectColumns    = `slug, name, path, status, registered_at, last_synced_at`
	digestColumns     = `project_slug, body, source_hash, generated_at, model`
	actionItemColumns = `id, project_slug, title, body, rationale, effort, status, created_at, updated_at, generated_by, context_hash`
	ideaColumns       = `id, title, pitch, rationale, status, started_project_slug, created_at, generated_by`
	sessionColumns    = `id, title, started_at`
	messageColumns    = `id, session_id, role, content, created_at, model, input_tokens, output_tokens`
	execRunColumns    = `id, action_item_id, executor, started_at, finished_at, exit_status, log_path`
)

// --- projects ---------------------------------------------------------------

// UpsertProject inserts a project or updates the mutable fields of an existing
// one. registered_at is preserved on update: it is when Apex first saw it.
func (s *Store) UpsertProject(ctx context.Context, p Project) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (`+projectColumns+`)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (slug) DO UPDATE SET
			name           = excluded.name,
			path           = excluded.path,
			status         = excluded.status,
			last_synced_at = excluded.last_synced_at`,
		p.Slug, p.Name, p.Path, optText(p.Status),
		formatTime(p.RegisteredAt), timeArg(p.LastSyncedAt))
	if err != nil {
		return fmt.Errorf("upsert project %s: %w", p.Slug, err)
	}
	return nil
}

// GetProject returns one project by slug.
func (s *Store) GetProject(ctx context.Context, slug string) (Project, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE slug = ?`, slug)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("project %s: %w", slug, ErrNotFound)
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project %s: %w", slug, err)
	}
	return p, nil
}

// ListProjects returns every registered project, ordered by slug.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return out, nil
}

// GetProjectByPath returns the project registered at a path.
//
// A project's identity is its path, not its name (DESIGN.md §6): the slug
// derives from the name, so matching by slug alone would turn a rename into a
// second row and orphan the first along with its cached digest. Paths are not
// unique by schema, so the lowest slug wins deterministically if two rows ever
// share one.
func (s *Store) GetProjectByPath(ctx context.Context, path string) (Project, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE path = ? ORDER BY slug LIMIT 1`, path)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("project at %s: %w", path, ErrNotFound)
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project at %s: %w", path, err)
	}
	return p, nil
}

// RenameProjectSlug re-keys a project, carrying its digest, action items, and
// ideas across with it, so renaming an entry in PROJECTS.md costs nothing
// (DESIGN.md §6).
//
// The schema declares no ON UPDATE action on the foreign keys, and migrations
// are additive-only (§7), so the parent key cannot be updated while foreign
// keys are enforced statement by statement. PRAGMA defer_foreign_keys moves
// enforcement to commit time: the whole re-key is one atomic step and the
// schema is left alone. The pragma is scoped to this transaction and clears
// when it ends.
func (s *Store) RenameProjectSlug(ctx context.Context, oldSlug, newSlug string) error {
	if oldSlug == newSlug {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rename project %s to %s: %w", oldSlug, newSlug, err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx makes this a no-op

	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return fmt.Errorf("rename project %s to %s: defer foreign keys: %w", oldSlug, newSlug, err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE projects SET slug = ? WHERE slug = ?`, newSlug, oldSlug)
	if err != nil {
		return fmt.Errorf("rename project %s to %s: %w", oldSlug, newSlug, err)
	}
	if err := requireRow(res, fmt.Sprintf("project %s", oldSlug)); err != nil {
		return err
	}

	// Every table that keys off a project slug. A new one added later must be
	// added here too, or a rename would silently orphan it.
	for _, child := range []struct {
		what string
		stmt string
	}{
		{"digest", `UPDATE digests SET project_slug = ? WHERE project_slug = ?`},
		{"action items", `UPDATE action_items SET project_slug = ? WHERE project_slug = ?`},
		{"ideas", `UPDATE ideas SET started_project_slug = ? WHERE started_project_slug = ?`},
	} {
		if _, err := tx.ExecContext(ctx, child.stmt, newSlug, oldSlug); err != nil {
			return fmt.Errorf("rename project %s to %s: %s: %w", oldSlug, newSlug, child.what, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rename project %s to %s: %w", oldSlug, newSlug, err)
	}
	return nil
}

// MarkProjectSynced records the time a project's digest was last refreshed.
func (s *Store) MarkProjectSynced(ctx context.Context, slug string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET last_synced_at = ? WHERE slug = ?`, formatTime(at), slug)
	if err != nil {
		return fmt.Errorf("mark project %s synced: %w", slug, err)
	}
	return requireRow(res, fmt.Sprintf("project %s", slug))
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanProject(sc scanner) (Project, error) {
	var (
		p            Project
		status       sql.NullString
		registeredAt string
		lastSynced   sql.NullString
	)
	if err := sc.Scan(&p.Slug, &p.Name, &p.Path, &status, &registeredAt, &lastSynced); err != nil {
		return Project{}, err
	}
	var err error
	p.Status = text(status)
	if p.RegisteredAt, err = parseTime(registeredAt); err != nil {
		return Project{}, err
	}
	if p.LastSyncedAt, err = nullTime(lastSynced); err != nil {
		return Project{}, err
	}
	return p, nil
}

// --- digests ----------------------------------------------------------------

// UpsertDigest replaces the cached digest for a project.
func (s *Store) UpsertDigest(ctx context.Context, d Digest) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO digests (`+digestColumns+`)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (project_slug) DO UPDATE SET
			body         = excluded.body,
			source_hash  = excluded.source_hash,
			generated_at = excluded.generated_at,
			model        = excluded.model`,
		d.ProjectSlug, d.Body, d.SourceHash, formatTime(d.GeneratedAt), d.Model)
	if err != nil {
		return fmt.Errorf("upsert digest for %s: %w", d.ProjectSlug, err)
	}
	return nil
}

// GetDigest returns the cached digest for one project.
func (s *Store) GetDigest(ctx context.Context, slug string) (Digest, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+digestColumns+` FROM digests WHERE project_slug = ?`, slug)
	d, err := scanDigest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Digest{}, fmt.Errorf("digest for %s: %w", slug, ErrNotFound)
	}
	if err != nil {
		return Digest{}, fmt.Errorf("get digest for %s: %w", slug, err)
	}
	return d, nil
}

// ListDigests returns every cached digest, ordered by project slug. This is the
// set the advisor loop reasons over.
func (s *Store) ListDigests(ctx context.Context) ([]Digest, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+digestColumns+` FROM digests ORDER BY project_slug`)
	if err != nil {
		return nil, fmt.Errorf("list digests: %w", err)
	}
	defer rows.Close()

	var out []Digest
	for rows.Next() {
		d, err := scanDigest(rows)
		if err != nil {
			return nil, fmt.Errorf("list digests: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list digests: %w", err)
	}
	return out, nil
}

func scanDigest(sc scanner) (Digest, error) {
	var (
		d           Digest
		generatedAt string
	)
	if err := sc.Scan(&d.ProjectSlug, &d.Body, &d.SourceHash, &generatedAt, &d.Model); err != nil {
		return Digest{}, err
	}
	var err error
	if d.GeneratedAt, err = parseTime(generatedAt); err != nil {
		return Digest{}, err
	}
	return d, nil
}

// --- action items -----------------------------------------------------------

// InsertActionItem writes a newly generated action item.
func (s *Store) InsertActionItem(ctx context.Context, a ActionItem) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO action_items (`+actionItemColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ProjectSlug, a.Title, optText(a.Body), optText(a.Rationale),
		optText(a.Effort), a.Status, formatTime(a.CreatedAt), formatTime(a.UpdatedAt),
		optText(a.GeneratedBy), optText(a.ContextHash))
	if err != nil {
		return fmt.Errorf("insert action item %s: %w", a.ID, err)
	}
	return nil
}

// GetActionItem returns one action item by ID.
func (s *Store) GetActionItem(ctx context.Context, id string) (ActionItem, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+actionItemColumns+` FROM action_items WHERE id = ?`, id)
	a, err := scanActionItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ActionItem{}, fmt.Errorf("action item %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return ActionItem{}, fmt.Errorf("get action item %s: %w", id, err)
	}
	return a, nil
}

// ActionItemFilter narrows ListActionItems. Zero values mean "no filter".
type ActionItemFilter struct {
	Status      string
	ProjectSlug string
}

// ListActionItems returns items matching the filter, newest first.
func (s *Store) ListActionItems(ctx context.Context, f ActionItemFilter) ([]ActionItem, error) {
	query := `SELECT ` + actionItemColumns + ` FROM action_items`
	var (
		where []string
		args  []any
	)
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.ProjectSlug != "" {
		where = append(where, "project_slug = ?")
		args = append(args, f.ProjectSlug)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at DESC, id DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list action items: %w", err)
	}
	defer rows.Close()

	var out []ActionItem
	for rows.Next() {
		a, err := scanActionItem(rows)
		if err != nil {
			return nil, fmt.Errorf("list action items: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list action items: %w", err)
	}
	return out, nil
}

// SetActionItemStatus moves an item through its lifecycle.
func (s *Store) SetActionItemStatus(ctx context.Context, id, status string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE action_items SET status = ?, updated_at = ? WHERE id = ?`,
		status, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("set action item %s status: %w", id, err)
	}
	return requireRow(res, fmt.Sprintf("action item %s", id))
}

func scanActionItem(sc scanner) (ActionItem, error) {
	var (
		a                    ActionItem
		body, rationale      sql.NullString
		effort, generatedBy  sql.NullString
		contextHash          sql.NullString
		createdAt, updatedAt string
	)
	if err := sc.Scan(&a.ID, &a.ProjectSlug, &a.Title, &body, &rationale, &effort,
		&a.Status, &createdAt, &updatedAt, &generatedBy, &contextHash); err != nil {
		return ActionItem{}, err
	}
	a.Body = text(body)
	a.Rationale = text(rationale)
	a.Effort = text(effort)
	a.GeneratedBy = text(generatedBy)
	a.ContextHash = text(contextHash)

	var err error
	if a.CreatedAt, err = parseTime(createdAt); err != nil {
		return ActionItem{}, err
	}
	if a.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return ActionItem{}, err
	}
	return a, nil
}

// --- ideas ------------------------------------------------------------------

// InsertIdea writes a newly generated project idea.
func (s *Store) InsertIdea(ctx context.Context, i Idea) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ideas (`+ideaColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.Title, optText(i.Pitch), optText(i.Rationale), i.Status,
		optText(i.StartedProjectSlug), formatTime(i.CreatedAt), optText(i.GeneratedBy))
	if err != nil {
		return fmt.Errorf("insert idea %s: %w", i.ID, err)
	}
	return nil
}

// GetIdea returns one idea by ID.
func (s *Store) GetIdea(ctx context.Context, id string) (Idea, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+ideaColumns+` FROM ideas WHERE id = ?`, id)
	i, err := scanIdea(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Idea{}, fmt.Errorf("idea %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Idea{}, fmt.Errorf("get idea %s: %w", id, err)
	}
	return i, nil
}

// ListIdeas returns ideas with the given status, or all of them when status is
// empty, newest first.
func (s *Store) ListIdeas(ctx context.Context, status string) ([]Idea, error) {
	query := `SELECT ` + ideaColumns + ` FROM ideas`
	var args []any
	if status != "" {
		query += " WHERE status = ?"
		args = append(args, status)
	}
	query += " ORDER BY created_at DESC, id DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list ideas: %w", err)
	}
	defer rows.Close()

	var out []Idea
	for rows.Next() {
		i, err := scanIdea(rows)
		if err != nil {
			return nil, fmt.Errorf("list ideas: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ideas: %w", err)
	}
	return out, nil
}

// SetIdeaStatus updates an idea's status, optionally recording the project it
// became. An empty slug leaves started_project_slug NULL.
func (s *Store) SetIdeaStatus(ctx context.Context, id, status, startedProjectSlug string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE ideas SET status = ?, started_project_slug = ? WHERE id = ?`,
		status, optText(startedProjectSlug), id)
	if err != nil {
		return fmt.Errorf("set idea %s status: %w", id, err)
	}
	return requireRow(res, fmt.Sprintf("idea %s", id))
}

func scanIdea(sc scanner) (Idea, error) {
	var (
		i                  Idea
		pitch, rationale   sql.NullString
		startedSlug, genBy sql.NullString
		createdAt          string
	)
	if err := sc.Scan(&i.ID, &i.Title, &pitch, &rationale, &i.Status,
		&startedSlug, &createdAt, &genBy); err != nil {
		return Idea{}, err
	}
	i.Pitch = text(pitch)
	i.Rationale = text(rationale)
	i.StartedProjectSlug = text(startedSlug)
	i.GeneratedBy = text(genBy)

	var err error
	if i.CreatedAt, err = parseTime(createdAt); err != nil {
		return Idea{}, err
	}
	return i, nil
}

// --- identifiers ------------------------------------------------------------

// NextActionItemID returns the next globally sequential action item ID (AI-014).
func (s *Store) NextActionItemID(ctx context.Context) (string, error) {
	return s.nextID(ctx, "action_items", "AI-")
}

// NextIdeaID returns the next globally sequential idea ID (IDEA-007).
func (s *Store) NextIdeaID(ctx context.Context) (string, error) {
	return s.nextID(ctx, "ideas", "IDEA-")
}

// nextID scans the existing IDs with the given prefix and returns prefix +
// (highest + 1), zero-padded to three digits. The table name is not user input:
// it comes from the two callers above.
func (s *Store) nextID(ctx context.Context, table, prefix string) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM `+table+` WHERE id LIKE ?`, prefix+"%")
	if err != nil {
		return "", fmt.Errorf("scan %s ids: %w", table, err)
	}
	defer rows.Close()

	highest := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("scan %s ids: %w", table, err)
		}
		n, err := strconv.Atoi(strings.TrimPrefix(id, prefix))
		if err != nil {
			continue // an ID that does not fit the scheme is not a fatal problem
		}
		if n > highest {
			highest = n
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("scan %s ids: %w", table, err)
	}
	return fmt.Sprintf("%s%03d", prefix, highest+1), nil
}

// --- sessions and messages --------------------------------------------------

// InsertSession opens a chat session.
func (s *Store) InsertSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (`+sessionColumns+`) VALUES (?, ?, ?)`,
		sess.ID, optText(sess.Title), formatTime(sess.StartedAt))
	if err != nil {
		return fmt.Errorf("insert session %s: %w", sess.ID, err)
	}
	return nil
}

// GetSession returns one session by ID.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	var (
		sess      Session
		title     sql.NullString
		startedAt string
	)
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	switch err := row.Scan(&sess.ID, &title, &startedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return Session{}, fmt.Errorf("session %s: %w", id, ErrNotFound)
	case err != nil:
		return Session{}, fmt.Errorf("get session %s: %w", id, err)
	}
	sess.Title = text(title)
	var err error
	if sess.StartedAt, err = parseTime(startedAt); err != nil {
		return Session{}, fmt.Errorf("get session %s: %w", id, err)
	}
	return sess, nil
}

// ListSessions returns sessions, most recent first.
func (s *Store) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	query := `SELECT ` + sessionColumns + ` FROM sessions ORDER BY started_at DESC`
	var args []any
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var (
			sess      Session
			title     sql.NullString
			startedAt string
		)
		if err := rows.Scan(&sess.ID, &title, &startedAt); err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		sess.Title = text(title)
		if sess.StartedAt, err = parseTime(startedAt); err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return out, nil
}

// AppendMessage adds a turn to a session and returns its autoincrement ID.
func (s *Store) AppendMessage(ctx context.Context, m Message) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (session_id, role, content, created_at, model, input_tokens, output_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.SessionID, m.Role, m.Content, formatTime(m.CreatedAt),
		optText(m.Model), optInt(m.InputTokens), optInt(m.OutputTokens))
	if err != nil {
		return 0, fmt.Errorf("append message to session %s: %w", m.SessionID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("append message to session %s: %w", m.SessionID, err)
	}
	return id, nil
}

// ListMessages returns a session's turns in order.
func (s *Store) ListMessages(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages for session %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var (
			m          Message
			model      sql.NullString
			in, outTok sql.NullInt64
			createdAt  string
		)
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &createdAt,
			&model, &in, &outTok); err != nil {
			return nil, fmt.Errorf("list messages for session %s: %w", sessionID, err)
		}
		m.Model = text(model)
		m.InputTokens = nullInt(in)
		m.OutputTokens = nullInt(outTok)
		if m.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, fmt.Errorf("list messages for session %s: %w", sessionID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages for session %s: %w", sessionID, err)
	}
	return out, nil
}

// --- exec runs --------------------------------------------------------------

// InsertExecRun opens a dispatch record.
func (s *Store) InsertExecRun(ctx context.Context, r ExecRun) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO exec_runs (`+execRunColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, optText(r.ActionItemID), r.Executor, formatTime(r.StartedAt),
		timeArg(r.FinishedAt), optText(r.ExitStatus), optText(r.LogPath))
	if err != nil {
		return fmt.Errorf("insert exec run %s: %w", r.ID, err)
	}
	return nil
}

// FinishExecRun closes a dispatch record with its exit status.
func (s *Store) FinishExecRun(ctx context.Context, id, exitStatus string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE exec_runs SET finished_at = ?, exit_status = ? WHERE id = ?`,
		formatTime(at), exitStatus, id)
	if err != nil {
		return fmt.Errorf("finish exec run %s: %w", id, err)
	}
	return requireRow(res, fmt.Sprintf("exec run %s", id))
}

// GetExecRun returns one dispatch record by ID.
func (s *Store) GetExecRun(ctx context.Context, id string) (ExecRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+execRunColumns+` FROM exec_runs WHERE id = ?`, id)
	r, err := scanExecRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecRun{}, fmt.Errorf("exec run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return ExecRun{}, fmt.Errorf("get exec run %s: %w", id, err)
	}
	return r, nil
}

// ListExecRuns returns the runs for one action item, newest first. An empty
// actionItemID returns every run.
func (s *Store) ListExecRuns(ctx context.Context, actionItemID string) ([]ExecRun, error) {
	query := `SELECT ` + execRunColumns + ` FROM exec_runs`
	var args []any
	if actionItemID != "" {
		query += " WHERE action_item_id = ?"
		args = append(args, actionItemID)
	}
	query += " ORDER BY started_at DESC, id DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list exec runs: %w", err)
	}
	defer rows.Close()

	var out []ExecRun
	for rows.Next() {
		r, err := scanExecRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list exec runs: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list exec runs: %w", err)
	}
	return out, nil
}

func scanExecRun(sc scanner) (ExecRun, error) {
	var (
		r                   ExecRun
		actionItemID        sql.NullString
		startedAt           string
		finishedAt          sql.NullString
		exitStatus, logPath sql.NullString
	)
	if err := sc.Scan(&r.ID, &actionItemID, &r.Executor, &startedAt,
		&finishedAt, &exitStatus, &logPath); err != nil {
		return ExecRun{}, err
	}
	r.ActionItemID = text(actionItemID)
	r.ExitStatus = text(exitStatus)
	r.LogPath = text(logPath)

	var err error
	if r.StartedAt, err = parseTime(startedAt); err != nil {
		return ExecRun{}, err
	}
	if r.FinishedAt, err = nullTime(finishedAt); err != nil {
		return ExecRun{}, err
	}
	return r, nil
}

// requireRow turns "updated nothing" into a not-found error, so a typo in an ID
// fails loudly rather than silently succeeding.
func requireRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}
