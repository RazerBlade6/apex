package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/provider"
)

// The chat view (DESIGN.md §12): input at the bottom, scrollback above it
// rendered through Glamour.
//
// Streaming is the pattern the spec names: the provider's Event channel is
// drained by a tea.Cmd that returns one message per event and re-issues
// itself, so tokens arrive in Update like any other message. Nothing blocks
// the event loop and the view stays responsive while a reply is arriving.
//
// Two details are not in the spec and matter in practice. Deltas are appended
// as plain text and only re-rendered through Glamour once the turn completes:
// running a markdown parser over a half-written document on every token is
// both expensive and wrong-looking, because the document keeps changing shape.
// And a stream is identified by the pointer to its handle, so a reply that
// arrives after the user cancelled is dropped rather than appended to whatever
// they asked next.

// chatRole labels a turn in the scrollback.
const (
	roleYou    = "you"
	roleApex   = "apex"
	roleSystem = "apex ·"
)

type chatTurn struct {
	role string
	body string
	// rendered is the Glamour output, cached because re-rendering the whole
	// scrollback on every delta is the obvious way to make this view slow.
	rendered string
}

type chatState struct {
	vp viewport.Model
	ta textarea.Model

	turns []chatTurn
	actx  *advisor.Context
	sess  *advisor.Chat

	stream  *streamHandle
	pending strings.Builder
	// sent is the user turn the in-flight reply answers, kept so the
	// observation extractor can be given the exchange rather than the reply
	// alone.
	sent string

	loading   bool
	observing bool
}

// streamHandle owns one in-flight provider stream.
type streamHandle struct {
	ch     <-chan provider.Event
	cancel context.CancelFunc
	usage  provider.Usage
	failed error
}

type contextLoadedMsg struct {
	actx *advisor.Context
	err  error
}

type streamEventMsg struct {
	h    *streamHandle
	ev   provider.Event
	open bool
}

type observedMsg struct {
	result *advisor.ExtractionResult
	err    error
}

func (m *Model) initChat() {
	ta := textarea.New()
	ta.Placeholder = "Ask about your portfolio…"
	ta.Prompt = "› "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(3)
	ta.SetWidth(contentWidth(m.width))

	m.chat = chatState{
		vp:      viewport.New(contentWidth(m.width), 10),
		ta:      ta,
		loading: true,
	}
	m.chat.say(roleSystem, "Loading your profile and project digests…")
}

// say appends a turn to the scrollback without rendering it. Callers that
// change the scrollback call rerenderChat afterwards.
func (c *chatState) say(role, body string) {
	c.turns = append(c.turns, chatTurn{role: role, body: body})
}

func (m *Model) layout() {
	w := contentWidth(m.width)
	m.chat.ta.SetWidth(w)
	m.chat.vp.Width = w
	h := m.bodyHeight() - m.chat.ta.Height() - 1
	if h < 3 {
		h = 3
	}
	m.chat.vp.Height = h
	m.items.height = m.bodyHeight()
	m.projects.height = m.bodyHeight()
}

// rerenderChat rebuilds the scrollback from the turns and sticks to the
// bottom, which is what a conversation wants: new text should be visible
// without a keypress.
func (m *Model) rerenderChat() {
	var b strings.Builder
	for i := range m.chat.turns {
		t := &m.chat.turns[i]
		if t.rendered == "" {
			t.rendered = m.renderTurn(*t)
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(t.rendered)
	}
	if m.chat.stream != nil {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(styleLabel.Render(roleApex))
		b.WriteString("\n")
		partial := m.chat.pending.String()
		if strings.TrimSpace(partial) == "" {
			b.WriteString(styleDim.Render("thinking…"))
		} else {
			b.WriteString(wrapPlain(partial, contentWidth(m.width)))
		}
	}
	m.chat.vp.SetContent(b.String())
	m.chat.vp.GotoBottom()
}

func (m *Model) renderTurn(t chatTurn) string {
	head := styleLabel.Render(t.role)
	switch t.role {
	case roleYou:
		return head + "\n" + styleAccent.Render(wrapPlain(t.body, contentWidth(m.width)))
	case roleSystem:
		return styleDim.Render(wrapPlain(t.role+" "+t.body, contentWidth(m.width)))
	default:
		return head + "\n" + m.render.markdown(t.body)
	}
}

func (m *Model) chatHint() string {
	if m.chat.loading {
		return "loading context…"
	}
	if m.chat.stream != nil {
		return "streaming — esc to stop"
	}
	if m.chat.actx == nil {
		return "no context loaded"
	}
	n := len(m.chat.actx.Digests)
	route := m.opts.Config.Models.Chat
	return fmt.Sprintf("%d digest(s) · %s/%s", n, route.Provider, route.Model)
}

func (m *Model) chatView() string {
	body := padLines(m.chat.vp.View(), m.chat.vp.Height)
	return body + "\n" + m.chat.ta.View()
}

func (m *Model) updateChat(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case contextLoadedMsg:
		m.chat.loading = false
		m.chat.turns = nil
		if msg.err != nil {
			m.chat.say(roleSystem, "could not load context: "+msg.err.Error())
			m.setStatus(msg.err.Error(), statusError)
			m.rerenderChat()
			return m, nil
		}
		m.chat.actx = msg.actx
		m.chat.sess = m.opts.Advisor.NewChat(msg.actx)
		m.chat.say(roleSystem, m.welcome(msg.actx))
		m.rerenderChat()
		return m, nil

	case streamEventMsg:
		return m.updateStream(msg)

	case observedMsg:
		m.chat.observing = false
		if msg.err != nil {
			m.setStatus("observation extraction failed: "+msg.err.Error(), statusWarn)
			return m, nil
		}
		if note := describeExtraction(msg.result); note != "" {
			m.chat.say(roleSystem, note)
			m.rerenderChat()
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			if m.chat.stream != nil {
				m.cancelStream()
				m.setStatus("stopped", statusWarn)
				return m, nil
			}
		case "enter":
			return m, m.send()
		case "ctrl+j":
			// A newline in the input, since enter sends.
			var cmd tea.Cmd
			m.chat.ta, cmd = m.chat.ta.Update(tea.KeyMsg{Type: tea.KeyEnter})
			return m, cmd
		case "pgup", "pgdown", "ctrl+u", "ctrl+d":
			var cmd tea.Cmd
			m.chat.vp, cmd = m.chat.vp.Update(msg)
			return m, cmd
		}
	}

	var cmd tea.Cmd
	m.chat.ta, cmd = m.chat.ta.Update(msg)
	return m, cmd
}

// welcome is the opening line: what context the conversation has, and what is
// missing from it.
//
// DESIGN.md §13 asks every command that depends on digests to report their
// age when it runs against stale data, and §6 asks the same of absent identity
// context. Both are invisible from the outside — an advisor call with no
// PROFILE.md still answers, it just answers generically — so the chat says so
// before the user judges the first reply.
func (m *Model) welcome(actx *advisor.Context) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("%d project digest(s) loaded.", len(actx.Digests)))
	if missing := actx.MissingIdentity(); len(missing) > 0 {
		parts = append(parts, "No "+strings.Join(missing, " or ")+
			" — answers will be generic until you write one.")
	}
	if stale := actx.StaleDigests(m.now(), staleAfter); len(stale) > 0 {
		parts = append(parts, fmt.Sprintf("%d digest(s) older than a week; run apex sync.", len(stale)))
	}
	return strings.Join(parts, " ")
}

// send starts a reply to whatever is in the input box.
func (m *Model) send() tea.Cmd {
	if m.chat.stream != nil {
		m.setStatus("still answering — esc to stop", statusWarn)
		return nil
	}
	if m.chat.sess == nil {
		m.setStatus("context is not loaded yet", statusWarn)
		return nil
	}
	input := strings.TrimSpace(m.chat.ta.Value())
	if input == "" {
		return nil
	}
	m.chat.ta.Reset()
	m.chat.say(roleYou, input)
	m.chat.sent = input
	m.chat.pending.Reset()
	m.setStatus("", statusNeutral)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := m.chat.sess.Send(ctx, input)
	if err != nil {
		cancel()
		m.chat.say(roleSystem, "could not send: "+err.Error())
		m.rerenderChat()
		return nil
	}
	h := &streamHandle{ch: events, cancel: cancel}
	m.chat.stream = h
	m.rerenderChat()
	return waitForEvent(h)
}

// waitForEvent is DESIGN.md §12's drain: one message per event, re-issued by
// Update, so a stream never blocks the loop.
func waitForEvent(h *streamHandle) tea.Cmd {
	return func() tea.Msg {
		ev, open := <-h.ch
		return streamEventMsg{h: h, ev: ev, open: open}
	}
}

func (m *Model) updateStream(msg streamEventMsg) (tea.Model, tea.Cmd) {
	// A stream the user already cancelled. Its events belong to a question
	// that is no longer on screen.
	if msg.h != m.chat.stream {
		return m, nil
	}
	if !msg.open {
		return m, m.finishStream()
	}

	switch msg.ev.Type {
	case provider.EventTextDelta:
		m.chat.pending.WriteString(msg.ev.Text)
		m.rerenderChat()
	case provider.EventDone:
		if msg.ev.Usage != nil {
			msg.h.usage = *msg.ev.Usage
		}
	case provider.EventError:
		if msg.h.failed == nil {
			msg.h.failed = msg.ev.Err
		}
	}
	return m, waitForEvent(msg.h)
}

// finishStream closes out a completed reply: record it, report what it cost,
// and screen the exchange for anything worth learning.
func (m *Model) finishStream() tea.Cmd {
	h := m.chat.stream
	m.chat.stream = nil
	if h == nil {
		return nil
	}
	defer h.cancel()

	reply := strings.TrimSpace(m.chat.pending.String())
	m.chat.pending.Reset()

	if h.failed != nil {
		m.chat.say(roleSystem, "the model call failed: "+h.failed.Error())
		m.setStatus(h.failed.Error(), statusError)
		m.rerenderChat()
		return nil
	}
	if reply == "" {
		m.chat.say(roleSystem, "the model returned no text.")
		m.rerenderChat()
		return nil
	}

	m.chat.say(roleApex, reply)
	if err := m.chat.sess.Complete(context.Background(), reply, h.usage); err != nil {
		m.setStatus("could not record the turn: "+err.Error(), statusWarn)
	} else {
		m.setStatus(describeUsage(h.usage), statusNeutral)
	}
	m.rerenderChat()

	turn := advisor.Turn{User: m.chat.sent, Assistant: reply}
	m.chat.sent = ""
	return m.observeCmd(turn)
}

func (m *Model) cancelStream() {
	if m.chat.stream == nil {
		return
	}
	m.chat.stream.cancel()
	m.chat.stream = nil
	if partial := strings.TrimSpace(m.chat.pending.String()); partial != "" {
		m.chat.say(roleApex, partial)
	}
	m.chat.pending.Reset()
	m.chat.sent = ""
	m.rerenderChat()
}

// describeUsage reports what a turn cost, in the terms DESIGN.md §8 asks for:
// cache reads AND writes, because reads alone cannot tell a cache that works
// from a prefix unstable enough to be rewritten every call.
func describeUsage(u provider.Usage) string {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheWriteTokens == 0 {
		return ""
	}
	s := fmt.Sprintf("%d in / %d out", u.InputTokens, u.OutputTokens)
	if u.CacheReadTokens > 0 || u.CacheWriteTokens > 0 {
		s += fmt.Sprintf(" · cache r=%d w=%d", u.CacheReadTokens, u.CacheWriteTokens)
	}
	if u.Model != "" {
		s += " · " + u.Model
	}
	return s
}

// describeExtraction turns an extraction result into the one line the user
// sees.
//
// It is never silent when something was written. DESIGN.md §6 names the risk
// as "an over-general extraction quietly skews every future suggestion with no
// visible cause"; saying so in the transcript, next to the remark it came
// from, is what makes the cause visible at the moment it is cheapest to
// correct.
func describeExtraction(r *advisor.ExtractionResult) string {
	if r == nil || !r.Changed() {
		return ""
	}
	var b strings.Builder
	b.WriteString("learned: ")
	for i, e := range r.Added {
		if i > 0 {
			b.WriteString(" / ")
		}
		fmt.Fprintf(&b, "#%d %s", e.N, e.Obs.Claim)
	}
	if len(r.Added) == 0 {
		b.Reset()
		b.WriteString("forgot ")
		for i, e := range r.Superseded {
			if i > 0 {
				b.WriteString(" / ")
			}
			b.WriteString(e.Obs.Claim)
		}
	} else if n := len(r.Superseded); n > 0 {
		fmt.Fprintf(&b, " (superseding %d)", n)
	}
	b.WriteString(" · apex profile --forget <n> to undo")
	return b.String()
}
