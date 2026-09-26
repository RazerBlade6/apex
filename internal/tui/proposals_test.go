package tui

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// chatFake answers the chat from memory: Text for a streamed reply, JSON for
// the Structured call behind `/items`.
type chatFake struct {
	mu   sync.Mutex
	Text string
	JSON string
	// Queue, when not empty, answers Structured calls in order before JSON
	// does, for a flow that makes several.
	Queue      []string
	streams    int
	structured int
	requests   []provider.Request
}

func (f *chatFake) Name() string { return "fake" }

func (f *chatFake) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	f.mu.Lock()
	f.streams++
	text := f.Text
	f.mu.Unlock()
	ch := make(chan provider.Event, 2)
	ch <- provider.Event{Type: provider.EventTextDelta, Text: text}
	ch <- provider.Event{Type: provider.EventDone, Usage: &provider.Usage{Model: "fake-model"}}
	close(ch)
	return ch, nil
}

func (f *chatFake) Structured(ctx context.Context, req provider.Request, schema json.RawMessage, out any) (provider.Usage, error) {
	f.mu.Lock()
	f.structured++
	f.requests = append(f.requests, req)
	body := f.JSON
	if len(f.Queue) > 0 {
		body, f.Queue = f.Queue[0], f.Queue[1:]
	}
	f.mu.Unlock()
	return provider.Usage{Model: "fake-model"}, json.Unmarshal([]byte(body), out)
}

func (f *chatFake) counts() (streams, structured int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams, f.structured
}

// pump runs a command and everything it leads to, the way the Bubble Tea
// runtime would, for the messages this package defines. Anything else — a
// cursor blink — is dropped rather than run, because it re-issues itself
// forever. A command that does not return promptly is abandoned for the same
// reason.
func pump(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(2 * time.Second):
		return
	}
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			pump(t, m, c)
		}
	case streamEventMsg, recordedMsg, proposalsMsg, contextReloadedMsg, contextLoadedMsg,
		itemsLoadedMsg, projectsLoadedMsg, observedMsg,
		ideasProposedMsg, ideaExploredMsg, ideaCreatedMsg, syncedMsg:
		_, next := m.Update(msg)
		pump(t, m, next)
	}
}

// chatModel is a Model with a loaded chat over one digested project, Atlas.
func chatModel(t *testing.T, fake *chatFake) *Model {
	t.Helper()
	m := newTestModel(t, fake)
	ctx := context.Background()
	if err := m.opts.Store.UpsertProject(ctx, store.Project{
		Slug: "atlas", Name: "Atlas", Path: "/tmp/atlas", RegisteredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.opts.Store.UpsertDigest(ctx, store.Digest{
		ProjectSlug: "atlas", Body: "A map of abandoned projects.", SourceHash: "h", GeneratedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	pump(t, m, m.loadContextCmd())
	if m.chat.sess == nil {
		t.Fatal("the chat context did not load")
	}
	return m
}

func typeAndSend(t *testing.T, m *Model, text string) {
	t.Helper()
	m.chat.ta.SetValue(text)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pump(t, m, cmd)
}

func key(m *Model, k string) tea.Cmd {
	var msg tea.KeyMsg
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case " ":
		msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	_, cmd := m.Update(msg)
	return cmd
}

func storedItems(t *testing.T, m *Model) []store.ActionItem {
	t.Helper()
	items, err := m.opts.Store.ListActionItems(context.Background(), store.ActionItemFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

const proposingReply = "Atlas needs a README before anything else.\n\n" +
	"```" + advisor.ProposalFence + "\n" +
	`{"items":[` +
	`{"project":"atlas","title":"Write a README","body":"Say what it is.","rationale":"Nobody can tell.","effort":"small"},` +
	`{"project":"nowhere","title":"Invent a project","body":"b","rationale":"r","effort":"small"}` +
	"]}\n```"

// TestChatProposalsWaitForApproval is the feature end to end: a reply that
// proposes action items shows them as a checklist, writes nothing until `y`,
// and then lands them in the items view, where they can be dispatched.
func TestChatProposalsWaitForApproval(t *testing.T) {
	m := chatModel(t, &chatFake{Text: proposingReply})
	typeAndSend(t, m, "What should I do next?")

	p := m.chat.proposal
	if p == nil {
		t.Fatal("the proposed items did not open a checklist")
	}
	if len(p.items) != 2 || !p.keep[0] || p.keep[1] {
		t.Fatalf("keep = %v; want the known project ticked and the unknown one not", p.keep)
	}
	if n := len(storedItems(t, m)); n != 0 {
		t.Fatalf("%d item(s) were recorded before the user approved anything", n)
	}
	out := m.chat.outVP.View()
	if !strings.Contains(out, "Write a README") || !strings.Contains(out, "needs a README") {
		t.Errorf("the reply and its checklist are not both on screen:\n%s", out)
	}
	if strings.Contains(out, `"items"`) || strings.Contains(out, advisor.ProposalFence) {
		t.Errorf("the proposal block is shown as JSON:\n%s", out)
	}
	if m.chat.ta.Focused() {
		t.Error("the input kept focus under an open checklist")
	}

	// A stray key — a reply typed without noticing the checklist — is
	// swallowed with a reminder, and enter, the send key, does not accept.
	key(m, "h")
	if !strings.Contains(m.status, "y add") || m.chat.ta.Value() != "" {
		t.Errorf("a stray key: status = %q, input = %q", m.status, m.chat.ta.Value())
	}
	key(m, "enter")
	if m.chat.proposal == nil || len(storedItems(t, m)) != 0 {
		t.Fatal("enter accepted the checklist")
	}

	// Space ticks and unticks; the unknown project cannot be ticked.
	key(m, " ")
	if p.keep[0] {
		t.Error("space did not untick the item under the cursor")
	}
	key(m, " ")
	key(m, "down")
	key(m, " ")
	if p.keep[1] {
		t.Error("an item on an unknown project was ticked")
	}

	pump(t, m, key(m, "y"))
	if m.chat.proposal != nil {
		t.Error("the checklist is still open after y")
	}
	items := storedItems(t, m)
	if len(items) != 1 || items[0].Title != "Write a README" || items[0].ProjectSlug != "atlas" ||
		items[0].Status != store.ItemProposed || items[0].GeneratedBy != "fake-model" {
		t.Fatalf("recorded %+v, want the one ticked item as proposed", items)
	}
	if !strings.Contains(m.status, items[0].ID) {
		t.Errorf("status = %q, want it to name the new item", m.status)
	}
	if _, ok := m.items.find(items[0].ID); !ok {
		t.Error("the items view was not reloaded, so the new item cannot be dispatched from it")
	}
	if !strings.Contains(m.chat.outVP.View(), items[0].ID) {
		t.Errorf("the answered checklist does not say where the item went:\n%s", m.chat.outVP.View())
	}
	if !m.chat.ta.Focused() {
		t.Error("the input did not get focus back")
	}

	// The model sees its own proposal on the next turn, so the history keeps
	// the whole reply even though the screen does not.
	last := m.chat.sess.History[len(m.chat.sess.History)-1]
	if !strings.Contains(last.Content, advisor.ProposalFence) {
		t.Error("the recorded reply lost its proposal block")
	}
}

func TestChatProposalsCanBeDiscarded(t *testing.T) {
	m := chatModel(t, &chatFake{Text: proposingReply})
	typeAndSend(t, m, "What should I do next?")
	if m.chat.proposal == nil {
		t.Fatal("no checklist opened")
	}
	key(m, "n")
	if m.chat.proposal != nil {
		t.Error("n did not close the checklist")
	}
	if n := len(storedItems(t, m)); n != 0 {
		t.Errorf("discarding recorded %d item(s)", n)
	}
	if !strings.Contains(m.chat.outVP.View(), "discarded") {
		t.Errorf("the checklist does not say it was discarded:\n%s", m.chat.outVP.View())
	}
}

// TestChatSlashCommands: `/items` asks for the conversation as action items
// through the same checklist, and no slash command is sent to the model.
func TestChatSlashCommands(t *testing.T) {
	fake := &chatFake{
		Text: "Atlas needs a README.",
		JSON: `{"items":[{"project":"atlas","title":"Write a README","body":"b","rationale":"r","effort":"small"}]}`,
	}
	m := chatModel(t, fake)

	typeAndSend(t, m, "/items")
	if m.chat.proposal != nil || !strings.Contains(m.status, "no conversation") {
		t.Errorf("/items with nothing said: proposal = %v, status = %q", m.chat.proposal, m.status)
	}
	typeAndSend(t, m, "/nonsense")
	if !strings.Contains(m.status, "/help") {
		t.Errorf("an unknown command: status = %q", m.status)
	}

	typeAndSend(t, m, "What should I do on Atlas?")
	typeAndSend(t, m, "/items")
	if streams, structured := fake.counts(); streams != 1 || structured != 1 {
		t.Errorf("streams = %d, structured = %d; want one of each — a command is not a message", streams, structured)
	}
	if m.chat.proposal == nil || len(m.chat.proposal.items) != 1 {
		t.Fatalf("/items did not open a checklist: %+v", m.chat.proposal)
	}
	pump(t, m, key(m, "y"))
	if items := storedItems(t, m); len(items) != 1 {
		t.Errorf("recorded %d item(s), want 1", len(items))
	}
	for _, turn := range m.chat.sess.History {
		if strings.HasPrefix(turn.Content, "/") {
			t.Errorf("a slash command reached the conversation: %q", turn.Content)
		}
	}
}

// TestChatReloadKeepsTheConversation: `/reload` is how a project started or
// synced in another terminal reaches a chat already open, and it must not cost
// the user what they had said.
func TestChatReloadKeepsTheConversation(t *testing.T) {
	m := chatModel(t, &chatFake{Text: "Atlas needs a README."})
	typeAndSend(t, m, "What should I do?")
	turns := len(m.chat.sess.History)

	if err := m.opts.Store.UpsertProject(context.Background(), store.Project{
		Slug: "worktree-switcher", Name: "Worktree Switcher", Path: "/tmp/nowhere", RegisteredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	typeAndSend(t, m, "/reload")

	if len(m.chat.actx.Undigested) != 1 {
		t.Fatalf("the reloaded context does not have the new project: %+v", m.chat.actx.Undigested)
	}
	if _, ok := m.chat.actx.ResolveProject("worktree-switcher"); !ok {
		t.Error("the new project cannot be given action items after a reload")
	}
	if len(m.chat.sess.History) != turns {
		t.Error("reloading lost the conversation")
	}
	if !strings.Contains(m.chat.outVP.View(), "Worktree Switcher") {
		t.Errorf("the reload does not say what it found:\n%s", m.chat.outVP.View())
	}
}

// TestProposalChecklistFitsTheFrame: the checklist is drawn by hand rather
// than by Glamour, so it is measured the same way the rest of the frame is.
func TestProposalChecklistFitsTheFrame(t *testing.T) {
	long := strings.Repeat("Replace the hand-rolled retry loop ", 4)
	for _, size := range []struct{ w, h int }{{80, 24}, {100, 30}, {60, 20}, {200, 50}, {40, 10}} {
		m := chatModel(t, &chatFake{})
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		m.openProposal([]advisor.ProposedItem{
			{Project: "atlas", Title: long, Effort: "medium"},
			{Project: strings.Repeat("no-such-project-", 6), Title: "Invent a project"},
		}, "fake-model")
		m.rerenderChat()
		assertFrameFits(t, m, size.w, size.h, "Chat, checklist open")

		key(m, "n")
		assertFrameFits(t, m, size.w, size.h, "Chat, checklist answered")
	}
}

// TestACancelledItemsCallCannotAnswerTheNextOne: the result of an `/items`
// the user stopped still arrives, and must not be taken for the one they
// asked for afterwards.
func TestACancelledItemsCallCannotAnswerTheNextOne(t *testing.T) {
	m := chatModel(t, &chatFake{Text: "Atlas needs a README."})
	typeAndSend(t, m, "What should I do?")

	m.chat.ta.SetValue("/items")
	_, first := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	key(m, "esc")
	m.chat.ta.SetValue("/items")
	_, second := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if first == nil || second == nil {
		t.Fatal("/items did not start a call")
	}

	stale := first().(proposalsMsg)
	m.Update(stale)
	if m.chat.proposing == nil {
		t.Fatal("the cancelled call's result was taken for the live one")
	}
}
