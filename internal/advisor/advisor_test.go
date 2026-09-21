package advisor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/project"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// failingProvider fails for one project and succeeds for the rest. It is
// keyed on the prompt text because the request is all a provider ever sees.
type failingProvider struct {
	fakeProvider
	failOn string
}

func (f *failingProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 2)
	go func() {
		defer close(ch)
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "Project: "+f.failOn) {
				ch <- provider.Event{Type: provider.EventError,
					Err: &provider.Error{Provider: "fake", Op: "stream", Kind: provider.KindServer,
						Message: "the vendor fell over"}}
				return
			}
		}
		ch <- provider.Event{Type: provider.EventTextDelta, Text: "a digest"}
		ch <- provider.Event{Type: provider.EventDone, Usage: &provider.Usage{}}
	}()
	return ch, nil
}

// waitFor blocks until cond holds, or fails the test. It exists so the
// concurrency test does not depend on a sleep.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the worker pool to fill")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPromptOrdering is the invariant the whole caching design rests on
// (DESIGN.md §8): identity context and digests go in System, the volatile
// instruction goes in Messages, and the breakpoint falls between them.
//
// Getting this backwards costs nothing in correctness and everything in
// money, which is exactly the kind of mistake that survives review — so it is
// asserted rather than trusted.
func TestPromptOrdering(t *testing.T) {
	h := newHarness(t, &fakeProvider{})
	h.writeIdentity("I am a Go developer who likes small tools.")
	h.seedProject("scholarrag", "ScholarRAG", "Retrieval over papers. Blocked on chunking.", "hash-a")
	h.seedProject("atlas", "Atlas", "A map of abandoned projects.", "hash-b")

	actx, err := h.LoadContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req := actx.Prompt("PROPOSE SOMETHING").Build("m", "high", 100)

	if !req.CacheSystem {
		t.Error("no cache breakpoint was requested for a prompt with a stable prefix")
	}
	if len(req.Messages) != 1 || req.Messages[0].Content != "PROPOSE SOMETHING" {
		t.Fatalf("the instruction is not the sole user message: %+v", req.Messages)
	}
	if strings.Contains(req.System, "PROPOSE SOMETHING") {
		t.Error("the volatile instruction leaked into the cached system prefix")
	}

	identity := strings.Index(req.System, "Go developer")
	atlas := strings.Index(req.System, "A map of abandoned")
	scholar := strings.Index(req.System, "Retrieval over papers")
	switch {
	case identity < 0 || scholar < 0 || atlas < 0:
		t.Fatalf("System is missing identity or digests:\n%s", req.System)
	case identity > atlas:
		t.Error("digests are ordered before the identity context")
	case atlas > scholar:
		// Ordered by slug, so atlas precedes scholarrag. Stability is the
		// point: a set that reorders between calls never hits the cache.
		t.Error("digests are not in a stable (slug) order")
	}
}

// TestPromptWithoutIdentitySaysSo covers the state this milestone ships into:
// neither PROFILE.md nor SKILLS.md exists. The prompt must acknowledge that
// rather than leave a model to invent a persona.
func TestPromptWithoutIdentitySaysSo(t *testing.T) {
	h := newHarness(t, &fakeProvider{})
	h.seedProject("atlas", "Atlas", "A map.", "hash-b")

	actx, err := h.LoadContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	missing := actx.MissingIdentity()
	if len(missing) != 2 {
		t.Fatalf("MissingIdentity = %v, want both files", missing)
	}
	system := actx.Prompt("go").Build("m", "", 10).System
	for _, want := range []string{contextfs.ProfileFile, "do not infer a persona"} {
		if !strings.Contains(system, want) {
			t.Errorf("System does not mention %q:\n%s", want, system)
		}
	}
}

// TestContextHashTracksDigestSources checks the provenance stamp: it moves
// when a project's sources move, and does not move when only the digest's
// wording changes. An item generated against a state that has since moved on
// is what DESIGN.md §7 wants this column to be able to report.
func TestContextHashTracksDigestSources(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &fakeProvider{})
	h.seedProject("atlas", "Atlas", "first wording", "hash-a")

	first, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base := first.Hash()

	// Same sources, different prose: the same context.
	h.seedProject("atlas", "Atlas", "second wording, same sources", "hash-a")
	reworded, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reworded.Hash() != base {
		t.Error("rewording a digest changed the context hash; it should cover sources, not prose")
	}

	// Different sources: a different context.
	h.seedProject("atlas", "Atlas", "second wording, same sources", "hash-b")
	moved, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if moved.Hash() == base {
		t.Error("a changed source hash did not change the context hash")
	}
}

// TestGenerateDigestCachesAgainstItsSources is the caching correctness check:
// a digest is stored against the hash of the sources it actually saw, and a
// sync that finds that hash unchanged does not call a model again.
func TestGenerateDigestCachesAgainstItsSources(t *testing.T) {
	ctx := context.Background()
	fake := &fakeProvider{
		Text: "Atlas is a map of abandoned projects.",
		// The serving model differs from the route's, which is the case
		// Usage.Model exists for: "opus" is an alias the CLI resolves.
		Usage: provider.Usage{InputTokens: 100, OutputTokens: 50, CacheWriteTokens: 90, Model: "served-by-this-model"},
	}
	h := newHarness(t, fake)
	h.seedProject("atlas", "Atlas", "", "")

	src := &project.DigestSource{
		Slug: "atlas", Name: "Atlas", Path: "/tmp/atlas",
		Doc: &contextfs.ProjectDoc{}, SourceHash: "hash-of-sources",
	}

	results, err := h.RefreshDigests(ctx, &Context{}, []*project.DigestSource{src}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err != nil {
		t.Fatalf("RefreshDigests: %v", results[0].Err)
	}
	if fake.Calls() != 1 {
		t.Fatalf("made %d model calls, want 1", fake.Calls())
	}

	stored, err := h.Store.GetDigest(ctx, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if stored.SourceHash != "hash-of-sources" {
		t.Errorf("digest cached against %q, want the source hash it was generated from", stored.SourceHash)
	}
	if stored.Body != fake.Text {
		t.Errorf("digest body = %q", stored.Body)
	}
	// Usage.Model wins over the route's model: a digest records what served
	// it, not what was asked for.
	if stored.Model != "served-by-this-model" {
		t.Errorf("digest model = %q, want the serving model from Usage", stored.Model)
	}

	// The cache key is what makes a warm sync free.
	fresh, _, err := project.CheckFreshness(ctx, h.Store, "atlas", "hash-of-sources")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.NeedsRegeneration() {
		t.Error("a digest generated from these exact sources was still reported as stale")
	}
	moved, _, err := project.CheckFreshness(ctx, h.Store, "atlas", "a-different-hash")
	if err != nil {
		t.Fatal(err)
	}
	if !moved.NeedsRegeneration() {
		t.Error("a changed source hash was not reported as stale")
	}
}

// TestRefreshDigestsBoundsConcurrency checks that the worker limit is real.
// DESIGN.md §8: a claude-cli route is one subprocess per call, so an
// unbounded fan-out starts one `claude` per project.
func TestRefreshDigestsBoundsConcurrency(t *testing.T) {
	ctx := context.Background()
	fake := &fakeProvider{Text: "a digest", Block: make(chan struct{})}
	h := newHarness(t, fake)

	var srcs []*project.DigestSource
	for _, slug := range []string{"a", "b", "c", "d", "e", "f"} {
		h.seedProject(slug, slug, "", "")
		srcs = append(srcs, &project.DigestSource{
			Slug: slug, Name: slug, Doc: &contextfs.ProjectDoc{}, SourceHash: "h-" + slug,
		})
	}

	done := make(chan []DigestResult, 1)
	go func() {
		results, err := h.RefreshDigests(ctx, &Context{}, srcs, 2)
		if err != nil {
			t.Error(err)
		}
		done <- results
	}()

	// Let the pool fill, then release everything.
	waitFor(t, func() bool { return fake.Calls() >= 2 })
	close(fake.Block)
	results := <-done

	if fake.Peak() > 2 {
		t.Errorf("%d calls were in flight at once, want at most 2", fake.Peak())
	}
	if len(results) != len(srcs) {
		t.Fatalf("got %d results, want %d", len(results), len(srcs))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("result %d: %v", i, r.Err)
		}
		if r.Slug != srcs[i].Slug {
			t.Errorf("result %d is for %s, want %s (results must keep input order)", i, r.Slug, srcs[i].Slug)
		}
	}
}

// TestRefreshDigestsIsolatesFailures: one project's failure must not cost the
// rest of the portfolio, and must not overwrite that project's cached digest
// with anything.
func TestRefreshDigestsIsolatesFailures(t *testing.T) {
	ctx := context.Background()
	fake := &failingProvider{failOn: "Bad"}
	h := newHarness(t, fake)
	h.seedProject("good", "Good", "", "")
	h.seedProject("bad", "Bad", "an older digest", "old-hash")

	srcs := []*project.DigestSource{
		{Slug: "good", Name: "Good", Doc: &contextfs.ProjectDoc{}, SourceHash: "h-good"},
		{Slug: "bad", Name: "Bad", Doc: &contextfs.ProjectDoc{}, SourceHash: "h-bad"},
	}
	results, err := h.RefreshDigests(ctx, &Context{}, srcs, 2)
	if err != nil {
		t.Fatalf("one failure aborted the whole refresh: %v", err)
	}
	if results[0].Err != nil {
		t.Errorf("the healthy project failed too: %v", results[0].Err)
	}
	if results[1].Err == nil {
		t.Error("the failing project was reported as successful")
	}

	if _, err := h.Store.GetDigest(ctx, "good"); err != nil {
		t.Errorf("the healthy project's digest was not stored: %v", err)
	}
	stale, err := h.Store.GetDigest(ctx, "bad")
	if err != nil {
		t.Fatal(err)
	}
	if stale.Body != "an older digest" || stale.SourceHash != "old-hash" {
		t.Errorf("a failed generation overwrote the cached digest: %+v", stale)
	}
}

// TestReviewDeduplicatesAndStamps covers the three things `apex review` must
// get right in the database: it does not re-propose what is already open, it
// does deduplicate within one response, and every row it writes carries the
// context hash it was generated from.
func TestReviewDeduplicatesAndStamps(t *testing.T) {
	ctx := context.Background()
	fake := &fakeProvider{JSON: `{"items":[
		{"project":"scholarrag","title":"Add table-aware chunking","body":"b1","rationale":"r1","effort":"medium"},
		{"project":"scholarrag","title":"add table aware chunking.","body":"b2","rationale":"r2","effort":"small"},
		{"project":"ScholarRAG","title":"Add citation spans","body":"b3","rationale":"r3","effort":"large"},
		{"project":"nosuchproject","title":"Something else","body":"b4","rationale":"r4","effort":"small"}
	]}`}
	h := newHarness(t, fake)
	h.seedProject("scholarrag", "ScholarRAG", "Retrieval over papers.", "hash-a")

	// Already open, in the exact form the model is about to repeat.
	if err := h.Store.InsertActionItem(ctx, store.ActionItem{
		ID: "AI-001", ProjectSlug: "scholarrag", Title: "Add table-aware chunking",
		Status: store.ItemProposed, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}); err != nil {
		t.Fatal(err)
	}

	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.Review(ctx, actx)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Inserted) != 1 {
		t.Fatalf("inserted %d items, want 1 (two duplicates and one unknown project): %+v",
			len(result.Inserted), result.Inserted)
	}
	got := result.Inserted[0]
	if got.Title != "Add citation spans" {
		t.Errorf("inserted the wrong item: %q", got.Title)
	}
	if got.ProjectSlug != "scholarrag" {
		t.Errorf("project resolved to %q; the model wrote the display name", got.ProjectSlug)
	}
	if got.Status != store.ItemProposed {
		t.Errorf("status = %q, want proposed", got.Status)
	}
	if got.ContextHash != actx.Hash() {
		t.Errorf("context hash = %q, want %q", got.ContextHash, actx.Hash())
	}
	if len(result.Duplicates) != 2 {
		t.Errorf("reported %d duplicates, want 2: %v", len(result.Duplicates), result.Duplicates)
	}
	if len(result.UnknownProjects) != 1 {
		t.Errorf("reported %v as unknown projects, want one", result.UnknownProjects)
	}

	// The table holds two rows and no more: the duplicate did not land.
	all, err := h.Store.ListActionItems(ctx, store.ActionItemFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("action_items holds %d rows, want 2", len(all))
	}
}

// TestReviewDismissedItemsMayReturn: a dismissed item is deliberately not in
// the dedup index. The user said no once; if the portfolio has moved and the
// advisor raises it again, that is the tool working, not a bug.
func TestReviewDismissedItemsMayReturn(t *testing.T) {
	ctx := context.Background()
	fake := &fakeProvider{JSON: `{"items":[
		{"project":"atlas","title":"Write a README","body":"b","rationale":"r","effort":"small"}
	]}`}
	h := newHarness(t, fake)
	h.seedProject("atlas", "Atlas", "A map.", "hash-b")
	if err := h.Store.InsertActionItem(ctx, store.ActionItem{
		ID: "AI-001", ProjectSlug: "atlas", Title: "Write a README",
		Status: store.ItemDismissed, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}); err != nil {
		t.Fatal(err)
	}

	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.Review(ctx, actx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Inserted) != 1 {
		t.Fatalf("a dismissed item blocked its own re-proposal: %+v", result)
	}
}

// TestIdeasDeduplicates mirrors the review check for the ideas backlog, where
// there is no project to key on.
func TestIdeasDeduplicates(t *testing.T) {
	ctx := context.Background()
	fake := &fakeProvider{JSON: `{"ideas":[
		{"title":"A TUI for git worktrees","pitch":"p1","rationale":"r1"},
		{"title":"a tui for git worktrees!","pitch":"p2","rationale":"r2"},
		{"title":"A local search index","pitch":"p3","rationale":"r3"}
	]}`}
	h := newHarness(t, fake)
	h.seedProject("atlas", "Atlas", "A map.", "hash-b")

	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.Ideas(ctx, actx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Inserted) != 2 {
		t.Fatalf("inserted %d ideas, want 2: %+v", len(result.Inserted), result.Inserted)
	}
	if len(result.Duplicates) != 1 {
		t.Errorf("reported %v as duplicates, want one", result.Duplicates)
	}
	if result.Inserted[0].ID != "IDEA-001" || result.Inserted[1].ID != "IDEA-002" {
		t.Errorf("ids are not sequential: %s, %s", result.Inserted[0].ID, result.Inserted[1].ID)
	}
}

// TestSchemasAreStrictCompatible checks the property DESIGN.md §8 requires of
// every schema: object root, every property required, additionalProperties
// false. A schema only one vendor accepts turns a config edit into a runtime
// 400, and on an all-subscription machine that arrives as a failed subprocess.
func TestSchemasAreStrictCompatible(t *testing.T) {
	for _, tc := range []struct{ name, schema string }{
		{"action items", actionItemsSchema},
		{"ideas", ideasSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal([]byte(tc.schema), &root); err != nil {
				t.Fatalf("not valid JSON: %v", err)
			}
			if root["type"] != "object" {
				t.Errorf("root type = %v, want object", root["type"])
			}
			checkStrict(t, "$", root)
		})
	}
}

func checkStrict(t *testing.T, path string, node map[string]any) {
	t.Helper()
	if node["type"] == "object" {
		props, _ := node["properties"].(map[string]any)
		if len(props) == 0 {
			t.Errorf("%s: object with no properties", path)
		}
		if extra, ok := node["additionalProperties"]; !ok || extra != false {
			t.Errorf("%s: additionalProperties must be false, got %v", path, extra)
		}
		required := map[string]bool{}
		for _, r := range asSlice(node["required"]) {
			required[fmt.Sprint(r)] = true
		}
		for name, child := range props {
			if !required[name] {
				t.Errorf("%s.%s is not in `required`; strict mode requires every property", path, name)
			}
			if m, ok := child.(map[string]any); ok {
				checkStrict(t, path+"."+name, m)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		checkStrict(t, path+"[]", items)
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
