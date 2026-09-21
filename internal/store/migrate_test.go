package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore opens a fresh database in a temp directory.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	backend := NewLocalBackend(filepath.Join(t.TempDir(), "apex.db"))
	t.Cleanup(func() { _ = backend.Close() })

	st, err := Open(context.Background(), backend)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

// migratedStore opens a store and applies every migration.
func migratedStore(t *testing.T) *Store {
	t.Helper()
	st := newTestStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

func TestMigrationsAreWellFormed(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations were embedded")
	}
	for i, m := range migrations {
		if i > 0 && m.Version <= migrations[i-1].Version {
			t.Errorf("migrations are not strictly ordered: %d after %d", m.Version, migrations[i-1].Version)
		}
		if m.Name == "" || strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %d is missing a name or body", m.Version)
		}
		// Additive-only (DESIGN.md §7): a destructive statement in an
		// embedded migration is a one-way door with no rollback path.
		upper := strings.ToUpper(m.SQL)
		for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "ALTER TABLE RENAME", "RENAME TO"} {
			if strings.Contains(upper, forbidden) {
				t.Errorf("migration %03d_%s contains %q; migrations must be additive-only",
					m.Version, m.Name, forbidden)
			}
		}
	}
}

func TestParseMigrationName(t *testing.T) {
	tests := []struct {
		filename    string
		wantVersion int
		wantName    string
		wantErr     bool
	}{
		{filename: "001_init.sql", wantVersion: 1, wantName: "init"},
		{filename: "012_add_tags.sql", wantVersion: 12, wantName: "add_tags"},
		{filename: "003_a_b_c.sql", wantVersion: 3, wantName: "a_b_c"},
		{filename: "init.sql", wantErr: true},
		{filename: "abc_init.sql", wantErr: true},
		{filename: "000_zero.sql", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			version, name, err := parseMigrationName(tt.filename)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseMigrationName(%q) = %d, %q; want an error", tt.filename, version, name)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMigrationName(%q): %v", tt.filename, err)
			}
			if version != tt.wantVersion || name != tt.wantName {
				t.Errorf("= %d, %q; want %d, %q", version, name, tt.wantVersion, tt.wantName)
			}
		})
	}
}

func TestMigrateAppliesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	before, err := st.Status(ctx)
	if err != nil {
		t.Fatalf("Status before: %v", err)
	}
	if len(before.Applied) != 0 || len(before.Pending) == 0 {
		t.Fatalf("fresh database: %d applied, %d pending; want 0 applied and some pending",
			len(before.Applied), len(before.Pending))
	}

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	after, err := st.Status(ctx)
	if err != nil {
		t.Fatalf("Status after: %v", err)
	}
	if len(after.Pending) != 0 {
		t.Errorf("%d migrations still pending after Migrate", len(after.Pending))
	}
	if len(after.Applied) != len(before.Pending) {
		t.Errorf("applied %d migrations, want %d", len(after.Applied), len(before.Pending))
	}
	if after.Current() != after.Applied[len(after.Applied)-1].Version {
		t.Errorf("Current() = %d, want the highest applied version", after.Current())
	}
	for _, a := range after.Applied {
		if a.AppliedBy == "" {
			t.Errorf("migration %d recorded no applied_by", a.Version)
		}
		if time.Since(a.AppliedAt) > time.Minute {
			t.Errorf("migration %d applied_at = %v, want a fresh timestamp", a.Version, a.AppliedAt)
		}
	}

	// Running Migrate twice must be a no-op, not an error and not a re-apply.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	again, err := st.Status(ctx)
	if err != nil {
		t.Fatalf("Status after second Migrate: %v", err)
	}
	if len(again.Applied) != len(after.Applied) {
		t.Errorf("second Migrate changed the applied set: %d, want %d", len(again.Applied), len(after.Applied))
	}
	if !again.Applied[0].AppliedAt.Equal(after.Applied[0].AppliedAt) {
		t.Error("second Migrate rewrote an existing schema_migrations row")
	}
}

func TestMigrateCreatesEverySchemaTable(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	want := []string{
		"projects", "digests", "action_items", "ideas",
		"sessions", "messages", "exec_runs",
		"schema_migrations", "migration_lock",
	}
	for _, table := range want {
		var name string
		err := st.DB().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing after migration: %v", table, err)
		}
	}
}

func TestMigrateReleasesTheMigrationLock(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	var held int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM migration_lock WHERE id = 1`).Scan(&held); err != nil {
		t.Fatalf("count migration_lock: %v", err)
	}
	if held != 0 {
		t.Errorf("migration_lock still holds %d row(s) after a successful migrate", held)
	}
}

func TestMigrateRefusesWhenOffline(t *testing.T) {
	ctx := context.Background()
	backend := &offlineBackend{LocalBackend: NewLocalBackend(filepath.Join(t.TempDir(), "apex.db"))}
	t.Cleanup(func() { _ = backend.Close() })

	st, err := Open(ctx, backend)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	err = st.Migrate(ctx)
	if !errors.Is(err, ErrOfflineMigration) {
		t.Fatalf("Migrate while offline = %v, want ErrOfflineMigration", err)
	}
	if st.Online() {
		t.Error("Online() = true on an offline backend")
	}
}

// offlineBackend is a LocalBackend that claims writes cannot reach the primary,
// standing in for a TursoBackend with no connectivity.
type offlineBackend struct{ *LocalBackend }

func (b *offlineBackend) Online() bool { return false }

func TestMigrationLockBlocksAConcurrentMigrator(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Simulate another machine mid-migration.
	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO migration_lock (id, holder, acquired_at) VALUES (1, ?, ?)`,
		"other-machine/pid=1", formatTime(time.Now())); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	err := st.Migrate(ctx)
	var busy *MigrationLockBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("Migrate = %v, want *MigrationLockBusyError", err)
	}
	if busy.Holder != "other-machine/pid=1" {
		t.Errorf("Holder = %q, want the seeded holder", busy.Holder)
	}
	if !busy.UserFixable() {
		t.Error("MigrationLockBusyError should be user-fixable")
	}

	// A lock older than the staleness window is reclaimed: a crashed process
	// cannot release a database row the way the kernel releases an flock.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE migration_lock SET acquired_at = ? WHERE id = 1`,
		formatTime(time.Now().Add(-2*migrationLockStaleAfter))); err != nil {
		t.Fatalf("age the lock: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate after the lock went stale: %v", err)
	}
}

func TestLocalBackend(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "apex.db")
	backend := NewLocalBackend(path)
	t.Cleanup(func() { _ = backend.Close() })

	db, err := backend.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if again, err := backend.Open(ctx); err != nil || again != db {
		t.Errorf("second Open returned a different handle: %v", err)
	}
	if !backend.Online() {
		t.Error("Online() = false, want true for a local file")
	}
	if err := backend.Sync(ctx); err != nil {
		t.Errorf("Sync() = %v, want nil (no-op)", err)
	}
	if !strings.Contains(backend.Describe(), path) {
		t.Errorf("Describe() = %q, want it to name the database path", backend.Describe())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database file was not created: %v", err)
	}

	empty := NewLocalBackend("")
	if _, err := empty.Open(ctx); err == nil {
		t.Error("Open with an empty path returned no error")
	}
}

// TestNoSelectStar enforces the invariant the additive-migration guarantee
// depends on (DESIGN.md §7, and the open question in §18).
func TestNoSelectStar(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".go" && ext != ".sql" {
			return nil
		}
		if strings.HasSuffix(path, "migrate_test.go") {
			return nil // this file names the pattern it forbids
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			// Comments may name the pattern; only code is checked.
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "--") ||
				strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "#") {
				continue
			}
			upper := strings.ToUpper(line)
			if strings.Contains(upper, "SELECT *") || strings.Contains(upper, "SELECT\t*") {
				t.Errorf("%s: SELECT * is forbidden; name every column\n\t%s", path, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// applyThrough applies migrations up to and including version n, leaving the
// rest pending. It is how a test stands in a database that predates a
// migration without hand-writing the schema.
func applyThrough(t *testing.T, st *Store, n int) {
	t.Helper()
	ctx := context.Background()
	if err := st.bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	all, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	for _, m := range all {
		if m.Version > n {
			return
		}
		if err := st.applyOne(ctx, m); err != nil {
			t.Fatalf("apply %03d_%s: %v", m.Version, m.Name, err)
		}
	}
}

func seedProjectAt(t *testing.T, st *Store, slug, path string) {
	t.Helper()
	err := st.UpsertProject(context.Background(), Project{
		Slug: slug, Name: slug, Path: path, RegisteredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("seed project %s: %v", slug, err)
	}
}

// TestUniqueProjectPathApplies covers migration 002 against the database it
// will actually meet: one with no duplicate paths in it.
func TestUniqueProjectPathApplies(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	status, err := st.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(status.Pending) != 0 {
		t.Fatalf("%d migrations pending after Migrate", len(status.Pending))
	}
	var applied bool
	for _, a := range status.Applied {
		if a.Version == 2 {
			applied = true
		}
	}
	if !applied {
		t.Fatalf("migration 002 is not in %+v", status.Applied)
	}

	// Two projects at two paths are fine.
	seedProjectAt(t, st, "apex", "/tmp/apex")
	seedProjectAt(t, st, "scholarrag", "/tmp/scholarrag")

	// A second project at an occupied path is now refused by the schema,
	// which is the whole point: path is identity (DESIGN.md §6), and until
	// now only application code said so.
	err = st.UpsertProject(ctx, Project{
		Slug: "apex-cli", Name: "Apex CLI", Path: "/tmp/apex", RegisteredAt: time.Now(),
	})
	if err == nil {
		t.Fatal("a second project was registered at an occupied path")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Errorf("err = %q, want a uniqueness violation", err)
	}

	// Moving a project onto a free path still works.
	seedProjectAt(t, st, "apex", "/tmp/apex-moved")
	got, err := st.GetProject(ctx, "apex")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Path != "/tmp/apex-moved" {
		t.Errorf("Path = %q, want the moved path", got.Path)
	}
}

// TestUniqueProjectPathFailsLoudlyOnDuplicates is the case the migration is
// allowed to fail in. Forward-only migrations have no rollback, so the
// failure has to be loud, has to leave schema_migrations untouched, and has
// to name the rows that caused it — none of which a bare "UNIQUE constraint
// failed: projects.path" does.
func TestUniqueProjectPathFailsLoudlyOnDuplicates(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// A database as it stood before 002 existed.
	applyThrough(t, st, 1)
	seedProjectAt(t, st, "apex", "/tmp/apex")
	seedProjectAt(t, st, "apex-cli", "/tmp/apex")
	seedProjectAt(t, st, "atlas", "/tmp/atlas")

	err := st.Migrate(ctx)
	if err == nil {
		t.Fatal("Migrate succeeded against duplicate paths")
	}

	var conflict *MigrationDataConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %T (%v), want *MigrationDataConflictError", err, err)
	}
	if conflict.Version != 2 {
		t.Errorf("Version = %d, want 2", conflict.Version)
	}
	if !conflict.UserFixable() {
		t.Error("duplicate rows are the user's to resolve, not an internal failure")
	}
	// The message must name the conflict: the path, and both projects holding
	// it. The project that is not in conflict must not be dragged in.
	for _, want := range []string{"/tmp/apex", "apex", "apex-cli"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "/tmp/atlas") {
		t.Errorf("err = %q, want it to name only the conflicting rows", err)
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Errorf("err = %q, want the underlying database error kept", err)
	}

	// Nothing was recorded: a migration that failed must not look applied.
	status, statusErr := st.Status(ctx)
	if statusErr != nil {
		t.Fatalf("Status: %v", statusErr)
	}
	for _, a := range status.Applied {
		if a.Version == 2 {
			t.Error("schema_migrations recorded a migration that failed")
		}
	}
	if len(status.Pending) != 1 || status.Pending[0].Version != 2 {
		t.Errorf("Pending = %+v, want migration 2 still outstanding", status.Pending)
	}

	// And the migration lock was released, so a rerun is not wedged.
	if err := st.Migrate(ctx); err == nil {
		t.Fatal("the second attempt succeeded; the duplicates are still there")
	}

	// Once the duplicate is gone, the same migration applies.
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM projects WHERE slug = ?`, "apex-cli"); err != nil {
		t.Fatalf("remove duplicate: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate after resolving the conflict: %v", err)
	}
}
