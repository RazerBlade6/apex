package advisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/store"
)

// seedStartedProject registers a project the way `apex start` leaves one: a
// row and a PROJECT.md on disk, and no digest.
func (h *harness) seedStartedProject(slug, name, doc string) string {
	h.T.Helper()
	dir := filepath.Join(h.Root, "projects", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.T.Fatal(err)
	}
	if doc != "" {
		if err := os.WriteFile(contextfs.ProjectDocPath(dir), []byte(doc), 0o644); err != nil {
			h.T.Fatal(err)
		}
	}
	if err := h.Store.UpsertProject(context.Background(), store.Project{
		Slug: slug, Name: name, Path: dir, Status: "active", RegisteredAt: fixedNow,
	}); err != nil {
		h.T.Fatal(err)
	}
	return dir
}

// TestUndigestedProjectsAreInTheContext: a project `apex start` has just
// created has a row and a PROJECT.md and no digest until the next sync. It must
// not be invisible to the advisor in the meantime — the user has just asked
// Apex to create it, and the next thing they ask is likely to be about it.
func TestUndigestedProjectsAreInTheContext(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &fakeProvider{})
	h.seedProject("atlas", "Atlas", "A map of abandoned projects.", "hash-a")

	before, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}

	h.seedStartedProject("worktree-switcher", "Worktree Switcher",
		"---\nname: Worktree Switcher\nstatus: active\n---\n\n## What it is\n\nA TUI over git worktrees.\n")
	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(actx.Undigested) != 1 || actx.Undigested[0].Project.Slug != "worktree-switcher" {
		t.Fatalf("Undigested = %+v, want the started project alone", actx.Undigested)
	}
	system := actx.Prompt("go").Build("m", "", 10).System
	for _, want := range []string{"Worktree Switcher (worktree-switcher)", "not digested yet", "A TUI over git worktrees."} {
		if !strings.Contains(system, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, system)
		}
	}
	if strings.Index(system, "A map of abandoned") > strings.Index(system, "A TUI over git worktrees") {
		t.Error("an undigested project precedes the digests; a first digest would reorder the prefix")
	}

	if slug, ok := actx.ResolveProject("Worktree Switcher"); !ok || slug != "worktree-switcher" {
		t.Errorf("ResolveProject = %q, %v; an undigested project cannot be given action items", slug, ok)
	}
	if actx.Hash() == before.Hash() {
		t.Error("adding a project to the context did not change its hash")
	}
	if only := actx.Filter("atlas"); len(only.Undigested) != 0 {
		t.Errorf("Filter kept another project's undigested entry: %+v", only.Undigested)
	}

	// Once it has a digest it is described by that instead, and only once.
	h.seedProject("worktree-switcher", "Worktree Switcher", "A digest of the switcher.", "hash-w")
	synced, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(synced.Undigested) != 0 {
		t.Errorf("a digested project is still listed as undigested: %+v", synced.Undigested)
	}
}

// TestFullySyncedContextHashIsUnchanged: undigested projects join the hash
// only when there are any, so every item already stamped against a fully
// synced portfolio still matches it.
func TestFullySyncedContextHashIsUnchanged(t *testing.T) {
	h := newHarness(t, &fakeProvider{})
	h.seedProject("atlas", "Atlas", "A map.", "hash-a")
	actx, err := h.LoadContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	withEmpty := &Context{Identity: actx.Identity, Digests: actx.Digests, Projects: actx.Projects, Undigested: []UndigestedProject{}}
	if actx.Hash() != withEmpty.Hash() {
		t.Error("an empty undigested list changed the hash")
	}
}

func TestExtractProposals(t *testing.T) {
	block := "```" + ProposalFence + "\n"
	tests := []struct {
		name      string
		reply     string
		wantProse string
		wantItems []string
		wantErr   bool
	}{
		{
			name:      "no block",
			reply:     "ScholarRAG is closest to done.",
			wantProse: "ScholarRAG is closest to done.",
		},
		{
			name: "wrapped",
			reply: "Two things would unblock it.\n\n" + block +
				`{"items":[{"project":"atlas","title":"Write a README","body":"b","rationale":"r","effort":"small"},` +
				`{"project":"atlas","title":"Add a CLI","body":"b","rationale":"r","effort":"large"}]}` + "\n```\n",
			wantProse: "Two things would unblock it.",
			wantItems: []string{"Write a README", "Add a CLI"},
		},
		{
			name:      "bare array",
			reply:     "One.\n" + block + `[{"project":"atlas","title":"Write a README"}]` + "\n```",
			wantProse: "One.",
			wantItems: []string{"Write a README"},
		},
		{
			name:      "prose after the block survives",
			reply:     "Before.\n" + block + `{"items":[]}` + "\n```\nAfter.",
			wantProse: "Before.\nAfter.",
		},
		{
			name:      "an ordinary code block is prose",
			reply:     "Run this:\n```sh\napex sync\n```",
			wantProse: "Run this:\n```sh\napex sync\n```",
		},
		{
			name:      "cut off",
			reply:     "Here.\n" + block + `{"items":[{"project":"atlas"`,
			wantProse: "Here.",
			wantErr:   true,
		},
		{
			name:      "not json",
			reply:     "Here.\n" + block + "- write a README\n```",
			wantProse: "Here.",
			wantErr:   true,
		},
		{
			name:      "untitled items are dropped",
			reply:     block + `{"items":[{"project":"atlas","title":"  "},{"project":"atlas","title":"Kept"}]}` + "\n```",
			wantItems: []string{"Kept"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prose, items, err := ExtractProposals(tt.reply)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if prose != tt.wantProse {
				t.Errorf("prose = %q, want %q", prose, tt.wantProse)
			}
			if strings.Contains(prose, ProposalFence) {
				t.Errorf("the block leaked into the prose: %q", prose)
			}
			var titles []string
			for _, it := range items {
				titles = append(titles, it.Title)
			}
			if strings.Join(titles, "|") != strings.Join(tt.wantItems, "|") {
				t.Errorf("items = %q, want %q", titles, tt.wantItems)
			}
		})
	}

	// A model that proposes a backlog gets the first few.
	var many strings.Builder
	many.WriteString(block + `{"items":[`)
	for i := 0; i < 9; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`{"project":"atlas","title":"Item ` + string(rune('A'+i)) + `"}`)
	}
	many.WriteString("]}\n```")
	if _, items, _ := ExtractProposals(many.String()); len(items) != maxChatProposals {
		t.Errorf("kept %d proposals, want %d", len(items), maxChatProposals)
	}
}

// TestVisibleReplyHidesTheBlockWhileItStreams: JSON arriving token by token is
// not something to read, and a half-typed fence flashing on screen is worse.
func TestVisibleReplyHidesTheBlockWhileItStreams(t *testing.T) {
	tests := []struct {
		partial      string
		wantVisible  string
		wantDrafting string
	}{
		{"Plain prose.", "Plain prose.", ""},
		{"Two items.\n```apex-items\n{\"items\":[", "Two items.", "action items"},
		{"Three ideas.\n```apex-ideas\n{\"ideas\":[", "Three ideas.", "project ideas"},
		{"Two items.\n```ape", "Two items.", ""},
		{"Two items.\n```", "Two items.", ""},
		{"Run:\n```sh", "Run:\n```sh", ""},
	}
	for _, tt := range tests {
		visible, drafting := VisibleReply(tt.partial)
		if visible != tt.wantVisible || drafting != tt.wantDrafting {
			t.Errorf("VisibleReply(%q) = %q, %q; want %q, %q",
				tt.partial, visible, drafting, tt.wantVisible, tt.wantDrafting)
		}
	}
}

// TestRecordItemsLandsLikeAReview: an item approved in the chat is the same
// row a review would have written, and goes through the same deduplication.
func TestRecordItemsLandsLikeAReview(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, &fakeProvider{})
	h.seedProject("atlas", "Atlas", "A map.", "hash-a")
	h.seedStartedProject("worktree-switcher", "Worktree Switcher", "")
	if err := h.Store.InsertActionItem(ctx, store.ActionItem{
		ID: "AI-001", ProjectSlug: "atlas", Title: "Write a README",
		Status: store.ItemAccepted, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}); err != nil {
		t.Fatal(err)
	}
	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}

	result, err := h.RecordItems(ctx, actx, []ProposedItem{
		{Project: "atlas", Title: "write a README.", Body: "b"},
		{Project: "worktree-switcher", Title: "List worktrees", Body: "b", Rationale: "r", Effort: "SMALL"},
		{Project: "nowhere", Title: "Something"},
	}, "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Inserted) != 1 {
		t.Fatalf("inserted %+v, want only the item on the new project", result.Inserted)
	}
	got := result.Inserted[0]
	switch {
	case got.ProjectSlug != "worktree-switcher":
		t.Errorf("project = %q", got.ProjectSlug)
	case got.Status != store.ItemProposed:
		t.Errorf("status = %q, want proposed", got.Status)
	case got.GeneratedBy != "claude-sonnet-5":
		t.Errorf("generated_by = %q", got.GeneratedBy)
	case got.ContextHash != actx.Hash():
		t.Errorf("context hash = %q, want %q", got.ContextHash, actx.Hash())
	case got.Effort != "small":
		t.Errorf("effort = %q, want it normalised", got.Effort)
	}
	if len(result.Duplicates) != 1 || len(result.UnknownProjects) != 1 {
		t.Errorf("duplicates = %v, unknown = %v; want one of each", result.Duplicates, result.UnknownProjects)
	}
}

// TestChatKnowsTheListAndTheProtocol: the chat can only avoid re-proposing
// what is already open if it is shown the list, and it has to be shown it
// fresh each turn, because the user approves items mid-conversation.
func TestChatKnowsTheListAndTheProtocol(t *testing.T) {
	ctx := context.Background()
	fake := &fakeProvider{Text: "ok"}
	h := newHarness(t, fake)
	h.seedProject("atlas", "Atlas", "A map.", "hash-a")
	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chat := h.NewChat(actx)
	drain(t, chat, ctx, "What next?")

	if err := h.Store.InsertActionItem(ctx, store.ActionItem{
		ID: "AI-001", ProjectSlug: "atlas", Title: "OPEN-ITEM-MARKER",
		Status: store.ItemProposed, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}); err != nil {
		t.Fatal(err)
	}
	drain(t, chat, ctx, "And after that?")

	first, second := fake.Requests[0].System, fake.Requests[1].System
	if !strings.Contains(first, "```"+ProposalFence) {
		t.Error("the chat is not told how to propose action items")
	}
	if strings.Contains(first, "OPEN-ITEM-MARKER") {
		t.Fatal("fixture: the item existed before it was inserted")
	}
	if !strings.Contains(second, "OPEN-ITEM-MARKER") {
		t.Error("an item approved mid-conversation is not on the list the next turn sees")
	}
	if strings.Index(second, "A map.") > strings.Index(second, "OPEN-ITEM-MARKER") {
		t.Error("the open items precede the portfolio")
	}
}

// TestProposeItemsRecordsNothing: `/items` is a question about the
// conversation, so it neither writes items nor becomes a turn in it.
func TestProposeItemsRecordsNothing(t *testing.T) {
	ctx := context.Background()
	fake := &fakeProvider{Text: "Write the README first.", JSON: `{"items":[
		{"project":"atlas","title":"Write a README","body":"b","rationale":"r","effort":"small"}
	]}`}
	h := newHarness(t, fake)
	h.seedProject("atlas", "Atlas", "A map.", "hash-a")
	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chat := h.NewChat(actx)

	if _, err := chat.ProposeItems(ctx); err == nil {
		t.Error("an empty conversation was turned into action items")
	}
	drain(t, chat, ctx, "What should I do on Atlas?")
	turns := len(chat.History)

	props, err := chat.ProposeItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(props.Items) != 1 || props.Items[0].Title != "Write a README" {
		t.Fatalf("proposals = %+v", props.Items)
	}
	if len(chat.History) != turns {
		t.Error("the request for proposals became a turn in the conversation")
	}
	items, err := h.Store.ListActionItems(ctx, store.ActionItemFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("proposing recorded %d item(s) before anyone approved them", len(items))
	}
	req := fake.Requests[len(fake.Requests)-1]
	if last := req.Messages[len(req.Messages)-1].Content; !strings.Contains(last, "Turn the conversation") {
		t.Errorf("the last message is not the proposal instruction: %q", last)
	}
}
