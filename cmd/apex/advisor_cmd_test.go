package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/store"
)

// seedReviewableProject gets a fixture to the state `apex review` needs: one
// registered project with a cached digest.
func seedReviewableProject(t *testing.T, f *fixture) {
	t.Helper()
	f.project(t, "Atlas", map[string]string{contextfs.ProjectFile: atlasDoc})
	f.writeRegistry(t, "# Projects\n\n## Atlas\npath: ~/Development/Atlas\n")
	if out, err := run(t, "sync"); err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
}

// TestReviewRecordsItemsAndDeduplicates is the command-level happy path:
// review writes proposed items, `items` lists them grouped by project, `show`
// finds one by a shortened id, and a second review adds nothing because every
// proposal is already open.
func TestReviewRecordsItemsAndDeduplicates(t *testing.T) {
	f := newFixture(t)
	seedReviewableProject(t, f)

	f.fake.JSON = `{"items":[
		{"project":"atlas","title":"Write a README","body":"Explain **what** it is.\n\n- one\n- two","rationale":"Nobody can tell what this is.","effort":"small"},
		{"project":"atlas","title":"Pick a status","body":"b2","rationale":"r2","effort":"medium"}
	]}`

	out, err := run(t, "review")
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	for _, want := range []string{"AI-001", "Write a README", "AI-002", "Recorded 2 action item(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("review output does not mention %q:\n%s", want, out)
		}
	}
	// Markdown is rendered, not dumped: emphasis markers are gone and the
	// list keeps its structure.
	if strings.Contains(out, "**what**") {
		t.Errorf("markdown emphasis was printed raw:\n%s", out)
	}

	// The rows landed as proposed, stamped with provenance.
	items := listItems(t)
	if len(items) != 2 {
		t.Fatalf("recorded %d items, want 2", len(items))
	}
	for _, item := range items {
		if item.Status != store.ItemProposed {
			t.Errorf("%s status = %q, want proposed", item.ID, item.Status)
		}
		if item.ContextHash == "" {
			t.Errorf("%s has no context_hash", item.ID)
		}
		if item.ProjectSlug != "atlas" {
			t.Errorf("%s project = %q", item.ID, item.ProjectSlug)
		}
	}

	// A second review proposing the same two things records nothing.
	out, err = run(t, "review")
	if err != nil {
		t.Fatalf("second review: %v\n%s", err, out)
	}
	if !strings.Contains(out, "No new action items") {
		t.Errorf("a repeat review recorded something:\n%s", out)
	}
	if n := len(listItems(t)); n != 2 {
		t.Errorf("action_items holds %d rows after a repeat review, want 2", n)
	}

	// items and show are read-only views over the same rows.
	out, err = run(t, "items")
	if err != nil {
		t.Fatalf("items: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Atlas") || !strings.Contains(out, "AI-001") {
		t.Errorf("items did not group the rows under their project:\n%s", out)
	}

	out, err = run(t, "items", "--status", "done")
	if err != nil {
		t.Fatalf("items --status done: %v\n%s", err, out)
	}
	if strings.Contains(out, "AI-001") {
		t.Errorf("--status done listed a proposed item:\n%s", out)
	}

	// The prefix and the padding are both optional.
	out, err = run(t, "show", "1")
	if err != nil {
		t.Fatalf("show 1: %v\n%s", err, out)
	}
	if !strings.Contains(out, "AI-001") || !strings.Contains(out, "Why now:") {
		t.Errorf("show did not print the item in full:\n%s", out)
	}
	// Provenance: nothing has moved, so the context is still current.
	if !strings.Contains(out, "still current") {
		t.Errorf("show did not report the context as current:\n%s", out)
	}
}

// TestReviewWithoutIdentitySaysSo covers the state this milestone ships into.
// The user has no PROFILE.md or SKILLS.md, and the output must say that the
// advice is generic rather than let it read as personalised.
func TestReviewWithoutIdentitySaysSo(t *testing.T) {
	f := newFixture(t)
	seedReviewableProject(t, f)
	f.fake.JSON = `{"items":[{"project":"atlas","title":"Do a thing","body":"b","rationale":"r","effort":"small"}]}`

	out, err := run(t, "review")
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	for _, want := range []string{contextfs.ProfileFile, contextfs.SkillsFile, "generic"} {
		if !strings.Contains(out, want) {
			t.Errorf("review did not warn about the missing identity context (%q):\n%s", want, out)
		}
	}
}

// TestReviewWithoutDigestsRefuses: there is nothing to reason over, and the
// fix is one command.
func TestReviewWithoutDigestsRefuses(t *testing.T) {
	f := newFixture(t)
	f.writeRegistry(t, "# Projects\n")

	out, err := run(t, "review")
	if err == nil {
		t.Fatalf("review with no digests succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "apex sync") {
		t.Errorf("the error does not name the fix: %v", err)
	}
	assertUserFixable(t, err)
	if f.fake.Calls() != 0 {
		t.Errorf("review reached a model with nothing to reason over")
	}
}

// TestIdeasRecordsProposals is the ideas happy path, including that `show`
// resolves an IDEA id as readily as an AI one.
func TestIdeasRecordsProposals(t *testing.T) {
	f := newFixture(t)
	seedReviewableProject(t, f)
	f.fake.JSON = `{"ideas":[
		{"title":"A worktree switcher","pitch":"A TUI over git worktrees.","rationale":"You keep writing small Go CLIs."}
	]}`

	out, err := run(t, "ideas")
	if err != nil {
		t.Fatalf("ideas: %v\n%s", err, out)
	}
	if !strings.Contains(out, "IDEA-001") || !strings.Contains(out, "A worktree switcher") {
		t.Errorf("ideas did not print the proposal:\n%s", out)
	}

	out, err = run(t, "show", "IDEA-1")
	if err != nil {
		t.Fatalf("show IDEA-1: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Why you:") {
		t.Errorf("show did not print the idea's rationale:\n%s", out)
	}
}

// TestReadOnlyCommandsDoNotMigrate is the other half of DESIGN.md §18's
// doctor decision: applying a forward-only schema change is sync's job, and a
// command that only reads must stop and say so rather than do it.
func TestReadOnlyCommandsDoNotMigrate(t *testing.T) {
	newFixture(t)

	// A database at the bootstrap schema, with every migration still
	// pending: exactly what an upgraded binary meets.
	dbPath, err := config.DatabasePath()
	if err != nil {
		t.Fatal(err)
	}
	backend := store.NewLocalBackend(dbPath)
	if _, err := store.Open(context.Background(), backend); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"items"}, {"show", "AI-001"}, {"review"}, {"ideas"}} {
		out, err := run(t, args...)
		if err == nil {
			t.Errorf("apex %s succeeded against a pending schema:\n%s", strings.Join(args, " "), out)
			continue
		}
		if !strings.Contains(err.Error(), "apex sync") {
			t.Errorf("apex %s: error does not send the user to sync: %v", strings.Join(args, " "), err)
		}
		assertUserFixable(t, err)
	}

	// doctor reports the same state as a WARNING, not a failure: a schema one
	// command behind is a normal state after an upgrade.
	keyring.MockInit() // never the real macOS keychain
	r := runDoctor(context.Background(), doctorOptions{})
	migrations := findCheck(t, r, "migrations")
	if migrations.Level != levelWarn {
		t.Errorf("migrations check is %v, want a warning: %+v", migrations.Level, migrations)
	}
	// Every embedded migration is outstanding against a bootstrap-only
	// database. The count is asserted from the embedded set rather than
	// hardcoded, so adding a migration does not fail this test for the wrong
	// reason.
	embedded, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d pending", len(embedded)); !strings.Contains(migrations.Detail, want) {
		t.Errorf("doctor did not report every pending migration (%s): %q", want, migrations.Detail)
	}
	if !strings.Contains(migrations.Fix, "apex sync") {
		t.Errorf("the fix does not name the command that owns migrations: %q", migrations.Fix)
	}

	// And it did NOT apply them. This is the whole point of the decision: in
	// M3, `apex doctor` silently applied migration 002 to the real database.
	st, closeStore := openFixtureStoreWithoutMigrating(t)
	defer closeStore()
	status, err := st.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Applied) != 0 {
		t.Errorf("doctor applied %d migration(s); it is read-only", len(status.Applied))
	}
}

// TestDigestRouteWarningNeedsAnAlternative is the refinement this milestone
// made to DESIGN.md §18. Warning whenever digests are routed at claude-cli
// would nag a subscription-only machine about a choice it cannot make
// differently; the warning fires only when an API key is actually available.
func TestDigestRouteWarningNeedsAnAlternative(t *testing.T) {
	newFixture(t)
	keyring.MockInit() // never the real macOS keychain

	cfg := config.Default()
	cfg.Models.Digest.Provider = "claude-cli"
	cfg.Models.Digest.Model = "opus"

	t.Run("no key anywhere", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("OPENAI_API_KEY", "")
		r := &report{}
		checkDigestRoute(context.Background(), r, &cfg)
		if len(r.checks) != 0 {
			t.Errorf("warned about the only configuration available: %+v", r.checks)
		}
	})

	t.Run("a key is available", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
		r := &report{}
		checkDigestRoute(context.Background(), r, &cfg)
		if len(r.checks) != 1 || r.checks[0].Level != levelWarn {
			t.Fatalf("expected one warning, got %+v", r.checks)
		}
		if strings.Contains(r.checks[0].Fix, "sk-ant-test") {
			t.Error("the warning printed the key")
		}
	})
}

func listItems(t *testing.T) []store.ActionItem {
	t.Helper()
	st, closeStore := openFixtureStore(t)
	defer closeStore()
	items, err := st.ListActionItems(context.Background(), store.ActionItemFilter{})
	if err != nil {
		t.Fatalf("ListActionItems: %v", err)
	}
	return items
}
