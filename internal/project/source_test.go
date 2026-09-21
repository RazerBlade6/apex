package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RazerBlade6/apex/internal/contextfs"
)

const projectDocFixture = `---
name: ScholarRAG
status: active
stack: [python, fastapi, postgres]
started: 2026-03-14
---

## What it is
Retrieval system over a personal corpus of academic papers.

## Where I'm stuck
Chunking loses table context.
`

// TestSourceHashIsDeterministic pins the exact bytes fed to SHA-256. M4's cache
// is only correct if two runs, on two machines, of the same project state
// produce the same key.
func TestSourceHashIsDeterministic(t *testing.T) {
	readme := "# ScholarRAG\n\nIngest, embed, query.\n"
	status := " M main.py\n?? scratch.txt\n"

	cases := []struct {
		name string
		in   HashInputs
	}{
		{name: "nothing at all"},
		{name: "head only", in: HashInputs{Head: "abc123"}},
		{name: "empty project doc present", in: HashInputs{HasProjectDoc: true}},
		{name: "empty readme present", in: HashInputs{HasReadme: true}},
		{
			name: "doc and readme",
			in:   HashInputs{ProjectDoc: projectDocFixture, HasProjectDoc: true, Readme: readme, HasReadme: true, Head: "abc123"},
		},
		{
			name: "doc, readme and a dirty tree",
			in:   HashInputs{ProjectDoc: projectDocFixture, HasProjectDoc: true, Readme: readme, HasReadme: true, Head: "abc123", GitStatus: status},
		},
	}

	seen := map[string]string{}
	for _, c := range cases {
		h := SourceHash(c.in)
		if len(h) != 64 {
			t.Errorf("%s: hash %q is not a hex SHA-256", c.name, h)
		}
		for i := 0; i < 8; i++ {
			if again := SourceHash(c.in); again != h {
				t.Fatalf("%s: SourceHash is not deterministic: %q then %q", c.name, h, again)
			}
		}
		if prev, dup := seen[h]; dup {
			t.Errorf("%s collides with %s: both %s", c.name, prev, h)
		}
		seen[h] = c.name
	}

	base := HashInputs{
		ProjectDoc: projectDocFixture, HasProjectDoc: true,
		Readme: readme, HasReadme: true,
		Head: "abc123", GitStatus: status,
	}
	baseHash := SourceHash(base)

	// Every one of the four documented inputs moves the hash.
	moved := []struct {
		name string
		in   HashInputs
	}{
		{"PROJECT.md edited", withFn(base, func(h *HashInputs) { h.ProjectDoc += "\n" })},
		{"README.md edited", withFn(base, func(h *HashInputs) { h.Readme += "\n" })},
		{"HEAD advanced", withFn(base, func(h *HashInputs) { h.Head = "def456" })},
		{"status changed", withFn(base, func(h *HashInputs) { h.GitStatus += "?? more.txt\n" })},
	}
	for _, m := range moved {
		if SourceHash(m.in) == baseHash {
			t.Errorf("%s did not change the hash", m.name)
		}
	}

	// Absent is not the same as empty, for either file.
	if SourceHash(HashInputs{}) == SourceHash(HashInputs{HasProjectDoc: true}) {
		t.Error("an absent and an empty PROJECT.md hash alike")
	}
	if SourceHash(HashInputs{}) == SourceHash(HashInputs{HasReadme: true}) {
		t.Error("an absent and an empty README.md hash alike")
	}

	// CRLF is normalised everywhere, so a checkout that converted line
	// endings does not invalidate every cached digest.
	crlf := base
	crlf.ProjectDoc = strings.ReplaceAll(base.ProjectDoc, "\n", "\r\n")
	crlf.Readme = strings.ReplaceAll(base.Readme, "\n", "\r\n")
	crlf.GitStatus = strings.ReplaceAll(base.GitStatus, "\n", "\r\n")
	if SourceHash(crlf) != baseHash {
		t.Error("CRLF and LF forms of the same state hash differently")
	}

	// Length prefixing: no body can imitate the fields that follow it.
	forged := SourceHash(HashInputs{
		ProjectDoc: "x\nreadme.md:absent\nhead:deadbeef\n", HasProjectDoc: true,
	})
	real := SourceHash(HashInputs{ProjectDoc: "x", HasProjectDoc: true, Head: "deadbeef"})
	if forged == real {
		t.Error("a PROJECT.md body forged the fields that follow it")
	}
	// ...and neither can a README body.
	if SourceHash(HashInputs{Readme: "y\nhead:deadbeef\n", HasReadme: true}) ==
		SourceHash(HashInputs{Readme: "y", HasReadme: true, Head: "deadbeef"}) {
		t.Error("a README body forged the fields that follow it")
	}

	// A clean tree and a directory that is not a repository both mean
	// "nothing uncommitted", and both hash to the digest of the empty string.
	if StatusDigest("") != StatusDigest("") {
		t.Error("StatusDigest is not deterministic")
	}
	if StatusDigest("") == StatusDigest(" M a.txt\n") {
		t.Error("a clean and a dirty status digest alike")
	}

	if SourceHashVersion != "apex-source-hash-v2" {
		t.Errorf("SourceHashVersion = %q, want apex-source-hash-v2", SourceHashVersion)
	}
}

func withFn(in HashInputs, mutate func(*HashInputs)) HashInputs {
	mutate(&in)
	return in
}

// TestSourceHashTracksAnAlreadyDirtyTree is the case the old boolean dirty flag
// got wrong, and the reason the scheme moved to v2. Once a tree goes dirty a
// boolean stops moving, so every later edit is invisible and the cached digest
// freezes for exactly the projects under active work.
func TestSourceHashTracksAnAlreadyDirtyTree(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, contextfs.ProjectFile), projectDocFixture)
	writeFile(t, filepath.Join(dir, "a.txt"), "one\n")
	initRepo(t, dir)
	commit(t, dir, "initial")

	c := Candidate{Slug: "d", Name: "D", Path: dir}
	hashNow := func(t *testing.T) (string, GitState) {
		t.Helper()
		src, err := AssembleSource(ctx, c)
		if err != nil {
			t.Fatalf("AssembleSource: %v", err)
		}
		return src.SourceHash, src.Git
	}

	clean, git := hashNow(t)
	if git.Dirty {
		t.Fatalf("a freshly committed tree is dirty: %v", git.Status)
	}

	// First uncommitted change. A boolean would move here, and only here.
	writeFile(t, filepath.Join(dir, "a.txt"), "two\n")
	dirtyOnce, git := hashNow(t)
	if !git.Dirty {
		t.Fatal("a modified tracked file did not make the tree dirty")
	}
	if dirtyOnce == clean {
		t.Error("going dirty did not change the hash")
	}

	// Second change to an ALREADY dirty tree. The old scheme missed this.
	writeFile(t, filepath.Join(dir, "b.txt"), "new file\n")
	dirtyTwice, git := hashNow(t)
	if !git.Dirty {
		t.Fatal("tree stopped being dirty")
	}
	if dirtyTwice == dirtyOnce {
		t.Error("a second edit to an already-dirty tree did not change the hash; " +
			"this is exactly what the boolean dirty flag got wrong")
	}

	// A third, to a file already counted in the status, must move it too.
	writeFile(t, filepath.Join(dir, "b.txt"), "different content\n")
	if third, _ := hashNow(t); third == dirtyTwice {
		// git status --short reports "?? b.txt" either way, so this one is
		// genuinely invisible to a status digest. Recorded, not asserted.
		t.Log("note: editing an already-untracked file leaves `git status --short` " +
			"unchanged, so the hash does not move; committed content is covered by HEAD")
	}

	// Committing must move the hash again, and back to clean.
	commit(t, dir, "second")
	committed, git := hashNow(t)
	if git.Dirty {
		t.Fatalf("tree is still dirty after a commit: %v", git.Status)
	}
	for name, other := range map[string]string{
		"clean":       clean,
		"dirty once":  dirtyOnce,
		"dirty twice": dirtyTwice,
	} {
		if committed == other {
			t.Errorf("the committed state hashes the same as %q", name)
		}
	}
}

// TestSourceHashTracksReadmeWithoutGit covers the other half of the v2 change:
// in a project that is not a repository, nothing but the files moves, so a
// README edit has to be in the hash or it is invisible forever.
func TestSourceHashTracksReadmeWithoutGit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, contextfs.ProjectFile), projectDocFixture)
	writeFile(t, filepath.Join(dir, contextfs.ReadmeFile), "# D\n\nFirst.\n")

	c := Candidate{Slug: "d", Name: "D", Path: dir}
	first, err := AssembleSource(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if first.Git.IsRepo {
		t.Fatal("fixture is unexpectedly a git repository")
	}

	writeFile(t, filepath.Join(dir, contextfs.ReadmeFile), "# D\n\nSecond.\n")
	second, err := AssembleSource(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if second.SourceHash == first.SourceHash {
		t.Error("a README edit in a non-git project did not change the hash")
	}

	// And removing it is not the same as emptying it.
	if err := os.Remove(filepath.Join(dir, contextfs.ReadmeFile)); err != nil {
		t.Fatal(err)
	}
	third, err := AssembleSource(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if third.SourceHash == second.SourceHash {
		t.Error("removing the README did not change the hash")
	}
}

func TestAssembleSource(t *testing.T) {
	ctx := context.Background()

	t.Run("full project", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, contextfs.ProjectFile), projectDocFixture)
		writeFile(t, filepath.Join(dir, contextfs.ReadmeFile), "# ScholarRAG\n\nA README.\n")
		initRepo(t, dir)
		commit(t, dir, "initial commit")

		c := Candidate{Slug: "scholarrag", Name: "ScholarRAG", Path: dir}
		src, err := AssembleSource(ctx, c)
		if err != nil {
			t.Fatalf("AssembleSource: %v", err)
		}
		if !src.HasProjectDoc() {
			t.Error("HasProjectDoc is false with a PROJECT.md present")
		}
		if src.Doc.Name() != "ScholarRAG" || src.Doc.Status() != "active" {
			t.Errorf("frontmatter = %+v", src.Doc.Front)
		}
		if !src.Readme.Present || !strings.Contains(src.Readme.Body, "A README") {
			t.Errorf("readme = %+v", src.Readme)
		}
		if !src.Git.IsRepo || src.Git.Head == "" {
			t.Errorf("git = %+v", src.Git)
		}
		if len(src.Git.Log) != 1 {
			t.Errorf("log = %v, want one commit", src.Git.Log)
		}
		if src.Git.Dirty {
			t.Errorf("a freshly committed tree is not dirty: %v", src.Git.Status)
		}
		want := SourceHash(HashInputs{
			ProjectDoc: projectDocFixture, HasProjectDoc: true,
			Readme: "# ScholarRAG\n\nA README.\n", HasReadme: true,
			Head: src.Git.Head, GitStatus: src.Git.StatusRaw,
		})
		if src.SourceHash != want {
			t.Errorf("SourceHash = %q, want %q", src.SourceHash, want)
		}
	})

	t.Run("no PROJECT.md, no README, no repository", func(t *testing.T) {
		dir := t.TempDir()
		c := Candidate{Slug: "bare", Name: "Bare", Path: dir}
		src, err := AssembleSource(ctx, c)
		if err != nil {
			t.Fatalf("AssembleSource: %v", err)
		}
		if src.HasProjectDoc() || src.Readme.Present || src.Git.IsRepo {
			t.Errorf("a bare directory reported content: %+v", src)
		}
		if want := SourceHash(HashInputs{}); src.SourceHash != want {
			t.Errorf("SourceHash = %q, want %q", src.SourceHash, want)
		}
	})

	t.Run("malformed PROJECT.md", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, contextfs.ProjectFile), "---\nname: [unclosed\n---\nBody.\n")
		c := Candidate{Slug: "broken", Name: "Broken", Path: dir}
		_, err := AssembleSource(ctx, c)
		if err == nil {
			t.Fatal("AssembleSource accepted a malformed PROJECT.md")
		}
		var malformed *contextfs.MalformedError
		if !errors.As(err, &malformed) {
			t.Fatalf("err = %T (%v), want *contextfs.MalformedError", err, err)
		}
		if !strings.Contains(err.Error(), "Broken") {
			t.Errorf("err = %q, want it to name the project", err)
		}
	})

}

func TestSummarize(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "lead plus status and stack",
			body: projectDocFixture,
			want: "Retrieval system over a personal corpus of academic papers. · active · python, fastapi, postgres",
		},
		{
			name: "lead only",
			body: "# Apex\n\nA personal hub.\n",
			want: "A personal hub.",
		},
		{
			name: "frontmatter only",
			body: "---\nstatus: paused\nstack: [go]\n---\n",
			want: "paused · go",
		},
		{
			name: "nothing to say",
			body: "---\nname: Apex\n---\n\n## Goals\n\n- a bullet\n",
			want: "",
		},
		{
			name: "long lead is truncated on a word boundary",
			body: "---\nstatus: active\n---\n\n" + strings.Repeat("word ", 60) + "\n",
			want: strings.TrimSpace(strings.Repeat("word ", 32)) + "… · active",
		},
		{
			name: "blank stack entries are dropped",
			body: "---\nstack: [go, \"\", sqlite]\n---\nBody.\n",
			want: "Body. · go, sqlite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := contextfs.ParseProjectDoc([]byte(tt.body), "PROJECT.md")
			if err != nil {
				t.Fatalf("ParseProjectDoc: %v", err)
			}
			if got := Summarize(doc); got != tt.want {
				t.Errorf("Summarize =\n %q\nwant\n %q", got, tt.want)
			}
		})
	}

	// An absent document generates nothing, so sync leaves whatever the user
	// wrote in PROJECTS.md alone.
	if got := Summarize(&contextfs.ProjectDoc{}); got != "" {
		t.Errorf("Summarize of an absent document = %q, want \"\"", got)
	}
	if got := Summarize(nil); got != "" {
		t.Errorf("Summarize(nil) = %q, want \"\"", got)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
