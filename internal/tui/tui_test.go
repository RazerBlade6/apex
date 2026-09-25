package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/project"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// TestTUIImportsDownwardOnly is the boundary DESIGN.md §12 and §17 depend on.
//
// It is a test rather than a convention for the same reason TestNoSelectStar
// is: the guarantee is only worth something if breaking it fails the build.
// The moment anything in advisor, provider, executor or store imports a
// terminal package, a second frontend stops being a package beside this one
// and becomes a refactor of everything beneath it — and nothing else would
// notice until that refactor was attempted.
func TestTUIImportsDownwardOnly(t *testing.T) {
	const self = "github.com/RazerBlade6/apex/internal/tui"

	// The whole module, so cmd/apex is covered too — it is the one package
	// that may import this one, and the test has to see it to say so.
	out, err := exec.Command("go", "list", "-json", "../../...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	type pkg struct {
		ImportPath   string
		Imports      []string
		TestImports  []string
		XTestImports []string
	}

	// Only cmd/apex is above this package, and it reaches it through exactly
	// one file (cmd/apex/tui.go). Everything else must not reach it at all.
	allowed := map[string]bool{"github.com/RazerBlade6/apex/cmd/apex": true}

	var seen int
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		seen++
		if p.ImportPath == self || allowed[p.ImportPath] {
			continue
		}
		for _, list := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
			for _, imp := range list {
				if imp == self {
					t.Errorf("%s imports %s; internal/tui must be imported downward only", p.ImportPath, self)
				}
			}
		}
	}
	if seen < 10 {
		t.Fatalf("go list reported only %d packages; the boundary was not actually checked", seen)
	}
}

// newTestModel builds a Model over a real in-memory-ish store and a fake
// provider, with no terminal involved.
func newTestModel(t *testing.T, fake provider.Provider) *Model {
	t.Helper()
	root := t.TempDir()
	backend := store.NewLocalBackend(filepath.Join(root, "apex.db"))
	t.Cleanup(func() { backend.Close() }) //nolint:errcheck
	st, err := store.Open(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	adv := advisor.New(st, &cfg, root, func(context.Context, config.ModelRoute) (provider.Provider, error) {
		return fake, nil
	})

	m := New(Options{Store: st, Config: &cfg, Advisor: adv, Root: root, Version: "test"})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

func TestTabCyclesTheThreeViews(t *testing.T) {
	m := newTestModel(t, nil)
	want := []view{viewItems, viewProjects, viewChat}
	for i, w := range want {
		m.Update(tea.KeyMsg{Type: tea.KeyTab})
		if m.view != w {
			t.Fatalf("tab %d landed on %v, want %v", i+1, m.view, w)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.view != viewProjects {
		t.Fatalf("shift+tab landed on %v, want %v", m.view, viewProjects)
	}
}

// TestTabIsNotSwallowedByTheChatInput: the chat textarea would otherwise take
// tab as an indent, and view switching has to work from every surface.
func TestTabIsNotSwallowedByTheChatInput(t *testing.T) {
	m := newTestModel(t, nil)
	m.chat.ta.Focus()
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.view == viewChat {
		t.Fatal("tab did not leave the chat view")
	}
	if strings.Contains(m.chat.ta.Value(), "\t") {
		t.Fatalf("tab was inserted into the input: %q", m.chat.ta.Value())
	}
}

// TestStreamDeltasArriveAsMessages is DESIGN.md §12's streaming shape: one
// message per event, re-issued, tokens landing in Update.
func TestStreamDeltasArriveAsMessages(t *testing.T) {
	m := newTestModel(t, nil)
	h := &streamHandle{cancel: func() {}}
	m.chat.stream = h
	m.chat.sess = nil

	for _, chunk := range []string{"Scholar", "RAG is ", "closest."} {
		_, cmd := m.Update(streamEventMsg{h: h, ev: provider.Event{
			Type: provider.EventTextDelta, Text: chunk,
		}, open: true})
		if cmd == nil {
			t.Fatal("the drain did not re-issue itself; the rest of the stream would never arrive")
		}
	}
	if got := m.chat.pending.String(); got != "ScholarRAG is closest." {
		t.Fatalf("accumulated %q", got)
	}
	if !strings.Contains(m.chat.outVP.View(), "ScholarRAG") {
		t.Errorf("deltas are not visible in the scrollback:\n%s", m.chat.outVP.View())
	}

	// EventDone carries what the turn cost, and the status line reports both
	// halves of the cache. DESIGN.md §8 proved reads alone cannot tell a
	// cache that works from a prefix re-written on every call.
	m.Update(streamEventMsg{h: h, ev: provider.Event{Type: provider.EventDone, Usage: &provider.Usage{
		InputTokens: 2, OutputTokens: 40, CacheWriteTokens: 1150, Model: "claude-sonnet-5",
	}}, open: true})
	for _, want := range []string{"2 in / 40 out", "r=0", "w=1150", "claude-sonnet-5"} {
		if !strings.Contains(describeUsage(h.usage), want) {
			t.Errorf("describeUsage = %q, want it to contain %q", describeUsage(h.usage), want)
		}
	}
}

// TestStaleStreamEventsAreDropped: a reply that arrives after the user
// cancelled must not be appended to whatever they asked next.
func TestStaleStreamEventsAreDropped(t *testing.T) {
	m := newTestModel(t, nil)
	old := &streamHandle{cancel: func() {}}
	current := &streamHandle{cancel: func() {}}
	m.chat.stream = current

	m.Update(streamEventMsg{h: old, ev: provider.Event{
		Type: provider.EventTextDelta, Text: "from the abandoned stream",
	}, open: true})

	if strings.Contains(m.chat.pending.String(), "abandoned") {
		t.Fatalf("a stale stream wrote into the live turn: %q", m.chat.pending.String())
	}
}

// TestDispatchAsksBeforeItSpends: enter selects, a second key confirms.
// A dispatch takes the project lock, spends subscription quota and lets an
// agent edit a real project, so it is not something a cursor key should start.
func TestDispatchAsksBeforeItSpends(t *testing.T) {
	var dispatched []string
	m := newTestModel(t, nil)
	m.opts.Dispatch = func(ctx context.Context, w io.Writer, id string) error {
		dispatched = append(dispatched, id)
		io.WriteString(w, "working\n") //nolint:errcheck
		return nil
	}
	m.view = viewItems
	m.Update(itemsLoadedMsg{
		items: []store.ActionItem{{
			ID: "AI-001", ProjectSlug: "p", Title: "Do the thing", Status: store.ItemProposed,
		}},
		projects: []store.Project{{Slug: "p", Name: "Project"}},
	})

	// Nothing is selected until the user selects it, and enter on nothing
	// only says so.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.items.confirm != "" || !strings.Contains(m.status, "select") {
		t.Fatalf("enter with nothing selected: confirm = %q, status = %q", m.items.confirm, m.status)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.items.confirm != "AI-001" {
		t.Fatalf("enter did not ask for confirmation; confirm = %q", m.items.confirm)
	}
	if len(dispatched) != 0 {
		t.Fatal("enter dispatched without confirmation")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if m.items.confirm != "" || len(dispatched) != 0 {
		t.Fatal("declining still dispatched")
	}

	// A closed item is never offered at all, mirroring `apex do`'s refusal:
	// dispatching one is almost certainly a mis-keyed selection, and the cost
	// of being wrong is an agent editing a project over work already closed.
	m.Update(itemsLoadedMsg{
		items: []store.ActionItem{{
			ID: "AI-009", ProjectSlug: "p", Title: "Already finished", Status: store.ItemDone,
		}},
		projects: []store.Project{{Slug: "p", Name: "Project"}},
	})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.items.confirm != "" {
		t.Fatal("a done item was offered for dispatch")
	}
	if !strings.Contains(m.status, "done") {
		t.Errorf("status = %q, want it to say why", m.status)
	}
}

func TestItemsFilterCyclesAndNarrows(t *testing.T) {
	m := newTestModel(t, nil)
	m.view = viewItems
	m.Update(itemsLoadedMsg{
		items: []store.ActionItem{
			{ID: "AI-001", ProjectSlug: "p", Title: "one", Status: store.ItemProposed},
			{ID: "AI-002", ProjectSlug: "p", Title: "two", Status: store.ItemDone},
		},
		projects: []store.Project{{Slug: "p", Name: "Project"}},
	})
	if got := len(m.items.visible()); got != 2 {
		t.Fatalf("unfiltered = %d, want 2", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	if itemStatuses[m.items.filter] != store.ItemProposed {
		t.Fatalf("filter = %q", itemStatuses[m.items.filter])
	}
	if got := len(m.items.visible()); got != 1 {
		t.Fatalf("filtered = %d, want 1", got)
	}
}

// TestProjectsViewFlagsStaleAndMissingDigests: staleness comes from the
// digest's own age, and a project with no digest row is reported as having
// none rather than as infinitely old.
func TestProjectsViewFlagsStaleAndMissingDigests(t *testing.T) {
	m := newTestModel(t, nil)
	m.view = viewProjects
	now := m.now()
	m.Update(projectsLoadedMsg{
		projects: []store.Project{
			{Slug: "fresh", Name: "Fresh", Path: "/tmp/fresh"},
			{Slug: "old", Name: "Old", Path: "/tmp/old"},
			{Slug: "none", Name: "None", Path: "/tmp/none"},
		},
		digests: []store.Digest{
			{ProjectSlug: "fresh", Body: "current", GeneratedAt: now},
			{ProjectSlug: "old", Body: "ancient", GeneratedAt: now.Add(-30 * 24 * 60 * 60 * 1e9)},
		},
	})
	hint := m.projectsHint()
	for _, want := range []string{"3 project(s)", "1 with no digest", "1 stale", "apex sync"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint = %q, want it to mention %q", hint, want)
		}
	}
	if !strings.Contains(m.projectsView(), "current") {
		t.Errorf("the selected project's digest preview is missing:\n%s", m.projectsView())
	}
}

// TestGlamourRendersTheScrollback is the deliberate half of DESIGN.md §18's
// open Glamour decision: styling in the TUI, plain text in the commands.
func TestGlamourRendersTheScrollback(t *testing.T) {
	// A named style, because WithAutoStyle correctly degrades to unstyled
	// when there is no terminal — which is what `go test` is.
	r := buildRenderer(60, glamour.WithStandardStyle("dark"))
	out := r.markdown("# Heading\n\nSome **bold** prose.\n")
	if strings.TrimSpace(out) == "" {
		t.Fatal("the renderer produced nothing")
	}
	if strings.Contains(out, "**bold**") {
		t.Errorf("markdown was passed through verbatim, so nothing rendered it:\n%q", out)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("no styling was applied:\n%q", out)
	}

	// And the fallback path, which is what a renderer that could not be
	// built uses. A chat view that refused to show a reply because it could
	// not colour it would be a worse failure than an unstyled one.
	plain := (&renderer{width: 20}).markdown("# Heading\n\nSome prose that is definitely longer than twenty columns.")
	if strings.TrimSpace(plain) == "" {
		t.Fatal("the fallback renderer produced nothing")
	}
	for _, line := range strings.Split(plain, "\n") {
		if len(line) > 20 {
			t.Errorf("the fallback did not wrap: %q", line)
		}
	}
}

// TestBackgroundLoadsReachTheirOwnView is the regression test for the first
// real bug in this package.
//
// Init loads chat context, items and projects at once. Chat is the default
// view, so routing every message to the focused view meant the items and
// projects loads arrived while chat was focused and were silently dropped —
// both views then read "loading…" forever, with no error anywhere to explain
// it. A driven-Update test caught nothing, because those tests set the view
// first; only launching the application showed it.
func TestBackgroundLoadsReachTheirOwnView(t *testing.T) {
	m := newTestModel(t, nil)
	if m.view != viewChat {
		t.Fatalf("default view = %v, want chat", m.view)
	}

	m.Update(itemsLoadedMsg{
		items:    []store.ActionItem{{ID: "AI-001", ProjectSlug: "p", Title: "one", Status: store.ItemProposed}},
		projects: []store.Project{{Slug: "p", Name: "Project"}},
	})
	m.Update(projectsLoadedMsg{
		projects: []store.Project{{Slug: "p", Name: "Project", Path: "/tmp/p"}},
		digests:  []store.Digest{{ProjectSlug: "p", Body: "a digest", GeneratedAt: m.now()}},
	})

	if !m.items.loaded {
		t.Error("the items load was dropped because chat was focused")
	}
	if !m.projects.loaded {
		t.Error("the projects load was dropped because chat was focused")
	}
	m.setView(viewItems)
	if !strings.Contains(m.itemsView(), "AI-001") {
		t.Errorf("the items view is still empty after tabbing to it:\n%s", m.itemsView())
	}
	m.setView(viewProjects)
	if !strings.Contains(m.projectsView(), "a digest") {
		t.Errorf("the projects view is still empty after tabbing to it:\n%s", m.projectsView())
	}
}

// ansiEscape matches the SGR sequences lipgloss emits, for the checks that
// compare characters by column.
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

// TestFrameFitsTheTerminal is the regression test for the bounding box.
//
// The content area is now a bordered box centred in the terminal, which turns
// the layout into arithmetic: the border and its padding cost four columns and
// two rows, so every line inside the box is measured against the inner width
// rather than against the screen, and the box's own rows come out of the
// height. Get either sum wrong by one and it does not look like an off-by-one
// — the border wraps into garbage, or the frame is taller than the screen and
// scrolls the alt buffer — and no other test in this package measures the whole
// frame, because they all assert on one view's body with strings.Contains.
//
// The cramped 40x10 case is the one that actually caught something: the
// 40-column floor on the inner width is wider than a 40-column terminal can
// hold once the chrome is paid for, so the box has to give up the floor.
//
// The items view is measured in both of its layouts: 80x24, 100x30 and 200x50
// take the three panes, 60x20 and 40x10 the single list. Its fixture carries a
// path far longer than any pane, because wrapping breaks only on whitespace and
// an unbreakable word that reaches lipgloss grows a pane by a row.
func TestFrameFitsTheTerminal(t *testing.T) {
	sizes := []struct{ w, h int }{{80, 24}, {100, 30}, {60, 20}, {200, 50}, {40, 10}}
	longPath := "/Users/someone/Development/" + strings.Repeat("deeply-nested-", 7) + "project/internal/provider/retry.go"
	for _, size := range sizes {
		m := newTestModel(t, nil)
		now := m.now()
		// Real rows, including ones long enough to need truncating, so the
		// frame is measured with something in it.
		m.Update(itemsLoadedMsg{
			items: []store.ActionItem{{
				ID: "AI-001", ProjectSlug: "p", Status: store.ItemProposed, Effort: "medium",
				Title:     "Replace the hand-rolled retry loop with the shared backoff helper",
				Body:      "Swap the loop in " + longPath + " for the shared helper.",
				Rationale: strings.Repeat("Two retry loops disagree on the ceiling. ", 8),
				CreatedAt: now.Add(-72 * time.Hour),
			}},
			projects: []store.Project{{Slug: "p", Name: "Project", Path: longPath}},
			digests:  []store.Digest{{ProjectSlug: "p", Body: longPath + " " + strings.Repeat("digest prose ", 40), GeneratedAt: now}},
		})
		m.Update(gitStateMsg{slug: "p", state: project.GitState{
			IsRepo: true, Branch: "feature/" + strings.Repeat("long-branch-", 6), Head: strings.Repeat("a1b2c3d", 6),
			LastCommit: now.Add(-3 * time.Hour), Log: []string{"a1b2c3d " + strings.Repeat("a long commit subject ", 6)},
		}})
		m.Update(projectsLoadedMsg{
			projects: []store.Project{{
				Slug: "p", Name: "Project", Path: "/Users/someone/Development/a/deeply/nested/project",
			}},
			digests: []store.Digest{{ProjectSlug: "p", Body: strings.Repeat("digest prose ", 40), GeneratedAt: m.now()}},
		})
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		if want := size.w >= 80; m.itemsThreePane() != want {
			t.Errorf("%dx%d: itemsThreePane = %v, want %v; the sizes no longer cover both items layouts",
				size.w, size.h, m.itemsThreePane(), want)
		}

		for _, v := range views {
			m.setView(v)
			assertFrameFits(t, m, size.w, size.h, v.String())
		}

		// The items view opens with nothing selected, so its side panes were
		// empty above. Select the item so that the long-path fixture goes
		// through them as well.
		m.setView(viewItems)
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
		if _, ok := m.items.selected(); !ok {
			t.Fatalf("%dx%d: down did not select the item", size.w, size.h)
		}
		assertFrameFits(t, m, size.w, size.h, "Items, selected")

		// And with nothing to show at all, where the sentence saying so sits
		// in the middle of three otherwise empty boxes. The projects view gets
		// the same treatment: its empty-registry sentence is longer than a
		// narrow box, and is what a fresh install sees first.
		empty := newTestModel(t, nil)
		empty.Update(itemsLoadedMsg{})
		empty.Update(projectsLoadedMsg{})
		empty.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		empty.setView(viewItems)
		assertFrameFits(t, empty, size.w, size.h, "Items, empty")
		empty.setView(viewProjects)
		assertFrameFits(t, empty, size.w, size.h, "Projects, empty")
	}
}

// assertFrameFits renders the whole frame and checks it against a w×h
// terminal: no taller, no line wider, and no box missing its bottom border.
func assertFrameFits(t *testing.T, m *Model, w, h int, label string) {
	t.Helper()
	lines := strings.Split(m.View(), "\n")
	if len(lines) > h {
		t.Errorf("%dx%d %s: the frame is %d lines tall, which does not fit %d", w, h, label, len(lines), h)
	}
	for i, line := range lines {
		if lw := lipgloss.Width(line); lw > w {
			t.Errorf("%dx%d %s: line %d is %d cells wide: %q", w, h, label, i+1, lw, line)
		}
	}

	// Every box that opens on the body's top row has to close on its bottom
	// row, in the same column. A pane with a line wider than itself does not
	// make the frame wider or taller — lipgloss wraps the line and MaxHeight
	// cuts the extra row back off — so the only visible symptom is a missing
	// bottom border, and only this sees it. Rows are compared by rune because
	// every rune before a border on those two rows is itself one column of
	// border or margin.
	top, bottom := bodyEdges(m)
	opened := 0
	for col, r := range top {
		if r != '╭' {
			continue
		}
		opened++
		if col >= len(bottom) || bottom[col] != '╰' {
			t.Errorf("%dx%d %s: the box opening at column %d has lost its bottom border:\n%s",
				w, h, label, col, strings.Join(lines, "\n"))
		}
	}
	if opened == 0 {
		t.Errorf("%dx%d %s: no box on the body's top row: %q", w, h, label, string(top))
	}
}

// bodyEdges returns the body's top and bottom border rows, stripped of
// styling: the frame is the tab row, a blank, the status line, then the body.
func bodyEdges(m *Model) (top, bottom []rune) {
	lines := strings.Split(m.View(), "\n")
	return []rune(ansiEscape.ReplaceAllString(lines[3], "")),
		[]rune(ansiEscape.ReplaceAllString(lines[3+m.bodyHeight()+1], ""))
}

// TestChatPanesSplitSpeakers is the regression test for the chat view's two
// panes.
//
// The layout is one conversation split by speaker, and a turn routed to the
// wrong column is invisible to every other test in this package: they all
// assert on a single rendered string, which contains both sides either way. The
// prompt pane must hold the user's turns and nothing else, the output pane
// Apex's replies and its system notes, and the narrow fallback has to stay
// reachable — at forty columns the left pane would be ten columns of text,
// which is not a layout.
func TestChatPanesSplitSpeakers(t *testing.T) {
	m := newTestModel(t, nil)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if !m.chatTwoPane() {
		t.Fatalf("100x30 did not take the two-pane path: inner=%d body=%d",
			m.innerWidth(), m.bodyHeight())
	}

	m.chat.say(roleYou, "which stall is cheapest?")
	h := &streamHandle{cancel: func() {}}
	m.chat.stream = h
	m.Update(streamEventMsg{h: h, ev: provider.Event{
		Type: provider.EventTextDelta, Text: "ScholarRAG's retry loop.",
	}, open: true})
	// Cancelling is how a partial becomes a finished apex turn without a
	// provider or a session row behind it.
	m.cancelStream()

	left, right := m.chat.inVP.View(), m.chat.outVP.View()
	if !strings.Contains(left, "cheapest") {
		t.Errorf("the prompt is not in the prompt pane:\n%s", left)
	}
	if strings.Contains(left, "ScholarRAG") {
		t.Errorf("Apex's reply leaked into the prompt pane:\n%s", left)
	}
	if !strings.Contains(right, "ScholarRAG") {
		t.Errorf("the reply is not in the output pane:\n%s", right)
	}
	if strings.Contains(right, "cheapest") {
		t.Errorf("the prompt leaked into the output pane:\n%s", right)
	}
	// System lines are Apex speaking too, so they belong on the right.
	if strings.Contains(left, "digests") {
		t.Errorf("a system line landed in the prompt pane:\n%s", left)
	}

	// And the fallback, where the two speakers share one column again. The
	// height is generous so that the assertions below measure the routing
	// rather than how much of a three-row viewport happens to be on screen.
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 24})
	if m.chatTwoPane() {
		t.Fatalf("50x24 took the two-pane path: inner=%d body=%d",
			m.innerWidth(), m.bodyHeight())
	}
	single := m.chat.outVP.View()
	for _, want := range []string{"cheapest", "ScholarRAG", roleYou} {
		if !strings.Contains(single, want) {
			t.Errorf("the fallback scrollback is missing %q:\n%s", want, single)
		}
	}
}

// TestItemsPanesFollowTheSelection is the regression test for the items view's
// three panes.
//
// The side panes describe whatever the cursor is on, and nothing else in this
// package would notice them describing the wrong thing: the three panes share
// every line of the frame, so a strings.Contains on it finds project A's
// digest whether it is beside A's item or B's. The panes are rendered one at a
// time instead. The git state is delivered while chat is focused, because it
// is a background result like the loads in TestBackgroundLoadsReachTheirOwnView
// and would be dropped the same way if it were routed by focus.
func TestItemsPanesFollowTheSelection(t *testing.T) {
	m := newTestModel(t, nil)
	now := m.now()
	m.Update(itemsLoadedMsg{
		items: []store.ActionItem{
			{ID: "AI-001", ProjectSlug: "a", Title: "Tidy the retry loop", Status: store.ItemProposed,
				Body: "Collapse the two loops into one.", Rationale: "They disagree on the ceiling.", CreatedAt: now},
			{ID: "AI-002", ProjectSlug: "b", Title: "Pin the model", Status: store.ItemAccepted, CreatedAt: now},
		},
		projects: []store.Project{
			{Slug: "a", Name: "Alpha", Path: "/tmp/alpha"},
			{Slug: "b", Name: "Bravo", Path: "/tmp/bravo"},
		},
		digests: []store.Digest{
			{ProjectSlug: "a", Body: "alpha digest prose", GeneratedAt: now},
			{ProjectSlug: "b", Body: "bravo digest prose", GeneratedAt: now},
		},
	})
	m.Update(gitStateMsg{slug: "a", state: project.GitState{IsRepo: true, Branch: "trunk", Head: "abcdef0123"}})
	if _, ok := m.items.git["a"]; !ok {
		t.Fatal("the git state was dropped because chat was focused")
	}

	m.setView(viewItems)
	if !m.itemsThreePane() {
		t.Fatalf("100x30 did not take the three-pane path: inner=%d body=%d", m.innerWidth(), m.bodyHeight())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	left, _, right := m.itemsPaneWidths()
	h := m.bodyHeight()

	pane := m.renderItemProject(left, h)
	for _, want := range []string{"Alpha", "alpha digest", "trunk"} {
		if !strings.Contains(pane, want) {
			t.Errorf("the project pane is missing %q:\n%s", want, pane)
		}
	}
	detail := m.renderItemDetail(right, h)
	for _, want := range []string{"Collapse the two", "They disagree"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail pane is missing %q:\n%s", want, detail)
		}
	}

	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	pane = m.renderItemProject(left, h)
	if !strings.Contains(pane, "Bravo") || strings.Contains(pane, "Alpha") {
		t.Errorf("the project pane did not follow the cursor to Bravo:\n%s", pane)
	}

	// And the fallback, so that removing it fails rather than producing
	// three panes of a dozen columns each.
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	if m.itemsThreePane() {
		t.Fatalf("60x20 took the three-pane path: inner=%d body=%d", m.innerWidth(), m.bodyHeight())
	}
}

// TestItemsStartWithNothingSelected: the items view opens on the list alone.
//
// The side boxes describe a selection, so until there is one they are empty
// rather than describing whichever item happened to sort first — and `enter`
// on that item would have dispatched something the user never chose. esc
// returns to nothing, a reload keeps the selection by id, and the empty
// database still gets the three boxes, with the reason there are no cards in
// the middle one.
func TestItemsStartWithNothingSelected(t *testing.T) {
	m := newTestModel(t, nil)
	m.setView(viewItems)
	load := itemsLoadedMsg{
		items: []store.ActionItem{
			{ID: "AI-001", ProjectSlug: "p", Title: "first", Body: "first body", Status: store.ItemProposed},
			{ID: "AI-002", ProjectSlug: "p", Title: "second", Body: "second body", Status: store.ItemProposed},
		},
		projects: []store.Project{{Slug: "p", Name: "Project", Path: "/tmp/p"}},
	}
	m.Update(load)
	left, _, right := m.itemsPaneWidths()
	h := m.bodyHeight()
	blank := func(pane string) bool { return strings.TrimSpace(pane) == "" }

	if !blank(m.renderItemProject(left, h)) || !blank(m.renderItemDetail(right, h)) {
		t.Error("the side boxes describe an item before one was selected")
	}
	if cmd := m.ensureGitCmd(); cmd != nil {
		t.Error("git is read with nothing selected")
	}
	if top, _ := bodyEdges(m); strings.Count(string(top), "╭") != 3 {
		t.Errorf("the body is not three boxes: %q", string(top))
	}

	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if blank(m.renderItemProject(left, h)) || !strings.Contains(m.renderItemDetail(right, h), "second body") {
		t.Error("selecting an item did not fill the side boxes")
	}

	// A reload that lists the items in a different order keeps AI-002
	// selected, not whatever now sits in its row.
	load.items[0], load.items[1] = load.items[1], load.items[0]
	m.Update(load)
	if got := m.items.selectedID(); got != "AI-002" {
		t.Errorf("after a reload the selection is %q, want AI-002", got)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !blank(m.renderItemProject(left, h)) || !blank(m.renderItemDetail(right, h)) {
		t.Error("esc did not return to nothing selected")
	}

	empty := newTestModel(t, nil)
	empty.setView(viewItems)
	empty.Update(itemsLoadedMsg{})
	if top, _ := bodyEdges(empty); strings.Count(string(top), "╭") != 3 {
		t.Errorf("the empty view is not three boxes: %q", string(top))
	}
	if !strings.Contains(empty.itemsPanes(), "No action items") {
		t.Errorf("the empty view does not say why it is empty:\n%s", empty.View())
	}
}

// TestItemCardsScrollToTheSelection: the card list scrolls by line, and the
// selected card has to end up on screen however far down the cursor goes.
func TestItemCardsScrollToTheSelection(t *testing.T) {
	m := newTestModel(t, nil)
	m.setView(viewItems)
	var items []store.ActionItem
	for i := 1; i <= 12; i++ {
		items = append(items, store.ActionItem{
			ID: fmt.Sprintf("AI-%03d", i), ProjectSlug: "p", Title: "item", Status: store.ItemProposed,
		})
	}
	m.Update(itemsLoadedMsg{items: items, projects: []store.Project{{Slug: "p", Name: "Project"}}})
	// Twelve presses: the first selects AI-001, the other eleven walk to
	// AI-012.
	for range items {
		m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	_, mid, _ := m.itemsPaneWidths()
	cards := m.renderItemCards(mid, m.bodyHeight())
	if !strings.Contains(cards, "AI-012") {
		t.Errorf("the selected last card is not on screen:\n%s", cards)
	}
	if strings.Contains(cards, "AI-001") {
		t.Errorf("the list did not scroll; the first card is still showing:\n%s", cards)
	}
}

// TestGlamourStyleIsResolvedBeforeTheProgramStarts is the regression test for
// `11;rgb:2828/2c2c/3434` appearing already typed into the chat input the
// moment the TUI opened.
//
// Resolving Glamour's "auto" style asks termenv for the terminal's background
// colour, and that is not a local lookup: it writes an OSC 11 query to the
// terminal and reads the reply back off stdin. The renderer is rebuilt
// whenever the wrap width changes, so the query ran on the first
// WindowSizeMsg — which arrives immediately after start — by which point
// Bubble Tea owned stdin. The terminal's reply came back through its input
// loop as ordinary keystrokes and landed in the focused textarea.
//
// The fix is that the style is resolved once, in New, before the program
// starts. Nothing about the rendered output can show that, so what is asserted
// is the lookup count: it must not move once the model exists, however many
// resizes arrive.
func TestGlamourStyleIsResolvedBeforeTheProgramStarts(t *testing.T) {
	var calls int
	orig := detectStyle
	detectStyle = func() string { calls++; return styles.NoTTYStyle }
	t.Cleanup(func() { detectStyle = orig })

	m := newTestModel(t, nil) // New, then one WindowSizeMsg
	if calls != 1 {
		t.Fatalf("construction resolved the style %d times, want exactly 1", calls)
	}

	for _, size := range []tea.WindowSizeMsg{
		{Width: 100, Height: 30}, {Width: 80, Height: 24}, {Width: 200, Height: 50},
	} {
		m.Update(size)
	}
	if calls != 1 {
		t.Errorf("a resize re-resolved the style: %d lookups, want 1 — the OSC 11 "+
			"query is back inside the running program, and its reply will be typed "+
			"into the chat input", calls)
	}
	if m.glamourStyle == "" {
		t.Error("the model did not keep the resolved style name")
	}
}
