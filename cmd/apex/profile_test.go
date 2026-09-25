package main

import (
	"os"
	"strings"
	"testing"

	"github.com/RazerBlade6/apex/internal/contextfs"
)

// `apex profile` is the user's half of DESIGN.md §6: they can see everything
// Apex believes about them, where it got it, and remove any of it.
//
// Every fixture here runs under APEX_HOME in a temp directory. The real
// ~/.apex/context files are hand-written and must never be touched by a test.

func TestProfileListsObservationsWithDatesAndSources(t *testing.T) {
	f := newFixture(t)
	writeIdentityFixture(t, f, contextfs.ProfileFile,
		"# Me\n\nI build small tools.\n\n## Observed\n"+
			"<!--apex 2026-09-01--> Prefers a single static binary. — source: said while choosing a stack\n")
	writeIdentityFixture(t, f, contextfs.SkillsFile,
		"# Skills\n\n## Observed\n"+
			"<!--apex 2026-09-02--> Is learning Zig. — source: mentioned starting a toy compiler\n")

	out, err := run(t, "profile")
	if err != nil {
		t.Fatalf("profile: %v\n%s", err, out)
	}
	for _, want := range []string{
		"PROFILE.md", "SKILLS.md",
		" 1. [profile] Prefers a single static binary.",
		"source: said while choosing a stack",
		" 2. [skills] Is learning Zig.",
		"apex profile --forget",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("profile output does not contain %q:\n%s", want, out)
		}
	}
}

func TestProfileForgetRemovesOneAndKeepsThePros(t *testing.T) {
	f := newFixture(t)
	const prose = "# Me\n\nI care about a clean diff more than a clever one.\n"
	writeIdentityFixture(t, f, contextfs.ProfileFile, prose+"\n## Observed\n"+
		"<!--apex 2026-09-01--> First. — source: a\n"+
		"<!--apex 2026-09-02--> Second. — source: b\n")

	out, err := run(t, "profile", "--forget", "1")
	if err != nil {
		t.Fatalf("profile --forget: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Forgot 1: First.") {
		t.Errorf("output does not say what was forgotten:\n%s", out)
	}

	body := readIdentityFixture(t, f, contextfs.ProfileFile)
	if strings.Contains(body, "First.") {
		t.Errorf("the observation survived:\n%s", body)
	}
	if !strings.Contains(body, "Second.") {
		t.Errorf("forgetting one removed the other:\n%s", body)
	}
	if !strings.HasPrefix(body, prose) {
		t.Errorf("the user's own prose was disturbed:\n%s", body)
	}
}

func TestProfileForgetRejectsANumberThatNamesNothing(t *testing.T) {
	newFixture(t)
	out, err := run(t, "profile", "--forget", "3")
	if err == nil {
		t.Fatalf("expected a failure, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no observation 3") {
		t.Errorf("error = %v, want it to name the number", err)
	}
}

func TestProfileSaysWhenNothingHasBeenLearned(t *testing.T) {
	newFixture(t)
	out, err := run(t, "profile")
	if err != nil {
		t.Fatalf("profile: %v\n%s", err, out)
	}
	if !strings.Contains(out, "has not learned anything about you yet") {
		t.Errorf("output does not explain the empty state:\n%s", out)
	}
	if !strings.Contains(out, "absent") {
		t.Errorf("output does not report the identity files as absent:\n%s", out)
	}
}

func writeIdentityFixture(t *testing.T, f *fixture, name, body string) {
	t.Helper()
	path := contextfs.ProfilePath(f.root)
	if name == contextfs.SkillsFile {
		path = contextfs.SkillsPath(f.root)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readIdentityFixture(t *testing.T, f *fixture, name string) string {
	t.Helper()
	path := contextfs.ProfilePath(f.root)
	if name == contextfs.SkillsFile {
		path = contextfs.SkillsPath(f.root)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
