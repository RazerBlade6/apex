package main

import (
	"context"
	"io"

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
		Store:    sess.Store,
		Config:   sess.Config,
		Advisor:  sess.advisorFor(),
		Root:     sess.Root,
		Version:  version,
		Dispatch: dispatcherFor(sess),
		Input:    in,
		Output:   out,
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
