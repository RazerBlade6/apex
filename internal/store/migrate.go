package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var embedded embed.FS

// ErrOfflineMigration is returned when a migration is attempted while writes
// cannot reach the primary. DDL forwards to the primary and is immediately
// global, so a half-applied migration is not an option (DESIGN.md §7).
var ErrOfflineMigration = errors.New("store: cannot migrate while offline (writes forward to the remote primary); reconnect and retry")

// migrationLockStaleAfter is how long a migration lock row may sit before
// another process reclaims it. A crashed process cannot release a database row
// the way the kernel releases a file lock, so this timeout exists here and
// deliberately does not exist in package lock.
const migrationLockStaleAfter = 5 * time.Minute

// Migration is one embedded .sql file.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// AppliedMigration is a row of schema_migrations.
type AppliedMigration struct {
	Version   int
	Name      string
	AppliedAt time.Time
	AppliedBy string
}

// MigrationLockBusyError reports that another machine holds the migration lock.
type MigrationLockBusyError struct {
	Holder     string
	AcquiredAt time.Time
}

func (e *MigrationLockBusyError) Error() string {
	return fmt.Sprintf("store: migration lock held by %s since %s",
		e.Holder, e.AcquiredAt.Format(time.RFC3339))
}

// UserFixable: the fix is to wait for, or investigate, the other migrator.
func (e *MigrationLockBusyError) UserFixable() bool { return true }

// bootstrapDDL creates the two tables the migrator itself needs before it can
// record or serialize anything. It mirrors the head of 001_init.sql, which
// declares both with IF NOT EXISTS for exactly this reason.
const bootstrapDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    applied_at  DATETIME NOT NULL,
    applied_by  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS migration_lock (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    holder      TEXT NOT NULL,
    acquired_at DATETIME NOT NULL
);`

// Migrations returns every embedded migration, ordered by version.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(embedded, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d (%s and %s)", version, prev, e.Name())
		}
		seen[version] = e.Name()
		body, err := embedded.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		out = append(out, Migration{Version: version, Name: name, SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseMigrationName splits NNN_name.sql into its version and name.
func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	numPart, namePart, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("migration %s: expected NNN_name.sql", filename)
	}
	version, err := strconv.Atoi(numPart)
	if err != nil {
		return 0, "", fmt.Errorf("migration %s: version prefix %q is not a number: %w", filename, numPart, err)
	}
	if version <= 0 {
		return 0, "", fmt.Errorf("migration %s: version must be positive", filename)
	}
	return version, namePart, nil
}

// MigrationStatus is what `apex doctor` reports.
type MigrationStatus struct {
	Applied []AppliedMigration
	Pending []Migration
}

// Current returns the highest applied version, or 0 if none.
func (s MigrationStatus) Current() int {
	if len(s.Applied) == 0 {
		return 0
	}
	return s.Applied[len(s.Applied)-1].Version
}

// Status reports which migrations have been applied and which remain. It is
// read-only apart from creating the bookkeeping tables, so it is safe to call
// before Migrate.
func (s *Store) Status(ctx context.Context) (MigrationStatus, error) {
	if err := s.bootstrap(ctx); err != nil {
		return MigrationStatus{}, err
	}
	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return MigrationStatus{}, err
	}
	all, err := Migrations()
	if err != nil {
		return MigrationStatus{}, err
	}
	return MigrationStatus{Applied: applied, Pending: pending(all, applied)}, nil
}

// Migrate applies every pending migration, in order, under the cross-machine
// migration lock. Migrations are forward-only: there is no rollback path, so a
// second call with nothing pending is a no-op.
func (s *Store) Migrate(ctx context.Context) error {
	if !s.backend.Online() {
		return ErrOfflineMigration // explicit, not a driver error
	}
	if err := s.backend.Sync(ctx); err != nil { // pull latest first
		return fmt.Errorf("sync before migrating: %w", err)
	}
	if err := s.bootstrap(ctx); err != nil {
		return err
	}

	release, err := s.acquireMigrationLock(ctx) // cross-machine
	if err != nil {
		return err
	}
	defer func() {
		// Releasing uses a fresh context: the lock row must come out even
		// when the caller's context is already cancelled.
		relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		release(relCtx)
	}()

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}
	all, err := Migrations()
	if err != nil {
		return err
	}
	for _, m := range pending(all, applied) {
		if err := s.applyOne(ctx, m); err != nil {
			if detail := s.diagnose(ctx, m); detail != "" {
				return &MigrationDataConflictError{
					Version: m.Version, Name: m.Name, Detail: detail, Err: err,
				}
			}
			return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

// MigrationDataConflictError reports a migration that failed because the rows
// already in the database violate the invariant it introduces.
//
// It is a distinct type because the response is different: a failed migration
// is usually a bug in the migration, but this one is a data problem the user
// fixes, and the message has to name the conflicting rows for that to be
// possible.
type MigrationDataConflictError struct {
	Version int
	Name    string
	Detail  string
	Err     error
}

func (e *MigrationDataConflictError) Error() string {
	return fmt.Sprintf("migration %d (%s): %v\n%s", e.Version, e.Name, e.Err, e.Detail)
}

// UserFixable: the fix is to resolve the conflicting rows and rerun.
func (e *MigrationDataConflictError) UserFixable() bool { return true }

func (e *MigrationDataConflictError) Unwrap() error { return e.Err }

// migrationDiagnostics maps a migration version to a function that explains a
// failure in terms of the data that caused it.
//
// Only migrations that can fail against valid-until-now data need an entry.
// The alternative — letting `UNIQUE constraint failed: projects.path` be the
// whole message — names the column but not the rows, which is the half of the
// answer the user cannot look up without opening SQLite themselves.
var migrationDiagnostics = map[int]func(ctx context.Context, db *sql.DB) string{
	2: duplicateProjectPaths,
}

// diagnose returns extra detail for a failed migration, or "" if there is
// none to give. A diagnostic that itself fails is silently dropped: it exists
// to improve an error message, never to replace one.
func (s *Store) diagnose(ctx context.Context, m Migration) string {
	fn, ok := migrationDiagnostics[m.Version]
	if !ok {
		return ""
	}
	return fn(ctx, s.db)
}

// duplicateProjectPaths names the projects that share a path, which is the
// only way migration 002 can fail against an otherwise healthy database.
func duplicateProjectPaths(ctx context.Context, db *sql.DB) string {
	rows, err := db.QueryContext(ctx, `
		SELECT path, COUNT(*) AS holders, GROUP_CONCAT(slug, ', ') AS slugs
		FROM projects
		GROUP BY path
		HAVING COUNT(*) > 1
		ORDER BY path`)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var (
			path    string
			holders int
			slugs   string
		)
		if err := rows.Scan(&path, &holders, &slugs); err != nil {
			return ""
		}
		fmt.Fprintf(&b, "  %d projects share the path %s: %s\n", holders, path, slugs)
	}
	if rows.Err() != nil || b.Len() == 0 {
		return ""
	}
	b.WriteString("  path is a project's identity (DESIGN.md §6): each one may hold only one.\n")
	b.WriteString("  remove the duplicate entry from PROJECTS.md and delete its row, then rerun.")
	return b.String()
}

func (s *Store) bootstrap(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, bootstrapDDL); err != nil {
		return fmt.Errorf("create migration bookkeeping tables: %w", err)
	}
	return nil
}

func (s *Store) appliedVersions(ctx context.Context) ([]AppliedMigration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT version, name, applied_at, applied_by
		FROM schema_migrations
		ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var (
			a         AppliedMigration
			appliedAt string
		)
		if err := rows.Scan(&a.Version, &a.Name, &appliedAt, &a.AppliedBy); err != nil {
			return nil, fmt.Errorf("scan schema_migrations row: %w", err)
		}
		if a.AppliedAt, err = parseTime(appliedAt); err != nil {
			return nil, fmt.Errorf("schema_migrations version %d: %w", a.Version, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return out, nil
}

func pending(all []Migration, applied []AppliedMigration) []Migration {
	done := make(map[int]bool, len(applied))
	for _, a := range applied {
		done[a.Version] = true
	}
	var out []Migration
	for _, m := range all {
		if !done[m.Version] {
			out = append(out, m)
		}
	}
	return out
}

// applyOne runs a migration and records it in the same transaction, so a
// migration is never applied without being recorded.
func (s *Store) applyOne(ctx context.Context, m Migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("execute: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO schema_migrations (version, name, applied_at, applied_by)
		VALUES (?, ?, ?, ?)`,
		m.Version, m.Name, formatTime(time.Now()), hostname()); err != nil {
		return fmt.Errorf("record in schema_migrations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// acquireMigrationLock takes the cross-machine lock row. Unlike a flock, a
// crashed holder leaves the row behind, so a sufficiently old lock is reclaimed.
func (s *Store) acquireMigrationLock(ctx context.Context) (func(context.Context) error, error) {
	holder := fmt.Sprintf("%s/pid=%d/%s", hostname(), os.Getpid(), formatTime(time.Now()))

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO migration_lock (id, holder, acquired_at)
		VALUES (1, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		holder, formatTime(time.Now())); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}

	current, acquiredRaw, acquiredAt, err := s.readMigrationLock(ctx)
	if err != nil {
		return nil, err
	}
	if current == holder {
		return s.releaseMigrationLockFunc(holder), nil
	}

	// Someone else holds it. Reclaim only if it is stale, and only with a
	// compare-and-swap against the exact row we read, so two machines
	// reclaiming at once cannot both win.
	if time.Since(acquiredAt) < migrationLockStaleAfter {
		return nil, &MigrationLockBusyError{Holder: current, AcquiredAt: acquiredAt}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE migration_lock
		SET holder = ?, acquired_at = ?
		WHERE id = 1 AND holder = ? AND acquired_at = ?`,
		holder, formatTime(time.Now()), current, acquiredRaw)
	if err != nil {
		return nil, fmt.Errorf("reclaim stale migration lock: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return nil, &MigrationLockBusyError{Holder: current, AcquiredAt: acquiredAt}
	}
	return s.releaseMigrationLockFunc(holder), nil
}

func (s *Store) releaseMigrationLockFunc(holder string) func(context.Context) error {
	return func(ctx context.Context) error {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM migration_lock WHERE id = 1 AND holder = ?`, holder); err != nil {
			return fmt.Errorf("release migration lock: %w", err)
		}
		return nil
	}
}

// readMigrationLock returns the holder, the acquired_at column exactly as
// stored (for compare-and-swap) and its parsed value.
func (s *Store) readMigrationLock(ctx context.Context) (holder, acquiredRaw string, acquiredAt time.Time, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT holder, acquired_at FROM migration_lock WHERE id = 1`)
	switch scanErr := row.Scan(&holder, &acquiredRaw); {
	case errors.Is(scanErr, sql.ErrNoRows):
		return "", "", time.Time{}, fmt.Errorf("migration lock row vanished after insert")
	case scanErr != nil:
		return "", "", time.Time{}, fmt.Errorf("read migration lock: %w", scanErr)
	}
	acquiredAt, err = parseTime(acquiredRaw)
	if err != nil {
		// An unparseable timestamp must not wedge migrations forever. Treat
		// it as infinitely old; the swap still matches on the raw value, so
		// reclaiming stays race-free.
		return holder, acquiredRaw, time.Time{}, nil
	}
	return holder, acquiredRaw, acquiredAt, nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-host"
	}
	return h
}
