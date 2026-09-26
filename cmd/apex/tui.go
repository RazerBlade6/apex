package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/store"
	"github.com/RazerBlade6/apex/internal/tui"
)

// Launching the TUI (DESIGN.md §11: `apex` with no arguments).
//
// This file is the only place cmd/apex touches internal/tui, and the
// dependency runs one way: internal/tui imports advisor, store and config, and
// nothing below it imports internal/tui. TestTUIImportsDownwardOnly in that
// package proves it rather than trusting it.

// runTUI opens a session and hands it to the Bubble Tea application.
//
// migrate is false for the same reason every read-only command passes false
// (DESIGN.md §18, item 8): a forward-only schema change should happen when the
// user ran the command that owns it, and launching the interface is not that
// command. A pending migration stops here with the same message and the same
// fix.
func runTUI(ctx context.Context, in io.Reader, out io.Writer) error {
	sess, err := openSession(ctx, false)
	if err != nil {
		return err
	}
	defer sess.Close()

	return tui.Run(ctx, tui.Options{
		Store:     sess.Store,
		Config:    sess.Config,
		Advisor:   sess.advisorFor(),
		Root:      sess.Root,
		Version:   version,
		Dispatch:  dispatcherFor(sess),
		StartIdea: ideaStarterFor(sess),
		Input:     in,
		Output:    out,
	})
}

// dispatcherFor adapts `apex do` to the callback the TUI takes.
//
// Everything the dispatch needs beyond the executor — the per-project
// exclusive lock, the exec_runs row, the item's status transitions, the log
// file — lives here in cmd/apex and stays here. The TUI supplies a writer and
// an item id and receives the same stream `apex do` prints to a terminal.
//
// The timeout is the command's default rather than none. A run started from
// the interface is nominally attended, but nothing stops the user switching
// views and forgetting it, and a wedged dispatch holds the project's lock for
// as long as it lives.
func dispatcherFor(sess *session) tui.Dispatcher {
	return func(ctx context.Context, w io.Writer, itemID string) error {
		return dispatchItem(ctx, w, sess, itemID, dispatchOptions{
			Timeout: defaultDispatchTimeout,
		})
	}
}

// ideaStarterFor turns an idea the user settled on in the chat into a project
// and its first action item.
//
// It records the idea, as `apex ideas` would have, then runs createProject —
// `apex start` short of the agent — and adds the scaffolding item to the new
// project. The agent pass that `apex start` would have run is exactly what
// dispatching that item does, so nothing is lost by stopping short: the user
// dispatches it from the items view when they are ready to spend the quota.
func ideaStarterFor(sess *session) tui.IdeaStarter {
	return func(ctx context.Context, w io.Writer, plan *advisor.IdeaPlan) (tui.IdeaStarted, error) {
		now := time.Now()
		id, err := sess.Store.NextIdeaID(ctx)
		if err != nil {
			return tui.IdeaStarted{}, err
		}
		idea := store.Idea{
			ID:          id,
			Title:       plan.Name,
			Pitch:       strings.TrimSpace(plan.Pitch),
			Rationale:   strings.TrimSpace(plan.Rationale),
			Status:      store.IdeaSaved,
			CreatedAt:   now,
			GeneratedBy: plan.Model,
		}

		opts := startOptions{Name: plan.Name, NoDispatch: true}
		if _, ok := scaffolderFor(plan.Stack); ok {
			opts.Stack = plan.Stack
		}

		// Everything that can refuse is checked before anything is written,
		// so a name that is taken leaves no orphaned idea behind it.
		if err := preflightProject(ctx, sess, idea, opts); err != nil {
			return tui.IdeaStarted{}, err
		}
		if err := sess.Store.InsertIdea(ctx, idea); err != nil {
			return tui.IdeaStarted{}, err
		}
		created, err := createProject(ctx, w, sess, idea, opts)
		if err != nil {
			return tui.IdeaStarted{}, err
		}

		itemID, err := sess.Store.NextActionItemID(ctx)
		if err != nil {
			return tui.IdeaStarted{}, err
		}
		body := strings.TrimSpace(plan.Item.Body)
		if created.Scaffold.Ran && created.Scaffold.Err == nil {
			body += fmt.Sprintf("\n\n`%s` has already run in this directory. Extend what it produced; "+
				"do not replace or duplicate it.", created.Scaffold.Command)
		}
		item := store.ActionItem{
			ID:          itemID,
			ProjectSlug: created.Slug,
			Title:       strings.TrimSpace(plan.Item.Title),
			Body:        body,
			Rationale:   strings.TrimSpace(plan.Item.Rationale),
			Effort:      plan.Item.Effort,
			// Accepted rather than proposed: the user has already been
			// through the questionnaire and said yes to this exact item.
			Status:      store.ItemAccepted,
			CreatedAt:   now,
			UpdatedAt:   now,
			GeneratedBy: plan.Model,
		}
		if err := sess.Store.InsertActionItem(ctx, item); err != nil {
			return tui.IdeaStarted{}, err
		}
		return tui.IdeaStarted{ProjectName: created.Name, Dir: created.Dir, Item: item}, nil
	}
}
