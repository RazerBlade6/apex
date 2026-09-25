// Package claudecode dispatches the builder loop to the `claude` CLI
// (DESIGN.md §9).
//
// # This is not the claude-cli provider
//
// internal/provider/claudecli runs the same binary for the opposite purpose.
// It is an inference transport: `--tools ""`, `--restricted`,
// `--no-session-persistence`, no working directory, nothing may touch a file.
// This package runs a coding agent: tools enabled, edits accepted without a
// prompt, cwd set to the project, and the session deliberately persisted so a
// stalled run can be resumed.
//
// Copying one command line onto the other in either direction would break it.
// What the two genuinely share — finding the binary, killing a process group,
// and decoding the stream-json wire format — lives in internal/claudecmd and
// is used by both.
//
// # Unattended means unattended
//
// Nothing is watching a TTY. `--permission-prompts none` is therefore not a
// convenience: without it, anything that would ask blocks forever on a
// terminal nobody is reading, and the dispatch hangs holding the project lock
// until the user notices. With it, the same operation is denied, the agent is
// told so, and the run ends with an answer. `--permission-mode acceptEdits`
// allows file edits without confirmation while still gating the riskier
// operations; `bypassPermissions` is deliberately not the default here and
// should stay a per-run opt-in.
//
// # The log is for humans
//
// Everything is teed to the run's log file: a `#`-prefixed header naming the
// run, the session, the working directory and the full brief, then the CLI's
// stream-json verbatim, one object per line, then a footer with the outcome
// and any stderr. Comment lines mean the file is not strictly JSON Lines, and
// that is the right trade: the first reader of a failed dispatch is a person
// asking what was asked for.
package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/RazerBlade6/apex/internal/claudecmd"
	"github.com/RazerBlade6/apex/internal/executor"
)

// Name is the value that appears as `executor.default = "claudecode"` in
// config.toml.
const Name = "claudecode"

// Defaults for a dispatch. DESIGN.md §9's verified argv shows `--model opus
// --effort high`: the builder loop is the expensive, judgment-heavy half, and
// it runs on the Claude Code subscription rather than on an API key.
const (
	DefaultModel  = "opus"
	DefaultEffort = "high"
	// DefaultPermissionMode allows file edits without confirmation while
	// still gating operations that are not edits. Under it a dispatched
	// agent can write code and cannot build or test it, because
	// --permission-prompts none denies rather than asks.
	DefaultPermissionMode = "acceptEdits"
	// BypassPermissionMode grants everything. DESIGN.md §9 keeps it a
	// per-run opt-in, and the command layer only ever passes it when the
	// user asked for it by name.
	BypassPermissionMode = "bypassPermissions"
)

// killGrace is how long a cancelled process has to die before os/exec stops
// waiting on its pipes. The kill is a SIGKILL to the process group, so this
// is a backstop against an unkillable child, not a graceful shutdown window.
const killGrace = 5 * time.Second

// Options configure the executor.
type Options struct {
	// Binary overrides the executable. Empty looks one up — PATH first, then
	// the directories the installer uses. Tests point it at a fake; a user
	// whose claude lives somewhere unusual has a legitimate reason to set it.
	Binary string
	// Model and Effort are the CLI's --model and --effort. Empty uses the
	// defaults above.
	Model  string
	Effort string
	// PermissionMode is the CLI's --permission-mode. Empty uses
	// DefaultPermissionMode. Setting it to bypassPermissions is a per-run
	// decision the caller makes deliberately.
	PermissionMode string
	// Timeout bounds one dispatch. Zero means none, which is right for an
	// interactive run the user can interrupt and wrong for a cron job.
	Timeout time.Duration
	// InheritUserConfig lets the dispatched agent pick up the user's own
	// Claude Code configuration — the global ~/.claude/CLAUDE.md, personal
	// skills, hooks, plugins and settings. It defaults to false; see
	// ProjectMemoryHeading for why.
	InheritUserConfig bool
}

// Executor dispatches to the claude CLI.
type Executor struct {
	opts Options
}

// New builds the executor.
//
// It does not resolve the binary, because a constructor that fails on a
// machine without claude would stop `apex doctor` from being able to report
// the fact. Availability is a question with an answer, not an error to
// propagate: see Available.
func New(opts Options) *Executor { return &Executor{opts: opts} }

// Name identifies the executor.
func (e *Executor) Name() string { return Name }

// Available reports whether claude can actually be run here.
//
// It resolves the binary and nothing more. Running `claude --version` would
// prove a little more and cost a process start on the path of every dispatch;
// `apex doctor` runs the version check, which is where a slower, more
// thorough answer belongs.
func (e *Executor) Available() error {
	if _, err := claudecmd.Resolve(e.opts.Binary); err != nil {
		return &executor.UnavailableError{
			Executor: Name,
			Reason:   err.Error(),
			Fix: "install the Claude Code CLI and ensure it is on PATH\n" +
				"apex dispatches implementation work to it (DESIGN.md §9)",
			Err: err,
		}
	}
	return nil
}

// Binary reports the resolved path, for `apex doctor` and for the log header.
func (e *Executor) Binary() (string, error) { return claudecmd.Resolve(e.opts.Binary) }

func (e *Executor) model() string {
	if m := strings.TrimSpace(e.opts.Model); m != "" {
		return m
	}
	return DefaultModel
}

func (e *Executor) effort() string {
	if f := strings.TrimSpace(e.opts.Effort); f != "" {
		return f
	}
	return DefaultEffort
}

func (e *Executor) permissionMode() string {
	if m := strings.TrimSpace(e.opts.PermissionMode); m != "" {
		return m
	}
	return DefaultPermissionMode
}

// argv builds the command line, verified against claude 2.1.278 (DESIGN.md §9)
// and re-verified against 2.1.282 during M5.
//
// Every flag here is doing something the dispatch depends on:
//
//   - --output-format stream-json + --verbose: the CLI refuses the former
//     without the latter ("--output-format=stream-json requires --verbose").
//     The structured stream is what lets this package distinguish a tool call
//     from a file edit from the final result instead of scraping prose.
//   - --include-partial-messages: text as it is produced, so a long run shows
//     progress rather than silence.
//   - --session-id: Apex's own UUID, recorded on the exec_runs row, so a
//     stalled run is resumable with `claude --resume`.
//   - --permission-mode / --permission-prompts: see the package comment.
//   - --append-system-prompt: the brief. APPEND rather than replace, because
//     the CLI's own system prompt is what makes it a competent coding agent;
//     replacing it would throw that away to say the same thing worse.
//   - --safe-mode: the mechanism DESIGN.md §9 left open. See
//     ProjectMemoryHeading.
func (e *Executor) argv(brief executor.TaskBrief, projectMemory string) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
		"--permission-mode", e.permissionMode(),
		"--permission-prompts", "none",
	}
	if !e.opts.InheritUserConfig {
		args = append(args, "--safe-mode")
	}
	if id := strings.TrimSpace(brief.SessionID); id != "" {
		args = append(args, "--session-id", id)
	}
	if m := e.model(); m != "" {
		args = append(args, "--model", m)
	}
	if f := e.effort(); f != "" {
		args = append(args, "--effort", f)
	}
	args = append(args, "--append-system-prompt", systemPrompt(brief, projectMemory))

	// The positional prompt. The brief itself is the system prompt, so this
	// is the turn that starts the work: short, imperative, and pointing at
	// where the detail is. The CLI needs a prompt — with none it reads stdin,
	// which is nil here, and an unattended run must not depend on that.
	args = append(args, startPrompt(brief))
	return args
}

// startPrompt is the user turn that begins a dispatch.
func startPrompt(brief executor.TaskBrief) string {
	title := strings.TrimSpace(brief.Title)
	if title == "" {
		title = "the task described in your system prompt"
	}
	return fmt.Sprintf("Carry out this task in the current directory: %s\n\n"+
		"Your system prompt has the full brief, including what \"done\" means. "+
		"Read the project's own files before you change anything, and leave the "+
		"work uncommitted so it can be reviewed as a diff.", title)
}

// Run dispatches the brief. The returned channel is closed exactly once.
func (e *Executor) Run(ctx context.Context, brief executor.TaskBrief) (<-chan executor.Event, error) {
	if err := brief.Validate(); err != nil {
		return nil, err
	}
	if err := e.Available(); err != nil {
		return nil, err
	}
	bin, err := e.Binary()
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(brief.ProjectPath)
	if err != nil {
		return nil, fmt.Errorf("executor: project directory %s: %w", brief.ProjectPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("executor: project path %s is not a directory", brief.ProjectPath)
	}

	log, err := openLog(brief.LogPath)
	if err != nil {
		return nil, err
	}

	ch := make(chan executor.Event, 16)
	go e.run(ctx, bin, brief, log, ch)
	return ch, nil
}

// run owns the channel and the log file. The deferred close is the only one
// in the package, so the channel is closed exactly once whichever way this
// returns.
func (e *Executor) run(ctx context.Context, bin string, brief executor.TaskBrief, log *runLog, ch chan<- executor.Event) {
	defer close(ch)
	defer log.Close()

	if e.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.opts.Timeout)
		defer cancel()
	}

	// The project's own CLAUDE.md, re-supplied because --safe-mode disabled
	// the discovery that would otherwise have found it (see memory.go). A
	// read failure is not fatal: a dispatch with no project conventions is
	// worse than one with them, and far better than none at all.
	var projectMemory string
	if !e.opts.InheritUserConfig {
		body, err := readProjectMemory(brief.ProjectPath)
		if err != nil {
			executor.Emit(ctx, ch, executor.Event{Type: executor.EventNotice, Text: err.Error()})
		}
		projectMemory = body
	}

	args := e.argv(brief, projectMemory)
	started := time.Now()
	log.header(bin, args, brief, started)

	result, err := e.invoke(ctx, bin, args, brief, log, ch)
	if err != nil {
		err.LogPath = brief.LogPath
		log.footer(err.Kind.String(), time.Since(started), err.Message)
		executor.EmitFinal(ctx, ch, executor.Event{Type: executor.EventError, Err: err})
		return
	}
	result.Duration = time.Since(started)
	result.LogPath = brief.LogPath
	log.footer("success", result.Duration, fmt.Sprintf("%d file(s) changed, %d turn(s)",
		len(result.FilesChanged), result.Turns))
	executor.EmitFinal(ctx, ch, executor.Event{Type: executor.EventDone, Result: result})
}

// invoke runs one claude process, parses its stream, and emits events.
func (e *Executor) invoke(
	ctx context.Context,
	bin string,
	args []string,
	brief executor.TaskBrief,
	log *runLog,
	ch chan<- executor.Event,
) (*executor.Result, *executor.RunError) {

	cmd := exec.CommandContext(ctx, bin, args...)
	// The whole point of the dispatch: the agent works in the project, so it
	// reads PROJECT.md and CLAUDE.md from the working tree for free.
	cmd.Dir = brief.ProjectPath
	// Nothing is piped in. Left unset, a child that decided to read stdin
	// would inherit Apex's, and an unattended dispatch would hang on a
	// terminal.
	cmd.Stdin = nil

	var stderr claudecmd.TailBuffer
	cmd.Stderr = &stderr

	claudecmd.SetProcessGroup(cmd)
	// exec.CommandContext's default cancel signals only the process it
	// started. This signals the whole group, so a claude that spawned
	// children — and a coding agent spawns plenty — leaves none behind.
	cmd.Cancel = func() error { return claudecmd.KillProcessGroup(cmd) }
	cmd.WaitDelay = killGrace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &executor.RunError{
			Executor: Name, RunID: brief.RunID, Kind: executor.FailureUnknown,
			Message: executor.Truncate(err.Error()), Err: err,
		}
	}
	if err := cmd.Start(); err != nil {
		var notFound *exec.Error
		kind := executor.FailureUnknown
		if errors.As(err, &notFound) {
			kind = executor.FailureUnavailable
		}
		return nil, &executor.RunError{
			Executor: Name, RunID: brief.RunID, Kind: kind,
			Message: executor.Truncate(err.Error()), Err: err,
		}
	}

	st := &state{
		result:    &executor.Result{SessionID: brief.SessionID, Model: e.model()},
		seenFiles: map[string]bool{},
		emit: func(ev executor.Event) bool {
			return executor.Emit(ctx, ch, ev)
		},
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), claudecmd.MaxLineBytes)
	for scanner.Scan() {
		raw := scanner.Text()
		log.line(raw)
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var ln claudecmd.Line
		if err := json.Unmarshal([]byte(raw), &ln); err != nil {
			// A line Apex cannot parse is not a reason to fail a run that is
			// otherwise working: the CLI is free to add event types. The
			// result event is what decides the outcome.
			continue
		}
		if !st.consume(&ln) {
			// The consumer walked away. Nothing will read the rest of
			// stdout, so the child would block on a full pipe forever.
			_ = claudecmd.KillProcessGroup(cmd)
			break
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	if tail := strings.TrimSpace(stderr.String()); tail != "" {
		log.stderr(tail)
	}
	if runErr := classify(ctx, brief.RunID, st, waitErr, scanErr, &stderr); runErr != nil {
		return nil, runErr
	}
	return st.finish(), nil
}

// openLog creates the run's log file. An empty path disables logging, which
// is what a caller that only wants the event stream passes.
func openLog(path string) (*runLog, error) {
	if strings.TrimSpace(path) == "" {
		return &runLog{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("executor: create log directory for %s: %w", path, err)
	}
	// 0o600: everything under ~/.apex is single-user, and a dispatch log
	// carries the user's own project work.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("executor: open log %s: %w", path, err)
	}
	return &runLog{f: f}, nil
}
