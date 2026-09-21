// Command apex is a personal project-management and development hub.
//
// M1 and M2 implement the foundation and the context layer: `apex doctor`,
// `apex config`, and `apex sync`. The advisory, executor, and TUI surfaces
// described in DESIGN.md §11 arrive in later milestones.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Ctrl-C cancels the context rather than killing the process outright, so
	// anything holding a lock or a transaction can unwind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		reportError(err)
		os.Exit(1)
	}
}

// userFixable marks errors caused by the environment (a missing key, a missing
// binary, a busy lock) rather than by a bug in Apex.
type userFixable interface{ UserFixable() bool }

func reportError(err error) {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "cancelled")
		return
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)

	var uf userFixable
	if errors.As(err, &uf) && uf.UserFixable() {
		// The message already names the fix; say nothing further.
		return
	}
	fmt.Fprintln(os.Stderr, "\nThis is an internal failure, not an environment problem.")
	fmt.Fprintln(os.Stderr, "Run `apex doctor` to rule out the environment first.")
}
