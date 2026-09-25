package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/executor"
	"github.com/RazerBlade6/apex/internal/lock"
	"github.com/RazerBlade6/apex/internal/store"
)

// Every dispatch in this file goes to a fake executor. Running the real one
// would edit files and spend the user's Claude Code quota on `go test`, which
// is the same argument the provider tests make one layer up — with more at
// stake, because this one writes.

// fakeExecutor records the briefs it was handed and replays a scripted run.
type fakeExecutor struct {
	mu sync.Mutex
	// Briefs is every brief that reached the executor, in order.
	Briefs []executor.TaskBrief
	// Fail, when set, is what Run reports instead of succeeding.
	Fail error
	// Unavailable, when set, is what Available reports.
	Unavailable error
	// Files is what the run claims to have changed.
	Files []string
	// Before, when set, runs before the events are emitted, so a test can
	// have the "agent" touch the working tree.
	Before func()
	// Started is closed on the first Run, so a test can observe a dispatch
	// in flight.
	Started chan struct{}
	// Release, when non-nil, holds the run open until it is closed.
	Release chan struct{}
}

func (f *fakeExecutor) Name() string { return "fake" }

func (f *fakeExecutor) Available() error { return f.Unavailable }

func (f *fakeExecutor) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Briefs)
}

func (f *fakeExecutor) LastBrief(t *testing.T) executor.TaskBrief {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Briefs) == 0 {
		t.Fatal("nothing was dispatched")
	}
	return f.Briefs[len(f.Briefs)-1]
}

func (f *fakeExecutor) Run(ctx context.Context, brief executor.TaskBrief) (<-chan executor.Event, error) {
	f.mu.Lock()
	f.Briefs = append(f.Briefs, brief)
	first := len(f.Briefs) == 1
	f.mu.Unlock()
	if first && f.Started != nil {
		close(f.Started)
	}

	ch := make(chan executor.Event, 8)
	go func() {
		defer close(ch)
		if f.Release != nil {
			select {
			case <-f.Release:
			case <-ctx.Done():
			}
		}
		if f.Before != nil {
			f.Before()
		}
		ch <- executor.Event{Type: executor.EventOutput, Text: "working\n"}
		if f.Fail != nil {
			ch <- executor.Event{Type: executor.EventError, Err: f.Fail}
			return
		}
		for _, path := range f.Files {
			ch <- executor.Event{Type: executor.EventFileEdit, Tool: "Edit", Path: path}
		}
		ch <- executor.Event{Type: executor.EventDone, Result: &executor.Result{
			SessionID:    brief.SessionID,
			Model:        "fake-sonnet",
			FilesChanged: f.Files,
			Summary:      "did the thing",
		}}
	}()
	return ch, nil
}

// installFakeExecutor points the command layer at an offline executor.
func installFakeExecutor(t *testing.T, fake *fakeExecutor) *fakeExecutor {
	t.Helper()
	previous := newExecutor
	newExecutor = func(*config.Config, dispatchOptions) (executor.Executor, error) {
		return fake, nil
	}
	t.Cleanup(func() { newExecutor = previous })
	return fake
}

// seedDispatchableItem gets a fixture to the state `apex do` needs: a
// registered project with a digest and one proposed action item.
func seedDispatchableItem(t *testing.T, f *fixture) store.ActionItem {
	t.Helper()
	seedReviewableProject(t, f)
	f.fake.JSON = `{"items":[{"project":"atlas","title":"Write the chunker",` +
		`"body":"Split on table boundaries.","rationale":"The digest names this as the blocker.","effort":"medium"}]}`
	if out, err := run(t, "review"); err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}
	items := listItems(t)
	if len(items) != 1 {
		t.Fatalf("seeded %d items, want 1", len(items))
	}
	return items[0]
}

func getItem(t *testing.T, id string) store.ActionItem {
	t.Helper()
	st, closeStore := openFixtureStore(t)
	defer closeStore()
	item, err := st.GetActionItem(context.Background(), id)
	if err != nil {
		t.Fatalf("GetActionItem(%s): %v", id, err)
	}
	return item
}

func execRuns(t *testing.T) []store.ExecRun {
	t.Helper()
	st, closeStore := openFixtureStore(t)
	defer closeStore()
	runs, err := st.ListExecRuns(context.Background(), "")
	if err != nil {
		t.Fatalf("ListExecRuns: %v", err)
	}
	return runs
}

// TestDoDispatchesAndLandsInReview is the happy path of DESIGN.md §14's
// `apex do`, including the status transition the spec is most emphatic about:
// a successful run reaches in_review and stops there.
func TestDoDispatchesAndLandsInReview(t *testing.T) {
	f := newFixture(t)
	item := seedDispatchableItem(t, f)
	fake := installFakeExecutor(t, &fakeExecutor{Files: []string{"chunk.go"}})

	out, err := run(t, "do", item.ID)
	if err != nil {
		t.Fatalf("do: %v\n%s", err, out)
	}
	if fake.Calls() != 1 {
		t.Fatalf("dispatched %d times, want 1", fake.Calls())
	}

	got := getItem(t, item.ID)
	if got.Status != store.ItemInReview {
		t.Errorf("status = %q after a successful run, want in_review", got.Status)
	}
	if got.Status == store.ItemDone {
		t.Error("apex marked an item done; that is the user's judgment after reading the diff")
	}
	if !strings.Contains(out, "does not mark items done") {
		t.Errorf("the output does not say who decides completion:\n%s", out)
	}

	// The run row: one per dispatch, closed, with the session id that makes
	// it resumable and the log that makes it readable (DESIGN.md §9).
	runs := execRuns(t)
	if len(runs) != 1 {
		t.Fatalf("recorded %d exec runs, want 1", len(runs))
	}
	row := runs[0]
	if row.ExitStatus != store.ExitSuccess || row.FinishedAt == nil {
		t.Errorf("run = %+v, want a closed successful run", row)
	}
	if row.SessionID == "" {
		t.Error("the run recorded no session id, so it cannot be resumed with claude --resume")
	}
	if row.ActionItemID != item.ID || row.ProjectSlug != "atlas" {
		t.Errorf("run = %+v, want it attributed to the item and the project", row)
	}
	// The row points at ~/.apex/logs/exec/<run-id>.log (DESIGN.md §5), and
	// the brief carries the same path, which is what makes the executor
	// write there. Writing the file is the executor's job and is covered by
	// its own tests; the fake one here does not.
	wantLog := filepath.Join(f.root, "logs", "exec", row.ID+".log")
	if row.LogPath != wantLog {
		t.Errorf("LogPath = %q, want %q", row.LogPath, wantLog)
	}
	if brief := fake.LastBrief(t); brief.LogPath != row.LogPath || brief.RunID != row.ID {
		t.Errorf("the brief and the run row disagree: %q/%q vs %q/%q",
			brief.RunID, brief.LogPath, row.ID, row.LogPath)
	}
}

// TestDoBriefCarriesIntentNotProjectBackground is the assertion behind
// DESIGN.md §9's design: the agent reads the project from the working tree, so
// the brief must not carry a digest that was generated at some earlier sync.
func TestDoBriefCarriesIntentNotProjectBackground(t *testing.T) {
	f := newFixture(t)
	item := seedDispatchableItem(t, f)
	fake := installFakeExecutor(t, &fakeExecutor{})

	if out, err := run(t, "do", item.ID); err != nil {
		t.Fatalf("do: %v\n%s", err, out)
	}
	brief := fake.LastBrief(t)

	for _, want := range []string{item.ID, "Write the chunker", "Split on table boundaries", "blocker"} {
		if !strings.Contains(brief.Instruction, want) {
			t.Errorf("the brief does not carry the intent %q:\n%s", want, brief.Instruction)
		}
	}

	// The digest body is what `apex sync` generated and cached. It is project
	// background by definition, and the authoritative version of it is in the
	// working tree the agent is about to read.
	st, closeStore := openFixtureStore(t)
	digest, err := st.GetDigest(context.Background(), "atlas")
	closeStore()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(brief.Instruction, strings.TrimSpace(digest.Body)) {
		t.Errorf("the brief pasted the cached digest into itself:\n%s", brief.Instruction)
	}

	// And the dispatch actually put the agent in the project.
	if brief.ProjectPath != filepath.Join(f.home, "Development", "Atlas") {
		t.Errorf("ProjectPath = %q, want the project directory", brief.ProjectPath)
	}
}

// TestDoFailureReturnsTheItemToAccepted: DESIGN.md §14 step 6. A failed run
// must not leave an item stranded in_progress.
func TestDoFailureReturnsTheItemToAccepted(t *testing.T) {
	f := newFixture(t)
	item := seedDispatchableItem(t, f)
	installFakeExecutor(t, &fakeExecutor{
		Fail: &executor.RunError{Executor: "fake", RunID: "x", Kind: executor.FailureUnknown,
			Message: "the agent gave up"},
	})

	out, err := run(t, "do", item.ID)
	if err == nil {
		t.Fatalf("a failed dispatch reported success:\n%s", out)
	}
	if got := getItem(t, item.ID).Status; got != store.ItemAccepted {
		t.Errorf("status = %q after a failed run, want accepted", got)
	}
	runs := execRuns(t)
	if len(runs) != 1 || runs[0].ExitStatus != store.ExitFailed || runs[0].FinishedAt == nil {
		t.Errorf("runs = %+v, want one closed failed run", runs)
	}
}

// TestDispatchIsBlockedByTheProjectLock is DESIGN.md §10's whole point, and
// the first real consumer of internal/lock. Two agents editing one working
// tree produce interleaved edits with no way to attribute either, so the
// second dispatch must refuse rather than proceed.
func TestDispatchIsBlockedByTheProjectLock(t *testing.T) {
	f := newFixture(t)
	item := seedDispatchableItem(t, f)
	fake := installFakeExecutor(t, &fakeExecutor{})

	// Stand in for a dispatch already running against this project.
	lockPath := filepath.Join(f.root, "locks", "atlas.lock")
	held, err := lock.AcquireAs(lockPath, lock.Exclusive, "7f3a91")
	if err != nil {
		t.Fatalf("hold the project lock: %v", err)
	}
	defer held.Release() //nolint:errcheck

	out, err := run(t, "do", item.ID)
	if err == nil {
		t.Fatalf("a second dispatch ran against a locked project:\n%s", out)
	}
	assertUserFixable(t, err)

	// The message names the run that holds it, not merely "locked"
	// (DESIGN.md §10).
	msg := err.Error()
	for _, want := range []string{"Atlas", "7f3a91", "logs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not report the holding run (%q): %v", want, msg)
		}
	}

	// Nothing was dispatched, and nothing was written: a refused dispatch
	// must leave the item exactly where it was.
	if fake.Calls() != 0 {
		t.Error("the executor ran against a project another dispatch holds")
	}
	if got := getItem(t, item.ID).Status; got != item.Status {
		t.Errorf("status = %q after a refused dispatch, want it unchanged at %q", got, item.Status)
	}
	if runs := execRuns(t); len(runs) != 0 {
		t.Errorf("recorded %d exec runs for a dispatch that never happened", len(runs))
	}
}

// TestDoRefusesClosedItems: dispatching a done or dismissed item is almost
// always a mistyped id, and the cost of being wrong is an agent editing over
// work the user already closed.
func TestDoRefusesClosedItems(t *testing.T) {
	f := newFixture(t)
	item := seedDispatchableItem(t, f)
	fake := installFakeExecutor(t, &fakeExecutor{})

	for _, status := range []string{store.ItemDone, store.ItemDismissed} {
		st, closeStore := openFixtureStore(t)
		err := st.SetActionItemStatus(context.Background(), item.ID, status, time.Now())
		closeStore()
		if err != nil {
			t.Fatal(err)
		}

		out, err := run(t, "do", item.ID)
		if err == nil {
			t.Fatalf("dispatched a %s item:\n%s", status, out)
		}
		assertUserFixable(t, err)
		if !strings.Contains(err.Error(), status) {
			t.Errorf("the error does not say why it refused: %v", err)
		}
	}
	if fake.Calls() != 0 {
		t.Error("a closed item reached the executor")
	}
}

// TestDoStopsWhenTheExecutorIsUnavailable: DESIGN.md §9 calls Available()
// load-bearing. Checking it after the status moved would leave the item wrong.
func TestDoStopsWhenTheExecutorIsUnavailable(t *testing.T) {
	f := newFixture(t)
	item := seedDispatchableItem(t, f)
	installFakeExecutor(t, &fakeExecutor{
		Unavailable: &executor.UnavailableError{
			Executor: "fake", Reason: "claude is not installed", Fix: "install the Claude Code CLI",
		},
	})

	out, err := run(t, "do", item.ID)
	if err == nil {
		t.Fatalf("dispatched with no executor:\n%s", out)
	}
	assertUserFixable(t, err)
	if got := getItem(t, item.ID).Status; got != item.Status {
		t.Errorf("status = %q, want it untouched at %q when nothing could run", got, item.Status)
	}
	if runs := execRuns(t); len(runs) != 0 {
		t.Errorf("opened %d run rows for a dispatch that could not start", len(runs))
	}
}

// TestReviewRecordsTheServingModel is the provenance half of DESIGN.md §18's
// Structured change. Before M5, digests.model recorded the model that actually
// served the call and action_items.generated_by could only record the route's
// alias — two columns, two answers, one run.
func TestReviewRecordsTheServingModel(t *testing.T) {
	f := newFixture(t)
	seedReviewableProject(t, f)
	f.fake.JSON = `{"items":[{"project":"atlas","title":"Do a thing","body":"b","rationale":"r","effort":"small"}]}`

	out, err := run(t, "review")
	if err != nil {
		t.Fatalf("review: %v\n%s", err, out)
	}

	items := listItems(t)
	if len(items) != 1 {
		t.Fatalf("recorded %d items, want 1", len(items))
	}
	// The fake reports "fake-model-5" as the serving model; the configured
	// route's model is claude-opus-5.
	if items[0].GeneratedBy != "fake-model-5" {
		t.Errorf("generated_by = %q, want the model that served the call", items[0].GeneratedBy)
	}
	st, closeStore := openFixtureStore(t)
	digest, digestErr := st.GetDigest(context.Background(), "atlas")
	closeStore()
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	if digest.Model != items[0].GeneratedBy {
		t.Errorf("digests.model = %q but action_items.generated_by = %q; the same run must not give two answers",
			digest.Model, items[0].GeneratedBy)
	}

	// And the call's cost is now visible at all, which it was not before.
	if !strings.Contains(out, "in=") || !strings.Contains(out, "cache") {
		t.Errorf("review did not report what the call cost:\n%s", out)
	}
}

// TestDoctorChecksTheIdentityFiles closes the third M5 item in DESIGN.md §18.
// An empty identity context silently produces generic advice, which is exactly
// the kind of quiet assumption doctor exists to catch.
func TestDoctorChecksTheIdentityFiles(t *testing.T) {
	f := newFixture(t)

	r := &report{}
	checkIdentity(context.Background(), r)
	got := findCheck(t, r, "identity context")
	if got.Level != levelWarn {
		t.Errorf("level = %v with neither file written, want a warning", got.Level)
	}
	for _, want := range []string{contextfs.ProfileFile, contextfs.SkillsFile} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", got.Detail, want)
		}
	}
	if !strings.Contains(got.Fix, "generic") {
		t.Errorf("fix = %q, want it to say what the absence actually costs", got.Fix)
	}

	// Written, and the check passes.
	for _, path := range []string{contextfs.ProfilePath(f.root), contextfs.SkillsPath(f.root)} {
		if err := os.WriteFile(path, []byte("# Me\n\nI build CLIs.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r = &report{}
	checkIdentity(context.Background(), r)
	if got := findCheck(t, r, "identity context"); got.Level != levelOK {
		t.Errorf("level = %v with both files written, want ok: %+v", got.Level, got)
	}

	// An empty file is not the same as an absent one, and is reported
	// separately: the user wrote it and left it blank.
	if err := os.WriteFile(contextfs.SkillsPath(f.root), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = &report{}
	checkIdentity(context.Background(), r)
	got = findCheck(t, r, "identity context")
	if got.Level != levelWarn || !strings.Contains(got.Detail, "empty") {
		t.Errorf("an empty identity file was not reported as such: %+v", got)
	}
}

// assertNoOrphanLock confirms a finished dispatch left its lock free.
func assertNoOrphanLock(t *testing.T, root, slug string) {
	t.Helper()
	l, err := lock.Acquire(filepath.Join(root, "locks", slug+".lock"), lock.Exclusive)
	if err != nil {
		var busy *lock.BusyError
		if errors.As(err, &busy) {
			t.Fatalf("the dispatch did not release %s: %v", slug, err)
		}
		t.Fatal(err)
	}
	l.Release() //nolint:errcheck
}

// TestDispatchReleasesTheLock: the lock is held for the run and no longer. A
// dispatch that leaked it would wedge the project until the process exited.
func TestDispatchReleasesTheLock(t *testing.T) {
	f := newFixture(t)
	item := seedDispatchableItem(t, f)
	installFakeExecutor(t, &fakeExecutor{})

	if out, err := run(t, "do", item.ID); err != nil {
		t.Fatalf("do: %v\n%s", err, out)
	}
	assertNoOrphanLock(t, f.root, "atlas")
}

// TestResultReportsTheWorkingTreeNotTheAgentsClaim is the other finding from
// M5's real dispatches. An agent that writes files through a shell instead of
// the edit tools reports no file changes at all, and Apex printed "no file
// changes" over a working tree with two new files in it.
func TestResultReportsTheWorkingTreeNotTheAgentsClaim(t *testing.T) {
	f := newFixture(t)
	item := seedDispatchableItem(t, f)

	// The report reads `git status --short`, so the fixture needs to be a
	// repository — which every real project Apex dispatches against is.
	dir := filepath.Join(f.home, "Development", "Atlas")
	gitInit(t, dir)

	// The fake claims nothing, and writes a file the way a shell would.
	fake := &fakeExecutor{}
	fake.Before = func() {
		if err := os.WriteFile(filepath.Join(dir, "chunk.go"), []byte("package atlas\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	installFakeExecutor(t, fake)

	out, err := run(t, "do", item.ID)
	if err != nil {
		t.Fatalf("do: %v\n%s", err, out)
	}
	if !strings.Contains(out, "chunk.go") {
		t.Errorf("the report does not name the file that actually changed:\n%s", out)
	}
	if strings.Contains(out, "no file changes") {
		t.Errorf("the report claimed nothing happened over a changed working tree:\n%s", out)
	}
}

// TestBriefStatesWhatTheAgentMayDo is a finding from the first real dispatch
// rather than a guess. Under the default permission mode the agent may write
// files and may not run commands, so its build-and-test command was denied; it
// spent a turn discovering that and then reported a change it could not
// verify. The brief now says so up front, either way.
func TestBriefStatesWhatTheAgentMayDo(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"the default cannot run commands", nil, "CANNOT run commands"},
		{"bypass can", []string{"--bypass-permissions"}, "You may run commands"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			item := seedDispatchableItem(t, f)
			fake := installFakeExecutor(t, &fakeExecutor{})

			args := append([]string{"do", item.ID}, tt.args...)
			if out, err := run(t, args...); err != nil {
				t.Fatalf("do: %v\n%s", err, out)
			}
			if got := fake.LastBrief(t).Instruction; !strings.Contains(got, tt.want) {
				t.Errorf("the brief does not say %q:\n%s", tt.want, got)
			}
		})
	}
}
