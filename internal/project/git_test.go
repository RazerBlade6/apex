package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireGit skips a test when git is not installed. Every other test in this
// package must still pass without it.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// A test must not depend on, or write to, the user's git configuration.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Apex Test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=Apex Test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"LC_ALL=C",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	requireGit(t)
	git(t, dir, "init", "--initial-branch=main", "--quiet")
}

func commit(t *testing.T, dir, message string) {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", message, "--quiet")
}

func headOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

func TestInspectGit(t *testing.T) {
	ctx := context.Background()

	t.Run("not a repository", func(t *testing.T) {
		// A directory that is not a repository is a valid project, not an
		// error: it degrades and reports.
		dir := t.TempDir()
		g, err := InspectGit(ctx, dir)
		if err != nil {
			t.Fatalf("InspectGit: %v", err)
		}
		if g.IsRepo {
			t.Errorf("IsRepo is true for %s", dir)
		}
		if g.Head != "" || g.Dirty || len(g.Log) != 0 || len(g.Status) != 0 {
			t.Errorf("a non-repository reported state: %+v", g)
		}
		if g.Note == "" {
			t.Error("Note is empty; the report has nothing to say why")
		}
		if d := g.Describe(); d == "" {
			t.Error("Describe is empty")
		}
	})

	t.Run("repository with no commits", func(t *testing.T) {
		dir := t.TempDir()
		initRepo(t, dir)
		g, err := InspectGit(ctx, dir)
		if err != nil {
			t.Fatalf("InspectGit: %v", err)
		}
		if !g.IsRepo {
			t.Fatal("IsRepo is false for a freshly initialised repository")
		}
		if g.Head != "" {
			t.Errorf("Head = %q, want empty for an unborn HEAD", g.Head)
		}
		if len(g.Log) != 0 {
			t.Errorf("Log = %v, want empty", g.Log)
		}
		if !g.LastCommit.IsZero() {
			t.Errorf("LastCommit = %v, want zero with no commits", g.LastCommit)
		}
		if !strings.Contains(g.Describe(), "no commits") {
			t.Errorf("Describe = %q, want it to say there are no commits", g.Describe())
		}
	})

	t.Run("clean repository", func(t *testing.T) {
		dir := t.TempDir()
		initRepo(t, dir)
		writeFile(t, filepath.Join(dir, "a.txt"), "one\n")
		commit(t, dir, "first")
		writeFile(t, filepath.Join(dir, "b.txt"), "two\n")
		commit(t, dir, "second")

		g, err := InspectGit(ctx, dir)
		if err != nil {
			t.Fatalf("InspectGit: %v", err)
		}
		if g.Head != headOf(t, dir) {
			t.Errorf("Head = %q, want %q", g.Head, headOf(t, dir))
		}
		if g.ShortHead() != g.Head[:7] {
			t.Errorf("ShortHead = %q", g.ShortHead())
		}
		if g.Branch != "main" {
			t.Errorf("Branch = %q, want main", g.Branch)
		}
		if g.Dirty || len(g.Status) != 0 {
			t.Errorf("a committed tree is dirty: %+v", g.Status)
		}
		if len(g.Log) != 2 {
			t.Errorf("Log = %v, want two commits", g.Log)
		}
		if !strings.Contains(g.Log[0], "second") {
			t.Errorf("Log[0] = %q, want the newest commit first", g.Log[0])
		}
		if age := time.Since(g.LastCommit); g.LastCommit.IsZero() || age < -time.Minute || age > 5*time.Minute {
			t.Errorf("LastCommit = %v, want the commit just made", g.LastCommit)
		}
		if g.Note != "" {
			t.Errorf("Note = %q, want empty for a clean read", g.Note)
		}
		if d := g.Describe(); !strings.Contains(d, "main") || !strings.Contains(d, "clean") {
			t.Errorf("Describe = %q", d)
		}
	})

	t.Run("dirty repository", func(t *testing.T) {
		dir := t.TempDir()
		initRepo(t, dir)
		writeFile(t, filepath.Join(dir, "a.txt"), "one\n")
		commit(t, dir, "first")
		writeFile(t, filepath.Join(dir, "a.txt"), "changed\n")
		writeFile(t, filepath.Join(dir, "untracked.txt"), "new\n")

		g, err := InspectGit(ctx, dir)
		if err != nil {
			t.Fatalf("InspectGit: %v", err)
		}
		if !g.Dirty {
			t.Fatalf("Dirty is false with a modified and an untracked file: %+v", g)
		}
		if len(g.Status) != 2 {
			t.Errorf("Status = %v, want two entries", g.Status)
		}
		if d := g.Describe(); !strings.Contains(d, "dirty") {
			t.Errorf("Describe = %q, want it to say dirty", d)
		}
	})

	t.Run("subdirectory of a repository", func(t *testing.T) {
		dir := t.TempDir()
		initRepo(t, dir)
		writeFile(t, filepath.Join(dir, "a.txt"), "one\n")
		commit(t, dir, "first")

		sub := filepath.Join(dir, "packages", "thing")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		g, err := InspectGit(ctx, sub)
		if err != nil {
			t.Fatalf("InspectGit: %v", err)
		}
		if !g.IsRepo {
			t.Fatal("IsRepo is false inside a monorepo")
		}
		// Root points above the project, which is the useful fact to report.
		gotRoot, err := filepath.EvalSymlinks(g.Root)
		if err != nil {
			t.Fatal(err)
		}
		wantRoot, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if gotRoot != wantRoot {
			t.Errorf("Root = %q, want %q", gotRoot, wantRoot)
		}
	})

	t.Run("detached head", func(t *testing.T) {
		dir := t.TempDir()
		initRepo(t, dir)
		writeFile(t, filepath.Join(dir, "a.txt"), "one\n")
		commit(t, dir, "first")
		git(t, dir, "checkout", "--detach", "--quiet", "HEAD")

		g, err := InspectGit(ctx, dir)
		if err != nil {
			t.Fatalf("InspectGit: %v", err)
		}
		if g.Branch != "HEAD" {
			t.Errorf("Branch = %q, want HEAD when detached", g.Branch)
		}
		if !strings.Contains(g.Describe(), "detached") {
			t.Errorf("Describe = %q, want it to say detached", g.Describe())
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		dir := t.TempDir()
		initRepo(t, dir)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := InspectGit(cancelled, dir); err == nil {
			t.Error("InspectGit ignored a cancelled context")
		}
	})
}

// TestInspectGitDoesNotWriteToTheWorkingTree backs the claim in the shared-lock
// design: reading a project must not modify it, or two concurrent syncs would
// be racing on the index.
func TestInspectGitDoesNotWriteToTheWorkingTree(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	initRepo(t, dir)
	writeFile(t, filepath.Join(dir, "a.txt"), "one\n")
	commit(t, dir, "first")

	index := filepath.Join(dir, ".git", "index")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	// Make the index look stale so a plain `git status` would want to refresh
	// it; --no-optional-locks must stop that.
	if err := os.Chtimes(index, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	if _, err := InspectGit(ctx, dir); err != nil {
		t.Fatalf("InspectGit: %v", err)
	}

	after, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("git index was rewritten: %v -> %v", before.ModTime(), after.ModTime())
	}
}
