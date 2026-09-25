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
	m.render = newRenderer(contentWidth(m.width))
	m.initChat()
	m.initItems()
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
		m.render = newRenderer(contentWidth(m.width))
		m.layout()
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
	case contextLoadedMsg, streamEventMsg, observedMsg:
		return m.updateChat(msg)
	case itemsLoadedMsg, dispatchLineMsg, dispatchDoneMsg:
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
	m.cancelDispatch()
	return tea.Quit
}

func (m *Model) setView(v view) {
	m.view = view((int(v) + len(views)) % len(views))
	m.layout()
	if m.view == viewChat {
		m.chat.ta.Focus()
		return
	}
	m.chat.ta.Blur()
}

func (m *Model) setStatus(text string, kind statusKind) {
	m.status = strings.TrimSpace(strings.ReplaceAll(text, "\n", " · "))
	m.statusIs = kind
}

// View renders the whole frame: header, status line, body, footer.
func (m *Model) View() string {
	if !m.ready {
		return "apex\n\nstarting…\n"
	}
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")
	b.WriteString(m.statusLine())
	b.WriteString("\n")

	switch m.view {
	case viewChat:
		b.WriteString(m.chatView())
	case viewItems:
		b.WriteString(m.itemsView())
	default:
		b.WriteString(m.projectsView())
	}
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

func (m *Model) header() string {
	tabs := make([]string, 0, len(views))
	for _, v := range views {
		label := fmt.Sprintf(" %s ", v)
		if v == m.view {
			tabs = append(tabs, styleTabActive.Render(label))
			continue
		}
		tabs = append(tabs, styleTab.Render(label))
	}
	left := strings.Join(tabs, "")
	right := styleDim.Render("apex " + m.opts.Version)
	return padBetween(left, right, m.width)
}

func (m *Model) statusLine() string {
	if m.status == "" {
		return styleDim.Render(truncate(m.hint(), m.width))
	}
	text := truncate(m.status, m.width)
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
	case viewItems:
		keys = "tab views · ↑↓ move · f filter · enter dispatch · r reload · ctrl+c quit"
	default:
		keys = "tab views · ↑↓ move · r reload · ctrl+c quit"
	}
	return styleDim.Render(truncate(keys, m.width))
}

// bodyHeight is how many rows the view's body gets: everything but the
// header, the status line, the footer, and the blank line above it.
func (m *Model) bodyHeight() int {
	h := m.height - 4
	if h < 3 {
		return 3
	}
	return h
}
