package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/store"
)

// The tea.Cmds that reach outside the model.
//
// Every one of them takes a context and every one runs off the event loop.
// The ones that read the database use a short timeout, because a wedged query
// must not freeze the interface; the ones that reach a model do not, because
// a model call takes as long as it takes and the user cancels it themselves.

// dbTimeout bounds a read against the local SQLite file. It is generous for
// what it covers — a handful of indexed selects — and exists only so a
// pathological case surfaces as an error line rather than a frozen frame.
const dbTimeout = 10 * time.Second

// staleAfter is when a digest starts being reported as old. DESIGN.md §13's
// default sync mode is explicit, so a digest is stale whenever the user has
// not run `apex sync` — a week is when that starts being worth a warning.
const staleAfter = 7 * 24 * time.Hour

func (m *Model) now() time.Time { return time.Now() }

// loadContextCmd loads the identity documents and every cached digest, which
// is what the chat view reasons over.
func (m *Model) loadContextCmd() tea.Cmd {
	adv := m.opts.Advisor
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		actx, err := adv.LoadContext(ctx)
		return contextLoadedMsg{actx: actx, err: err}
	}
}

// loadItemsCmd reads every action item and the projects they belong to.
func (m *Model) loadItemsCmd() tea.Cmd {
	st := m.opts.Store
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		items, err := st.ListActionItems(ctx, store.ActionItemFilter{})
		if err != nil {
			return itemsLoadedMsg{err: err}
		}
		projects, err := st.ListProjects(ctx)
		if err != nil {
			return itemsLoadedMsg{err: err}
		}
		return itemsLoadedMsg{items: items, projects: projects}
	}
}

// loadProjectsCmd reads the registry rows and their cached digests.
func (m *Model) loadProjectsCmd() tea.Cmd {
	st := m.opts.Store
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		projects, err := st.ListProjects(ctx)
		if err != nil {
			return projectsLoadedMsg{err: err}
		}
		digests, err := st.ListDigests(ctx)
		if err != nil {
			return projectsLoadedMsg{err: err}
		}
		return projectsLoadedMsg{projects: projects, digests: digests}
	}
}

// observeCmd screens one chat turn and, only if it passes, spends a call
// extracting what it revealed (DESIGN.md §6).
//
// The screen runs here rather than in Update for one reason: it must not be
// possible to add a code path that extracts without screening. The gate and
// the call are in the same function, and Observe screens again itself.
func (m *Model) observeCmd(turn advisor.Turn) tea.Cmd {
	if !advisor.ScreenTurn(turn.User) {
		// A turn revealing nothing costs nothing — not even a goroutine.
		return nil
	}
	adv := m.opts.Advisor
	root := m.opts.Root
	m.chat.observing = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		obs, err := advisor.LoadObservations(ctx, root)
		if err != nil {
			return observedMsg{err: err}
		}
		result, err := adv.Observe(ctx, obs, turn)
		return observedMsg{result: result, err: err}
	}
}
