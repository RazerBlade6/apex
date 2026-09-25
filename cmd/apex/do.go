package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/RazerBlade6/apex/internal/executor"
	"github.com/RazerBlade6/apex/internal/project"
	"github.com/RazerBlade6/apex/internal/store"
)

// defaultDispatchTimeout bounds a dispatch that nobody is watching.
//
// Zero would be defensible for an interactive run the user can interrupt, but
// `apex do` is meant to be cron-able (DESIGN.md §11), and an unbounded run
// that wedges holds the project's exclusive lock until someone notices. Half
// an hour is longer than any brief Apex writes should need and short enough
// that a stuck run is not a lost afternoon.
const defaultDispatchTimeout = 30 * time.Minute

func newDoCmd() *cobra.Command {
	var opts dispatchOptions

	cmd := &cobra.Command{
		Use:   "do <id>",
		Short: "Dispatch an action item to the coding agent",
		Long: `Hand one action item to a coding agent running inside the project directory,
and stream what it does.

Apex assembles the brief and owns the bookkeeping; the agent writes the code.
The brief carries intent — what to do, why it matters now, and what would make
it done — and deliberately not project background: the agent is standing in
the project, so it reads PROJECT.md, README.md and any CLAUDE.md for itself.

The project is locked exclusively for the whole run. A second dispatch against
the same project reports the run that holds it and stops rather than letting
two agents interleave edits in one working tree.

On success the item moves to in_review. On failure it goes back to accepted.
Apex never marks an item done: that is your call after reading the diff, and
nothing here commits, stages or pushes anything.

This spends Claude Code subscription quota — the same window as your own
sessions — rather than an API key.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDo(cmd.Context(), cmd.OutOrStdout(), args[0], opts)
		},
	}

	cmd.Flags().StringVar(&opts.Model, "model", "",
		"model for this run, overriding executor.model in config.toml")
	cmd.Flags().StringVar(&opts.Effort, "effort", "",
		"effort for this run, overriding executor.effort in config.toml")
	cmd.Flags().BoolVar(&opts.Bypass, "bypass-permissions", false,
		"grant the agent every permission, not only file edits, so it can build and test its own work")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", defaultDispatchTimeout,
		"stop the run after this long; 0 waits indefinitely")
	return cmd
}

func runDo(ctx context.Context, w io.Writer, rawID string, opts dispatchOptions) error {
	sess, err := openSession(ctx, false)
	if err != nil {
		return err
	}
	defer sess.Close()
	return dispatchItem(ctx, w, sess, rawID, opts)
}

// dispatchItem is runDo against a session the caller already opened.
//
// The split exists for M6's TUI, which holds a store open for its whole
// lifetime and dispatches from the items view: opening a second connection to
// the same database for the duration of a run would be a second writer against
// a file that already has one, for no gain.
func dispatchItem(ctx context.Context, w io.Writer, sess *session, rawID string, opts dispatchOptions) error {
	// 1. The item, its project, and its digest (DESIGN.md §14).
	item, err := loadDispatchableItem(ctx, sess, rawID)
	if err != nil {
		return err
	}
	proj, err := sess.Store.GetProject(ctx, item.ProjectSlug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &orphanedItemError{ID: item.ID, Slug: item.ProjectSlug}
		}
		return err
	}
	candidate := project.Candidate{Slug: proj.Slug, Name: proj.Name, RawPath: proj.Path, Path: proj.Path}
	if err := candidate.Verify(); err != nil {
		return err
	}

	// The digest is loaded and deliberately NOT put in the brief. It is read
	// for one thing only: saying whether the item was written against a state
	// the project has since moved past, which is the question
	// action_items.context_hash exists to answer. Pasting the digest into the
	// brief would ship a summary generated at some earlier sync as though it
	// were current, when the authoritative version is in the working tree the
	// agent is about to read.
	reportStaleness(ctx, w, sess, item)

	exec, err := newExecutor(sess.Config, opts)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "%s  %s\n", item.ID, item.Title)
	fmt.Fprintf(w, "%s · was %s\n\n", proj.Name, item.Status)

	_, err = runDispatch(ctx, w, sess, exec, dispatchSpec{
		ProjectSlug:  proj.Slug,
		ProjectPath:  proj.Path,
		ProjectName:  proj.Name,
		ActionItemID: item.ID,
		Title:        item.Title,
		Intent:       itemIntent(item, proj.Name),
		Bypass:       opts.Bypass,
		OnStatus: func(ctx context.Context, status string) error {
			return sess.Store.SetActionItemStatus(ctx, item.ID, status, time.Now())
		},
	})
	return err
}

// itemIntent turns a stored action item into the brief's intent.
//
// Everything here came from Apex's own judgment: the title and body the
// advisor proposed, the rationale it recorded for why this matters now, and
// the effort it sized the work at. None of it is project background, and none
// of it is recoverable from the working tree — which is precisely the test for
// whether something belongs in a brief.
func itemIntent(item store.ActionItem, projectName string) executor.Intent {
	return executor.Intent{
		Kind:        "action item",
		ID:          item.ID,
		ProjectName: projectName,
		Title:       item.Title,
		What:        item.Body,
		Why:         item.Rationale,
		Effort:      item.Effort,
	}
}

// loadDispatchableItem resolves the id and refuses the statuses that should
// not be dispatched.
//
// `done` and `dismissed` are refused because dispatching either is almost
// certainly a typo in an id, and the cost of being wrong is an agent editing a
// project over work the user already closed. `in_progress` is deliberately NOT
// refused: the lock is the real guard against two concurrent runs, and a
// process killed mid-dispatch leaves that status behind with no holder, so
// refusing it would wedge the item until someone edited the database.
func loadDispatchableItem(ctx context.Context, sess *session, rawID string) (store.ActionItem, error) {
	itemID, _ := normalizeID(rawID)
	if itemID == "" {
		return store.ActionItem{}, &unknownIDError{ID: rawID}
	}
	item, err := sess.Store.GetActionItem(ctx, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return store.ActionItem{}, &unknownIDError{ID: rawID}
	}
	if err != nil {
		return store.ActionItem{}, err
	}
	switch item.Status {
	case store.ItemDone, store.ItemDismissed:
		return store.ActionItem{}, &undispatchableItemError{ID: item.ID, Status: item.Status}
	}
	return item, nil
}

// reportStaleness says whether the portfolio has moved since the item was
// generated. It is a warning, never a refusal: an item written against an
// older state is often still the right thing to do, and the user is the one
// who can tell.
func reportStaleness(ctx context.Context, w io.Writer, sess *session, item store.ActionItem) {
	if item.ContextHash == "" {
		return
	}
	actx, err := sess.advisorFor().LoadContext(ctx)
	if err != nil || actx.Hash() == item.ContextHash {
		return
	}
	fmt.Fprintf(w, "This item was generated against context %s, and the portfolio has changed since.\n",
		short(item.ContextHash))
	fmt.Fprintf(w, "  apex show %s   to read it before dispatching\n\n", item.ID)
}

// undispatchableItemError refuses a status that should not be dispatched.
type undispatchableItemError struct{ ID, Status string }

func (e *undispatchableItemError) Error() string {
	return fmt.Sprintf("%s is %s, so there is nothing to dispatch\n"+
		"  apex show %s   to read it\n"+
		"  check the id: apex items", e.ID, e.Status, e.ID)
}

func (e *undispatchableItemError) UserFixable() bool { return true }

// orphanedItemError reports an item whose project row has gone.
type orphanedItemError struct{ ID, Slug string }

func (e *orphanedItemError) Error() string {
	return fmt.Sprintf("%s belongs to project %q, which is no longer registered\n"+
		"  re-add it to PROJECTS.md, then run: apex sync", e.ID, e.Slug)
}

func (e *orphanedItemError) UserFixable() bool { return true }
