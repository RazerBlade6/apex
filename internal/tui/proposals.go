package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/RazerBlade6/apex/internal/advisor"
)

// Action items proposed from the chat.
//
// A reply can end with a fenced block of proposed action items, and `/items`
// asks for the conversation so far as a list through a Structured call. Either
// way the proposals arrive as a checklist in Apex's pane and nothing is written
// until the user answers it: `y` records the ticked items exactly as `apex
// review` would — resolved, deduplicated, stamped, proposed — and the items
// view reloads, so they are there to dispatch on the next tab.
//
// The approval is the same shape as the items view's dispatch confirmation,
// for the same reason: a proposal the model wrote is not something the user
// asked for until they say so. While the checklist is open the input is
// blurred and it has the keyboard; any key that is not one of its answers is
// swallowed with a reminder of what they are. `enter` is deliberately not one
// of them. It is the chat's send key, and a user who typed a reply without
// noticing the checklist should be reminded, not have their enter accept it.

// proposal is one checklist, from arrival to answer.
type proposal struct {
	items []advisor.ProposedItem
	// slugs is each item's project, resolved against the context it will be
	// recorded against, or "" when it names no project in it. An unresolved
	// item cannot be ticked: recording would only skip it.
	slugs []string
	keep  []bool
	// cursor is the item the space bar toggles.
	cursor int
	// model generated the proposals, and is recorded as generated_by.
	model string

	// saving is set between `y` and the write returning; decided once the
	// checklist has been answered, after which it is a record of what
	// happened rather than a question.
	saving  bool
	decided bool
	// outcome is each item's fate once decided: an item id, or why not.
	outcome []string
}

// proposeRun is one `/items` call in flight. Like a stream, it is identified
// by its pointer: a call the user cancelled can still deliver its result, and
// if they have asked again since, that result must not be taken for the new
// one's.
type proposeRun struct {
	cancel context.CancelFunc
}

// proposalsMsg carries the result of `/items` back from proposeCmd.
type proposalsMsg struct {
	run   *proposeRun
	props *advisor.Proposals
	err   error
}

// recordedMsg carries the result of writing the approved items.
type recordedMsg struct {
	p      *proposal
	result *advisor.ReviewResult
	err    error
}

// contextReloadedMsg carries a context reloaded by `/reload`. Unlike
// contextLoadedMsg it keeps the conversation: only what the next turn reasons
// over changes.
type contextReloadedMsg struct {
	actx *advisor.Context
	err  error
}

// openProposal puts a checklist in front of the user, with every item that
// names a known project ticked.
func (m *Model) openProposal(items []advisor.ProposedItem, model string) {
	p := &proposal{
		items:   items,
		slugs:   make([]string, len(items)),
		keep:    make([]bool, len(items)),
		outcome: make([]string, len(items)),
		model:   model,
	}
	for i, it := range items {
		if slug, ok := m.chat.actx.ResolveProject(it.Project); ok {
			p.slugs[i] = slug
			p.keep[i] = true
		}
	}
	m.chat.proposal = p
	m.chat.turns = append(m.chat.turns, chatTurn{role: roleProposal, prop: p})
	m.chat.ta.Blur()
	m.setStatus("", statusNeutral)
}

// closeProposal ends the checklist's hold on the keyboard.
func (m *Model) closeProposal() {
	m.chat.proposal = nil
	if m.view == viewChat {
		m.chat.ta.Focus()
	}
}

// touchProposal re-renders the checklist after its state changed. Turns cache
// their rendering, and this one is the only turn whose content moves after it
// was said.
func (m *Model) touchProposal(p *proposal) {
	for i := range m.chat.turns {
		if m.chat.turns[i].prop == p {
			m.chat.turns[i].rendered = ""
		}
	}
	m.rerenderChat()
}

// proposalKey answers the open checklist.
func (m *Model) proposalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.chat.proposal
	if p.saving {
		return m, nil
	}
	switch msg.String() {
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(p.items)-1 {
			p.cursor++
		}
	case " ", "space":
		if p.slugs[p.cursor] == "" {
			m.setStatus("that item names no project in the portfolio, so it cannot be added", statusWarn)
			return m, nil
		}
		p.keep[p.cursor] = !p.keep[p.cursor]
	case "y", "Y":
		var chosen []advisor.ProposedItem
		for i, it := range p.items {
			if p.keep[i] {
				chosen = append(chosen, it)
			}
		}
		if len(chosen) == 0 {
			m.setStatus("nothing is ticked — space to tick an item, n to discard them all", statusWarn)
			return m, nil
		}
		p.saving = true
		m.touchProposal(p)
		return m, m.recordCmd(p, chosen)
	case "n", "N", "esc":
		p.decided = true
		for i := range p.outcome {
			p.outcome[i] = "discarded"
		}
		m.closeProposal()
		m.setStatus(fmt.Sprintf("discarded %d proposed action item(s)", len(p.items)), statusNeutral)
		m.touchProposal(p)
		return m, nil
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		m.chat.outVP, cmd = m.chat.outVP.Update(msg)
		return m, cmd
	default:
		m.setStatus("answer the proposed action items first: y add · n discard", statusWarn)
		return m, nil
	}
	m.setStatus("", statusNeutral)
	m.touchProposal(p)
	return m, nil
}

// recordCmd writes the approved items off the event loop.
func (m *Model) recordCmd(p *proposal, chosen []advisor.ProposedItem) tea.Cmd {
	adv := m.opts.Advisor
	actx := m.chat.actx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		result, err := adv.RecordItems(ctx, actx, chosen, p.model)
		return recordedMsg{p: p, result: result, err: err}
	}
}

// recorded closes the checklist once the write has returned, and reloads the
// items view so the new items are there to dispatch.
func (m *Model) recorded(msg recordedMsg) (tea.Model, tea.Cmd) {
	p := msg.p
	p.saving = false
	if msg.err != nil {
		m.setStatus("could not add the action items: "+msg.err.Error(), statusError)
		m.touchProposal(p)
		return m, nil
	}
	p.decided = true
	if m.chat.proposal == p {
		m.closeProposal()
	}

	// Match each outcome back to its line in the checklist. Titles are what
	// the result reports, and within one checklist they are what the user
	// could tell apart.
	inserted := map[string]string{}
	for _, row := range msg.result.Inserted {
		inserted[row.Title] = row.ID
	}
	dups := map[string]bool{}
	for _, t := range msg.result.Duplicates {
		dups[t] = true
	}
	var ids []string
	for i, it := range p.items {
		title := strings.TrimSpace(it.Title)
		switch {
		case !p.keep[i]:
			p.outcome[i] = "discarded"
		case inserted[title] != "":
			p.outcome[i] = inserted[title]
			ids = append(ids, inserted[title])
		case dups[title]:
			p.outcome[i] = "already on the list"
		default:
			p.outcome[i] = "not added"
		}
	}

	if len(ids) == 0 {
		m.setStatus("nothing new to add — every ticked item is already on the list", statusWarn)
	} else {
		m.setStatus(fmt.Sprintf("added %s as proposed · tab to Items to dispatch", strings.Join(ids, ", ")), statusNeutral)
	}
	m.touchProposal(p)
	return m, m.loadItemsCmd()
}

// proposeCmd is `/items`: the conversation so far, as action items.
func (m *Model) proposeCmd() tea.Cmd {
	sess := m.chat.sess
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	run := &proposeRun{cancel: cancel}
	m.chat.proposing = run
	return func() tea.Msg {
		defer cancel()
		props, err := sess.ProposeItems(ctx)
		return proposalsMsg{run: run, props: props, err: err}
	}
}

// cancelProposing stops an `/items` call in flight, if there is one.
func (m *Model) cancelProposing() {
	if m.chat.proposing == nil {
		return
	}
	m.chat.proposing.cancel()
	m.chat.proposing = nil
}

// proposed opens the checklist `/items` asked for.
func (m *Model) proposed(msg proposalsMsg) (tea.Model, tea.Cmd) {
	if msg.run != m.chat.proposing {
		// Cancelled while it was in flight.
		return m, nil
	}
	m.chat.proposing = nil
	if msg.err != nil {
		m.chat.say(roleSystem, "could not propose action items: "+msg.err.Error())
		m.setStatus(msg.err.Error(), statusError)
		m.rerenderChat()
		return m, nil
	}
	if len(msg.props.Items) == 0 {
		m.chat.say(roleSystem, "Nothing in this conversation is concrete enough to be an action item yet.")
		m.setStatus(describeUsage(msg.props.Usage), statusNeutral)
		m.rerenderChat()
		return m, nil
	}
	m.openProposal(msg.props.Items, msg.props.Model)
	m.rerenderChat()
	return m, nil
}

// reloadContextCmd is `/reload`: the identity files, the digests and the
// registry read again, for a project started or synced in another terminal
// since the TUI opened.
func (m *Model) reloadContextCmd() tea.Cmd {
	adv := m.opts.Advisor
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		actx, err := adv.LoadContext(ctx)
		return contextReloadedMsg{actx: actx, err: err}
	}
}

func (m *Model) contextReloaded(msg contextReloadedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.setStatus("could not reload context: "+msg.err.Error(), statusError)
		return m, nil
	}
	m.chat.actx = msg.actx
	if m.chat.sess != nil {
		m.chat.sess.SetContext(msg.actx)
	} else {
		m.chat.sess = m.opts.Advisor.NewChat(msg.actx)
	}
	m.chat.say(roleSystem, "Reloaded. "+m.welcome(msg.actx))
	m.rerenderChat()
	return m, tea.Batch(m.loadItemsCmd(), m.loadProjectsCmd())
}

// renderProposal draws the checklist for Apex's pane.
//
// It is plain styled text rather than markdown: its state changes with every
// key, and Glamour would restyle it out from under the cursor. It is indented
// to Glamour's margin, like the system notes, so the pane keeps one left edge.
func (m *Model) renderProposal(p *proposal) string {
	width := m.replyWidth() - glamourMargin
	const gutter = 6 // "› [x] "
	textWidth := max(width-gutter, 8)

	var b strings.Builder
	// The keys are on the status line and in the footer while the checklist
	// is open; repeating them here would cost a row in a narrow pane.
	head := "Proposed action items"
	if p.saving {
		head += " — adding…"
	}
	b.WriteString(fitStyled(head, width, styleLabel))

	for i, it := range p.items {
		b.WriteString("\n")
		pointer := "  "
		if !p.decided && i == p.cursor {
			pointer = styleAccent.Render("› ")
		}
		box := "[ ] "
		switch {
		case p.slugs[i] == "":
			box = "[-] "
		case p.keep[i]:
			box = "[x] "
		}

		titleStyle := styleTitle
		if p.decided && !strings.HasPrefix(p.outcome[i], "AI-") {
			titleStyle = styleDim
		}
		title := fitLines(strings.TrimSpace(it.Title), textWidth)
		for j, line := range title {
			if j == 0 {
				b.WriteString(pointer + box + titleStyle.Render(line))
				continue
			}
			b.WriteString("\n" + strings.Repeat(" ", gutter) + titleStyle.Render(line))
		}

		meta := m.proposalMeta(p, i)
		for _, line := range fitLines(meta, textWidth) {
			b.WriteString("\n" + strings.Repeat(" ", gutter))
			switch {
			case p.slugs[i] == "":
				b.WriteString(styleWarn.Render(line))
			case p.decided && strings.HasPrefix(p.outcome[i], "AI-"):
				b.WriteString(styleOK.Render(line))
			default:
				b.WriteString(styleDim.Render(line))
			}
		}
	}
	return indentBlock(b.String(), glamourMargin)
}

// proposalMeta is the line under an item's title: where it would go, and
// once the checklist is answered, where it went.
func (m *Model) proposalMeta(p *proposal, i int) string {
	it := p.items[i]
	if p.slugs[i] == "" {
		return fmt.Sprintf("%q is not a project in the portfolio — it will not be added", it.Project)
	}
	parts := []string{m.chat.actx.ProjectName(p.slugs[i])}
	if e := strings.TrimSpace(it.Effort); e != "" {
		parts = append(parts, e+" effort")
	}
	if p.decided && p.outcome[i] != "" {
		parts = append(parts, p.outcome[i])
	}
	return strings.Join(parts, " · ")
}
