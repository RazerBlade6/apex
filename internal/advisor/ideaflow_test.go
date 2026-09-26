package advisor

import (
	"context"
	"strings"
	"testing"

	"github.com/RazerBlade6/apex/internal/store"
)

func TestParseReplyFindsIdeas(t *testing.T) {
	reply := "Three that fit you.\n\n```" + IdeaFence + "\n" +
		`{"ideas":[{"title":"Worktree Switcher","pitch":"A TUI.","rationale":"You write Go CLIs."},` +
		`{"title":"  ","pitch":"untitled","rationale":"r"}]}` + "\n```"
	r := ParseReply(reply)
	if r.Prose != "Three that fit you." {
		t.Errorf("prose = %q", r.Prose)
	}
	if r.IdeasErr != nil || len(r.Ideas) != 1 || r.Ideas[0].Title != "Worktree Switcher" {
		t.Fatalf("ideas = %+v, err = %v", r.Ideas, r.IdeasErr)
	}
	if len(r.Items) != 0 || r.ItemsErr != nil {
		t.Errorf("an ideas block was read as items: %+v, %v", r.Items, r.ItemsErr)
	}

	cut := ParseReply("Here.\n```" + IdeaFence + "\n" + `{"ideas":[`)
	if cut.IdeasErr == nil || cut.Prose != "Here." {
		t.Errorf("a cut-off ideas block: prose = %q, err = %v", cut.Prose, cut.IdeasErr)
	}
}

// TestIdeaFlowCallsRecordNothing: exploring an idea and planning it are
// questions, not writes. The idea, the project and the item are created by
// the caller once the user says yes; until then only the conversation grows,
// and only through Note.
func TestIdeaFlowCallsRecordNothing(t *testing.T) {
	ctx := context.Background()
	fake := &fakeProvider{}
	h := newHarness(t, fake)
	h.seedProject("atlas", "Atlas", "A map.", "hash-a")
	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chat := h.NewChat(actx)
	idea := ProposedIdea{Title: "Worktree Switcher", Pitch: "A TUI over git worktrees.", Rationale: "Go CLIs."}

	fake.JSON = `{"details":"It lists worktrees.","questions":["Terminal or GUI?"," ","Which OS?"]}`
	brief, err := chat.ExploreIdea(ctx, idea)
	if err != nil {
		t.Fatal(err)
	}
	if brief.Details != "It lists worktrees." || len(brief.Questions) != 2 {
		t.Errorf("brief = %+v; blank questions should be dropped", brief)
	}
	if last := fake.Requests[0].Messages[len(fake.Requests[0].Messages)-1].Content; !strings.Contains(last, "A TUI over git worktrees.") {
		t.Errorf("the explore request does not carry the idea: %q", last)
	}

	if err := chat.Note(ctx, store.RoleUser, "I'd like to pursue it."); err != nil {
		t.Fatal(err)
	}
	if len(chat.History) != 1 || chat.SessionID == "" {
		t.Errorf("Note did not add a recorded turn: %+v", chat.History)
	}

	fake.JSON = `{"name":"Worktree Switcher","pitch":"A terminal TUI.","rationale":"r","stack":"Go",` +
		`"item":{"project":"x","title":"Build the list view","body":"b","rationale":"r","effort":"SMALL"}}`
	plan, err := chat.PlanIdea(ctx, idea, []Answer{{Question: "Terminal or GUI?", Answer: "terminal"}, {Question: "Which OS?"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Name != "Worktree Switcher" || plan.Stack != "go" || plan.Item.Effort != "small" ||
		plan.Item.Project != "Worktree Switcher" || plan.Source.Title != idea.Title {
		t.Errorf("plan = %+v", plan)
	}
	last := fake.Requests[1].Messages[len(fake.Requests[1].Messages)-1].Content
	for _, want := range []string{"terminal", "skipped"} {
		if !strings.Contains(last, want) {
			t.Errorf("the plan request does not carry %q:\n%s", want, last)
		}
	}

	items, _ := h.Store.ListActionItems(ctx, store.ActionItemFilter{})
	ideas, _ := h.Store.ListIdeas(ctx, "")
	if len(items) != 0 || len(ideas) != 0 {
		t.Errorf("the idea flow's calls wrote %d item(s) and %d idea(s)", len(items), len(ideas))
	}
}

// TestChatKnowsTheIdeasBacklog: the chat is told not to propose an idea
// already on the backlog, so it has to be shown the backlog.
func TestChatKnowsTheIdeasBacklog(t *testing.T) {
	ctx := context.Background()
	fake := &fakeProvider{Text: "ok"}
	h := newHarness(t, fake)
	if err := h.Store.InsertIdea(ctx, store.Idea{
		ID: "IDEA-001", Title: "BACKLOG-MARKER", Status: store.IdeaProposed, CreatedAt: fixedNow,
	}); err != nil {
		t.Fatal(err)
	}
	actx, err := h.LoadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, h.NewChat(actx), ctx, "Any project ideas?")
	system := fake.Requests[0].System
	if !strings.Contains(system, "BACKLOG-MARKER") || !strings.Contains(system, "```"+IdeaFence) {
		t.Errorf("the chat is not shown the backlog or the ideas protocol:\n%s", system)
	}
}
