package tui

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/provider"
)

// The chat view (UI.md §2): two side-by-side boxes, the user's prompts and
// input on the left, Apex's replies on the right, rendered through Glamour.
//
// The two scrollbacks are independent. Aligning turn N's prompt with turn N's
// reply would mean padding one column to the other's height, which a streaming
// reply changes on every token; each pane therefore accumulates its own side of
// the conversation and sticks to the bottom. In a terminal too narrow or too
// short to hold two panes the view falls back to one box with the scrollback
// above the input, which is the layout this view had before.
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
	// roleProposal is a checklist of proposed action items (proposals.go).
	// Its body is drawn from prop, not from body.
	roleProposal = "apex ☐"
)

type chatTurn struct {
	role string
	body string
	prop *proposal
	// rendered is the Glamour output, cached because re-rendering the whole
	// scrollback on every delta is the obvious way to make this view slow.
	rendered string
}

type chatState struct {
	// inVP holds the user's own turns, outVP everything Apex says. In
	// fallback mode inVP is unused and outVP is the one scrollback.
	inVP  viewport.Model
	outVP viewport.Model
	ta    textarea.Model

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

	// proposal is the checklist awaiting an answer, if there is one. While
	// it is open it has the keyboard.
	proposal *proposal
	// proposing is the `/items` call in flight; nil when there is none.
	proposing *proposeRun
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
	ta.Placeholder = placeholderWide
	ta.Prompt = "› "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	// The input is chrome too, so it gets the same palette as the rest of the
	// frame: the prompt and the cursor in the accent the user's own turns are
	// rendered in, the placeholder in the same grey as every other hint. Only
	// the focused prompt is coloured — a blurred input should recede.
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Foreground(colBlue)
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(colGrey)
	ta.BlurredStyle.Prompt = lipgloss.NewStyle().Foreground(colGrey)
	ta.BlurredStyle.Placeholder = lipgloss.NewStyle().Foreground(colGrey)
	ta.Cursor.Style = lipgloss.NewStyle().Foreground(colBlue)
	ta.SetHeight(3)
	ta.SetWidth(m.innerWidth())

	m.chat = chatState{
		inVP:    viewport.New(m.innerWidth(), 10),
		outVP:   viewport.New(m.innerWidth(), 10),
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

// chatTwoPane is the predicate for the side-by-side layout: two panes need room,
// and below this threshold the chat view falls back to the single box it used to
// be. At 40 columns the left pane would be ten columns of text, which is not a
// layout; at fewer than eight body rows the input box alone would eat the pane.
func (m *Model) chatTwoPane() bool {
	return m.innerWidth() >= minTwoPaneWidth && m.bodyHeight() >= minTwoPaneHeight
}

// chatPaneWidths splits the inner width between the two panes.
//
// Each pane pays the four columns of border and padding that boxChrome already
// deducted from innerWidth once, and one column of gap sits between them, so the
// pair costs four columns and one gap more than the single box:
//
//	leftInner + rightInner = innerWidth - boxChrome - paneGap = innerWidth - 5
//
// which makes leftOuter + 1 + rightOuter come to exactly frameWidth, so chat
// lines up with the box on the other two tabs. The right half is derived by
// subtraction rather than rounded on its own: rounding both sides independently
// drifts a column away from the frame at some widths.
func (m *Model) chatPaneWidths() (left, right int) {
	avail := m.innerWidth() - paneChrome - paneGap
	left = int(math.Round(paneSplit * float64(avail)))
	if left < minPaneInner {
		left = minPaneInner
	}
	if left > maxPaneInner {
		left = maxPaneInner
	}
	return left, avail - left
}

// replyWidth is the width Apex's output is wrapped to: the right pane in
// two-pane mode, the whole box in fallback. The Glamour renderer is built at
// this width, so getting it wrong wraps every reply to a column that is not
// the one it is displayed in.
func (m *Model) replyWidth() int {
	if m.chatTwoPane() {
		_, right := m.chatPaneWidths()
		return right
	}
	return m.innerWidth()
}

// promptWidth is the mirror of replyWidth for the user's own turns.
func (m *Model) promptWidth() int {
	if m.chatTwoPane() {
		left, _ := m.chatPaneWidths()
		return left
	}
	return m.innerWidth()
}

// The placeholder is set per layout: the full sentence wraps onto two of the
// three input rows in a narrow left pane, each carrying its own prompt glyph,
// which reads as three empty inputs rather than one.
const (
	placeholderWide   = "Ask about your portfolio…"
	placeholderNarrow = "Ask Apex…"
)

func (m *Model) layout() {
	m.items.height = m.bodyHeight()
	m.projects.height = m.bodyHeight()

	if m.chatTwoPane() {
		left, right := m.chatPaneWidths()
		// The input is its own bordered box pinned to the bottom of the left
		// pane — two border rows around the three-row textarea — so the
		// prompts above it get what is left, and the textarea pays the border
		// and the padding a second time.
		h := m.bodyHeight() - inputBoxHeight
		if h < 1 {
			h = 1
		}
		m.chat.inVP.Width, m.chat.inVP.Height = left, h
		m.chat.outVP.Width, m.chat.outVP.Height = right, m.bodyHeight()
		m.chat.ta.SetWidth(left - paneChrome)
		m.chat.ta.Placeholder = placeholderNarrow
		return
	}

	w := m.innerWidth()
	m.chat.ta.SetWidth(w)
	m.chat.ta.Placeholder = placeholderWide
	h := m.bodyHeight() - m.chat.ta.Height() - 1
	if h < 3 {
		h = 3
	}
	m.chat.inVP.Width, m.chat.inVP.Height = w, h
	m.chat.outVP.Width, m.chat.outVP.Height = w, h
}

// rerenderChat rebuilds both scrollbacks from the turns and sticks each to the
// bottom, which is what a conversation wants: new text should be visible
// without a keypress.
//
// Routing is by role: the user's turns go left, everything Apex says — replies,
// system notes and the in-flight partial — goes right. In fallback mode there is
// only one pane, so everything goes to outVP, which is the one the scroll keys
// drive.
func (m *Model) rerenderChat() {
	two := m.chatTwoPane()
	var in, out strings.Builder
	block := func(b *strings.Builder, s string) {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(s)
	}

	for i := range m.chat.turns {
		t := &m.chat.turns[i]
		if t.rendered == "" {
			t.rendered = m.renderTurn(*t)
		}
		if two && t.role == roleYou {
			block(&in, t.rendered)
			continue
		}
		block(&out, t.rendered)
	}
	if m.chat.stream != nil {
		var partial strings.Builder
		if !two {
			// The label only earns its row when both speakers share a column.
			partial.WriteString(styleLabel.Render(roleApex))
			partial.WriteString("\n")
		}
		// The partial is indented to Glamour's margin too. Without it a reply
		// visibly jumps two columns right the moment the stream ends and the
		// finished turn is re-rendered as markdown.
		// A proposal block is JSON until the reply ends and it becomes a
		// checklist; while it streams, it is summarised in one dim line.
		text, drafting := advisor.VisibleReply(m.chat.pending.String())
		switch {
		case strings.TrimSpace(text) == "" && drafting:
			partial.WriteString(styleDim.Render(indentBlock("drafting action items…", glamourMargin)))
		case strings.TrimSpace(text) == "":
			partial.WriteString(styleDim.Render(indentBlock("thinking…", glamourMargin)))
		default:
			partial.WriteString(indentBlock(
				wrapPlain(strings.TrimSpace(text), m.replyWidth()-glamourMargin), glamourMargin))
			if drafting {
				partial.WriteString("\n\n" + styleDim.Render(indentBlock("drafting action items…", glamourMargin)))
			}
		}
		block(&out, partial.String())
	}
	if m.chat.proposing != nil {
		block(&out, styleDim.Render(indentBlock("turning the conversation into action items…", glamourMargin)))
	}

	m.chat.inVP.SetContent(in.String())
	m.chat.inVP.GotoBottom()
	m.chat.outVP.SetContent(out.String())
	m.chat.outVP.GotoBottom()
}

// invalidateRenders drops the cached Glamour output. The cache is keyed on
// nothing, so a resize — which changes the wrap width, and can cross the
// two-pane threshold and with it whether a turn carries a role label — has to
// clear it rather than redisplay text measured for the old frame.
func (c *chatState) invalidateRenders() {
	for i := range c.turns {
		c.turns[i].rendered = ""
	}
}

// renderTurn renders one turn for whichever pane owns it.
//
// In two-pane mode the per-turn role labels are dropped: the column already says
// who is speaking, and a yellow "you" over every prompt in an eighteen-column
// pane costs a row to repeat what the layout states. System lines stay distinct
// by being dim. In fallback mode the labels remain, because there the two
// speakers share one column and nothing else tells them apart.
func (m *Model) renderTurn(t chatTurn) string {
	two := m.chatTwoPane()
	head := styleLabel.Render(t.role)
	switch t.role {
	case roleYou:
		body := styleAccent.Render(wrapPlain(t.body, m.promptWidth()))
		if two {
			return body
		}
		return head + "\n" + body
	case roleProposal:
		return m.renderProposal(t.prop)
	case roleSystem:
		if two {
			// Indented to Glamour's margin so the right pane has one left
			// edge rather than two.
			return styleDim.Render(indentBlock(
				wrapPlain(t.body, m.replyWidth()-glamourMargin), glamourMargin))
		}
		return styleDim.Render(wrapPlain(t.role+" "+t.body, m.replyWidth()))
	default:
		body := m.render.markdown(t.body)
		if two {
			return body
		}
		return head + "\n" + body
	}
}

func (m *Model) chatHint() string {
	if m.chat.loading {
		return "loading context…"
	}
	if m.chat.stream != nil {
		return "streaming — esc to stop"
	}
	if m.chat.proposing != nil {
		return "proposing action items — esc to stop"
	}
	if p := m.chat.proposal; p != nil {
		return fmt.Sprintf("%d proposed action item(s) · y add ticked · n discard", len(p.items))
	}
	if m.chat.actx == nil {
		return "no context loaded"
	}
	n := len(m.chat.actx.Digests)
	route := m.opts.Config.Models.Chat
	hint := fmt.Sprintf("%d digest(s) · %s/%s", n, route.Provider, route.Model)
	if u := len(m.chat.actx.Undigested); u > 0 {
		hint += fmt.Sprintf(" · %d not synced", u)
	}
	return hint + " · /help"
}

// chatView is the fallback body: one scrollback above the input, to be wrapped
// in the single bounding box the other two views use.
func (m *Model) chatView() string {
	body := padLines(m.chat.outVP.View(), m.chat.outVP.Height)
	// A blank row between the scrollback and the input. layout() already
	// reserves it; joining with a single newline instead spent it as padding
	// under the input, where it separated nothing.
	return body + "\n\n" + m.chat.ta.View()
}

// chatPanes is the two-pane body: two finished boxes side by side, already the
// frame's full width.
//
// The gap between them is the right pane's left margin, which is why the
// arithmetic in chatPaneWidths counts it. Both panes are the same height as the
// single box, and the input is its own box pinned to the bottom of the left one.
func (m *Model) chatPanes() string {
	leftInner, rightInner := m.chatPaneWidths()
	h := m.bodyHeight()
	taH := m.chat.ta.Height()

	// Width is the pane's inner width less the input box's own padding, so the
	// box renders flush with the pane's text column.
	input := styleBox.
		Width(leftInner - 2).
		Height(taH).
		MaxHeight(taH + 2).
		Render(m.chat.ta.View())

	left := styleBox.
		Width(leftInner + 2).
		Height(h).
		MaxHeight(h + 2).
		Render(padLines(padLines(m.chat.inVP.View(), m.chat.inVP.Height)+"\n"+input, h))
	right := styleBoxRight.
		Width(rightInner + 2).
		Height(h).
		MaxHeight(h + 2).
		Render(padLines(m.chat.outVP.View(), h))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
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

	case proposalsMsg:
		return m.proposed(msg)

	case recordedMsg:
		return m.recorded(msg)

	case contextReloadedMsg:
		return m.contextReloaded(msg)

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
		if m.chat.proposal != nil {
			return m.proposalKey(msg)
		}
		switch msg.String() {
		case "esc":
			if m.chat.stream != nil {
				m.cancelStream()
				m.setStatus("stopped", statusWarn)
				return m, nil
			}
			if m.chat.proposing != nil {
				m.cancelProposing()
				m.setStatus("stopped", statusWarn)
				m.rerenderChat()
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
			// Apex's output is the pane the user is reading; their own
			// prompts stay pinned to the bottom.
			var cmd tea.Cmd
			m.chat.outVP, cmd = m.chat.outVP.Update(msg)
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
	if len(actx.Undigested) > 0 {
		names := make([]string, 0, len(actx.Undigested))
		for _, u := range actx.Undigested {
			names = append(names, actx.ProjectName(u.Project.Slug))
		}
		parts = append(parts, fmt.Sprintf("Not synced yet, so read from PROJECT.md: %s.",
			strings.Join(names, ", ")))
	}
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
	if m.chat.proposing != nil {
		m.setStatus("still proposing action items — esc to stop", statusWarn)
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
	if strings.HasPrefix(input, "/") {
		m.chat.ta.Reset()
		return m.chatCommand(input)
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

// chatCommands is what `/help` lists.
const chatCommands = `/items   turn this conversation into action items to approve
/reload  re-read your profile, digests and PROJECTS.md — after an apex start or sync elsewhere
/help    this list`

// chatCommand runs a line that starts with a slash. They are commands to
// Apex rather than messages to the model, so none of them is sent, recorded,
// or screened for observations.
func (m *Model) chatCommand(input string) tea.Cmd {
	name := strings.ToLower(strings.Fields(input)[0])
	switch name {
	case "/items":
		if len(m.chat.sess.History) == 0 {
			m.setStatus("there is no conversation to turn into action items yet", statusWarn)
			return nil
		}
		cmd := m.proposeCmd()
		m.rerenderChat()
		return cmd
	case "/reload":
		m.setStatus("reloading context…", statusNeutral)
		return m.reloadContextCmd()
	case "/help", "/?":
		m.chat.say(roleSystem, chatCommands)
		m.rerenderChat()
		return nil
	default:
		m.setStatus(fmt.Sprintf("no command %s — /help lists them", name), statusWarn)
		return nil
	}
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

	full := strings.TrimSpace(m.chat.pending.String())
	m.chat.pending.Reset()
	// The reply as the user reads it has its proposal block taken out; the
	// transcript and the history keep the whole of it, so the model can see
	// on the next turn what it proposed.
	reply, proposals, proposalErr := advisor.ExtractProposals(full)

	if h.failed != nil {
		m.chat.say(roleSystem, "the model call failed: "+h.failed.Error())
		m.setStatus(h.failed.Error(), statusError)
		m.rerenderChat()
		return nil
	}
	if full == "" {
		m.chat.say(roleSystem, "the model returned no text.")
		m.rerenderChat()
		return nil
	}

	if reply != "" {
		m.chat.say(roleApex, reply)
	}
	if err := m.chat.sess.Complete(context.Background(), full, h.usage); err != nil {
		m.setStatus("could not record the turn: "+err.Error(), statusWarn)
	} else {
		m.setStatus(describeUsage(h.usage), statusNeutral)
	}
	switch {
	case proposalErr != nil:
		m.chat.say(roleSystem, "could not read the proposed action items: "+proposalErr.Error()+
			" — /items asks for them again")
	case len(proposals) > 0:
		model := h.usage.Model
		if model == "" {
			model = m.opts.Config.Models.Chat.Model
		}
		m.openProposal(proposals, model)
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
