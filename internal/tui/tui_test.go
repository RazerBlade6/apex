package tui

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/config"
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
	if !strings.Contains(m.chat.vp.View(), "ScholarRAG") {
		t.Errorf("deltas are not visible in the scrollback:\n%s", m.chat.vp.View())
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
