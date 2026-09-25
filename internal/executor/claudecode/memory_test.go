package claudecode

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The `CLAUDE.md` exclusion (DESIGN.md §9), as it actually ships.
//
// The decision was recorded in M5 and the mechanism settled in M6: --safe-mode
// so the user's global CLAUDE.md is not discovered, plus the project's own
// CLAUDE.md re-supplied verbatim in the appended system prompt. The half that
// can regress silently is the second one — dropping --safe-mode in would be
// noticed the first time a run behaved oddly, whereas losing the re-supply
// just makes dispatches quietly worse at following the project's conventions.

func TestSafeModeExcludesTheGlobalAndResuppliesTheProjectCLAUDEmd(t *testing.T) {
	dir := projectDir(t)
	const conventions = "# Fixture conventions\n\nNever add a dependency without asking.\n"
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(conventions), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFake(t, emit(lineInit, lineText, lineResult("done")))
	e := New(Options{Binary: f.bin, Model: "sonnet", Effort: "low"})
	if _, _, err := drain(t, e, briefFor(t, dir)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	args := f.args(t)

	if !slices.Contains(args, "--safe-mode") {
		t.Errorf("argv = %q, want --safe-mode; without it the user's global CLAUDE.md silently shapes every dispatch", args)
	}
	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, "Never add a dependency without asking.") {
		t.Errorf("the project's own CLAUDE.md was not re-supplied, so --safe-mode threw it away too:\n%s", joined)
	}
	if !strings.Contains(joined, ProjectMemoryHeading) {
		t.Error("the re-supplied conventions are not labelled, so the agent cannot tell them from Apex's brief")
	}
	// The brief still leads: it is what this run is for.
	if i, j := strings.Index(joined, "# Apex dispatch"), strings.Index(joined, ProjectMemoryHeading); i < 0 || i > j {
		t.Errorf("the project's conventions precede the brief in the system prompt")
	}
	// And --bare stays forbidden: it skips CLAUDE.md discovery too, and
	// kills subscription auth doing it.
	if slices.Contains(args, "--bare") {
		t.Error("argv carries --bare, which forces ANTHROPIC_API_KEY and never reads OAuth")
	}

	// A project with no CLAUDE.md gets no empty section bolted onto its brief.
	bare := projectDir(t)
	g := newFake(t, emit(lineInit, lineResult("done")))
	if _, _, err := drain(t, New(Options{Binary: g.bin}), briefFor(t, bare)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := strings.Join(g.args(t), "\n"); strings.Contains(got, ProjectMemoryHeading) {
		t.Errorf("an empty conventions section was appended to the brief:\n%s", got)
	}
}

func TestInheritUserConfigRestoresTheDefaultBehaviour(t *testing.T) {
	dir := projectDir(t)
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("# Conventions\n\nUNIQUE-MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFake(t, emit(lineInit, lineResult("done")))
	e := New(Options{Binary: f.bin, InheritUserConfig: true})
	if _, _, err := drain(t, e, briefFor(t, dir)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	args := f.args(t)

	if slices.Contains(args, "--safe-mode") {
		t.Error("inherit_user_config = true still passed --safe-mode")
	}
	// With discovery working normally the agent reads the file itself, so
	// inlining it would send the same content twice.
	if strings.Contains(strings.Join(args, "\n"), "UNIQUE-MARKER") {
		t.Error("the project CLAUDE.md was inlined even though the CLI would have discovered it")
	}
}
