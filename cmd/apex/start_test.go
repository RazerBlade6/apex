package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/store"
)

// seedIdea gets a fixture to the state `apex start` needs: one proposed idea.
func seedIdea(t *testing.T, f *fixture, pitch string) store.Idea {
	t.Helper()
	seedReviewableProject(t, f)
	f.fake.JSON = `{"ideas":[{"title":"Worktree Switcher","pitch":"` + pitch +
		`","rationale":"You keep writing small command line tools."}]}`
	if out, err := run(t, "ideas"); err != nil {
		t.Fatalf("ideas: %v\n%s", err, out)
	}

	st, closeStore := openFixtureStore(t)
	defer closeStore()
	ideas, err := st.ListIdeas(context.Background(), "")
	if err != nil || len(ideas) != 1 {
		t.Fatalf("ListIdeas = %v, %v; want one idea", ideas, err)
	}
	return ideas[0]
}

// fakeTool puts an executable of the given name on PATH for one test, and
// records that it ran.
func fakeTool(t *testing.T, name, body string) (dir, marker string) {
	t.Helper()
	dir = t.TempDir()
	marker = filepath.Join(dir, name+".ran")
	script := "#!/bin/sh\n: > '" + marker + "'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, marker
}

// TestStartTierOneCreatesARealProject: everything deterministic, with no model
// involved anywhere (DESIGN.md §14, tier 1).
func TestStartTierOneCreatesARealProject(t *testing.T) {
	f := newFixture(t)
	idea := seedIdea(t, f, "A TUI over git worktrees.")
	fake := installFakeExecutor(t, &fakeExecutor{})

	dir := filepath.Join(f.home, "Development", "Worktree Switcher")
	out, err := run(t, "start", idea.ID, "--no-dispatch")
	if err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	if fake.Calls() != 0 {
		t.Error("--no-dispatch still sent a brief to the agent")
	}

	// The directory, with a git repository and both files.
	for _, rel := range []string{".git", contextfs.ProjectFile, ".gitignore"} {
		if _, statErr := os.Stat(filepath.Join(dir, rel)); statErr != nil {
			t.Errorf("tier 1 did not create %s: %v", rel, statErr)
		}
	}

	// PROJECT.md is written from the idea rather than from a blank template,
	// so the file says something true on day one.
	body, err := os.ReadFile(contextfs.ProjectDocPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(body)
	for _, want := range []string{"name: Worktree Switcher", "status: active", "A TUI over git worktrees.", idea.ID} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s does not carry %q:\n%s", contextfs.ProjectFile, want, doc)
		}
	}

	// The registry, which is the only file that answers where a project lives.
	registry := f.readRegistry(t)
	if !strings.Contains(registry, "## Worktree Switcher") || !strings.Contains(registry, "path: ~/Development/Worktree Switcher") {
		t.Errorf("the project was not registered:\n%s", registry)
	}
	// Tier 1 writes no summary line under the new entry: summaries are
	// generated from PROJECT.md by sync, and one written here would be
	// regenerated immediately. Atlas keeps the one sync already gave it.
	_, added, _ := strings.Cut(registry, "## Worktree Switcher")
	if strings.Contains(added, contextfs.SummaryMarker) {
		t.Errorf("start wrote a generated summary line:\n%s", registry)
	}
	// The existing entry survived, byte for byte.
	if !strings.Contains(registry, "## Atlas") {
		t.Errorf("start rewrote the registry instead of appending to it:\n%s", registry)
	}

	// The row and the idea's new status.
	st, closeStore := openFixtureStore(t)
	defer closeStore()
	proj, err := st.GetProject(context.Background(), "worktree-switcher")
	if err != nil {
		t.Fatalf("the project was not registered in the database: %v", err)
	}
	if proj.Path != dir {
		t.Errorf("project path = %q, want %q", proj.Path, dir)
	}
	updated, err := st.GetIdea(context.Background(), idea.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != store.IdeaStarted || updated.StartedProjectSlug != "worktree-switcher" {
		t.Errorf("idea = %+v, want it started as worktree-switcher", updated)
	}

	// A second start is refused rather than scaffolding over the first.
	if out, err := run(t, "start", idea.ID); err == nil {
		t.Errorf("started the same idea twice:\n%s", out)
	} else {
		assertUserFixable(t, err)
	}
}

// TestStartTierTwoSkipsAMissingScaffolder is the environment check this
// machine specifically needs: it has Go and may not have uv, cargo or npm. A
// start that refused to finish because an ecosystem tool was absent would be
// worse than one that says so and carries on.
func TestStartTierTwoSkipsAMissingScaffolder(t *testing.T) {
	f := newFixture(t)
	idea := seedIdea(t, f, "A TUI over git worktrees.")
	installFakeExecutor(t, &fakeExecutor{})

	// A PATH with nothing on it but git, so cargo is genuinely absent.
	gitDir := filepath.Dir(mustLookPath(t, "git"))
	t.Setenv("PATH", gitDir)

	out, err := run(t, "start", idea.ID, "--stack", "rust", "--no-dispatch")
	if err != nil {
		t.Fatalf("start refused to finish without cargo: %v\n%s", err, out)
	}
	if !strings.Contains(out, "cargo is not installed") {
		t.Errorf("the skip was not reported:\n%s", out)
	}

	// Tier 1 still produced a real project.
	dir := filepath.Join(f.home, "Development", "Worktree Switcher")
	if _, statErr := os.Stat(contextfs.ProjectDocPath(dir)); statErr != nil {
		t.Errorf("a skipped tier 2 took tier 1 down with it: %v", statErr)
	}
}

// TestStartTierTwoRunsTheScaffolder: when the tool is there, it runs, in the
// project directory, before the file writes.
func TestStartTierTwoRunsTheScaffolder(t *testing.T) {
	f := newFixture(t)
	idea := seedIdea(t, f, "A TUI over git worktrees.")
	installFakeExecutor(t, &fakeExecutor{})

	// A fake cargo that writes a file, the way a real scaffolder would.
	toolDir, marker := fakeTool(t, "cargo", "printf 'fn main() {}\\n' > src_main_placeholder")
	gitDir := filepath.Dir(mustLookPath(t, "git"))
	t.Setenv("PATH", toolDir+string(os.PathListSeparator)+gitDir)

	out, err := run(t, "start", idea.ID, "--stack", "rust", "--no-dispatch")
	if err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the scaffolder did not run: %v", statErr)
	}
	if !strings.Contains(out, "cargo init") {
		t.Errorf("the scaffolder was not reported:\n%s", out)
	}

	dir := filepath.Join(f.home, "Development", "Worktree Switcher")
	if _, statErr := os.Stat(filepath.Join(dir, "src_main_placeholder")); statErr != nil {
		t.Errorf("the scaffolder did not run in the project directory: %v", statErr)
	}
}

// TestStartInfersAStackOnlyFromUnambiguousText: guessing wrong scaffolds a
// project in the wrong language, quietly. Skipping the tier is the safe
// failure, and the user is told how to override.
func TestStartInfersAStackOnlyFromUnambiguousText(t *testing.T) {
	for _, tt := range []struct {
		name  string
		text  string
		stack string
	}{
		{"an explicit language", "a small CLI written in Rust", "rust"},
		{"a framework", "a FastAPI service for notes", "python"},
		{"the English word go", "a tool to go through your inbox", ""},
		{"nothing at all", "a thing that does a thing", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stack, _, ok := inferStack(tt.text)
			if tt.stack == "" {
				if ok {
					t.Errorf("inferStack(%q) = %q, want no guess at all", tt.text, stack)
				}
				return
			}
			if !ok || stack != tt.stack {
				t.Errorf("inferStack(%q) = %q, %v; want %q", tt.text, stack, ok, tt.stack)
			}
		})
	}
}

// TestStartRefusesANonEmptyDirectory: `apex start` runs a scaffolder and then
// an agent with edit permissions, so a mistyped --path must not aim both at
// existing work.
func TestStartRefusesANonEmptyDirectory(t *testing.T) {
	f := newFixture(t)
	idea := seedIdea(t, f, "A TUI over git worktrees.")
	fake := installFakeExecutor(t, &fakeExecutor{})

	occupied := filepath.Join(f.home, "Existing")
	if err := os.MkdirAll(occupied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "start", idea.ID, "--path", occupied)
	if err == nil {
		t.Fatalf("scaffolded into a directory that already held work:\n%s", out)
	}
	assertUserFixable(t, err)
	if fake.Calls() != 0 {
		t.Error("an agent was dispatched against an existing directory")
	}
	// The user's file is untouched.
	if body, readErr := os.ReadFile(filepath.Join(occupied, "main.go")); readErr != nil || string(body) != "package main\n" {
		t.Errorf("the existing file was modified: %q, %v", body, readErr)
	}
}

// TestStartTierThreeDispatchesAScaffoldingBrief: the brief tells the agent
// what the idea is and what the earlier tiers already did, so it extends a
// skeleton rather than recreating one.
func TestStartTierThreeDispatchesAScaffoldingBrief(t *testing.T) {
	f := newFixture(t)
	idea := seedIdea(t, f, "A TUI over git worktrees.")
	fake := installFakeExecutor(t, &fakeExecutor{})

	out, err := run(t, "start", idea.ID)
	if err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	if fake.Calls() != 1 {
		t.Fatalf("dispatched %d times, want 1", fake.Calls())
	}

	brief := fake.LastBrief(t)
	for _, want := range []string{"new project", idea.ID, "A TUI over git worktrees.", "Do not commit"} {
		if !strings.Contains(brief.Instruction, want) {
			t.Errorf("the scaffolding brief does not carry %q:\n%s", want, brief.Instruction)
		}
	}
	// PROJECT.md is Apex's file; the agent is told to leave it alone.
	if !strings.Contains(brief.Instruction, contextfs.ProjectFile) {
		t.Errorf("the brief says nothing about %s:\n%s", contextfs.ProjectFile, brief.Instruction)
	}
	// A scaffolding run has no action item, and the exec_runs row says which
	// project it was for instead.
	if brief.ActionItemID != "" {
		t.Errorf("ActionItemID = %q, want none for a scaffolding dispatch", brief.ActionItemID)
	}
	runs := execRuns(t)
	if len(runs) != 1 || runs[0].ProjectSlug != "worktree-switcher" || runs[0].ActionItemID != "" {
		t.Errorf("runs = %+v, want one run attributed to the project and to no item", runs)
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not on PATH", name)
	}
	return path
}

// TestAStartedProjectIsRecognisedBeforeItsFirstSync: `apex start` registers a
// project and stops short of generating a digest, so until the user runs sync
// the only record of it is its PROJECTS.md entry, its row and its PROJECT.md.
// That has to be enough for the advisor: an item proposed for it must land on
// it, not be skipped as a project nobody has heard of.
func TestAStartedProjectIsRecognisedBeforeItsFirstSync(t *testing.T) {
	f := newFixture(t)
	idea := seedIdea(t, f, "A TUI over git worktrees.")
	installFakeExecutor(t, &fakeExecutor{})
	if out, err := run(t, "start", idea.ID, "--no-dispatch"); err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}

	f.fake.JSON = `{"items":[
		{"project":"worktree-switcher","title":"List the worktrees","body":"b","rationale":"r","effort":"small"}
	]}`
	out, err := run(t, "review")
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if strings.Contains(out, "not in the portfolio") {
		t.Fatalf("the project start just created was not recognised:\n%s", out)
	}
	var found bool
	for _, item := range listItems(t) {
		if item.ProjectSlug == "worktree-switcher" && item.Title == "List the worktrees" {
			found = true
		}
	}
	if !found {
		t.Errorf("no item was recorded against the started project:\n%s", out)
	}

	// And the next sync picks it up from the entry start appended.
	if out, err := run(t, "sync"); err != nil {
		t.Fatalf("sync after start: %v\n%s", err, out)
	} else if !strings.Contains(out, "Worktree Switcher") {
		t.Errorf("sync did not find the started project in PROJECTS.md:\n%s", out)
	}
}

// TestStartRefusesANameThatIsAlreadyRegisteredElsewhere: an entry of the same
// name pointing somewhere else would mean the append is skipped and the new
// project is never registered. It is refused before anything is created.
func TestStartRefusesANameThatIsAlreadyRegisteredElsewhere(t *testing.T) {
	f := newFixture(t)
	idea := seedIdea(t, f, "A TUI over git worktrees.")
	f.writeRegistry(t, f.readRegistry(t)+"\n## Worktree Switcher\npath: ~/Elsewhere/switcher\n")

	out, err := run(t, "start", idea.ID, "--no-dispatch")
	if err == nil {
		t.Fatalf("start registered a project over an existing entry:\n%s", out)
	}
	assertUserFixable(t, err)
	if !strings.Contains(err.Error(), "--name") {
		t.Errorf("the refusal does not say how to fix it: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.home, "Development", "Worktree Switcher")); statErr == nil {
		t.Error("the directory was created before the conflict was found")
	}

	// --name is the way out, and it registers under the name given.
	if out, err := run(t, "start", idea.ID, "--no-dispatch", "--name", "Switcher"); err != nil {
		t.Fatalf("start --name: %v\n%s", err, out)
	}
	if registry := f.readRegistry(t); !strings.Contains(registry, "## Switcher\npath: ~/Development/Switcher") {
		t.Errorf("start --name did not register under the new name:\n%s", registry)
	}
}

// TestStartRefusesToInheritAnUnlistedProject: a row still holding the slug at
// another path — a project taken out of PROJECTS.md but never pruned — would
// otherwise be treated as having moved into the new directory, and the new
// project would inherit its digest and its action items.
func TestStartRefusesToInheritAnUnlistedProject(t *testing.T) {
	f := newFixture(t)
	idea := seedIdea(t, f, "A TUI over git worktrees.")

	st, closeStore := openFixtureStore(t)
	if err := st.UpsertProject(context.Background(), store.Project{
		Slug: "worktree-switcher", Name: "Worktree Switcher", Path: "/somewhere/else", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	closeStore()

	out, err := run(t, "start", idea.ID, "--no-dispatch")
	if err == nil {
		t.Fatalf("start adopted an unlisted project's row:\n%s", out)
	}
	assertUserFixable(t, err)
}
