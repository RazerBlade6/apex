package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/executor"
	"github.com/RazerBlade6/apex/internal/executor/claudecode"
	"github.com/RazerBlade6/apex/internal/lock"
	"github.com/RazerBlade6/apex/internal/project"
	"github.com/RazerBlade6/apex/internal/store"
)

// This file is the half of `apex do` and `apex start` that is identical:
// taking the project lock, opening the exec_runs row, streaming the agent's
// output, and closing both records however the run ends.
//
// It exists because the bookkeeping is where an unattended dispatch goes
// quietly wrong. An item left `in_progress` after a Ctrl-C, or an exec_runs
// row with no finished_at, is invisible until the user wonders why a project
// looks stuck — so every exit path from runDispatch closes both.

// dispatchOptions are the per-run flags `do` and `start` share.
type dispatchOptions struct {
	// Model and Effort override [executor] in config.toml for one run.
	Model  string
	Effort string
	// Timeout bounds the run. Zero means none.
	Timeout time.Duration
	// Bypass runs the agent with every permission granted rather than only
	// file edits. DESIGN.md §9 keeps this a per-run opt-in and this flag is
	// exactly that; see permissionConstraint for why it exists at all.
	Bypass bool
}

// newExecutor builds the configured executor.
//
// It is a variable rather than a direct call for the same reason
// providerFactory is: every command test must be able to run a dispatch end
// to end without starting the real `claude`, which would spend the user's
// subscription quota on `go test`. Nothing in Apex reassigns it; only tests
// do, and they restore it.
var newExecutor = func(cfg *config.Config, opts dispatchOptions) (executor.Executor, error) {
	model := opts.Model
	if model == "" {
		model = cfg.Executor.Model
	}
	effort := opts.Effort
	if effort == "" {
		effort = cfg.Executor.Effort
	}
	mode := claudecode.DefaultPermissionMode
	if opts.Bypass {
		mode = claudecode.BypassPermissionMode
	}
	switch cfg.Executor.Default {
	case claudecode.Name:
		return claudecode.New(claudecode.Options{
			Model:          model,
			Effort:         effort,
			PermissionMode: mode,
			Timeout:        opts.Timeout,
		}), nil
	default:
		return nil, &unknownExecutorError{Name: cfg.Executor.Default, ConfigPath: cfg.Path()}
	}
}

// dispatchSpec is one unit of work to hand to an executor.
type dispatchSpec struct {
	// ProjectSlug and ProjectPath identify what is being worked on. The slug
	// names the lock file and the exec_runs row; the path is the agent's
	// working directory.
	ProjectSlug string
	ProjectPath string
	ProjectName string
	// ActionItemID is empty for a dispatch with no item behind it.
	ActionItemID string
	// Title is the one line the run is about.
	Title string
	// Intent becomes the brief. It carries what to do and why, never the
	// project background the agent reads from the working tree.
	Intent executor.Intent
	// Bypass records that this run was granted every permission, so the
	// brief can tell the agent what it may actually do.
	Bypass bool
	// OnStatus moves the action item through its lifecycle. It is a callback
	// rather than a status field because `apex start` has no item to move.
	// It is always called with a context that is still live, even when the
	// run was cancelled.
	OnStatus func(ctx context.Context, status string) error
}

// runDispatch is DESIGN.md §14's `apex do`, steps 2 and 4 through 6, shared
// with `apex start`'s third tier.
func runDispatch(ctx context.Context, w io.Writer, sess *session, exec executor.Executor, spec dispatchSpec) (*executor.Result, error) {
	// 2. Available() before anything else. Apex must never assume a binary is
	//    present (DESIGN.md §9), and finding out after the status has moved
	//    to in_progress would leave the item wrong.
	if err := exec.Available(); err != nil {
		return nil, err
	}

	runID := lock.NewRunID()
	sessionID, err := executor.NewSessionID()
	if err != nil {
		return nil, err
	}
	logPath, err := config.ExecLogPath(runID)
	if err != nil {
		return nil, err
	}

	// The per-project exclusive lock, held for the whole run (DESIGN.md §10).
	// Two agents editing one working tree produce interleaved edits and
	// conflicting writes with no way to attribute either. flock is the right
	// primitive because the kernel releases it when this process dies.
	lockPath := filepath.Join(sess.Root, "locks", spec.ProjectSlug+".lock")
	l, err := lock.AcquireAs(lockPath, lock.Exclusive, runID)
	if err != nil {
		var busy *lock.BusyError
		if errors.As(err, &busy) {
			return nil, busyProjectError(sess.Root, spec.ProjectName, busy)
		}
		return nil, err
	}
	defer l.Release() //nolint:errcheck // the kernel releases it either way

	// A cancelled run still has to close its own records. The user pressing
	// Ctrl-C cancels ctx, and every store call on it would then fail — which
	// is exactly how an item gets stranded in_progress with an exec_runs row
	// that never finished.
	book := context.WithoutCancel(ctx)

	started := time.Now()
	row := store.ExecRun{
		ID:           runID,
		ActionItemID: spec.ActionItemID,
		ProjectSlug:  spec.ProjectSlug,
		Executor:     exec.Name(),
		SessionID:    sessionID,
		StartedAt:    started,
		LogPath:      logPath,
	}
	if err := sess.Store.InsertExecRun(ctx, row); err != nil {
		return nil, err
	}
	if spec.OnStatus != nil {
		if err := spec.OnStatus(ctx, store.ItemInProgress); err != nil {
			return nil, err
		}
	}

	// What the agent can actually do is part of its intent, not a detail of
	// the transport: an agent that does not know its commands will be denied
	// spends a turn finding out, and then reports work it could not verify
	// as though verification had merely been skipped.
	spec.Intent.Constraints = append(spec.Intent.Constraints, permissionConstraint(spec.Bypass))

	brief := executor.TaskBrief{
		RunID:        runID,
		SessionID:    sessionID,
		ActionItemID: spec.ActionItemID,
		ProjectPath:  spec.ProjectPath,
		Title:        spec.Title,
		Instruction:  spec.Intent.Render(),
		ContextPaths: spec.Intent.ContextPaths,
		LogPath:      logPath,
	}

	fmt.Fprintf(w, "run %s · %s · session %s\n", runID, exec.Name(), sessionID)
	fmt.Fprintf(w, "working in %s\n", spec.ProjectPath)
	fmt.Fprintf(w, "log %s\n\n", logPath)

	result, runErr := stream(ctx, w, exec, brief)

	// Close both records, whatever happened. Apex never writes `done`
	// (DESIGN.md §14): the furthest a successful run moves an item is
	// in_review, because whether the work is finished is the user's judgment
	// after reading the diff.
	exitStatus := store.ExitSuccess
	itemStatus := store.ItemInReview
	if runErr != nil {
		exitStatus = store.ExitFailed
		itemStatus = store.ItemAccepted
		var rerr *executor.RunError
		if errors.As(runErr, &rerr) && rerr.Kind == executor.FailureCancelled {
			exitStatus = store.ExitCancelled
		}
	}
	if err := sess.Store.FinishExecRun(book, runID, exitStatus, time.Now()); err != nil {
		return nil, err
	}
	if spec.OnStatus != nil {
		if err := spec.OnStatus(book, itemStatus); err != nil {
			return nil, err
		}
	}
	if runErr != nil {
		return nil, runErr
	}

	writeResult(ctx, w, result, spec)
	return result, nil
}

// permissionConstraint tells the agent what this run is allowed to do.
//
// This exists because of what the first real dispatch did. Under
// --permission-mode acceptEdits the agent may write files and may NOT run
// commands, so its build-and-test command was denied; it spent a turn
// discovering that, then correctly reported a change it had not been able to
// verify. The brief asks for "the change builds, and the project's existing
// tests still pass", which under that mode is an acceptance criterion the
// agent cannot possibly meet.
//
// Saying so up front is the honest half of the fix. --bypass-permissions is
// the other half, and it stays opt-in per DESIGN.md §9: granting an
// unattended agent every permission in a project directory is a decision the
// user should make on purpose, not one Apex makes for them.
func permissionConstraint(bypass bool) string {
	if bypass {
		return "You may run commands in this directory, including builds and tests. " +
			"Verify your change rather than asserting it works."
	}
	return "You can edit files but you CANNOT run commands in this run: anything needing approval " +
		"is denied automatically, with no prompt. Do not retry a denied command. Make the change, " +
		"then state plainly which acceptance criteria you could not verify and what the user should run."
}

// stream drains the executor's events onto the terminal.
//
// The agent's prose is written as it arrives, because a dispatch that takes
// four minutes and prints nothing is indistinguishable from one that hung.
// Tool calls and edits get their own marked lines, which is what parsing the
// structured stream rather than scraping stdout buys: a reader can see the
// shape of the work without reading every word of it.
func stream(ctx context.Context, w io.Writer, exec executor.Executor, brief executor.TaskBrief) (*executor.Result, error) {
	events, err := exec.Run(ctx, brief)
	if err != nil {
		return nil, err
	}

	var (
		result  *executor.Result
		failure error
		midLine bool
	)
	// newline closes a partially written line of agent prose before a marked
	// line interrupts it.
	newline := func() {
		if midLine {
			fmt.Fprintln(w)
			midLine = false
		}
	}

	for ev := range events {
		switch ev.Type {
		case executor.EventOutput:
			if ev.Text == "" {
				continue
			}
			fmt.Fprint(w, ev.Text)
			midLine = !strings.HasSuffix(ev.Text, "\n")
		case executor.EventFileEdit:
			newline()
			fmt.Fprintf(w, "  ± %s %s\n", ev.Tool, ev.Path)
		case executor.EventToolUse:
			newline()
			if ev.Text == "" {
				fmt.Fprintf(w, "  · %s\n", ev.Tool)
				continue
			}
			fmt.Fprintf(w, "  · %s %s\n", ev.Tool, ev.Text)
		case executor.EventNotice:
			newline()
			fmt.Fprintf(w, "  ! %s\n", ev.Text)
		case executor.EventDone:
			result = ev.Result
		case executor.EventError:
			// Keep draining: the contract is that the channel closes, and
			// abandoning it here would strand the executor's goroutine.
			if failure == nil {
				failure = ev.Err
			}
		}
	}
	newline()

	if failure != nil {
		return nil, failure
	}
	if result == nil {
		return nil, fmt.Errorf("the executor ended without reporting a result")
	}
	return result, nil
}

// writeResult prints what the run did and what the user does next.
//
// The file list comes from `git status --short`, not from the agent's own
// account of itself, and the second real dispatch of M5 is why. Run with
// --bypass-permissions, the agent wrote both its files with shell heredocs
// rather than the Write tool, so the tool-derived list was empty while the
// working tree held two new files — and Apex told the user nothing had
// changed. The agent's list is a claim; the working tree is the fact, and the
// fact is also what the user is about to read as a diff.
func writeResult(ctx context.Context, w io.Writer, result *executor.Result, spec dispatchSpec) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Done in %s · %s · %s",
		result.Duration.Round(time.Second), dash(result.Model), describeUsage(result.Usage))
	if result.CostUSD > 0 {
		fmt.Fprintf(w, " · $%.4f", result.CostUSD)
	}
	fmt.Fprintln(w)

	changed, tracked := workingTreeChanges(ctx, spec.ProjectPath)
	switch {
	case !tracked && len(result.FilesChanged) > 0:
		// Not a repository: the agent's own list is all there is.
		fmt.Fprintf(w, "%d file(s) the agent reported changing:\n", len(result.FilesChanged))
		for _, path := range result.FilesChanged {
			fmt.Fprintf(w, "  %s\n", relativeTo(spec.ProjectPath, path))
		}
	case !tracked:
		fmt.Fprintln(w, "Not a git repository, and the agent reported no file changes.")
		fmt.Fprintln(w, "Read the log before assuming nothing happened.")
	case len(changed) == 0:
		fmt.Fprintln(w, "The working tree is unchanged. Read the log before assuming nothing happened:")
		fmt.Fprintf(w, "  %s\n", result.LogPath)
	default:
		fmt.Fprintf(w, "%d change(s) in the working tree:\n", len(changed))
		for _, line := range changed {
			fmt.Fprintf(w, "  %s\n", line)
		}
		// A disagreement is worth one line. An agent that edited through a
		// shell rather than the edit tools reports nothing, and a list that
		// silently differed from the diff would be worse than no list.
		if n := len(result.FilesChanged); n != len(changed) {
			fmt.Fprintf(w, "(the agent itself reported %d; the working tree is what counts)\n", n)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  git -C %s diff    to review the work\n", spec.ProjectPath)
	if spec.ActionItemID != "" {
		fmt.Fprintf(w, "  apex show %s      the item, now in_review\n", spec.ActionItemID)
		fmt.Fprintln(w, "\nApex does not mark items done. That is yours to decide after reading the diff.")
	}
}

// workingTreeChanges returns `git status --short` for the project, and whether
// it is a repository at all. A git failure is not worth failing a finished
// dispatch over: the run already happened and the log already has everything.
func workingTreeChanges(ctx context.Context, dir string) (lines []string, isRepo bool) {
	state, err := project.InspectGit(ctx, dir)
	if err != nil || !state.IsRepo {
		return nil, false
	}
	return state.Status, true
}

// relativeTo shortens an absolute path the agent reported to one relative to
// the project, which is how the user thinks about their own files.
func relativeTo(base, path string) string {
	if rel, err := filepath.Rel(base, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// busyProjectError is DESIGN.md §10's message: the contention is reported
// with the run that holds it, not as a bare "locked".
func busyProjectError(root, projectName string, busy *lock.BusyError) error {
	h := busy.HolderInfo()
	var b strings.Builder
	fmt.Fprintf(&b, "%s is already being worked on", projectName)
	if h.RunID != "" {
		fmt.Fprintf(&b, "\n  run %s", h.RunID)
		if h.PID != 0 {
			fmt.Fprintf(&b, " (pid %d)", h.PID)
		}
		if !h.Started.IsZero() {
			fmt.Fprintf(&b, ", started %s", humanAge(h.Started))
		}
		fmt.Fprintf(&b, "\n  %s   the run's output",
			filepath.Join(root, "logs", "exec", h.RunID+".log"))
	} else {
		b.WriteString("\n  by another apex process")
	}
	b.WriteString("\n  wait for it to finish, or stop that process")
	return &busyError{msg: b.String()}
}

type busyError struct{ msg string }

func (e *busyError) Error() string     { return e.msg }
func (e *busyError) UserFixable() bool { return true }

// unknownExecutorError reports an executor.default this build cannot build.
type unknownExecutorError struct{ Name, ConfigPath string }

func (e *unknownExecutorError) Error() string {
	return fmt.Sprintf("executor %q is not implemented in this build\n"+
		"  set executor.default = %q in %s",
		e.Name, claudecode.Name, e.ConfigPath)
}

func (e *unknownExecutorError) UserFixable() bool { return true }
