package tui

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/store"
)

const ideasReply = "Three that fit you.\n\n```" + advisor.IdeaFence + "\n" +
	`{"ideas":[` +
	`{"title":"Worktree Switcher","pitch":"A TUI over git worktrees.","rationale":"You write Go CLIs."},` +
	`{"title":"Paper Shelf","pitch":"A reading queue for papers.","rationale":"ScholarRAG needs input."},` +
	`{"title":"Dotfile Doctor","pitch":"Lints your dotfiles.","rationale":"You like small tools."}` +
	"]}\n```"

const exploreJSON = `{"details":"Paper Shelf keeps a **queue** of papers.","questions":["Terminal or web?","Where do papers come from?"]}`

const planJSON = `{"name":"Paper Shelf","pitch":"A terminal reading queue.","rationale":"r","stack":"go",` +
	`"item":{"project":"Paper Shelf","title":"Build the queue command","body":"Add, list, pop.","rationale":"First step.","effort":"small"}}`

// fakeStarter stands in for cmd/apex's IdeaStarter: it registers the project
// and inserts the item in the real store, so the items view has something to
// reload.
type fakeStarter struct {
	plans []*advisor.IdeaPlan
	err   error
}

func (s *fakeStarter) start(st *store.Store) IdeaStarter {
	return func(ctx context.Context, w io.Writer, plan *advisor.IdeaPlan) (IdeaStarted, error) {
		s.plans = append(s.plans, plan)
		if s.err != nil {
			return IdeaStarted{}, s.err
		}
		io.WriteString(w, "created\n") //nolint:errcheck
		now := time.Now()
		if err := st.UpsertProject(ctx, store.Project{
			Slug: "paper-shelf", Name: plan.Name, Path: "/tmp/paper-shelf", Status: "active", RegisteredAt: now,
		}); err != nil {
			return IdeaStarted{}, err
		}
		item := store.ActionItem{
			ID: "AI-001", ProjectSlug: "paper-shelf", Title: plan.Item.Title, Body: plan.Item.Body,
			Status: store.ItemAccepted, CreatedAt: now, UpdatedAt: now,
		}
		if err := st.InsertActionItem(ctx, item); err != nil {
			return IdeaStarted{}, err
		}
		return IdeaStarted{ProjectName: plan.Name, Dir: "/tmp/paper-shelf", Item: item}, nil
	}
}

// ideaModel is a chatModel wired to a fakeStarter.
func ideaModel(t *testing.T, fake *chatFake) (*Model, *fakeStarter) {
	t.Helper()
	m := chatModel(t, fake)
	starter := &fakeStarter{}
	m.opts.StartIdea = starter.start(m.opts.Store)
	return m, starter
}

// TestChatIdeasBecomeAnActionItem is the feature end to end: ask for project
// ideas, pick one from the list, read more about it, answer the questionnaire,
// say yes — and the project's first action item is in the items view.
func TestChatIdeasBecomeAnActionItem(t *testing.T) {
	fake := &chatFake{Text: ideasReply, Queue: []string{exploreJSON, planJSON}}
	m, starter := ideaModel(t, fake)

	typeAndSend(t, m, "Suggest some projects I could start.")
	f := m.chat.idea
	if f == nil || f.stage != ideaPick || len(f.ideas) != 3 {
		t.Fatalf("the ideas did not open a list to pick from: %+v", f)
	}
	out := m.chat.outVP.View()
	if !strings.Contains(out, "Paper Shelf") || strings.Contains(out, `"ideas"`) {
		t.Errorf("the list is not shown, or is shown as JSON:\n%s", out)
	}
	if m.chat.ta.Focused() {
		t.Error("the input kept focus under an open list")
	}

	// A stray key is swallowed; down and enter pick the second.
	key(m, "x")
	if m.chat.idea.stage != ideaPick || !strings.Contains(m.status, "pick") {
		t.Fatalf("a stray key: stage = %v, status = %q", m.chat.idea.stage, m.status)
	}
	key(m, "down")
	pump(t, m, key(m, "enter"))

	if f.chosen != 1 || f.stage != ideaAsking {
		t.Fatalf("chosen = %d, stage = %v; want the second idea and the questionnaire", f.chosen, f.stage)
	}
	out = m.chat.outVP.View()
	for _, want := range []string{"queue", "Question 1 of 2", "Terminal or web?"} {
		if !strings.Contains(out, want) {
			t.Errorf("the detail and the first question are not on screen (%q):\n%s", want, out)
		}
	}
	if !m.chat.ta.Focused() {
		t.Error("the questionnaire is answered in the input, which does not have focus")
	}

	typeAndSend(t, m, "Terminal, please.")
	if !strings.Contains(m.chat.outVP.View(), "Question 2 of 2") {
		t.Fatalf("the second question was not asked:\n%s", m.chat.outVP.View())
	}
	typeAndSend(t, m, "") // skipped
	if f.stage != ideaConfirm || m.chat.ta.Focused() {
		t.Fatalf("stage = %v, input focused = %v; want the y/n with the keyboard", f.stage, m.chat.ta.Focused())
	}
	if !strings.Contains(m.chat.outVP.View(), "Make “Paper Shelf” an action item?") {
		t.Errorf("the final question is not asked:\n%s", m.chat.outVP.View())
	}
	if len(starter.plans) != 0 || len(storedItems(t, m)) != 0 {
		t.Fatal("something was created before the user said yes")
	}
	if streams, _ := fake.counts(); streams != 1 {
		t.Errorf("the questionnaire answers were sent as chat messages: %d streams", streams)
	}

	pump(t, m, key(m, "y"))
	if len(starter.plans) != 1 || starter.plans[0].Name != "Paper Shelf" {
		t.Fatalf("the project was not created from the plan: %+v", starter.plans)
	}
	planReq := fake.requests[len(fake.requests)-1]
	last := planReq.Messages[len(planReq.Messages)-1].Content
	for _, want := range []string{"Paper Shelf", "Terminal, please.", "skipped"} {
		if !strings.Contains(last, want) {
			t.Errorf("the plan was not written from the answers (%q):\n%s", want, last)
		}
	}
	if m.chat.idea != nil {
		t.Error("the flow is still open after the item was made")
	}
	if _, ok := m.items.find("AI-001"); !ok {
		t.Error("the items view was not reloaded, so the new item cannot be dispatched from it")
	}
	if !strings.Contains(m.status, "AI-001") || !strings.Contains(m.chat.outVP.View(), "AI-001") {
		t.Errorf("the chat does not say what was made: status = %q", m.status)
	}
	if !m.chat.ta.Focused() {
		t.Error("the input did not get focus back")
	}
	if strings.Contains(m.chat.outVP.View(), "creating the project…") {
		t.Error("the working line outlived the work")
	}

	// The conversation remembers all of it.
	var history strings.Builder
	for _, turn := range m.chat.sess.History {
		history.WriteString(turn.Content + "\n")
	}
	for _, want := range []string{"pursue the idea “Paper Shelf”", "Terminal or web?", "Terminal, please.", "AI-001"} {
		if !strings.Contains(history.String(), want) {
			t.Errorf("the conversation lost %q:\n%s", want, history.String())
		}
	}
}

func TestChatIdeaCanBeLeftAsAnIdea(t *testing.T) {
	fake := &chatFake{Text: ideasReply, Queue: []string{`{"details":"More.","questions":[]}`}}
	m, starter := ideaModel(t, fake)
	typeAndSend(t, m, "Ideas?")
	pump(t, m, key(m, "1"))

	// No questions goes straight to the y/n.
	if m.chat.idea == nil || m.chat.idea.stage != ideaConfirm {
		t.Fatalf("with no questions the flow should ask y/n: %+v", m.chat.idea)
	}
	key(m, "n")
	if m.chat.idea != nil || len(starter.plans) != 0 || len(storedItems(t, m)) != 0 {
		t.Error("n still made something")
	}
	if !strings.Contains(m.chat.outVP.View(), "nothing was recorded") {
		t.Errorf("n does not say what happened:\n%s", m.chat.outVP.View())
	}
}

// TestChatIdeaFailureKeepsTheAnswers: a project that cannot be created — a
// directory already there — says why, and y tries again without asking the
// questionnaire twice.
func TestChatIdeaFailureKeepsTheAnswers(t *testing.T) {
	fake := &chatFake{Text: ideasReply, Queue: []string{`{"details":"More.","questions":[]}`, planJSON, planJSON}}
	m, starter := ideaModel(t, fake)
	starter.err = &testErr{"~/Development/Paper Shelf already exists and is not empty"}
	typeAndSend(t, m, "Ideas?")
	pump(t, m, key(m, "2"))
	pump(t, m, key(m, "y"))

	if m.chat.idea == nil || m.chat.idea.stage != ideaConfirm {
		t.Fatalf("a failed creation closed the flow: %+v", m.chat.idea)
	}
	if !strings.Contains(m.chat.outVP.View(), "already exists") {
		t.Errorf("the failure is not reported:\n%s", m.chat.outVP.View())
	}
	starter.err = nil
	pump(t, m, key(m, "y"))
	if len(storedItems(t, m)) != 1 {
		t.Error("y did not try again")
	}
}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

// TestSlashIdeasOpensTheList: `/ideas` works on an empty conversation, and
// never reaches the model as a chat message.
func TestSlashIdeasOpensTheList(t *testing.T) {
	fake := &chatFake{JSON: `{"ideas":[{"title":"Paper Shelf","pitch":"p","rationale":"r"}]}`}
	m, _ := ideaModel(t, fake)
	typeAndSend(t, m, "/ideas")
	if m.chat.idea == nil || m.chat.idea.stage != ideaPick || len(m.chat.idea.ideas) != 1 {
		t.Fatalf("/ideas did not open a list: %+v", m.chat.idea)
	}
	if streams, structured := fake.counts(); streams != 0 || structured != 1 {
		t.Errorf("streams = %d, structured = %d", streams, structured)
	}
	key(m, "esc")
	if m.chat.idea != nil || !m.chat.ta.Focused() {
		t.Error("esc did not close the list and return the input")
	}
}

// TestIdeaListFitsTheFrame: the list is drawn by hand, so it is measured.
func TestIdeaListFitsTheFrame(t *testing.T) {
	long := strings.Repeat("An unreasonably long project name ", 4)
	for _, size := range []struct{ w, h int }{{80, 24}, {100, 30}, {60, 20}, {200, 50}, {40, 10}} {
		m, _ := ideaModel(t, &chatFake{})
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		m.openIdeas([]advisor.ProposedIdea{
			{Title: long, Pitch: strings.Repeat("/a/path/without/spaces/", 12)},
			{Title: "Short", Pitch: strings.Repeat("A pitch that goes on. ", 20)},
		})
		m.rerenderChat()
		assertFrameFits(t, m, size.w, size.h, "Chat, ideas open")
		m.setView(viewItems)
		m.setView(viewChat)
		if m.chat.ta.Focused() {
			t.Errorf("%dx%d: tabbing back gave the input focus under an open list", size.w, size.h)
		}
	}
}
