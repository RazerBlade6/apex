package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/lock"
	"github.com/RazerBlade6/apex/internal/project"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// fixture is a throwaway ~/.apex plus a throwaway home directory holding fake
// projects. Nothing outside t.TempDir is read or written.
type fixture struct {
	home string // stands in for $HOME, so `path: ~/...` resolves into it
	root string // stands in for ~/.apex
	fake *fakeCmdProvider
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{
		home: filepath.Join(base, "home"),
		root: filepath.Join(base, "home", ".apex"),
	}
	if err := os.MkdirAll(contextfs.ContextDir(f.root), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", f.home)
	t.Setenv("APEX_HOME", f.root)
	f.fake = installFakeProvider(t)
	return f
}

// installFakeProvider points the command layer at an offline provider for the
// duration of one test.
//
// Every command from M4 onwards can reach a model, so the command tests would
// otherwise need a credential and a network — or, worse, would quietly spend
// the user's subscription quota when someone ran `go test`. The seam is the
// package-level providerFactory; it is restored afterwards.
func installFakeProvider(t *testing.T) *fakeCmdProvider {
	t.Helper()
	fake := &fakeCmdProvider{Text: "A generated digest."}
	previous := providerFactory
	providerFactory = func(context.Context, config.ModelRoute) (provider.Provider, error) {
		return fake, nil
	}
	t.Cleanup(func() { providerFactory = previous })
	return fake
}

// fakeCmdProvider answers every call from memory.
type fakeCmdProvider struct {
	mu    sync.Mutex
	calls int
	// Text is returned by Stream, which is what digest generation uses.
	Text string
	// JSON is what Structured decodes, for review and ideas.
	JSON string
}

func (f *fakeCmdProvider) Name() string { return "fake" }

func (f *fakeCmdProvider) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCmdProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	ch := make(chan provider.Event, 3)
	ch <- provider.Event{Type: provider.EventTextDelta, Text: f.Text}
	ch <- provider.Event{Type: provider.EventDone, Usage: &provider.Usage{
		InputTokens: 120, OutputTokens: 60, CacheWriteTokens: 100, Model: "fake-model-5",
	}}
	close(ch)
	return ch, nil
}

func (f *fakeCmdProvider) Structured(ctx context.Context, req provider.Request, schema json.RawMessage, out any) (provider.Usage, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	// Usage is reported here too, which is the whole point of M5's interface
	// change: review and ideas carry the largest prompt Apex sends and were
	// the only calls whose cost was invisible (DESIGN.md §18).
	return provider.Usage{
		InputTokens: 900, OutputTokens: 240, CacheWriteTokens: 850, Model: "fake-model-5",
	}, json.Unmarshal([]byte(f.JSON), out)
}

// project creates a fake project directory under the fixture home.
func (f *fixture) project(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(f.home, "Development", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func (f *fixture) writeRegistry(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(contextfs.RegistryPath(f.root), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) readRegistry(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(contextfs.RegistryPath(f.root))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// run executes the CLI the way a shell would, and returns its combined output.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Apex Test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Apex Test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	for _, args := range [][]string{
		{"init", "--initial-branch=main", "--quiet"},
		{"add", "-A"},
		{"commit", "-m", "initial commit", "--quiet"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

const scholarDoc = `---
name: ScholarRAG
status: active
stack: [python, fastapi, postgres]
started: 2026-03-14
tags: [research]
---

## What it is
Retrieval system over a personal corpus of academic papers.

## Where I'm stuck
Chunking loses table context.
`

const atlasDoc = `---
name: Atlas
status: paused
stack: [go]
---

## What it is
A map of everything I have ever started and abandoned.
`

// registryFixture is deliberately full of things a careless rewriter would
// destroy: a title, a comment, prose between entries, an entry with no summary,
// a malformed entry, and a trailing note.
const registryFixture = `# Projects
<!-- Summary lines are generated by ` + "`apex sync`" + `. Edit each project's PROJECT.md instead. -->

## ScholarRAG
path: ~/Development/ScholarRAG
<!--apex-->An old summary that sync should replace.

I keep notes to myself down here.

## Atlas
path: ~/Development/Atlas

## Ghost
path: ~/Development/Ghost
This project moved and I never fixed the path.

## Broken
I forgot the path line entirely.

<!-- end of registry -->
`

func TestSyncEndToEnd(t *testing.T) {
	f := newFixture(t)

	scholar := f.project(t, "ScholarRAG", map[string]string{
		contextfs.ProjectFile: scholarDoc,
		contextfs.ReadmeFile:  "# ScholarRAG\n\nA README.\n",
		"main.py":             "print('hi')\n",
	})
	gitInit(t, scholar)

	// Atlas is a valid project that is not a git repository.
	f.project(t, "Atlas", map[string]string{contextfs.ProjectFile: atlasDoc})

	f.writeRegistry(t, registryFixture)

	out, err := run(t, "sync")
	if !errors.Is(err, errSyncProblems) {
		t.Fatalf("sync err = %v, want errSyncProblems (Ghost is missing)\n%s", err, out)
	}
	t.Logf("apex sync:\n%s", out)

	for _, want := range []string{
		"ScholarRAG", "Atlas", "Ghost", "missing", "main", "not a git repository",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Generated 2 digest(s)") {
		t.Errorf("output does not report the two digests it generated:\n%s", out)
	}
	if !strings.Contains(out, "Broken") {
		t.Errorf("output does not report the malformed entry:\n%s", out)
	}

	// --- the registry rewrite -------------------------------------------
	got := f.readRegistry(t)

	want := `# Projects
<!-- Summary lines are generated by ` + "`apex sync`" + `. Edit each project's PROJECT.md instead. -->

## ScholarRAG
path: ~/Development/ScholarRAG
<!--apex-->Retrieval system over a personal corpus of academic papers. · active · python, fastapi, postgres

I keep notes to myself down here.

## Atlas
path: ~/Development/Atlas
<!--apex-->A map of everything I have ever started and abandoned. · paused · go

## Ghost
path: ~/Development/Ghost
This project moved and I never fixed the path.

## Broken
I forgot the path line entirely.

<!-- end of registry -->
`
	if got != want {
		t.Errorf("PROJECTS.md after sync =\n%q\nwant\n%q", got, want)
	}

	// --- the registry rows ----------------------------------------------
	rows := listProjects(t)
	if len(rows) != 2 {
		t.Fatalf("registered %d projects, want 2 (Ghost is missing): %+v", len(rows), rows)
	}
	byslug := map[string]store.Project{}
	for _, p := range rows {
		byslug[p.Slug] = p
	}
	if p := byslug["scholarrag"]; p.Name != "ScholarRAG" || p.Status != "active" || p.Path != scholar {
		t.Errorf("scholarrag row = %+v", p)
	}
	if p := byslug["atlas"]; p.Status != "paused" {
		t.Errorf("atlas row = %+v", p)
	}
	if _, registered := byslug["ghost"]; registered {
		t.Error("a project whose path does not exist was registered")
	}

	// --- a second run is a no-op ----------------------------------------
	before := f.readRegistry(t)
	out2, err := run(t, "sync")
	if !errors.Is(err, errSyncProblems) {
		t.Fatalf("second sync err = %v\n%s", err, out2)
	}
	if after := f.readRegistry(t); after != before {
		t.Errorf("a second sync changed PROJECTS.md:\n%q", after)
	}
	if !strings.Contains(out2, "PROJECTS.md is unchanged") {
		t.Errorf("second run did not report the file as unchanged:\n%s", out2)
	}
	// The first run cached both digests against their source hashes, and
	// nothing has moved since, so the second run must reach no model at all.
	// This is the property that makes a warm sync free (DESIGN.md §6).
	if !strings.Contains(out2, "No digests are out of date") {
		t.Errorf("a second sync did not treat the cached digests as fresh:\n%s", out2)
	}
	if n := f.fake.Calls(); n != 2 {
		t.Errorf("%d model calls across two syncs, want 2: a fresh digest was regenerated", n)
	}
}

// TestSyncStalenessAndRegeneration is DESIGN.md §14 step 4 end to end: a
// cached digest whose source hash still matches is left alone, editing
// PROJECT.md makes it stale, and only then does a model get called.
//
// The --dry-run leg is the guard that matters for the user's wallet: it must
// report exactly what a real run would regenerate and send nothing.
func TestSyncStalenessAndRegeneration(t *testing.T) {
	f := newFixture(t)
	dir := f.project(t, "Atlas", map[string]string{contextfs.ProjectFile: atlasDoc})
	f.writeRegistry(t, "# Projects\n\n## Atlas\npath: ~/Development/Atlas\n")

	// Register the project without generating anything, then cache a digest
	// against its current sources by hand: there is nothing to do.
	if out, err := run(t, "sync", "--dry-run"); err != nil {
		t.Fatalf("sync --dry-run: %v\n%s", err, out)
	}
	hash := project.SourceHash(project.HashInputs{ProjectDoc: atlasDoc, HasProjectDoc: true})
	seedDigest(t, "atlas", hash)

	out, err := run(t, "sync")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if !strings.Contains(out, "fresh") || !strings.Contains(out, "No digests are out of date") {
		t.Errorf("a matching source hash was not reported as fresh:\n%s", out)
	}
	if n := f.fake.Calls(); n != 0 {
		t.Errorf("a fresh digest still cost %d model call(s)", n)
	}

	// Editing PROJECT.md must make it stale.
	edited := strings.Replace(atlasDoc, "status: paused", "status: active", 1)
	if err := os.WriteFile(filepath.Join(dir, contextfs.ProjectFile), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	// --dry-run reports the work and does none of it.
	out, err = run(t, "sync", "--dry-run")
	if err != nil {
		t.Fatalf("sync --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "stale") || !strings.Contains(out, "1 digest(s) would be regenerated") {
		t.Errorf("--dry-run did not name the stale digest:\n%s", out)
	}
	if n := f.fake.Calls(); n != 0 {
		t.Errorf("--dry-run made %d model call(s)", n)
	}
	if d := getDigest(t, "atlas"); d.Body != "a digest from a later milestone" {
		t.Errorf("--dry-run rewrote the cached digest: %q", d.Body)
	}

	// A real run regenerates exactly that one.
	out, err = run(t, "sync")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if n := f.fake.Calls(); n != 1 {
		t.Errorf("%d model call(s) for one stale project, want 1", n)
	}
	d := getDigest(t, "atlas")
	if d.Body != f.fake.Text {
		t.Errorf("digest body = %q, want the generated text", d.Body)
	}
	newHash := project.SourceHash(project.HashInputs{ProjectDoc: edited, HasProjectDoc: true})
	if d.SourceHash != newHash {
		t.Errorf("digest cached against %q, want the edited sources' hash %q", d.SourceHash, newHash)
	}
	if d.Model != "fake-model-5" {
		t.Errorf("digest model = %q, want the serving model the provider reported", d.Model)
	}
	// last_synced_at records when digests were refreshed (DESIGN.md §7).
	for _, p := range listProjects(t) {
		if p.Slug == "atlas" && p.LastSyncedAt == nil {
			t.Error("regenerating a digest did not stamp last_synced_at")
		}
	}
}

// TestSyncLocking covers DESIGN.md §10: sync takes a SHARED lock, so it runs
// alongside another reader but skips a project held exclusively by a dispatch.
func TestSyncLocking(t *testing.T) {
	tests := []struct {
		name     string
		mode     lock.Mode
		wantBusy bool
	}{
		{name: "another reader holds it shared", mode: lock.Shared, wantBusy: false},
		{name: "a dispatch holds it exclusively", mode: lock.Exclusive, wantBusy: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.project(t, "Atlas", map[string]string{contextfs.ProjectFile: atlasDoc})
			f.writeRegistry(t, "# Projects\n\n## Atlas\npath: ~/Development/Atlas\n<!--apex-->Old summary.\n")

			held, err := lock.AcquireAs(filepath.Join(f.root, "locks", "atlas.lock"), tt.mode, "abc123")
			if err != nil {
				t.Fatalf("AcquireAs(%s): %v", tt.mode, err)
			}
			defer held.Release()

			out, err := run(t, "sync")
			if err != nil {
				t.Fatalf("sync: %v\n%s", err, out)
			}

			registry := f.readRegistry(t)
			if tt.wantBusy {
				if !strings.Contains(out, "busy") {
					t.Errorf("output does not report the project as busy:\n%s", out)
				}
				if !strings.Contains(out, "abc123") {
					t.Errorf("output does not name the lock holder:\n%s", out)
				}
				if !strings.Contains(registry, "Old summary.") {
					t.Errorf("a busy project's summary was rewritten:\n%s", registry)
				}
				if len(listProjects(t)) != 0 {
					t.Error("a busy project was registered anyway")
				}
				return
			}
			if strings.Contains(out, "busy") {
				t.Errorf("sync took an exclusive lock: it blocked on another reader\n%s", out)
			}
			if strings.Contains(registry, "Old summary.") {
				t.Errorf("summary was not regenerated:\n%s", registry)
			}
		})
	}
}

func TestSyncSingleProject(t *testing.T) {
	f := newFixture(t)
	f.project(t, "Atlas", map[string]string{contextfs.ProjectFile: atlasDoc})
	f.project(t, "ScholarRAG", map[string]string{contextfs.ProjectFile: scholarDoc})
	f.writeRegistry(t, "# Projects\n\n## Atlas\npath: ~/Development/Atlas\n<!--apex-->Old A.\n\n## ScholarRAG\npath: ~/Development/ScholarRAG\n<!--apex-->Old S.\n")

	out, err := run(t, "sync", "atlas") // matched by slug, not by exact name
	if err != nil {
		t.Fatalf("sync atlas: %v\n%s", err, out)
	}
	if strings.Contains(out, "ScholarRAG") {
		t.Errorf("sync <project> touched another project:\n%s", out)
	}

	registry := f.readRegistry(t)
	if strings.Contains(registry, "Old A.") {
		t.Errorf("the named project was not regenerated:\n%s", registry)
	}
	if !strings.Contains(registry, "Old S.") {
		t.Errorf("sync <project> rewrote another project's summary:\n%s", registry)
	}
	if rows := listProjects(t); len(rows) != 1 || rows[0].Slug != "atlas" {
		t.Errorf("registered %+v, want only atlas", rows)
	}
}

func TestSyncUserFixableFailures(t *testing.T) {
	t.Run("no registry", func(t *testing.T) {
		newFixture(t)
		out, err := run(t, "sync")
		if err == nil {
			t.Fatalf("sync succeeded with no PROJECTS.md\n%s", out)
		}
		if !strings.Contains(err.Error(), contextfs.ProjectsFile) {
			t.Errorf("err = %q, want it to name the missing file", err)
		}
		assertUserFixable(t, err)
	})

	t.Run("unknown project", func(t *testing.T) {
		f := newFixture(t)
		f.writeRegistry(t, "# Projects\n\n## Atlas\npath: ~/Development/Atlas\n")
		_, err := run(t, "sync", "nope")
		if err == nil {
			t.Fatal("sync succeeded for a project that is not registered")
		}
		if !strings.Contains(err.Error(), "nope") {
			t.Errorf("err = %q, want it to name the project", err)
		}
		assertUserFixable(t, err)
	})

	t.Run("malformed PROJECT.md is reported and skipped", func(t *testing.T) {
		f := newFixture(t)
		f.project(t, "Broken", map[string]string{contextfs.ProjectFile: "---\nname: [unclosed\n---\nx\n"})
		f.project(t, "Atlas", map[string]string{contextfs.ProjectFile: atlasDoc})
		f.writeRegistry(t, "# Projects\n\n## Broken\npath: ~/Development/Broken\n\n## Atlas\npath: ~/Development/Atlas\n")

		out, err := run(t, "sync")
		if !errors.Is(err, errSyncProblems) {
			t.Fatalf("err = %v, want errSyncProblems\n%s", err, out)
		}
		if !strings.Contains(out, "ERROR") || !strings.Contains(out, "invalid YAML") {
			t.Errorf("output does not report the malformed document:\n%s", out)
		}
		// One bad project must not stop the rest of the run.
		if !strings.Contains(f.readRegistry(t), "A map of everything") {
			t.Errorf("the good project was not synced:\n%s", f.readRegistry(t))
		}
	})
}

// TestSyncLeavesRegistryAloneWithoutProjectDoc: with nothing to generate a
// summary from, the user's line must survive.
func TestSyncLeavesRegistryAloneWithoutProjectDoc(t *testing.T) {
	f := newFixture(t)
	f.project(t, "Plain", map[string]string{"notes.txt": "hello\n"})
	body := "# Projects\n\n## Plain\npath: ~/Development/Plain\nSomething I wrote by hand.\n"
	f.writeRegistry(t, body)

	out, err := run(t, "sync")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if got := f.readRegistry(t); got != body {
		t.Errorf("PROJECTS.md changed for a project with no PROJECT.md:\n%q", got)
	}
	if !strings.Contains(out, "no "+contextfs.ProjectFile) {
		t.Errorf("output does not say the project has no %s:\n%s", contextfs.ProjectFile, out)
	}
	if rows := listProjects(t); len(rows) != 1 {
		t.Errorf("a project without a PROJECT.md was not registered: %+v", rows)
	}
}

// TestSyncRenameCarriesDigestForward: a project's identity is its path, so
// renaming its heading in PROJECTS.md must re-key the row rather than abandon
// it and its cached digest (DESIGN.md §6).
func TestSyncRenameCarriesDigestForward(t *testing.T) {
	f := newFixture(t)
	f.project(t, "Atlas", map[string]string{contextfs.ProjectFile: atlasDoc})
	f.writeRegistry(t, "# Projects\n\n## Atlas\npath: ~/Development/Atlas\n")

	if out, err := run(t, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	hash := project.SourceHash(project.HashInputs{ProjectDoc: atlasDoc, HasProjectDoc: true})
	seedDigest(t, "atlas", hash)

	// Same path, new name.
	f.writeRegistry(t, "# Projects\n\n## Atlas Reborn\npath: ~/Development/Atlas\n")

	out, err := run(t, "sync")
	if err != nil {
		t.Fatalf("sync after rename: %v\n%s", err, out)
	}
	if !strings.Contains(out, "re-keyed from atlas") {
		t.Errorf("output does not report the re-key:\n%s", out)
	}
	if !strings.Contains(out, "fresh") {
		t.Errorf("the digest did not survive the rename:\n%s", out)
	}
	if strings.Contains(out, "orphan") {
		t.Errorf("a rename was reported as an orphan:\n%s", out)
	}

	rows := listProjects(t)
	if len(rows) != 1 {
		t.Fatalf("a rename created a second row: %+v", rows)
	}
	if rows[0].Slug != "atlas-reborn" || rows[0].Name != "Atlas Reborn" {
		t.Errorf("row = %+v, want the re-keyed project", rows[0])
	}
	d := getDigest(t, "atlas-reborn")
	if d.SourceHash != hash {
		t.Errorf("digest = %+v, want the one cached before the rename", d)
	}
}

// TestSyncReportsOrphansWithoutPruning: removing an entry reports the row, and
// never deletes it — pruning destroys a digest and every action item keyed to
// the project, so it needs a confirmation this build cannot ask for.
func TestSyncReportsOrphansWithoutPruning(t *testing.T) {
	f := newFixture(t)
	f.project(t, "Atlas", map[string]string{contextfs.ProjectFile: atlasDoc})
	f.project(t, "ScholarRAG", map[string]string{contextfs.ProjectFile: scholarDoc})
	both := "# Projects\n\n## Atlas\npath: ~/Development/Atlas\n\n## ScholarRAG\npath: ~/Development/ScholarRAG\n"
	f.writeRegistry(t, both)

	if out, err := run(t, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if rows := listProjects(t); len(rows) != 2 {
		t.Fatalf("registered %+v, want two", rows)
	}

	// The user deletes the Atlas entry.
	f.writeRegistry(t, "# Projects\n\n## ScholarRAG\npath: ~/Development/ScholarRAG\n")

	out, err := run(t, "sync")
	if err != nil {
		t.Fatalf("sync after removing an entry: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no longer in PROJECTS.md") {
		t.Errorf("output does not report the orphan:\n%s", out)
	}
	if !strings.Contains(out, "NOT removed") {
		t.Errorf("output does not say the row was kept:\n%s", out)
	}
	if !strings.Contains(out, "Atlas") {
		t.Errorf("output does not name the orphan:\n%s", out)
	}
	if rows := listProjects(t); len(rows) != 2 {
		t.Errorf("sync pruned an orphan: %+v", rows)
	}

	// Restoring the entry clears the report, and does not re-register.
	f.writeRegistry(t, both)
	out, err = run(t, "sync")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	if strings.Contains(out, "no longer in PROJECTS.md") {
		t.Errorf("a restored entry is still reported as orphaned:\n%s", out)
	}
	if strings.Contains(out, "newly registered") {
		t.Errorf("a restored entry was treated as new:\n%s", out)
	}
}

// TestSyncSingleProjectDoesNotOrphanTheRest: orphan detection asks what the
// WHOLE registry points at, so a filtered run must not report every other
// project as orphaned.
func TestSyncSingleProjectDoesNotOrphanTheRest(t *testing.T) {
	f := newFixture(t)
	f.project(t, "Atlas", map[string]string{contextfs.ProjectFile: atlasDoc})
	f.project(t, "ScholarRAG", map[string]string{contextfs.ProjectFile: scholarDoc})
	f.writeRegistry(t, "# Projects\n\n## Atlas\npath: ~/Development/Atlas\n\n## ScholarRAG\npath: ~/Development/ScholarRAG\n")

	if out, err := run(t, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}

	out, err := run(t, "sync", "atlas")
	if err != nil {
		t.Fatalf("sync atlas: %v\n%s", err, out)
	}
	if strings.Contains(out, "no longer in PROJECTS.md") {
		t.Errorf("a filtered sync reported the rest of the portfolio as orphaned:\n%s", out)
	}
}

// TestSyncNeverAdoptsAnUnmarkedLine: only a marked line is Apex's. A line the
// user wrote is left where it is, and the generated summary is inserted.
func TestSyncNeverAdoptsAnUnmarkedLine(t *testing.T) {
	f := newFixture(t)
	f.project(t, "Atlas", map[string]string{contextfs.ProjectFile: atlasDoc})
	f.writeRegistry(t, "# Projects\n\n## Atlas\npath: ~/Development/Atlas\nA line I wrote myself.\n")

	if out, err := run(t, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	got := f.readRegistry(t)
	want := "# Projects\n\n## Atlas\npath: ~/Development/Atlas\n" +
		"<!--apex-->A map of everything I have ever started and abandoned. · paused · go\n\n" +
		"A line I wrote myself.\n"
	if got != want {
		t.Errorf("PROJECTS.md =\n%q\nwant\n%q", got, want)
	}

	// The second sync rewrites the marked line in place and adds no second one.
	if out, err := run(t, "sync"); err != nil {
		t.Fatalf("second sync: %v\n%s", err, out)
	}
	if after := f.readRegistry(t); after != want {
		t.Errorf("a second sync changed the file:\n%q", after)
	}
	if n := strings.Count(want, contextfs.SummaryMarker); n != 1 {
		t.Errorf("%d markers, want 1", n)
	}
}

// --- store helpers ----------------------------------------------------------

// openFixtureStoreWithoutMigrating opens the fixture database and leaves the
// schema exactly as it found it, which is what a test asserting that some
// other command did not migrate needs.
func openFixtureStoreWithoutMigrating(t *testing.T) (*store.Store, func()) {
	t.Helper()
	path, err := config.DatabasePath()
	if err != nil {
		t.Fatal(err)
	}
	backend := store.NewLocalBackend(path)
	st, err := store.Open(context.Background(), backend)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st, func() { _ = backend.Close() }
}

func openFixtureStore(t *testing.T) (*store.Store, func()) {
	t.Helper()
	path, err := config.DatabasePath()
	if err != nil {
		t.Fatal(err)
	}
	backend := store.NewLocalBackend(path)
	st, err := store.Open(context.Background(), backend)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st, func() { _ = backend.Close() }
}

func listProjects(t *testing.T) []store.Project {
	t.Helper()
	st, closeStore := openFixtureStore(t)
	defer closeStore()
	rows, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	return rows
}

func seedDigest(t *testing.T, slug, hash string) {
	t.Helper()
	st, closeStore := openFixtureStore(t)
	defer closeStore()
	if err := st.UpsertDigest(context.Background(), store.Digest{
		ProjectSlug: slug,
		Body:        "a digest from a later milestone",
		SourceHash:  hash,
		GeneratedAt: time.Now(),
		Model:       "claude-opus-5",
	}); err != nil {
		t.Fatalf("UpsertDigest: %v", err)
	}
}

func getDigest(t *testing.T, slug string) store.Digest {
	t.Helper()
	st, closeStore := openFixtureStore(t)
	defer closeStore()
	d, err := st.GetDigest(context.Background(), slug)
	if err != nil {
		t.Fatalf("GetDigest: %v", err)
	}
	return d
}

func assertUserFixable(t *testing.T, err error) {
	t.Helper()
	var uf interface{ UserFixable() bool }
	if !errors.As(err, &uf) || !uf.UserFixable() {
		t.Errorf("err %T (%v) is not reported as user-fixable", err, err)
	}
}

// TestChatSyncerWritesTheReport: the TUI's `/sync` runs the real sync on the
// session the TUI already holds, and writes the report `apex sync` prints.
func TestChatSyncerWritesTheReport(t *testing.T) {
	f := newFixture(t)
	seedReviewableProject(t, f)
	sess, err := openSession(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	var out bytes.Buffer
	if err := syncerFor(sess)(context.Background(), &out); err != nil {
		t.Fatalf("syncer: %v\n%s", err, out.String())
	}
	for _, want := range []string{"registry:", "Atlas"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not mention %q:\n%s", want, out.String())
		}
	}
}
