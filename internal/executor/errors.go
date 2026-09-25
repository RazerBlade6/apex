package executor

import (
	"fmt"
	"strings"
	"time"
)

// FailureKind classifies a dispatch that did not succeed.
//
// It is much coarser than provider.Kind, and deliberately so: the advisor loop
// retries and routes around failures, while a dispatch either produced a diff
// or did not. What the command layer actually needs from this is three
// decisions — what exit_status to record on the exec_runs row, whether to say
// anything about quota, and whether to blame the environment or Apex.
type FailureKind int

const (
	// FailureUnknown is a run that failed for a reason Apex could not
	// classify. The agent's own message is carried verbatim.
	FailureUnknown FailureKind = iota
	// FailureCancelled is the user's context ending: Ctrl-C, or a timeout.
	// It records exit_status "cancelled", not "failed".
	FailureCancelled
	// FailureSessionLimit is the Claude subscription's usage window being
	// spent. DESIGN.md §8: this is not an API rate limit, it resets in
	// hours, and the window is shared with the user's own Claude Code work.
	FailureSessionLimit
	// FailureUnavailable is the executor's binary being absent. It is
	// normally caught by Available() before a run starts.
	FailureUnavailable
)

func (k FailureKind) String() string {
	switch k {
	case FailureCancelled:
		return "cancelled"
	case FailureSessionLimit:
		return "session_limit"
	case FailureUnavailable:
		return "unavailable"
	default:
		return "failed"
	}
}

// RunError is a dispatch that failed, classified.
//
// Message holds the agent's own words, truncated. It never holds a credential:
// the executor passes none — the agent authenticates itself through the user's
// own Claude Code login (DESIGN.md §9) — and Apex only ever reads the agent's
// stdout and stderr.
type RunError struct {
	Executor string
	RunID    string
	Kind     FailureKind
	Message  string
	// ResetsAt is when a spent subscription window reopens, when the agent
	// reported one. Nothing infers it.
	ResetsAt time.Time
	// LogPath is where the whole run was recorded, which is the first thing
	// a user wants after a failure.
	LogPath string
	Err     error
}

func (e *RunError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s run %s %s", e.Executor, e.RunID, e.Kind)
	if !e.ResetsAt.IsZero() {
		fmt.Fprintf(&b, " (resets %s)", e.ResetsAt.Format(time.RFC3339))
	}
	switch {
	case e.Message != "":
		fmt.Fprintf(&b, ": %s", e.Message)
	case e.Err != nil:
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	if e.LogPath != "" {
		fmt.Fprintf(&b, "\n  full output: %s", e.LogPath)
	}
	return b.String()
}

func (e *RunError) Unwrap() error { return e.Err }

// UserFixable marks everything except an unclassifiable failure as an
// environment problem rather than a bug in Apex.
func (e *RunError) UserFixable() bool { return e.Kind != FailureUnknown }

// maxMessageLen bounds what an agent's message contributes to an error
// string. A run that fails by printing a stack trace must not fill the
// terminal.
const maxMessageLen = 400

// Truncate shortens an agent message for display.
func Truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxMessageLen {
		return s
	}
	return s[:maxMessageLen] + "…"
}
