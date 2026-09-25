package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/RazerBlade6/apex/internal/project"
	"github.com/RazerBlade6/apex/internal/store"
)

// The items view (DESIGN.md §12): action items grouped by project, filterable
// by status, `enter` dispatches. Where there is room it is three boxes (UI.md
// §3) — the selected item's project, the items as cards, the selected item's
// detail — and below that it is the single list it always was.
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
	projects map[string]store.Project
	digests  map[string]store.Digest
	rows     []itemRow
	cursor   int // the selected row, or -1 when nothing is selected
	offset   int // the fallback list's first visible row
	filter   int // index into itemStatuses
	height   int
	loaded   bool
	loadErr  error
	confirm  string // the item id awaiting a y/n answer
	dispatch *dispatchRun
	output   []string
	// outputFor is the item the dispatch log belongs to. It outlives the
	// dispatch itself, because the log stays on screen after the run ends.
	outputFor string

	// cardOffset is the card list's first visible line and detailOffset the
	// detail pane's. They are lines rather than rows, and separate from
	// offset, because the three-pane layout draws a different number of lines
	// per item than the fallback does.
	cardOffset   int
	detailOffset int

	// git is each project's repository state, read on demand the first time
	// an item from that project is selected and kept until `r` or a dispatch
	// into that project. gitLoading marks a read in flight, so that moving the
	// cursor back and forth does not start a second one.
	git        map[string]gitRead
	gitLoading map[string]bool
}

// gitRead is one finished read of a project's repository. err is set only
// when the read itself did not finish; a directory that is not a repository
// is a state, reported through the GitState's own Note.
type gitRead struct {
	state project.GitState
	err   error
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
	digests  []store.Digest
	err      error
}

// gitStateMsg carries one project's repository state back from ensureGitCmd.
type gitStateMsg struct {
	slug  string
	state project.GitState
	err   error
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
	m.items = itemsState{
		names:      map[string]string{},
		projects:   map[string]store.Project{},
		digests:    map[string]store.Digest{},
		git:        map[string]gitRead{},
		gitLoading: map[string]bool{},
		cursor:     -1,
		height:     10,
	}
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
	hint := fmt.Sprintf("%d item(s) · filter: %s", len(m.items.visible()), filter)
	// The scroll keys are named here rather than in the footer, and only
	// once there is a detail box to scroll: in the footer they pushed the key
	// list past the point where the version fits, which dropped it from the
	// Items tab at every width under about a hundred columns.
	switch {
	case len(m.items.rows) > 0 && m.items.cursor < 0:
		hint += " · ↑↓ to select"
	case m.items.cursor >= 0 && m.itemsThreePane():
		hint += " · pgup/pgdn scroll the detail"
	}
	return hint
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

// rebuild lays the visible items out as rows, grouped by project, keeping
// whatever was selected selected.
func (s *itemsState) rebuild() {
	keep := s.selectedID()
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
	s.reselect(keep)
}

// reselect puts the cursor back on the item with the given id, or on nothing.
//
// The selection is kept by item rather than by row index. A reload, a filter
// change or a finished dispatch rebuilds the rows, and an index that survives
// that points at whichever item now happens to occupy the slot — which the
// side panes would then describe, and `enter` would then offer to dispatch,
// with nothing on screen to say the selection had moved. An item the rebuild
// no longer shows, because it was filtered out or deleted, leaves nothing
// selected rather than something the user did not choose; and nothing
// selected stays that way.
func (s *itemsState) reselect(id string) {
	s.cursor = -1
	if id == "" {
		return
	}
	for i, row := range s.rows {
		if !row.group && row.item.ID == id {
			s.cursor = i
			return
		}
	}
}

// move steps the cursor to the next item in the direction of delta, past any
// group heading. From no selection either direction selects the first item:
// the view opens with nothing selected, and the first key the user presses is
// asking for something to be.
func (s *itemsState) move(delta int) {
	if s.cursor < 0 {
		for i, row := range s.rows {
			if !row.group {
				s.cursor = i
				return
			}
		}
		return
	}
	for i := s.cursor + delta; i >= 0 && i < len(s.rows); i += delta {
		if !s.rows[i].group {
			s.cursor = i
			return
		}
	}
}

// selectedID is the id of the item under the cursor, or "" when there is none.
func (s *itemsState) selectedID() string {
	it, _ := s.selected()
	return it.ID
}

// find returns the item with the given id, filtered out or not.
func (s *itemsState) find(id string) (store.ActionItem, bool) {
	for _, it := range s.items {
		if it.ID == id {
			return it, true
		}
	}
	return store.ActionItem{}, false
}

// selected returns the item under the cursor.
func (s *itemsState) selected() (store.ActionItem, bool) {
	if s.cursor < 0 || s.cursor >= len(s.rows) || s.rows[s.cursor].group {
		return store.ActionItem{}, false
	}
	return s.rows[s.cursor].item, true
}

// updateItems handles every message the items view owns.
//
// The bookkeeping that follows a change of selection is done here, once, after
// the message rather than at each place the selection can move: a cursor key,
// a filter change and a reload all move it, and a reload moves it without the
// user doing anything. So the detail pane's scroll is reset whenever the
// selected item is no longer the one it was, and the selected item's project
// has its git state read if nothing has read it yet. ensureGitCmd is a no-op
// when the state is cached or already being read, which is what makes calling
// it after every message cheap.
func (m *Model) updateItems(msg tea.Msg) (tea.Model, tea.Cmd) {
	before := m.items.selectedID()
	model, cmd := m.itemsMsg(msg)
	if m.items.selectedID() != before {
		m.items.detailOffset = 0
	}
	if git := m.ensureGitCmd(); git != nil {
		cmd = tea.Batch(cmd, git)
	}
	return model, cmd
}

func (m *Model) itemsMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case itemsLoadedMsg:
		m.items.loaded = true
		m.items.loadErr = msg.err
		if msg.err != nil {
			return m, nil
		}
		m.items.items = msg.items
		m.items.names = map[string]string{}
		m.items.projects = make(map[string]store.Project, len(msg.projects))
		for _, p := range msg.projects {
			m.items.names[p.Slug] = p.Name
			m.items.projects[p.Slug] = p
		}
		m.items.digests = make(map[string]store.Digest, len(msg.digests))
		for _, d := range msg.digests {
			m.items.digests[d.ProjectSlug] = d
		}
		m.items.rebuild()
		return m, nil

	case gitStateMsg:
		delete(m.items.gitLoading, msg.slug)
		m.items.git[msg.slug] = gitRead{state: msg.state, err: msg.err}
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
		// The item's status moved, so the list is stale, and the agent will
		// have changed files, so the project's git state is too.
		if it, ok := m.items.find(msg.h.itemID); ok {
			delete(m.items.git, it.ProjectSlug)
		}
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
		m.items.rebuild()
	case "pgdown", "ctrl+d":
		m.items.detailOffset += m.detailStep()
	case "pgup", "ctrl+u":
		m.items.detailOffset = max(m.items.detailOffset-m.detailStep(), 0)
	case "r":
		// A reload is also how the user asks for fresh git state: nothing
		// else invalidates it short of a dispatch.
		m.items.git = map[string]gitRead{}
		m.items.gitLoading = map[string]bool{}
		return m, m.loadItemsCmd()
	case "esc":
		if m.items.dispatch != nil {
			m.items.dispatch.cancel()
			m.setStatus("cancelling the dispatch…", statusWarn)
			return m, nil
		}
		// One layer at a time: the finished run's log first, because it is
		// covering the item, then the selection itself.
		if len(m.items.output) > 0 {
			m.items.output = nil
			return m, nil
		}
		m.items.cursor = -1
		m.items.detailOffset = 0
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
			if len(m.items.rows) > 0 {
				m.setStatus("select an item with ↑↓ first", statusNeutral)
			}
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
	m.items.outputFor = itemID
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

// itemsStateMessage returns the sentence that stands in for the list when there is
// no list to show — loading, a failed load, nothing at all, nothing the filter
// admits — and the style it is drawn in. ok is false when there are rows.
func (m *Model) itemsStateMessage() (text string, style lipgloss.Style, ok bool) {
	switch {
	case m.items.loadErr != nil:
		return "could not read the action items: " + m.items.loadErr.Error(), styleError, true
	case !m.items.loaded:
		return "loading…", styleDim, true
	case len(m.items.rows) == 0 && len(m.items.items) == 0:
		return "No action items yet. Run `apex review` to generate some.", styleDim, true
	case len(m.items.rows) == 0:
		return "No items with status " + itemStatuses[m.items.filter] + ". Press f to change the filter.", styleDim, true
	}
	return "", styleDim, false
}

func (m *Model) renderItemList(height int) string {
	if text, style, ok := m.itemsStateMessage(); ok {
		return fitStyled(text, m.innerWidth(), style)
	}

	// Keep the cursor on screen without redrawing the whole list every frame.
	// With nothing selected there is nothing to keep on screen, and the
	// offset is left where it was.
	if c := m.items.cursor; c >= 0 {
		if c < m.items.offset {
			m.items.offset = c
		}
		if c >= m.items.offset+height {
			m.items.offset = c - height + 1
		}
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

// itemsThreePane is the predicate for the three-box layout (UI.md §3). Below
// it the view falls back to the single list and run log it used to be, which
// needs no more than a column per character of an item's id.
func (m *Model) itemsThreePane() bool {
	return m.innerWidth() >= minThreePaneWidth && m.bodyHeight() >= minThreePaneHeight
}

// itemsPaneWidths splits the frame between the three panes.
//
// Each pane pays paneChrome, and two paneGap columns sit between them, so
// against the single box's frameWidth (innerWidth + boxChrome) the trio has
//
//	left + mid + right = innerWidth + 4 - 3×4 - 2 = innerWidth - 10
//
// inner columns to share. The two sides are rounded once, together, and the
// middle is what is left: rounding all three independently drifts a column off
// the frame at some widths, which is the same trap chatPaneWidths avoids.
func (m *Model) itemsPaneWidths() (left, mid, right int) {
	avail := m.innerWidth() + boxChrome - 3*paneChrome - 2*paneGap
	side := int(math.Round(itemsSideSplit * float64(avail)))
	return side, avail - 2*side, side
}

// detailStep is how far pgup and pgdown move the detail pane: half of it, so
// the line the eye was on stays on screen.
func (m *Model) detailStep() int {
	return max(m.bodyHeight()/2, 1)
}

// itemsPanes is the three-pane body: three finished boxes side by side,
// already the frame's full width. The middle and right panes carry the gap as
// their left margin, as chat's output pane does.
//
// The three boxes are the page's shape, so they are drawn in every state. When
// there is no list — loading, a failed load, nothing to show — its sentence
// takes the list's place in the middle box and the side boxes stay empty,
// except that a dispatch log is still shown on the right: a reload that fails
// after a dispatch must not hide what the dispatch said.
func (m *Model) itemsPanes() string {
	leftInner, midInner, rightInner := m.itemsPaneWidths()
	h := m.bodyHeight()
	pane := func(style lipgloss.Style, inner int, body string) string {
		return style.
			Width(inner + 2).
			Height(h).
			MaxHeight(h + 2).
			Render(padLines(body, h))
	}

	// A failed reload keeps the rows it could not replace, so the selection
	// can outlive the list it came from; the side boxes are therefore
	// emptied by the state, not left to the selection.
	var left, mid, right string
	if text, style, ok := m.itemsStateMessage(); ok {
		mid = fitStyled(text, midInner, style)
		if len(m.items.output) > 0 {
			right = m.renderDispatchLog(rightInner, h)
		}
	} else {
		left = m.renderItemProject(leftInner, h)
		mid = m.renderItemCards(midInner, h)
		right = m.renderItemDetail(rightInner, h)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		pane(styleBox, leftInner, left),
		pane(styleBoxRight, midInner, mid),
		pane(styleBoxRight, rightInner, right),
	)
}

// renderItemCards is the middle pane: a heading per project and a card per
// item, scrolled by line so that the selected card is always whole on screen.
//
// Every card is itemCardHeight lines, so the selected card's span is known as
// the lines are built and the scroll is two comparisons. When the selected
// card is the first in its group the heading above it is kept in view as well,
// because a card whose project has scrolled off is a card without context.
func (m *Model) renderItemCards(w, h int) string {
	var lines []string
	top, last := -1, -1
	for i, row := range m.items.rows {
		if row.group {
			// A blank line between groups, so that a heading reads as the
			// start of one rather than as the tail of the card above it.
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, styleLabel.Render(truncate(row.label, w)))
			continue
		}
		selected := i == m.items.cursor
		if selected {
			top = len(lines)
			if i > 0 && m.items.rows[i-1].group {
				top--
			}
		}
		lines = append(lines, strings.Split(m.renderItemCard(row.item, w, selected), "\n")...)
		if selected {
			last = len(lines) - 1
		}
	}

	off := m.items.cardOffset
	if top >= 0 {
		if top < off {
			off = top
		}
		if last >= off+h {
			off = last - h + 1
		}
	}
	off = max(min(off, len(lines)-h), 0)
	m.items.cardOffset = off
	return strings.Join(lines[off:min(off+h, len(lines))], "\n")
}

// renderItemCard draws one item as a bordered card exactly w columns wide:
// the title on one line, the id, age and status on the next.
//
// The text is cut to the card's inner width before any of it is styled,
// because truncate counts raw runes and would count an escape sequence as
// text; a line that is one column too wide is wrapped by lipgloss, which
// grows the card by a row and throws the scroll arithmetic out with it.
func (m *Model) renderItemCard(it store.ActionItem, w int, selected bool) string {
	text := max(w-boxChrome, 1)

	title := truncate(oneLine(it.Title), text)
	switch {
	case selected:
		title = styleTitle.Render(title)
	case it.Status == store.ItemDismissed:
		title = styleDim.Render(title)
	}

	status, statusStyle := it.Status, cardStatusStyle(it.Status)
	if d := m.items.dispatch; d != nil && d.itemID == it.ID {
		status, statusStyle = "running", styleAccent
	}
	status = truncate(status, text)
	// The id and the status matter more than the age, so the age is what
	// gives way first when the card is narrow.
	var meta string
	if room := text - lipgloss.Width(status) - 1; room > 0 {
		left := it.ID + " · " + ago(m.now(), it.CreatedAt)
		if lipgloss.Width(left) > room {
			left = it.ID
		}
		meta = padBetween(styleDim.Render(truncate(left, room)), statusStyle.Render(status), text)
	} else {
		meta = statusStyle.Render(status)
	}

	border := colBG3
	switch {
	case it.ID == m.items.confirm:
		border = colOrange
	case selected:
		border = colBlue
	}
	return styleBox.BorderForeground(border).Width(w - 2).Render(title + "\n" + meta)
}

func cardStatusStyle(status string) lipgloss.Style {
	switch status {
	case store.ItemDone:
		return styleOK
	case store.ItemInProgress, store.ItemInReview:
		return styleAccent
	default:
		return styleDim
	}
}

// renderItemProject is the left pane: what Apex knows about the project the
// selected item belongs to — the registry row, the repository, the digest and
// the project's other items — and then as much of the digest as fits.
func (m *Model) renderItemProject(w, h int) string {
	it, ok := m.items.selected()
	if !ok {
		return ""
	}
	slug := it.ProjectSlug
	p := m.items.projects[slug]
	name := m.items.names[slug]
	if name == "" {
		name = slug
	}
	plain := lipgloss.NewStyle()
	now := m.now()

	lines := []string{
		styleLabel.Render(truncate(name, w)),
		styleDim.Render(truncate(orDash(p.Path), w)),
		"",
		keyValue("status", orDash(p.Status), plain, w),
	}

	branch, branchStyle := "—", plain
	var subject string
	var when time.Time
	read, isRead := m.items.git[slug]
	switch {
	case !isRead && m.items.gitLoading[slug]:
		branch, branchStyle = "reading git…", styleDim
	case !isRead:
		// A project the registry does not know, or one with no path: there
		// is nothing to read, and "reading git…" would never resolve.
	case read.err != nil:
		branch, branchStyle = "could not read git: "+read.err.Error(), styleWarn
	case !read.state.IsRepo:
		branch, branchStyle = read.state.Note, styleDim
		if branch == "" {
			branch = "not a git repository"
		}
	default:
		branch = describeBranch(read.state)
		when = read.state.LastCommit
		if len(read.state.Log) > 0 {
			// Log lines are `--oneline`: the short sha, a space, the subject.
			_, subject, _ = strings.Cut(read.state.Log[0], " ")
		}
	}
	lines = append(lines,
		keyValue("branch", branch, branchStyle, w),
		keyValue("commit", ago(now, when), plain, w),
	)
	if subject != "" {
		lines = append(lines, styleDim.Render(truncate(oneLine(subject), w)))
	}

	d, hasDigest := m.items.digests[slug]
	digest, digestStyle := "none", styleWarn
	if hasDigest {
		digest = digestAge(now, d.GeneratedAt)
		if now.Sub(d.GeneratedAt) <= staleAfter {
			digestStyle = plain
		}
	}
	// Counted over every item, not the filtered ones: the filter is a way of
	// looking at the list, and the project's workload does not change with it.
	open, done := 0, 0
	for _, x := range m.items.items {
		if x.ProjectSlug != slug {
			continue
		}
		switch x.Status {
		case store.ItemDone:
			done++
		case store.ItemDismissed:
		default:
			open++
		}
	}
	lines = append(lines,
		keyValue("digest", digest, digestStyle, w),
		keyValue("items", fmt.Sprintf("%d open · %d done", open, done), plain, w),
	)

	// The digest takes whatever is left, and only if there is room for its
	// heading and at least one line of it.
	if len(lines)+3 > h {
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "", styleLabel.Render(truncate("Digest", w)))
	body, bodyStyle := fitLines("No digest yet. Run `apex sync`.", w), styleDim
	if hasDigest && strings.TrimSpace(d.Body) != "" {
		body, bodyStyle = fitLines(strings.TrimSpace(d.Body), w), plain
	}
	cut := false
	if room := h - len(lines); len(body) > room {
		body, cut = body[:room-1], true
	}
	for _, l := range body {
		lines = append(lines, bodyStyle.Render(l))
	}
	if cut {
		lines = append(lines, styleDim.Render("…"))
	}
	return strings.Join(lines, "\n")
}

// keyValue is one row of the left pane's summary: a dim key in a fixed column
// and its value after it, the value cut before it is styled.
func keyValue(key, value string, style lipgloss.Style, w int) string {
	vw := w - kvKeyWidth
	if vw <= 0 {
		return styleDim.Render(truncate(key, w))
	}
	return styleDim.Render(padTo(key, kvKeyWidth)) + style.Render(truncate(oneLine(value), vw))
}

// describeBranch is the left pane's one-line summary of a repository:
// branch, short head, and whether the tree is clean.
func describeBranch(g project.GitState) string {
	var parts []string
	switch g.Branch {
	case "":
	case "HEAD":
		parts = append(parts, "detached")
	default:
		parts = append(parts, g.Branch)
	}
	if head := g.ShortHead(); head != "" {
		parts = append(parts, head)
	} else {
		parts = append(parts, "no commits")
	}
	switch {
	case g.Note != "":
		// InspectGit sets a note on a repository only when `status` failed,
		// so clean or dirty is not known.
		parts = append(parts, "status unknown")
	case g.Dirty:
		parts = append(parts, fmt.Sprintf("dirty:%d", len(g.Status)))
	default:
		parts = append(parts, "clean")
	}
	return strings.Join(parts, " · ")
}

// renderItemDetail is the right pane: the dispatch log while there is one,
// otherwise the selected item in full, otherwise nothing.
//
// It scrolls by line under pgup and pgdown, and the offset is clamped here,
// against the lines actually built, because the key handler does not know how
// long the item is. A "…" on the last row says there is more below.
func (m *Model) renderItemDetail(w, h int) string {
	if len(m.items.output) > 0 {
		return m.renderDispatchLog(w, h)
	}
	it, ok := m.items.selected()
	if !ok {
		return ""
	}
	now := m.now()

	var lines []string
	add := func(style lipgloss.Style, text string) {
		for _, l := range fitLines(text, w) {
			lines = append(lines, style.Render(l))
		}
	}
	add(styleTitle, it.Title)
	meta := it.ID + " · " + it.Status
	if effort := strings.TrimSpace(it.Effort); effort != "" {
		meta += " · " + effort + " effort"
	}
	add(styleDim, meta)
	var dates []string
	if !it.CreatedAt.IsZero() {
		dates = append(dates, "created "+ago(now, it.CreatedAt))
	}
	if !it.UpdatedAt.IsZero() && !it.UpdatedAt.Equal(it.CreatedAt) {
		dates = append(dates, "updated "+ago(now, it.UpdatedAt))
	}
	if len(dates) > 0 {
		add(styleDim, strings.Join(dates, " · "))
	}

	plain := lipgloss.NewStyle()
	for _, sec := range []struct{ label, text string }{
		{"Changes", it.Body},
		{"Why now", it.Rationale},
	} {
		if text := strings.TrimSpace(sec.text); text != "" {
			lines = append(lines, "", styleLabel.Render(truncate(sec.label, w)))
			add(plain, text)
		}
	}
	if by := strings.TrimSpace(it.GeneratedBy); by != "" {
		lines = append(lines, "")
		add(styleDim, "generated by "+by)
	}

	off := max(min(m.items.detailOffset, len(lines)-h), 0)
	m.items.detailOffset = off
	end := min(off+h, len(lines))
	shown := append([]string(nil), lines[off:end]...)
	if end < len(lines) {
		shown[len(shown)-1] = styleDim.Render("…")
	}
	return strings.Join(shown, "\n")
}

// renderDispatchLog is the right pane during and after a dispatch: a heading,
// then the tail of the output that fits — the same lines renderRun shows in
// the fallback.
func (m *Model) renderDispatchLog(w, h int) string {
	label := truncate("dispatch "+m.items.outputFor, w)
	head := styleLabel.Render(label)
	if tail := " · running"; m.items.dispatch != nil && lipgloss.Width(label)+lipgloss.Width(tail) <= w {
		head += styleDim.Render(tail)
	}
	lines := m.items.output
	if room := max(h-1, 0); len(lines) > room {
		lines = lines[len(lines)-room:]
	}
	var b strings.Builder
	b.WriteString(head)
	for _, line := range lines {
		b.WriteString("\n")
		b.WriteString(styleDim.Render(truncate(oneLine(line), w)))
	}
	return b.String()
}

// oneLine makes a string safe to measure as one line. A tab measures as no
// columns but prints as up to eight, and a stray newline or carriage return
// would add a row the layout did not count, so a title or an agent's output
// line cannot be trusted to be what truncate thinks it is.
func oneLine(s string) string {
	return strings.NewReplacer("\t", "    ", "\r", "", "\n", " ").Replace(s)
}

// ago is how long before now t was, for a card or a detail line: hours and
// days while that is the useful unit, then a date. The zero time is "—",
// because a missing date is not the same as an old one.
//
// It is not digestAge: that one reports staleness, and a digest past a week
// has to say so, where an item created a month ago is simply a month old.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Hour:
		return "just now"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	t = t.In(now.Location())
	if t.Year() == now.Year() {
		return t.Format("2 Jan")
	}
	return t.Format("Jan 2006")
}
