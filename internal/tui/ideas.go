package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/store"
)

// Project ideas from the chat, from a list to an action item.
//
// A reply that proposes new projects, or `/ideas`, puts them in front of the
// user as a list to pick from. Picking one asks Apex for it in more depth and
// for a short questionnaire, which the user answers in the ordinary input box,
// one question at a time. Then one question of Apex's own: make it an action
// item? Yes creates the project — directory, git, PROJECT.md, the PROJECTS.md
// entry and the projects row, the deterministic half of `apex start` — and adds
// its first action item, so it is in the items view ready to dispatch.
//
// The project has to be created at that point, not at dispatch. An action item
// belongs to a registered project; one that pointed at a directory that did not
// exist yet would be the one row in the items view that could not be
// dispatched, synced or shown with its project beside it.
//
// Nothing is written before the yes, except the conversation itself: the pick
// and the answers are added to it, so what the user said during the
// questionnaire is not lost to the rest of the chat.

// IdeaStarter creates the project an idea became and its first action item,
// writing progress to w. Like Dispatcher it is a callback, because creating a
// project is filesystem and registry work that lives in cmd/apex.
type IdeaStarter func(ctx context.Context, w io.Writer, plan *advisor.IdeaPlan) (IdeaStarted, error)

// IdeaStarted is what an IdeaStarter made.
type IdeaStarted struct {
	ProjectName string
	Dir         string
	Item        store.ActionItem
}

type ideaStage int

const (
	ideaPick      ideaStage = iota // the list is open and has the keyboard
	ideaExploring                  // waiting for the detail and the questions
	ideaAsking                     // the input box answers the questionnaire
	ideaConfirm                    // y or n: make it an action item?
	ideaCreating                   // planning, then creating the project
)

// ideaFlow is one list of ideas, from arrival to an action item or to nothing.
type ideaFlow struct {
	ideas  []advisor.ProposedIdea
	cursor int
	chosen int // -1 until one is picked
	stage  ideaStage
	// closed is set once the flow is over, after which the list is a record
	// of what was picked rather than a question.
	closed bool

	brief   *advisor.IdeaBrief
	answers []advisor.Answer

	run *ideaRun
}

// ideaRun is one model call or creation in flight, identified by pointer so
// that a result arriving after the user stopped it is dropped.
type ideaRun struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type ideasProposedMsg struct {
	run   *ideaRun
	props *advisor.IdeaProposals
	err   error
}

type ideaExploredMsg struct {
	run   *ideaRun
	brief *advisor.IdeaBrief
	err   error
}

type ideaCreatedMsg struct {
	run     *ideaRun
	plan    *advisor.IdeaPlan
	started IdeaStarted
	output  string
	err     error
}

// chatModal reports whether something other than the input has the keyboard.
func (m *Model) chatModal() bool {
	if m.chat.proposal != nil {
		return true
	}
	f := m.chat.idea
	return f != nil && f.stage != ideaAsking
}

// focusInput gives the input the keyboard back, if nothing modal holds it.
func (m *Model) focusInput() {
	if m.view == viewChat && !m.chatModal() {
		m.chat.ta.Focus()
		return
	}
	m.chat.ta.Blur()
}

// openIdeas puts a list of ideas in front of the user.
func (m *Model) openIdeas(ideas []advisor.ProposedIdea) {
	f := &ideaFlow{ideas: ideas, chosen: -1}
	m.chat.idea = f
	m.chat.turns = append(m.chat.turns, chatTurn{role: roleIdeas, ideas: f})
	m.focusInput()
	m.setStatus("", statusNeutral)
}

// endIdeas closes the flow, leaving its list in the scrollback as a record.
func (m *Model) endIdeas() {
	f := m.chat.idea
	m.chat.idea = nil
	m.focusInput()
	if f == nil {
		m.rerenderChat()
		return
	}
	f.closed = true
	if f.run != nil {
		f.run.cancel()
		f.run = nil
	}
	// After the flow is cleared, so the "working…" line it drew goes too.
	m.touchIdeas(f)
}

// touchIdeas re-renders the list after its state changed.
func (m *Model) touchIdeas(f *ideaFlow) {
	for i := range m.chat.turns {
		if m.chat.turns[i].ideas == f {
			m.chat.turns[i].rendered = ""
		}
	}
	m.rerenderChat()
}

// ideaKey handles a key while the flow has the keyboard.
func (m *Model) ideaKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := m.chat.idea
	k := msg.String()
	switch f.stage {
	case ideaPick:
		switch k {
		case "up", "k":
			if f.cursor > 0 {
				f.cursor--
			}
		case "down", "j":
			if f.cursor < len(f.ideas)-1 {
				f.cursor++
			}
		case "enter", "y", "Y":
			return m, m.pickIdea(f.cursor)
		case "n", "N", "esc":
			m.chat.say(roleSystem, "No idea picked. /ideas brings up a new list.")
			m.endIdeas()
			m.setStatus("", statusNeutral)
			return m, nil
		case "pgup", "pgdown", "ctrl+u", "ctrl+d":
			var cmd tea.Cmd
			m.chat.outVP, cmd = m.chat.outVP.Update(msg)
			return m, cmd
		default:
			if n := int(k[0] - '0'); len(k) == 1 && n >= 1 && n <= len(f.ideas) {
				return m, m.pickIdea(n - 1)
			}
			m.setStatus("pick an idea first: ↑↓ and enter, or its number · n for none", statusWarn)
			return m, nil
		}
		m.setStatus("", statusNeutral)
		m.touchIdeas(f)
		return m, nil

	case ideaConfirm:
		switch k {
		case "y", "Y":
			return m, m.createIdea()
		case "n", "N", "esc":
			title := f.ideas[f.chosen].Title
			m.chat.say(roleSystem, fmt.Sprintf("Left “%s” as an idea; nothing was recorded.", title))
			m.endIdeas()
			m.setStatus("", statusNeutral)
			return m, nil
		case "pgup", "pgdown", "ctrl+u", "ctrl+d":
			var cmd tea.Cmd
			m.chat.outVP, cmd = m.chat.outVP.Update(msg)
			return m, cmd
		}
		m.setStatus("make it an action item? y / n", statusWarn)
		return m, nil

	default: // exploring, creating
		if k == "esc" {
			m.chat.say(roleSystem, "Stopped.")
			m.endIdeas()
			m.setStatus("stopped", statusWarn)
			return m, nil
		}
		m.setStatus("still working — esc to stop", statusWarn)
		return m, nil
	}
}

// pickIdea records the choice in the conversation and asks for the detail.
func (m *Model) pickIdea(i int) tea.Cmd {
	f := m.chat.idea
	f.chosen, f.cursor, f.stage = i, i, ideaExploring
	idea := f.ideas[i]
	m.chat.say(roleYou, fmt.Sprintf("Let's go with “%s”.", idea.Title))
	if err := m.chat.sess.Note(context.Background(), store.RoleUser,
		fmt.Sprintf("I'd like to pursue the idea “%s”.\n\n%s", idea.Title, idea.Pitch)); err != nil {
		m.setStatus("could not record the turn: "+err.Error(), statusWarn)
	} else {
		m.setStatus("", statusNeutral)
	}
	m.touchIdeas(f)

	sess := m.chat.sess
	run := m.newIdeaRun(5 * time.Minute)
	return func() tea.Msg {
		defer run.cancel()
		brief, err := sess.ExploreIdea(run.ctx, idea)
		return ideaExploredMsg{run: run, brief: brief, err: err}
	}
}

func (m *Model) explored(msg ideaExploredMsg) (tea.Model, tea.Cmd) {
	f := m.chat.idea
	if f == nil || msg.run != f.run {
		return m, nil
	}
	f.run = nil
	if msg.err != nil {
		m.chat.say(roleSystem, "could not describe the idea: "+msg.err.Error())
		m.setStatus(msg.err.Error(), statusError)
		m.endIdeas()
		return m, nil
	}
	f.brief = msg.brief
	idea := f.ideas[f.chosen]

	var record strings.Builder
	record.WriteString(msg.brief.Details)
	if details := strings.TrimSpace(msg.brief.Details); details != "" {
		m.chat.say(roleApex, details)
	}
	if len(msg.brief.Questions) > 0 {
		record.WriteString("\n\nA few questions:\n")
		for _, q := range msg.brief.Questions {
			record.WriteString("- " + q + "\n")
		}
	}
	if err := m.chat.sess.Note(context.Background(), store.RoleAssistant, record.String()); err != nil {
		m.setStatus("could not record the turn: "+err.Error(), statusWarn)
	} else {
		m.setStatus(describeUsage(msg.brief.Usage), statusNeutral)
	}

	if len(msg.brief.Questions) == 0 {
		m.askConfirm(idea)
		return m, nil
	}
	f.stage = ideaAsking
	m.askQuestion()
	m.focusInput()
	return m, nil
}

// askQuestion shows the next unanswered question.
func (m *Model) askQuestion() {
	f := m.chat.idea
	qs := f.brief.Questions
	n := len(f.answers)
	m.chat.say(roleQuestion, fmt.Sprintf("Question %d of %d. %s", n+1, len(qs), qs[n]))
	m.rerenderChat()
}

// answerIdea takes what the user typed as the answer to the current question.
func (m *Model) answerIdea(input string) tea.Cmd {
	f := m.chat.idea
	qs := f.brief.Questions
	shown := input
	if shown == "" {
		shown = "(skipped)"
	}
	m.chat.say(roleYou, shown)
	f.answers = append(f.answers, advisor.Answer{Question: qs[len(f.answers)], Answer: input})
	if len(f.answers) < len(qs) {
		m.askQuestion()
		return nil
	}

	var qa strings.Builder
	qa.WriteString("My answers:\n")
	for _, a := range f.answers {
		ans := a.Answer
		if ans == "" {
			ans = "(skipped)"
		}
		fmt.Fprintf(&qa, "- %s\n  %s\n", a.Question, ans)
	}
	if err := m.chat.sess.Note(context.Background(), store.RoleUser, qa.String()); err != nil {
		m.setStatus("could not record the turn: "+err.Error(), statusWarn)
	}
	m.askConfirm(f.ideas[f.chosen])
	return nil
}

// askConfirm puts the last question: make it an action item?
func (m *Model) askConfirm(idea advisor.ProposedIdea) {
	f := m.chat.idea
	f.stage = ideaConfirm
	m.chat.say(roleQuestion, fmt.Sprintf("Make “%s” an action item? y / n", idea.Title))
	m.chat.say(roleSystem, fmt.Sprintf("Yes creates the project under ~/%s, registers it in PROJECTS.md, "+
		"and adds its first action item to Items, ready to dispatch.", defaultProjectParent))
	m.focusInput()
	m.rerenderChat()
}

// defaultProjectParent mirrors where `apex start` puts a project, for the
// sentence that says so. The directory itself is chosen by the IdeaStarter.
const defaultProjectParent = "Development"

// createIdea plans the project from the answers and creates it.
func (m *Model) createIdea() tea.Cmd {
	f := m.chat.idea
	if m.opts.StartIdea == nil {
		m.setStatus("creating projects is not available in this build", statusWarn)
		return nil
	}
	f.stage = ideaCreating
	m.setStatus("", statusNeutral)
	m.rerenderChat()

	sess, start := m.chat.sess, m.opts.StartIdea
	idea, answers := f.ideas[f.chosen], append([]advisor.Answer(nil), f.answers...)
	run := m.newIdeaRun(10 * time.Minute)
	return func() tea.Msg {
		defer run.cancel()
		plan, err := sess.PlanIdea(run.ctx, idea, answers)
		if err != nil {
			return ideaCreatedMsg{run: run, err: err}
		}
		var out bytes.Buffer
		started, err := start(run.ctx, &out, plan)
		return ideaCreatedMsg{run: run, plan: plan, started: started, output: out.String(), err: err}
	}
}

func (m *Model) created(msg ideaCreatedMsg) (tea.Model, tea.Cmd) {
	f := m.chat.idea
	if f == nil || msg.run != f.run {
		return m, nil
	}
	f.run = nil
	if msg.err != nil {
		// The answers are worth keeping: whatever stopped it — a directory
		// already there, a name already registered — may be fixed from
		// another terminal, and y tries again.
		f.stage = ideaConfirm
		m.chat.say(roleSystem, "could not make it an action item: "+msg.err.Error()+
			" — y to try again, n to leave it")
		m.setStatus(firstLine(msg.err.Error()), statusError)
		m.rerenderChat()
		return m, nil
	}

	it := msg.started.Item
	note := fmt.Sprintf("Created %s at %s and registered it in PROJECTS.md. Added %s “%s” to Items — "+
		"tab to Items to dispatch it.", msg.started.ProjectName, msg.started.Dir, it.ID, it.Title)
	m.chat.say(roleSystem, note)
	if err := m.chat.sess.Note(context.Background(), store.RoleAssistant, note); err != nil {
		m.setStatus("could not record the turn: "+err.Error(), statusWarn)
	} else {
		m.setStatus(fmt.Sprintf("added %s · tab to Items to dispatch", it.ID), statusNeutral)
	}
	m.endIdeas()
	return m, tea.Batch(m.loadItemsCmd(), m.loadProjectsCmd(), m.reloadContextQuietlyCmd())
}

// ideasCmd is `/ideas`: new project ideas, in the light of the conversation.
func (m *Model) ideasCmd() tea.Cmd {
	f := &ideaFlow{chosen: -1, stage: ideaExploring}
	m.chat.idea = f
	m.focusInput()
	sess := m.chat.sess
	run := m.newIdeaRun(5 * time.Minute)
	return func() tea.Msg {
		defer run.cancel()
		props, err := sess.ProposeIdeas(run.ctx)
		return ideasProposedMsg{run: run, props: props, err: err}
	}
}

func (m *Model) ideasProposed(msg ideasProposedMsg) (tea.Model, tea.Cmd) {
	f := m.chat.idea
	if f == nil || msg.run != f.run {
		return m, nil
	}
	m.chat.idea = nil
	if msg.err != nil {
		m.chat.say(roleSystem, "could not propose ideas: "+msg.err.Error())
		m.setStatus(msg.err.Error(), statusError)
		m.focusInput()
		m.rerenderChat()
		return m, nil
	}
	if len(msg.props.Ideas) == 0 {
		m.chat.say(roleSystem, "No new ideas this time.")
		m.focusInput()
		m.rerenderChat()
		return m, nil
	}
	m.openIdeas(msg.props.Ideas)
	m.setStatus(describeUsage(msg.props.Usage), statusNeutral)
	m.rerenderChat()
	return m, nil
}

// newIdeaRun starts a cancellable run on the current flow.
func (m *Model) newIdeaRun(timeout time.Duration) *ideaRun {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	run := &ideaRun{ctx: ctx, cancel: cancel}
	m.chat.idea.run = run
	return run
}

// ideaHint is the status line while a flow is open.
func (m *Model) ideaHint() string {
	f := m.chat.idea
	switch f.stage {
	case ideaPick:
		return fmt.Sprintf("%d project idea(s) · ↑↓ and enter to pick · n for none", len(f.ideas))
	case ideaAsking:
		return fmt.Sprintf("question %d of %d · enter answers, empty skips · esc to stop",
			len(f.answers)+1, len(f.brief.Questions))
	case ideaConfirm:
		return "make it an action item? y / n"
	case ideaCreating:
		return "creating the project — esc to stop"
	default:
		if len(f.ideas) == 0 {
			return "thinking of project ideas — esc to stop"
		}
		return "describing the idea — esc to stop"
	}
}

// ideaFooter is the key list while a flow has the keyboard.
func (m *Model) ideaFooter() string {
	switch m.chat.idea.stage {
	case ideaPick:
		return "tab views · ↑↓ move · enter pick · n none · ctrl+c quit"
	case ideaConfirm:
		return "tab views · y make it an action item · n leave it · ctrl+c quit"
	case ideaAsking:
		return "tab views · enter answer · esc stop · ctrl+c quit"
	default:
		return "tab views · esc stop · ctrl+c quit"
	}
}

// renderIdeas draws the list for Apex's pane, as renderProposal draws a
// checklist: plain styled text, indented to Glamour's margin, every line fitted
// to the pane.
func (m *Model) renderIdeas(f *ideaFlow) string {
	width := m.replyWidth() - glamourMargin
	const gutter = 5 // "› 1. "
	textWidth := max(width-gutter, 8)

	var b strings.Builder
	b.WriteString(fitStyled("Project ideas", width, styleLabel))
	for i, idea := range f.ideas {
		b.WriteString("\n")
		pointer := "  "
		if f.stage == ideaPick && !f.closed && i == f.cursor {
			pointer = styleAccent.Render("› ")
		}
		num := fmt.Sprintf("%d. ", i+1)

		titleStyle := styleTitle
		switch {
		case f.chosen == i:
			titleStyle = styleOK
		case f.chosen >= 0 || f.closed:
			titleStyle = styleDim
		}
		title := strings.TrimSpace(idea.Title)
		if f.chosen == i {
			title += " — picked"
		}
		for j, line := range fitLines(title, textWidth) {
			if j == 0 {
				b.WriteString(pointer + num + titleStyle.Render(line))
				continue
			}
			b.WriteString("\n" + strings.Repeat(" ", gutter) + titleStyle.Render(line))
		}

		// Two lines of the pitch: enough to choose between them, and the
		// whole of it arrives once one is picked.
		lines := fitLines(firstParagraph(idea.Pitch), textWidth)
		if len(lines) > 2 {
			lines = lines[:2]
			lines[1] = truncate(lines[1]+" …", textWidth)
		}
		for _, line := range lines {
			if line == "" {
				continue
			}
			b.WriteString("\n" + strings.Repeat(" ", gutter) + styleDim.Render(line))
		}
	}
	return indentBlock(b.String(), glamourMargin)
}

// firstParagraph is a pitch's opening paragraph, folded onto one line.
func firstParagraph(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i > 0 {
		s = s[:i]
	}
	return strings.Join(strings.Fields(s), " ")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
