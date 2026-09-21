package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/store"
)

func migratedStore(t *testing.T) *store.Store {
	t.Helper()
	backend := store.NewLocalBackend(filepath.Join(t.TempDir(), "apex.db"))
	t.Cleanup(func() { _ = backend.Close() })

	st, err := store.Open(context.Background(), backend)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

func TestSlug(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"ScholarRAG", "scholarrag"},
		{"Apex", "apex"},
		{"go-toml v2", "go-toml-v2"},
		{"  Leading and trailing  ", "leading-and-trailing"},
		{"Many---separators", "many-separators"},
		{"Über", "über"},
		{"2026 plans", "2026-plans"},
		{"", ""},
		{"!!!", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Slug(tt.name); got != tt.want {
				t.Errorf("Slug(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestExpandPath(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "test")

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "tilde slash", raw: "~/Development/Apex", want: filepath.Join(home, "Development", "Apex")},
		{name: "bare tilde", raw: "~", want: home},
		{name: "absolute", raw: "/opt/src/apex", want: "/opt/src/apex"},
		{name: "absolute with dots", raw: "/opt/src/../src/apex", want: "/opt/src/apex"},
		{name: "relative resolves against home", raw: "Development/Apex", want: filepath.Join(home, "Development", "Apex")},
		{name: "surrounding whitespace", raw: "  ~/Apex  ", want: filepath.Join(home, "Apex")},
		{name: "empty", raw: "   ", wantErr: "is empty"},
		{name: "other user", raw: "~someone/Apex", wantErr: "~user"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandPath(tt.raw, home)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ExpandPath(%q) = %q, want an error", tt.raw, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpandPath(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("ExpandPath(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestResolveAndVerify(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, "Development", "Apex")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		entry       contextfs.Entry
		wantResolve string // substring of the resolve error, "" when it should succeed
		wantVerify  string // substring of the verify error, "" when it should succeed
	}{
		{
			name:  "existing directory",
			entry: contextfs.Entry{Name: "Apex", Path: "~/Development/Apex"},
		},
		{
			name:       "missing directory",
			entry:      contextfs.Entry{Name: "Ghost", Path: "~/Development/Ghost"},
			wantVerify: "does not exist",
		},
		{
			name:       "path is a file",
			entry:      contextfs.Entry{Name: "File", Path: "~/notadir"},
			wantVerify: "is not a directory",
		},
		{
			name:        "unexpandable path",
			entry:       contextfs.Entry{Name: "Other", Path: "~someone/x"},
			wantResolve: "~user",
		},
		{
			name:        "name with no slug",
			entry:       contextfs.Entry{Name: "!!!", Path: "~/Development/Apex"},
			wantResolve: "no letters or digits",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Resolve(tt.entry, home)
			if tt.wantResolve != "" {
				var pe *PathError
				if !errors.As(err, &pe) {
					t.Fatalf("err = %T (%v), want *PathError", err, err)
				}
				if !pe.UserFixable() {
					t.Error("a bad path should be user-fixable")
				}
				if !strings.Contains(err.Error(), tt.wantResolve) {
					t.Errorf("err = %q, want it to mention %q", err, tt.wantResolve)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if c.RawPath != tt.entry.Path {
				t.Errorf("RawPath = %q, want the registry text %q", c.RawPath, tt.entry.Path)
			}

			err = c.Verify()
			if tt.wantVerify == "" {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				return
			}
			var pe *PathError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %T (%v), want *PathError", err, err)
			}
			if !strings.Contains(err.Error(), tt.wantVerify) {
				t.Errorf("err = %q, want it to mention %q", err, tt.wantVerify)
			}
		})
	}
}

func TestCandidateLockPath(t *testing.T) {
	c := Candidate{Slug: "scholarrag"}
	want := filepath.Join("/root", "locks", "scholarrag.lock")
	if got := c.LockPath("/root"); got != want {
		t.Errorf("LockPath = %q, want %q", got, want)
	}
}

// TestUpsertPreservesRegistration checks the thing a naive upsert gets wrong:
// store.UpsertProject writes whatever last_synced_at it is handed, so a sync
// that regenerated nothing must not wipe the time one last did.
func TestUpsertPreservesRegistration(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	c := Candidate{Slug: "apex", Name: "Apex", Path: "/tmp/apex"}

	first := time.Now().Add(-48 * time.Hour).Truncate(time.Millisecond)
	res, err := Upsert(ctx, st, c, "active", first)
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	if !res.Registered {
		t.Error("the first Upsert did not report a new registration")
	}

	synced := time.Now().Add(-1 * time.Hour).Truncate(time.Millisecond)
	if err := st.MarkProjectSynced(ctx, "apex", synced); err != nil {
		t.Fatalf("MarkProjectSynced: %v", err)
	}

	if _, err := Upsert(ctx, st, c, "paused", time.Now()); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	got, err := st.GetProject(ctx, "apex")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.Status != "paused" {
		t.Errorf("status was not updated: %+v", got)
	}
	if !got.RegisteredAt.Equal(first) {
		t.Errorf("RegisteredAt = %v, want the original %v", got.RegisteredAt, first)
	}
	if got.LastSyncedAt == nil || !got.LastSyncedAt.Equal(synced) {
		t.Errorf("LastSyncedAt = %v, want the preserved %v", got.LastSyncedAt, synced)
	}

	// An empty status must not blank a status that is already recorded.
	if _, err := Upsert(ctx, st, c, "", time.Now()); err != nil {
		t.Fatalf("third Upsert: %v", err)
	}
	got, err = st.GetProject(ctx, "apex")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "paused" {
		t.Errorf("Status = %q, want the preserved \"paused\"", got.Status)
	}
}

// TestUpsertRenameCarriesEverythingForward is the point of matching on path:
// renaming an entry in PROJECTS.md must re-key the row, not abandon it along
// with its cached digest and every action item pointing at it.
func TestUpsertRenameCarriesEverythingForward(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	const path = "/tmp/apex"
	registered := time.Now().Add(-72 * time.Hour).Truncate(time.Millisecond)
	original := Candidate{Slug: "apex", Name: "Apex", Path: path}
	if _, err := Upsert(ctx, st, original, "active", registered); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	synced := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	if err := st.MarkProjectSynced(ctx, "apex", synced); err != nil {
		t.Fatal(err)
	}

	// The history a prune-by-rename would silently destroy.
	if err := st.UpsertDigest(ctx, store.Digest{
		ProjectSlug: "apex", Body: "a cached digest", SourceHash: "hash-1",
		GeneratedAt: time.Now(), Model: "claude-opus-5",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertActionItem(ctx, store.ActionItem{
		ID: "AI-001", ProjectSlug: "apex", Title: "Finish the executor",
		Status: store.ItemAccepted, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Same path, new name.
	renamed := Candidate{Slug: "apex-cli", Name: "Apex CLI", Path: path}
	res, err := Upsert(ctx, st, renamed, "active", time.Now())
	if err != nil {
		t.Fatalf("Upsert after rename: %v", err)
	}
	if res.RenamedFrom != "apex" {
		t.Errorf("RenamedFrom = %q, want apex", res.RenamedFrom)
	}
	if res.Registered {
		t.Error("a rename was reported as a new registration")
	}

	rows, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("a rename created a second row: %+v", rows)
	}
	got := rows[0]
	if got.Slug != "apex-cli" || got.Name != "Apex CLI" || got.Path != path {
		t.Errorf("row = %+v, want the re-keyed project", got)
	}
	if !got.RegisteredAt.Equal(registered) {
		t.Errorf("RegisteredAt = %v, want the original %v", got.RegisteredAt, registered)
	}
	if got.LastSyncedAt == nil || !got.LastSyncedAt.Equal(synced) {
		t.Errorf("LastSyncedAt = %v, want the preserved %v", got.LastSyncedAt, synced)
	}

	d, err := st.GetDigest(ctx, "apex-cli")
	if err != nil {
		t.Fatalf("the digest did not come across: %v", err)
	}
	if d.SourceHash != "hash-1" || d.Body != "a cached digest" {
		t.Errorf("digest = %+v", d)
	}
	if _, err := st.GetDigest(ctx, "apex"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old digest row survived: %v", err)
	}

	item, err := st.GetActionItem(ctx, "AI-001")
	if err != nil {
		t.Fatalf("the action item did not come across: %v", err)
	}
	if item.ProjectSlug != "apex-cli" {
		t.Errorf("action item still points at %q", item.ProjectSlug)
	}
}

func TestUpsertIdentity(t *testing.T) {
	ctx := context.Background()

	t.Run("moving a project updates its path", func(t *testing.T) {
		st := migratedStore(t)
		if _, err := Upsert(ctx, st, Candidate{Slug: "apex", Name: "Apex", Path: "/tmp/a"}, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		res, err := Upsert(ctx, st, Candidate{Slug: "apex", Name: "Apex", Path: "/tmp/b"}, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if res.MovedFrom != "/tmp/a" {
			t.Errorf("MovedFrom = %q, want /tmp/a", res.MovedFrom)
		}
		rows, _ := st.ListProjects(ctx)
		if len(rows) != 1 || rows[0].Path != "/tmp/b" {
			t.Errorf("rows = %+v, want one row at /tmp/b", rows)
		}
	})

	t.Run("the same name at a new path is a move, not a second project", func(t *testing.T) {
		st := migratedStore(t)
		if _, err := Upsert(ctx, st, Candidate{Slug: "apex", Name: "Apex", Path: "/tmp/a"}, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		// Nothing is registered at /tmp/b, so the slug match wins and the row
		// follows the project rather than a duplicate appearing.
		res, err := Upsert(ctx, st, Candidate{Slug: "apex", Name: "Apex", Path: "/tmp/b"}, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if res.MovedFrom != "/tmp/a" || res.Registered {
			t.Errorf("result = %+v, want a move from /tmp/a", res)
		}
		rows, _ := st.ListProjects(ctx)
		if len(rows) != 1 || rows[0].Path != "/tmp/b" {
			t.Errorf("rows = %+v, want one row at /tmp/b", rows)
		}
	})

	t.Run("a rename onto an occupied key is refused", func(t *testing.T) {
		st := migratedStore(t)
		if _, err := Upsert(ctx, st, Candidate{Slug: "apex", Name: "Apex", Path: "/tmp/a"}, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := Upsert(ctx, st, Candidate{Slug: "atlas", Name: "Atlas", Path: "/tmp/b"}, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		// Renaming Atlas to Apex would take a key another project holds.
		_, err := Upsert(ctx, st, Candidate{Slug: "apex", Name: "Apex", Path: "/tmp/b"}, "", time.Now())
		var conflict *SlugConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("err = %T (%v), want *SlugConflictError", err, err)
		}
		if !conflict.UserFixable() {
			t.Error("a slug collision should be user-fixable")
		}
		for _, want := range []string{"apex", "/tmp/a", "/tmp/b"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want it to mention %q", err, want)
			}
		}
		rows, _ := st.ListProjects(ctx)
		if len(rows) != 2 {
			t.Errorf("the refused rename changed the rows: %+v", rows)
		}
	})
}

func TestOrphans(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)

	for _, c := range []Candidate{
		{Slug: "apex", Name: "Apex", Path: "/tmp/apex"},
		{Slug: "atlas", Name: "Atlas", Path: "/tmp/atlas"},
		{Slug: "loom", Name: "Loom", Path: "/tmp/loom"},
	} {
		if _, err := Upsert(ctx, st, c, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name  string
		paths map[string]bool
		want  []string
	}{
		{
			name:  "every project still registered",
			paths: map[string]bool{"/tmp/apex": true, "/tmp/atlas": true, "/tmp/loom": true},
		},
		{
			name:  "one entry removed from PROJECTS.md",
			paths: map[string]bool{"/tmp/apex": true, "/tmp/loom": true},
			want:  []string{"atlas"},
		},
		{
			name:  "an empty registry orphans everything",
			paths: map[string]bool{},
			want:  []string{"apex", "atlas", "loom"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Orphans(ctx, st, tt.paths)
			if err != nil {
				t.Fatalf("Orphans: %v", err)
			}
			var slugs []string
			for _, o := range got {
				slugs = append(slugs, o.Slug)
			}
			if len(slugs) != len(tt.want) {
				t.Fatalf("Orphans = %v, want %v", slugs, tt.want)
			}
			for i := range slugs {
				if slugs[i] != tt.want[i] {
					t.Errorf("Orphans = %v, want %v", slugs, tt.want)
					break
				}
			}
		})
	}

	// Reporting an orphan must never remove it.
	if rows, err := st.ListProjects(ctx); err != nil || len(rows) != 3 {
		t.Errorf("Orphans pruned rows: %+v (%v)", rows, err)
	}
}

func TestCheckFreshness(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	c := Candidate{Slug: "apex", Name: "Apex", Path: "/tmp/apex"}
	if _, err := Upsert(ctx, st, c, "active", time.Now()); err != nil {
		t.Fatal(err)
	}

	f, cached, err := CheckFreshness(ctx, st, "apex", "hash-1")
	if err != nil {
		t.Fatalf("CheckFreshness: %v", err)
	}
	if f != Unknown || cached != "" {
		t.Errorf("with no cached digest: got (%v, %q), want (none, \"\")", f, cached)
	}
	if !f.NeedsRegeneration() {
		t.Error("a project with no digest needs one generated")
	}

	if err := st.UpsertDigest(ctx, store.Digest{
		ProjectSlug: "apex",
		Body:        "a digest",
		SourceHash:  "hash-1",
		GeneratedAt: time.Now(),
		Model:       "claude-opus-5",
	}); err != nil {
		t.Fatal(err)
	}

	f, cached, err = CheckFreshness(ctx, st, "apex", "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if f != Fresh || cached != "hash-1" {
		t.Errorf("matching hash: got (%v, %q), want (fresh, hash-1)", f, cached)
	}
	if f.NeedsRegeneration() {
		t.Error("a fresh digest does not need regenerating")
	}

	f, cached, err = CheckFreshness(ctx, st, "apex", "hash-2")
	if err != nil {
		t.Fatal(err)
	}
	if f != Stale || cached != "hash-1" {
		t.Errorf("changed hash: got (%v, %q), want (stale, hash-1)", f, cached)
	}
	if !f.NeedsRegeneration() {
		t.Error("a stale digest needs regenerating")
	}
}

func TestFreshnessString(t *testing.T) {
	for _, tt := range []struct {
		f    Freshness
		want string
	}{{Fresh, "fresh"}, {Stale, "stale"}, {Unknown, "none"}} {
		if got := tt.f.String(); got != tt.want {
			t.Errorf("Freshness(%d) = %q, want %q", tt.f, got, tt.want)
		}
	}
}
