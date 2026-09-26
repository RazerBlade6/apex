// Package tui is Apex's Bubble Tea application (DESIGN.md §12).
//
// Three views switched by tab: chat, action items, and the project registry.
//
// # This package imports downward only
//
// Nothing in advisor/, provider/, executor/ or store/ may import it, and a
// test in this package walks the module to prove it. That boundary is the
// whole of DESIGN.md §17's "non-terminal frontend" deferral: as long as no
// lower layer reaches up into a terminal, a second frontend is a new package
// beside this one rather than a refactor of everything beneath it.
//
// The one place that boundary would naturally break is dispatch. Running an
// action item means the exec_runs row, the per-project lock and the status
// transitions — bookkeeping that lives in cmd/apex, above this package.
// Rather than import upwards or duplicate it, the TUI takes a Dispatcher
// callback and streams whatever it writes.
//
// # Everything is a message
//
// Model requests are drained by a tea.Cmd that returns one message per
// provider event and re-issues itself, so tokens arrive in Update like any
// other message (DESIGN.md §12). A dispatch does the same thing over an
// io.Pipe. Nothing in here blocks the event loop, and every long-running
// operation carries a context the user can cancel.
package tui

import (
	"context"
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/store"
)

// Dispatcher runs one action item to completion, writing the agent's output
// to w as it arrives.
//
// It is a callback rather than a call into internal/executor because a
// dispatch is more than a subprocess: it takes the project's exclusive lock,
// opens an exec_runs row and moves the item through its lifecycle, all of
// which lives in cmd/apex. Injecting it keeps this package below that one.
type Dispatcher func(ctx context.Context, w io.Writer, itemID string) error

// Options is everything the TUI needs from its caller.
type Options struct {
	Store   *store.Store
	Config  *config.Config
	Advisor *advisor.Advisor
	// Root is the Apex state directory, for the identity files the
	// observation extractor writes to.
	Root string
	// Dispatch runs an action item. Nil disables dispatch and the items
	// view says so rather than pretending.
	Dispatch Dispatcher
	// StartIdea creates the project a chosen idea became, and its first
	// action item. Nil disables that last step and the chat says so.
	StartIdea IdeaStarter
	// Version is shown in the header.
	Version string

	// Input and Output override the terminal. They exist so the application
	// can be launched non-interactively — which is the only way to verify
	// that a Bubble Tea program actually starts, since no unit test drives
	// a real renderer. Nothing in Apex sets them in production.
	Input  io.Reader
	Output io.Writer
}

// view identifies which of the three surfaces is showing.
type view int

const (
	viewChat view = iota
	viewItems
	viewProjects
)

func (v view) String() string {
	switch v {
	case viewChat:
		return "Chat"
	case viewItems:
		return "Items"
	case viewProjects:
		return "Projects"
	default:
		return "?"
	}
}

var views = []view{viewChat, viewItems, viewProjects}

// Model is the application.
type Model struct {
	opts   Options
	view   view
	width  int
	height int
	ready  bool

	// status is the one-line message under the header: an error, a cost
	// report, a confirmation. It is cleared by the next interaction.
	status   string
	statusIs statusKind

	chat     chatState
	items    itemsState
	projects projectsState

	render *renderer
	// glamourStyle is the style name resolved at construction and reused for
	// every later rebuild, so that no terminal query happens while the
	// program holds stdin.
	glamourStyle string
}

type statusKind int

const (
	statusNeutral statusKind = iota
	statusWarn
	statusError
)

// New builds the model without starting a program, so tests can drive Update
// and View directly.
func New(opts Options) *Model {
	m := &Model{opts: opts, width: 80, height: 24}
	// Resolved once, here, because New runs before the Bubble Tea program
	// starts and this asks the terminal a question (see detectGlamourStyle).
	m.glamourStyle = detectStyle()
	m.render = newRenderer(m.replyWidth(), m.glamourStyle)
	m.initChat()
	m.initItems()
	m.layout()
	return m
}

// Run starts the Bubble Tea program and blocks until the user quits.
//
// Cancelling ctx stops it: tea.WithContext makes the program return, and
// every in-flight stream is cancelled by the same context on the way out.
func Run(ctx context.Context, opts Options) error {
	m := New(opts)

	teaOpts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithContext(ctx)}
	if opts.Input != nil {
		teaOpts = append(teaOpts, tea.WithInput(opts.Input))
	}
	if opts.Output != nil {
		teaOpts = append(teaOpts, tea.WithOutput(opts.Output))
	}

	p := tea.NewProgram(m, teaOpts...)
	if _, err := p.Run(); err != nil {
		// A cancelled context is how the user quits with Ctrl-C at the
		// shell, not a failure worth reporting as one.
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// Init loads everything the first frame needs.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.loadContextCmd(),
		m.loadItemsCmd(),
		m.loadProjectsCmd(),
		m.chat.ta.Focus(),
	)
}

// Update is the single entry point for every event.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.render = newRenderer(m.replyWidth(), m.glamourStyle)
		m.layout()
		// The cached renders were measured against the old frame, and a resize
		// can cross the two-pane threshold as well as change the wrap width.
		m.chat.invalidateRenders()
		m.rerenderChat()
		return m, nil

	case tea.KeyMsg:
		// Tab switches views before anything else sees the key
		// (DESIGN.md §12). The chat input would otherwise swallow it as an
		// indent, and view switching is the one binding that has to work
		// from every surface.
		switch msg.String() {
		case "ctrl+c":
			return m, m.quit()
		case "tab":
			m.setView(m.view + 1)
			return m, nil
		case "shift+tab":
			m.setView(m.view + view(len(views)) - 1)
			return m, nil
		}
		return m.updateKey(msg)

	// Everything a tea.Cmd produces goes to the view that owns it, not to the
	// view that happens to be focused. Routing these by m.view was the first
	// real bug in this package: Init loads all three surfaces at once, the
	// chat view is the default, so the items and projects loads arrived while
	// chat was focused and were dropped — leaving both views reading
	// "loading…" forever with no error anywhere. The same applies to a stream
	// or a dispatch the user tabbed away from mid-flight.
	case contextLoadedMsg, streamEventMsg, observedMsg, proposalsMsg, recordedMsg, contextReloadedMsg,
		ideasProposedMsg, ideaExploredMsg, ideaCreatedMsg:
		return m.updateChat(msg)
	case itemsLoadedMsg, dispatchLineMsg, dispatchDoneMsg, gitStateMsg:
		return m.updateItems(msg)
	case projectsLoadedMsg:
		return m.updateProjects(msg)
	}

	// Anything left is a component's own message — a cursor blink, a viewport
	// tick — and belongs to whatever is focused.
	switch m.view {
	case viewChat:
		return m.updateChat(msg)
	case viewItems:
		return m.updateItems(msg)
	default:
		return m.updateProjects(msg)
	}
}

// updateKey routes a key to the focused view.
func (m *Model) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.view {
	case viewChat:
		return m.updateChat(msg)
	case viewItems:
		return m.updateItems(msg)
	default:
		return m.updateProjects(msg)
	}
}

// quit cancels anything in flight before the program returns. A stream left
// running would keep a subprocess alive past the last frame.
func (m *Model) quit() tea.Cmd {
	m.cancelStream()
	m.cancelProposing()
	if f := m.chat.idea; f != nil && f.run != nil {
		f.run.cancel()
	}
	m.cancelDispatch()
	return tea.Quit
}

func (m *Model) setView(v view) {
	m.view = view((int(v) + len(views)) % len(views))
	m.layout()
	// The input stays blurred under an open checklist or list, which has
	// the keyboard until it is answered.
	m.focusInput()
}

func (m *Model) setStatus(text string, kind statusKind) {
	m.status = strings.TrimSpace(strings.ReplaceAll(text, "\n", " · "))
	m.statusIs = kind
}

// innerWidth is the width every line inside the box is measured against.
//
// It is contentWidth, except in a terminal too narrow to hold it. The
// 40-column floor on contentWidth is there to keep prose readable, but a floor
// wider than the screen would push the border past the last column, where it
// wraps and the whole frame comes apart. In that case the box gives up the
// floor rather than the fit.
func (m *Model) innerWidth() int {
	w := contentWidth(m.width)
	if fits := m.width - boxChrome; w > fits {
		w = fits
	}
	if w < 1 {
		return 1
	}
	return w
}

// frameWidth is the box's rendered outer width: the inner width plus the two
// border columns and two padding columns. Every row of the frame is laid out
// against it and the block is then centred in the terminal as a unit, so that
// the tab row, the status line, the box and the footer share one left edge.
func (m *Model) frameWidth() int {
	return m.innerWidth() + boxChrome
}

// View renders the whole frame: tab row, blank, status line, body, footer.
//
// That is bodyHeight+6 rows against a terminal of m.height, leaving the one row
// of slack the frame has always left — a frame that fills the last row scrolls
// the alt screen on some terminals, and the cost of the slack is one unused
// row.
func (m *Model) View() string {
	if !m.ready {
		return "apex\n\nstarting…\n"
	}

	// Joined left-aligned first, which pads every row to the box's width, so
	// that centring moves the block as a unit. Centring the rows individually
	// would leave the status line and the footer drifting against the box.
	block := lipgloss.JoinVertical(lipgloss.Left,
		m.header(),
		"",
		styleGutter.Render(m.statusLine()),
		m.bodyBlock(),
		styleGutter.Render(m.footer()),
	)
	return lipgloss.PlaceHorizontal(m.width, lipgloss.Center, block)
}

// bodyBlock is the finished content area, already frameWidth wide.
//
// For the projects view — and for chat and items in a terminal too small for
// their panes — that is the one bounding box, with the view's body inside it.
// The chat view's two panes and the items view's three bring their own boxes,
// sized so that they plus the columns between them come to the same width.
func (m *Model) bodyBlock() string {
	if m.view == viewChat && m.chatTwoPane() {
		return m.chatPanes()
	}
	if m.view == viewItems && m.itemsThreePane() {
		return m.itemsPanes()
	}

	var body string
	switch m.view {
	case viewChat:
		body = m.chatView()
	case viewItems:
		body = m.itemsView()
	default:
		body = m.projectsView()
	}

	// Width is the inner width plus the padding, because lipgloss counts
	// padding inside Width and adds the border outside it. MaxHeight is the
	// backstop: content that somehow still overflows costs the bottom border
	// rather than a scrolled screen.
	return styleBox.
		Width(m.innerWidth() + 2).
		Height(m.bodyHeight()).
		MaxHeight(m.bodyHeight() + 2).
		Render(padLines(body, m.bodyHeight()))
}

// header is the tab row: the three views, centred over the box.
func (m *Model) header() string {
	tabs := make([]string, 0, len(views))
	for _, v := range views {
		label := fmt.Sprintf("  %s  ", v)
		if v == m.view {
			tabs = append(tabs, styleTabActive.Render(label))
			continue
		}
		tabs = append(tabs, styleTab.Render(label))
	}
	return lipgloss.PlaceHorizontal(m.frameWidth(), lipgloss.Center, strings.Join(tabs, tabGap))
}

// tabGap is the separation between two tabs. Together with the two columns of
// padding inside each label it is what keeps the row from reading as one word.
const tabGap = "   "

func (m *Model) statusLine() string {
	if m.status == "" {
		return styleDim.Render(truncate(m.hint(), m.innerWidth()))
	}
	text := truncate(m.status, m.innerWidth())
	switch m.statusIs {
	case statusError:
		return styleError.Render(text)
	case statusWarn:
		return styleWarn.Render(text)
	default:
		return styleDim.Render(text)
	}
}

// hint is the default status line: what this view is for, in one line.
func (m *Model) hint() string {
	switch m.view {
	case viewChat:
		return m.chatHint()
	case viewItems:
		return m.itemsHint()
	default:
		return m.projectsHint()
	}
}

func (m *Model) footer() string {
	var keys string
	switch m.view {
	case viewChat:
		keys = "tab views · enter send · esc stop · ctrl+c quit"
		switch {
		case m.chat.proposal != nil:
			keys = "tab views · ↑↓ move · space tick · y add · n discard · ctrl+c quit"
		case m.chat.idea != nil:
			keys = m.ideaFooter()
		}
	case viewItems:
		keys = "tab views · ↑↓ move · f filter · enter dispatch · r reload · ctrl+c quit"
	default:
		keys = "tab views · ↑↓ move · r reload · ctrl+c quit"
	}
	// The version sits here rather than on the tab row: a right-aligned element
	// beside the tabs would make "centred" a lie, and the footer already has a
	// left-hand half and nothing in its right.
	inner := m.innerWidth()
	return styleDim.Render(padBetween(truncate(keys, inner), "apex "+m.opts.Version, inner))
}

// bodyHeight is how many inner rows the box holds: everything but the tab row,
// the blank line under it, the status line, the box's own two border rows, the
// footer, and the row of slack the frame leaves at the bottom.
func (m *Model) bodyHeight() int {
	h := m.height - 7
	if h < 3 {
		return 3
	}
	return h
}
