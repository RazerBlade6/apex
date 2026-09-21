// Package store owns Apex's SQLite persistence: the backend abstraction, the
// forward-only migrator, and typed query helpers.
//
// Two invariants hold throughout, and both come from DESIGN.md §7:
//   - Never SELECT *. Every column is named explicitly. This is what makes an
//     additive migration safe for a binary that predates it.
//   - Migrations are additive-only and forward-only. There is no rollback path.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	// modernc.org/sqlite is a pure-Go driver. A CGO driver (mattn/go-sqlite3,
	// go-libsql) would forfeit static linking and cross-compilation, which is
	// the main reason Go was chosen. See DESIGN.md §3.
	_ "modernc.org/sqlite"
)

// Backend is the persistence target (DESIGN.md §7). LocalBackend is the
// default; a TursoBackend replicating to Turso Cloud fits behind the same
// interface without changing anything around it.
type Backend interface {
	Open(ctx context.Context) (*sql.DB, error)
	Sync(ctx context.Context) error // no-op for local-only
	Online() bool                   // whether writes can currently succeed
	Describe() string               // shown by `apex doctor`
}

// LocalBackend is a plain local SQLite file: no network, no sync, no
// degradation offline.
type LocalBackend struct {
	// Path is the database file. An empty path is rejected by Open rather
	// than silently becoming an anonymous temporary database.
	Path string

	mu sync.Mutex
	db *sql.DB
}

// NewLocalBackend returns a backend over the SQLite file at path.
func NewLocalBackend(path string) *LocalBackend {
	return &LocalBackend{Path: path}
}

// Open connects to the database, creating the file and its parent directory if
// needed. Repeated calls return the same handle: *sql.DB is a pool and is safe
// for concurrent use.
func (b *LocalBackend) Open(ctx context.Context) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.db != nil {
		return b.db, nil
	}
	if b.Path == "" {
		return nil, fmt.Errorf("store: local backend has no database path")
	}
	if dir := filepath.Dir(b.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}

	// _pragma arguments are applied to every pooled connection by the
	// modernc driver. WAL suits a CLI with overlapping short-lived
	// processes; foreign_keys must be on per-connection for the ON DELETE
	// clauses in the schema to mean anything.
	dsn := b.Path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", b.Path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to sqlite %s: %w", b.Path, err)
	}
	b.db = db
	return db, nil
}

// Sync is a no-op: there is no remote primary to reconcile with.
func (b *LocalBackend) Sync(ctx context.Context) error { return ctx.Err() }

// Online is always true: writes to a local file never require connectivity.
func (b *LocalBackend) Online() bool { return true }

// Describe is shown by `apex doctor`.
func (b *LocalBackend) Describe() string {
	return fmt.Sprintf("local sqlite (modernc, pure Go) at %s", b.Path)
}

// Close releases the pool. Not part of Backend: only the process that opened
// the backend closes it.
func (b *LocalBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.db == nil {
		return nil
	}
	err := b.db.Close()
	b.db = nil
	if err != nil {
		return fmt.Errorf("close sqlite %s: %w", b.Path, err)
	}
	return nil
}
