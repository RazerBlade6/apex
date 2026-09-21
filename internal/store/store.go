package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Store is the database handle plus the backend that produced it.
type Store struct {
	backend Backend
	db      *sql.DB
}

// Open connects through the backend and returns a Store. It does not migrate;
// callers decide when that happens (see Migrate).
func Open(ctx context.Context, b Backend) (*Store, error) {
	if b == nil {
		return nil, fmt.Errorf("store: nil backend")
	}
	db, err := b.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return &Store{backend: b, db: db}, nil
}

// DB exposes the underlying pool for query helpers and tests.
func (s *Store) DB() *sql.DB { return s.db }

// Backend returns the backend this store was opened through.
func (s *Store) Backend() Backend { return s.backend }

// Online reports whether writes can currently succeed.
func (s *Store) Online() bool { return s.backend.Online() }

// Sync pulls the latest state from the remote primary, if there is one.
func (s *Store) Sync(ctx context.Context) error {
	if err := s.backend.Sync(ctx); err != nil {
		return fmt.Errorf("sync store: %w", err)
	}
	return nil
}

// timeLayout is how Apex writes DATETIME values. Storing an explicit UTC
// RFC3339 string keeps the representation independent of driver-specific time
// handling, and keeps values comparable as text in SQL.
const timeLayout = time.RFC3339Nano

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		// Tolerate second-precision values written by another tool or an
		// older binary.
		if t2, err2 := time.Parse(time.RFC3339, s); err2 == nil {
			return t2.UTC(), nil
		}
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// nullTime converts a nullable DATETIME column into a *time.Time.
func nullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// timeArg converts an optional timestamp into a query argument.
func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// nullString converts an optional text column into a plain string.
func text(ns sql.NullString) string { return ns.String }

// optText converts a possibly-empty string into a query argument, writing NULL
// rather than "" so the distinction survives a round trip.
func optText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// optInt converts an optional integer into a query argument.
func optInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullInt(ni sql.NullInt64) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int64)
	return &v
}
