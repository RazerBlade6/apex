// Package project turns registry entries into projects Apex can reason about:
// it resolves their paths, reads their git state, assembles the sources a
// digest is generated from, and keeps the projects table in step.
//
// It generates no digests. Digest generation needs a provider and arrives with
// the advisor loop (DESIGN.md §16, M4); this package assembles the inputs and
// computes the cache key those inputs hash to.
package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/store"
)

// Slug derives the projects-table primary key from a registry name: lowercase,
// with every run of characters that is not a letter or digit folded to a single
// hyphen. "ScholarRAG" becomes "scholarrag"; "go-toml v2" becomes "go-toml-v2".
func Slug(name string) string {
	var b strings.Builder
	lastHyphen := true // suppresses a leading hyphen
	for _, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// PathError reports a registry path Apex could not use. It is user-fixable:
// the fix is to correct the `path:` line, or to move the project back.
type PathError struct {
	Name string
	Path string // as written in PROJECTS.md
	Msg  string
	Err  error
}

func (e *PathError) Error() string {
	msg := fmt.Sprintf("%s: path %q %s", e.Name, e.Path, e.Msg)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// UserFixable marks a bad path as an edit the user makes, not a bug in Apex.
func (e *PathError) UserFixable() bool { return true }

func (e *PathError) Unwrap() error { return e.Err }

// ExpandPath resolves a path as written in PROJECTS.md.
//
// A leading `~` or `~/` expands to home. A relative path resolves against home
// as well, deliberately rather than against the working directory: the registry
// is a file in ~/.apex, so `apex sync` must mean the same thing from every
// directory it is run in.
func ExpandPath(raw, home string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", errors.New("is empty")
	}
	switch {
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "~"):
		// ~otheruser is not supported: Apex is single-user and guessing at
		// another account's home directory would be worse than refusing.
		return "", errors.New("uses ~user, which Apex does not expand")
	case !filepath.IsAbs(p):
		p = filepath.Join(home, p)
	}
	return filepath.Clean(p), nil
}

// Home returns the directory registry paths resolve against.
func Home() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return home, nil
}

// Candidate is a registry entry resolved to a directory on this machine. It is
// a candidate rather than a project because the directory may not exist: the
// registry says where a project should be, and only the filesystem says whether
// it is there.
type Candidate struct {
	Slug    string
	Name    string
	RawPath string // exactly as written in PROJECTS.md
	Path    string // resolved absolute path
	Summary string // the registry's current summary line
	Line    int    // 1-based line of the entry's heading
}

// Resolve turns a parsed registry entry into a Candidate.
func Resolve(entry contextfs.Entry, home string) (Candidate, error) {
	slug := Slug(entry.Name)
	if slug == "" {
		return Candidate{}, &PathError{
			Name: entry.Name,
			Path: entry.Path,
			Msg:  "belongs to an entry whose name has no letters or digits",
		}
	}
	path, err := ExpandPath(entry.Path, home)
	if err != nil {
		return Candidate{}, &PathError{Name: entry.Name, Path: entry.Path, Msg: err.Error()}
	}
	return Candidate{
		Slug:    slug,
		Name:    entry.Name,
		RawPath: entry.Path,
		Path:    path,
		Summary: entry.Summary,
		Line:    entry.Line,
	}, nil
}

// Verify reports whether the candidate's directory exists. A missing directory
// is reported and skipped by the caller, never fatal: one project moved out
// from under the registry must not stop the rest of a sync.
func (c Candidate) Verify() error {
	info, err := os.Stat(c.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &PathError{Name: c.Name, Path: c.RawPath, Msg: "does not exist"}
	case err != nil:
		return &PathError{Name: c.Name, Path: c.RawPath, Msg: "could not be read", Err: err}
	case !info.IsDir():
		return &PathError{Name: c.Name, Path: c.RawPath, Msg: "is not a directory"}
	}
	return nil
}

// LockPath returns the machine-local lock file guarding this project's working
// tree (DESIGN.md §10), given the Apex state root.
func (c Candidate) LockPath(root string) string {
	return filepath.Join(root, "locks", c.Slug+".lock")
}

// UpsertResult reports what registering a project actually did, so `apex sync`
// can say "renamed" or "moved" rather than silently doing either.
type UpsertResult struct {
	// Registered is true when no row existed and one was created.
	Registered bool
	// RenamedFrom is the previous slug when the row was re-keyed because the
	// entry's name changed. Empty otherwise.
	RenamedFrom string
	// MovedFrom is the previous path when the project was found by slug at a
	// different location. Empty otherwise.
	MovedFrom string
}

// SlugConflictError reports two registry entries whose names reduce to one
// slug. It is user-fixable: the fix is to rename one of them.
type SlugConflictError struct {
	Slug       string
	Name       string
	WantPath   string
	HolderPath string
}

func (e *SlugConflictError) Error() string {
	return fmt.Sprintf("%s at %s would take the key %q, which %s already holds\n"+
		"  rename one of the two entries in PROJECTS.md",
		e.Name, e.WantPath, e.Slug, e.HolderPath)
}

// UserFixable marks a slug collision as an edit the user makes.
func (e *SlugConflictError) UserFixable() bool { return true }

// Upsert writes the registry row for a project, matching an existing row by
// PATH first (DESIGN.md §6).
//
// Path is a project's identity; the name is a label on it. Matching by slug
// alone would turn `## Apex` -> `## Apex CLI` into a second row, orphaning the
// first along with its cached digest — silent, and more expensive the longer it
// goes unnoticed. So:
//
//   - a row at this path whose slug differs is RE-KEYED, carrying its digest
//     and history forward;
//   - a row with this slug at another path has MOVED, and its path is updated;
//   - otherwise the project is new.
//
// registered_at is read from the matched row and written back: it records when
// Apex first saw the project, and a re-registration is not a new one.
// last_synced_at needs no such care since M3 — store.UpsertProject now
// preserves it unless a caller explicitly sets it, which closes the footgun
// DESIGN.md §18 logged as open.
func Upsert(ctx context.Context, st *store.Store, c Candidate, status string, now time.Time) (UpsertResult, error) {
	var result UpsertResult

	row := store.Project{
		Slug:         c.Slug,
		Name:         c.Name,
		Path:         c.Path,
		Status:       status,
		RegisteredAt: now,
	}

	existing, matched, err := matchExisting(ctx, st, c)
	if err != nil {
		return result, err
	}

	switch {
	case !matched:
		result.Registered = true

	case existing.Path == c.Path && existing.Slug != c.Slug:
		// Renamed in place. The target key must be free, or two entries are
		// fighting over one slug and the user has to settle it.
		if err := requireSlugFree(ctx, st, c, existing.Slug); err != nil {
			return result, err
		}
		if err := st.RenameProjectSlug(ctx, existing.Slug, c.Slug); err != nil {
			return result, err
		}
		result.RenamedFrom = existing.Slug
		carryForward(&row, existing, status)

	case existing.Path != c.Path:
		// Same name, new location: the project moved on disk.
		result.MovedFrom = existing.Path
		carryForward(&row, existing, status)

	default:
		carryForward(&row, existing, status)
	}

	if err := st.UpsertProject(ctx, row); err != nil {
		return result, fmt.Errorf("register project %s: %w", c.Slug, err)
	}
	return result, nil
}

// matchExisting finds the row this candidate refers to: by path first, then by
// slug for a project that moved.
func matchExisting(ctx context.Context, st *store.Store, c Candidate) (store.Project, bool, error) {
	byPath, err := st.GetProjectByPath(ctx, c.Path)
	switch {
	case err == nil:
		return byPath, true, nil
	case !errors.Is(err, store.ErrNotFound):
		return store.Project{}, false, fmt.Errorf("look up project at %s: %w", c.Path, err)
	}

	bySlug, err := st.GetProject(ctx, c.Slug)
	switch {
	case err == nil:
		return bySlug, true, nil
	case !errors.Is(err, store.ErrNotFound):
		return store.Project{}, false, fmt.Errorf("look up project %s: %w", c.Slug, err)
	}
	return store.Project{}, false, nil
}

// requireSlugFree refuses a re-key that would collide with a different project.
func requireSlugFree(ctx context.Context, st *store.Store, c Candidate, from string) error {
	holder, err := st.GetProject(ctx, c.Slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("look up project %s: %w", c.Slug, err)
	case holder.Slug == from || holder.Path == c.Path:
		return nil
	}
	return &SlugConflictError{
		Slug:       c.Slug,
		Name:       c.Name,
		WantPath:   c.Path,
		HolderPath: holder.Path,
	}
}

// carryForward preserves the fields a sync must not invent.
//
// last_synced_at is absent on purpose: leaving it nil is what tells
// store.UpsertProject to keep whatever the row already had, so a sync that
// refreshed no digest cannot move the timestamp in either direction.
func carryForward(row *store.Project, existing store.Project, status string) {
	row.RegisteredAt = existing.RegisteredAt
	if status == "" {
		row.Status = existing.Status
	}
}

// Orphan is a registered project whose path no longer appears in PROJECTS.md.
//
// It is reported and never removed. Pruning destroys a digest and every action
// item keyed to the project, so it needs an explicit confirmation, and the
// surface for asking for one does not exist yet (DESIGN.md §6).
type Orphan struct {
	Slug string
	Name string
	Path string
}

// Orphans returns the registered projects no registry entry points at.
//
// registryPaths must be the resolved paths of EVERY entry in PROJECTS.md, not
// just the ones a filtered sync looked at: `apex sync <project>` must not
// report the rest of the portfolio as orphaned.
func Orphans(ctx context.Context, st *store.Store, registryPaths map[string]bool) ([]Orphan, error) {
	rows, err := st.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list registered projects: %w", err)
	}
	var out []Orphan
	for _, p := range rows {
		if registryPaths[p.Path] {
			continue
		}
		out = append(out, Orphan{Slug: p.Slug, Name: p.Name, Path: p.Path})
	}
	return out, nil
}

// Freshness says whether a cached digest still matches the project's sources.
type Freshness int

const (
	// Unknown means no digest has ever been generated for this project.
	Unknown Freshness = iota
	// Stale means a digest exists but was generated from different sources.
	Stale
	// Fresh means the cached digest was generated from exactly these sources.
	Fresh
)

func (f Freshness) String() string {
	switch f {
	case Fresh:
		return "fresh"
	case Stale:
		return "stale"
	default:
		return "none"
	}
}

// NeedsRegeneration reports whether a digest would have to be generated. It is
// true for both Unknown and Stale.
func (f Freshness) NeedsRegeneration() bool { return f != Fresh }

// CheckFreshness compares a freshly computed source hash against the cached
// digest's. It reports the cached hash too, so a caller can say what changed.
func CheckFreshness(ctx context.Context, st *store.Store, slug, sourceHash string) (Freshness, string, error) {
	d, err := st.GetDigest(ctx, slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Unknown, "", nil
	case err != nil:
		return Unknown, "", fmt.Errorf("read cached digest for %s: %w", slug, err)
	}
	if d.SourceHash == sourceHash {
		return Fresh, d.SourceHash, nil
	}
	return Stale, d.SourceHash, nil
}
