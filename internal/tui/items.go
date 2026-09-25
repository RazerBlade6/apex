package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/RazerBlade6/apex/internal/store"
)

// The items view (DESIGN.md §12): action items grouped by project, filterable
// by status, `enter` dispatches.
//
// One addition to the spec, made deliberately. `enter` asks for confirmation
// before it dispatches. A dispatch is not a navigation: it takes the project's
// exclusive lock, spends Claude Code subscription quota out of the same window
// as the user's own sessions, and lets an agent edit files in a real project.
// Every other irreversible thing in Apex is opt-in by name — `--bypass-permissions`,
// pruning an orphaned project, marking an item done — and a single keystroke
// starting one from a list the cursor is already moving through would be the
// exception. The confirmation is one key.

// itemStatuses is the lifecycle from DESIGN.md §7, in order, with "" meaning
// no filter.
var itemStatuses = []string{
	"", store.ItemProposed, store.ItemAccepted, store.ItemInProgress,
	store.ItemInReview, store.ItemDone, store.ItemDismissed,
}

type itemRow struct {
	// group is true for a project heading rather than an item.
	group bool
	label string
	item  store.ActionItem
}

type itemsState struct {
	items    []store.ActionItem
	names    map[string]string
	rows     []itemRow
	cursor   int
	offset   int
	filter   int // index into itemStatuses
	height   int
	loaded   bool
	loadErr  error
	confirm  string // the item id awaiting a y/n answer
	dispatch *dispatchRun
	output   []string
}

// dispatchRun is one in-flight dispatch. Its output arrives line by line over
// a pipe, which is the same drain-and-re-issue pattern the chat stream uses.
type dispatchRun struct {
	itemID string
	lines  <-chan string
	done   <-chan error
	cancel context.CancelFunc
}

type itemsLoadedMsg struct {
	items    []store.ActionItem
	projects []store.Project
	err      error
}

type dispatchLineMsg struct {
	h    *dispatchRun
	line string
	open bool
}

type dispatchDoneMsg struct {
	h   *dispatchRun
	err error
}

func (m *Model) initItems() {
	m.items = itemsState{names: map[string]string{}, height: 10}
}

func (m *Model) itemsHint() string {
	if m.items.dispatch != nil {
		return "dispatching " + m.items.dispatch.itemID + " — esc to stop"
	}
	if m.items.confirm != "" {
		return "dispatch " + m.items.confirm + " to the coding agent? y / n"
	}
	if m.items.loadErr != nil {
		return m.items.loadErr.Error()
	}
	filter := itemStatuses[m.items.filter]
	if filter == "" {
		filter = "all"
	}
	return fmt.Sprintf("%d item(s) · filter: %s", len(m.items.visible()), filter)
}

// visible returns the items the current filter admits.
func (s *itemsState) visible() []store.ActionItem {
	want := itemStatuses[s.filter]
	if want == "" {
		return s.items
	}
	var out []store.ActionItem
	for _, it := range s.items {
		if it.Status == want {
			out = append(out, it)
		}
	}
	return out
}

// rebuild lays the visible items out as rows, grouped by project.
func (s *itemsState) rebuild() {
	grouped := map[string][]store.ActionItem{}
	for _, it := range s.visible() {
		grouped[it.ProjectSlug] = append(grouped[it.ProjectSlug], it)
	}
	slugs := make([]string, 0, len(grouped))
	for slug := range grouped {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	s.rows = nil
	for _, slug := range slugs {
		name := s.names[slug]
		if name == "" {
			name = slug
		}
		s.rows = append(s.rows, itemRow{group: true, label: name})
		for _, it := range grouped[slug] {
			s.rows = append(s.rows, itemRow{item: it})
		}
	}
	s.clampCursor()
}

// clampCursor keeps the cursor on a selectable row. A group heading is not
// one, so the cursor steps past it rather than sitting on it.
func (s *itemsState) clampCursor() {
	if len(s.rows) == 0 {
		s.cursor = 0
		return
	}
	if s.cursor >= len(s.rows) {
		s.cursor = len(s.rows) - 1
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
	if !s.rows[s.cursor].group {
		return
	}
	for i := s.cursor; i < len(s.rows); i++ {
		if !s.rows[i].group {
			s.cursor = i
			return
		}
	}
	for i := s.cursor; i >= 0; i-- {
		if !s.rows[i].group {
			s.cursor = i
			return
		}
	}
}

func (s *itemsState) move(delta int) {
	for i := s.cursor + delta; i >= 0 && i < len(s.rows); i += delta {
		if !s.rows[i].group {
			s.cursor = i
			return
		}
	}
}

// selected returns the item under the cursor.
func (s *itemsState) selected() (store.ActionItem, bool) {
	if s.cursor < 0 || s.cursor >= len(s.rows) || s.rows[s.cursor].group {
		return store.ActionItem{}, false
	}
	return s.rows[s.cursor].item, true
}

func (m *Model) updateItems(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case itemsLoadedMsg:
		m.items.loaded = true
		m.items.loadErr = msg.err
		if msg.err != nil {
			return m, nil
		}
		m.items.items = msg.items
		m.items.names = map[string]string{}
		for _, p := range msg.projects {
			m.items.names[p.Slug] = p.Name
		}
		m.items.rebuild()
		return m, nil

	case dispatchLineMsg:
		if msg.h != m.items.dispatch {
			return m, nil
		}
		if !msg.open {
			return m, nil
		}
		m.items.appendOutput(msg.line)
		return m, waitForDispatchLine(msg.h)

	case dispatchDoneMsg:
		if msg.h != m.items.dispatch {
			return m, nil
		}
		m.items.dispatch = nil
		msg.h.cancel()
		if msg.err != nil {
			m.items.appendOutput("")
			m.items.appendOutput("dispatch failed: " + msg.err.Error())
			m.setStatus(msg.err.Error(), statusError)
		} else {
			m.setStatus(msg.h.itemID+" finished — review the diff, then decide if it is done", statusNeutral)
		}
		// The item's status moved, so the list is stale.
		return m, m.loadItemsCmd()

	case tea.KeyMsg:
		return m.itemsKey(msg)
	}
	return m, nil
}

func (m *Model) itemsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A pending confirmation swallows every key but its answer, so a stray
	// keystroke cannot start a dispatch.
	if m.items.confirm != "" {
		switch msg.String() {
		case "y", "Y":
			id := m.items.confirm
			m.items.confirm = ""
			return m, m.startDispatch(id)
		default:
			m.items.confirm = ""
			m.setStatus("not dispatched", statusNeutral)
			return m, nil
		}
	}

	switch msg.String() {
	case "up", "k":
		m.items.move(-1)
	case "down", "j":
		m.items.move(1)
	case "f":
		m.items.filter = (m.items.filter + 1) % len(itemStatuses)
		m.items.cursor = 0
		m.items.rebuild()
	case "r":
		return m, m.loadItemsCmd()
	case "esc":
		if m.items.dispatch != nil {
			m.items.dispatch.cancel()
			m.setStatus("cancelling the dispatch…", statusWarn)
			return m, nil
		}
		m.items.output = nil
	case "enter":
		if m.items.dispatch != nil {
			m.setStatus("a dispatch is already running", statusWarn)
			return m, nil
		}
		if m.opts.Dispatch == nil {
			m.setStatus("dispatch is not available in this build", statusWarn)
			return m, nil
		}
		item, ok := m.items.selected()
		if !ok {
			return m, nil
		}
		switch item.Status {
		case store.ItemDone, store.ItemDismissed:
			m.setStatus(item.ID+" is "+item.Status+", so there is nothing to dispatch", statusWarn)
			return m, nil
		}
		m.items.confirm = item.ID
	}
	return m, nil
}

// startDispatch runs the injected Dispatcher and streams its output.
//
// The output arrives over an io.Pipe read line by line, which is the same
// shape as the provider stream: one message per line, re-issued by Update. The
// alternative — buffering the whole run and showing it at the end — would make
// a four-minute dispatch indistinguishable from a hang, which is exactly what
// cmd/apex's streaming exists to avoid.
func (m *Model) startDispatch(itemID string) tea.Cmd {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())

	lines := make(chan string, 64)
	done := make(chan error, 1)
	h := &dispatchRun{itemID: itemID, lines: lines, done: done, cancel: cancel}
	m.items.dispatch = h
	m.items.output = []string{"dispatching " + itemID + "…"}
	m.setStatus("", statusNeutral)

	go func() {
		err := m.opts.Dispatch(ctx, pw, itemID)
		// Closing the writer ends the scanner below, which closes lines,
		// which is what lets the reader goroutine finish before done is
		// reported.
		pw.CloseWithError(err) //nolint:errcheck
		done <- err
	}()
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 16<<10), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		pr.Close() //nolint:errcheck
	}()

	return tea.Batch(waitForDispatchLine(h), waitForDispatchDone(h))
}

func waitForDispatchLine(h *dispatchRun) tea.Cmd {
	return func() tea.Msg {
		line, open := <-h.lines
		return dispatchLineMsg{h: h, line: line, open: open}
	}
}

func waitForDispatchDone(h *dispatchRun) tea.Cmd {
	return func() tea.Msg {
		return dispatchDoneMsg{h: h, err: <-h.done}
	}
}

func (m *Model) cancelDispatch() {
	if m.items.dispatch == nil {
		return
	}
	m.items.dispatch.cancel()
	m.items.dispatch = nil
}

// outputLimit bounds the retained dispatch log in memory. The full transcript
// is on disk at ~/.apex/logs/exec/<run-id>.log, which is where anyone reading
// a failed run should go; this is a live tail, not an archive.
const outputLimit = 500

func (s *itemsState) appendOutput(line string) {
	s.output = append(s.output, line)
	if len(s.output) > outputLimit {
		s.output = s.output[len(s.output)-outputLimit:]
	}
}

func (m *Model) itemsView() string {
	h := m.items.height
	if len(m.items.output) > 0 {
		// Split the pane: the list above, the run below.
		listH := h / 2
		if listH < 3 {
			listH = 3
		}
		runH := h - listH - 1
		if runH < 3 {
			runH = 3
		}
		return padLines(m.renderItemList(listH), listH) + "\n" +
			padLines(m.renderRun(runH), runH)
	}
	return padLines(m.renderItemList(h), h)
}

func (m *Model) renderItemList(height int) string {
	switch {
	case m.items.loadErr != nil:
		return styleError.Render("could not read the action items: " + m.items.loadErr.Error())
	case !m.items.loaded:
		return styleDim.Render("loading…")
	case len(m.items.rows) == 0 && len(m.items.items) == 0:
		return styleDim.Render("No action items yet. Run `apex review` to generate some.")
	case len(m.items.rows) == 0:
		return styleDim.Render("No items with status " + itemStatuses[m.items.filter] + ". Press f to change the filter.")
	}

	// Keep the cursor on screen without redrawing the whole list every frame.
	if m.items.cursor < m.items.offset {
		m.items.offset = m.items.cursor
	}
	if m.items.cursor >= m.items.offset+height {
		m.items.offset = m.items.cursor - height + 1
	}
	if m.items.offset < 0 {
		m.items.offset = 0
	}

	var b strings.Builder
	end := min(m.items.offset+height, len(m.items.rows))
	for i := m.items.offset; i < end; i++ {
		if i > m.items.offset {
			b.WriteString("\n")
		}
		row := m.items.rows[i]
		if row.group {
			b.WriteString(styleLabel.Render(row.label))
			continue
		}
		b.WriteString(m.renderItemRow(row.item, i == m.items.cursor))
	}
	return b.String()
}

func (m *Model) renderItemRow(item store.ActionItem, selected bool) string {
	marker := "  "
	if selected {
		marker = "› "
	}
	line := fmt.Sprintf("%s%-7s %-12s %-7s %s",
		marker, item.ID, item.Status, orDash(item.Effort), item.Title)
	// Truncate to the inner width, then pad back out to it: the selected row
	// carries a background, and a highlight that stops at the end of the title
	// looks like a fault rather than a cursor.
	w := m.innerWidth()
	line = padTo(truncate(line, w), w)
	switch {
	case selected:
		return styleSelected.Render(line)
	case item.Status == store.ItemDone:
		return styleOK.Render(line)
	case item.Status == store.ItemDismissed:
		return styleDim.Render(line)
	default:
		return line
	}
}

func (m *Model) renderRun(height int) string {
	lines := m.items.output
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(styleDim.Render(truncate(line, m.innerWidth())))
	}
	return b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
