package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RazerBlade6/apex/internal/executor"
)

// Nothing in this file runs the real claude. Every test replays the
// stream-json shape captured in internal/claudecmd from a shell script, so the
// suite is offline, deterministic, and costs no subscription quota — which
// matters more here than in the provider package, because a real run of THIS
// code would edit files.

// fake is a shell script impersonating the claude CLI. It records its argv and
// its working directory, so a test can assert on both.
type fake struct {
	bin  string
	argv string
	cwd  string
}

func newFake(t *testing.T, body string) *fake {
	t.Helper()
	dir := t.TempDir()
	f := &fake{
		bin:  filepath.Join(dir, "claude"),
		argv: filepath.Join(dir, "argv"),
		cwd:  filepath.Join(dir, "cwd"),
	}
	script := strings.Join([]string{
		"#!/bin/sh",
		": > '" + f.argv + "'",
		`for a in "$@"; do printf '%s\n' "$a" >> '` + f.argv + `'; done`,
		"pwd > '" + f.cwd + "'",
		body,
		"",
	}, "\n")
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return f
}

func (f *fake) args(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(f.argv)
	if err != nil {
		t.Fatalf("the fake claude recorded no argv: %v", err)
	}
	out := strings.Split(string(body), "\n")
	if n := len(out); n > 0 && out[n-1] == "" {
		out = out[:n-1]
	}
	return out
}

func (f *fake) workingDir(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(f.cwd)
	if err != nil {
		t.Fatalf("the fake claude recorded no working directory: %v", err)
	}
	return strings.TrimSpace(string(body))
}

// emit renders shell that prints each line to stdout, one JSON object per
// line, exactly as the CLI does.
func emit(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		if strings.Contains(l, "'") {
			panic("fixture contains a single quote: " + l)
		}
		fmt.Fprintf(&b, "printf '%%s\\n' '%s'\n", l)
	}
	return b.String()
}

const (
	lineInit = `{"type":"system","subtype":"init","model":"claude-sonnet-5","apiKeySource":"none","session_id":"11111111-2222-4333-8444-555555555555","cwd":"/tmp"}`
	lineText = `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Working on it."}}}`
	// One tool call that changes a file and one that does not, in the
	// assembled assistant message the CLI emits alongside the deltas.
	lineTools = `{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-5","content":[` +
		`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}},` +
		`{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"/work/chunk.go"}},` +
		`{"type":"tool_use","id":"t3","name":"Write","input":{"file_path":"/work/chunk_test.go"}}]}}`
	lineDenied = `{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t9","is_error":true,"content":"permission denied: Bash"}]}}`
)

func lineResult(text string) string {
	return `{"type":"result","subtype":"success","is_error":false,"result":"` + text + `",` +
		`"num_turns":3,"total_cost_usd":0.0421,` +
		`"usage":{"input_tokens":12,"output_tokens":340,"cache_read_input_tokens":900,"cache_creation_input_tokens":1500},` +
		`"modelUsage":{"claude-sonnet-5":{}}}`
}

func lineFailure(text string) string {
	return `{"type":"result","subtype":"error","is_error":true,"result":"` + text + `","num_turns":1}`
}

// projectDir is a throwaway working tree for a dispatch.
func projectDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PROJECT.md"), []byte("---\nname: Fixture\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func briefFor(t *testing.T, dir string) executor.TaskBrief {
	t.Helper()
	return executor.TaskBrief{
		RunID:        "abc123",
		SessionID:    "11111111-2222-4333-8444-555555555555",
		ActionItemID: "AI-007",
		ProjectPath:  dir,
		Title:        "Make chunking table-aware",
		Instruction:  "# Apex dispatch\n\nDo the thing, because the digest says so.",
		LogPath:      filepath.Join(t.TempDir(), "abc123.log"),
	}
}

// drain runs a dispatch to completion and returns everything it emitted.
func drain(t *testing.T, e *Executor, brief executor.TaskBrief) ([]executor.Event, *executor.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	events, err := e.Run(ctx, brief)
	if err != nil {
		return nil, nil, err
	}
	var (
		all    []executor.Event
		result *executor.Result
		failed error
	)
	for ev := range events {
		all = append(all, ev)
		switch ev.Type {
		case executor.EventDone:
			result = ev.Result
		case executor.EventError:
			failed = ev.Err
		}
	}
	return all, result, failed
}

// TestArgvIsADispatchNotAnAdvisorCall is the flag-level statement that this
// executor and the claude-cli PROVIDER have opposite requirements. Copying
// either command line onto the other breaks it, so both halves are asserted:
// what a dispatch must have, and what it must never inherit.
func TestArgvIsADispatchNotAnAdvisorCall(t *testing.T) {
	dir := projectDir(t)
	f := newFake(t, emit(lineInit, lineText, lineResult("done")))
	e := New(Options{Binary: f.bin, Model: "sonnet", Effort: "low"})

	if _, _, err := drain(t, e, briefFor(t, dir)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	args := f.args(t)

	for _, pair := range [][2]string{
		{"--permission-mode", "acceptEdits"},
		{"--permission-prompts", "none"},
		{"--session-id", "11111111-2222-4333-8444-555555555555"},
		{"--model", "sonnet"},
		{"--effort", "low"},
		{"--output-format", "stream-json"},
	} {
		i := slices.Index(args, pair[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != pair[1] {
			t.Errorf("argv = %q, want %s %s", args, pair[0], pair[1])
		}
	}
	for _, flag := range []string{"-p", "--verbose", "--include-partial-messages"} {
		if !slices.Contains(args, flag) {
			t.Errorf("argv = %q, want %s", args, flag)
		}
	}

	// The brief travels as an appended system prompt, not a replacement: the
	// CLI's own system prompt is what makes it a competent coding agent.
	//
	// The fake records one line per line of argv, so a multi-line brief
	// arrives split; the flag's position is checked exactly and the body by
	// containment.
	i := slices.Index(args, "--append-system-prompt")
	if i < 0 || i+1 >= len(args) || args[i+1] != "# Apex dispatch" {
		t.Errorf("argv = %q, want the brief on --append-system-prompt", args)
	}
	if !strings.Contains(strings.Join(args, "\n"), "Do the thing") {
		t.Errorf("argv = %q, want the whole brief passed through", args)
	}
	if slices.Contains(args, "--system-prompt") {
		t.Error("argv replaces the CLI's system prompt instead of appending to it")
	}

	// The provider's isolation flags would make a dispatch useless: with no
	// tools it cannot edit anything, and --restricted strips the tools that
	// run code. This is the assertion that stops a future refactor from
	// "sharing" the two argv builders.
	for _, forbidden := range []string{"--tools", "--restricted", "--no-session-persistence", "--bare"} {
		if slices.Contains(args, forbidden) {
			t.Errorf("argv carries %s, which belongs to the advisor provider and breaks a dispatch: %q", forbidden, args)
		}
	}

	// And the agent must actually be standing in the project.
	if got := f.workingDir(t); !strings.HasSuffix(got, filepath.Base(dir)) {
		t.Errorf("the agent ran in %q, want the project directory %q", got, dir)
	}
}

// TestStreamIsParsedIntoTypedEvents covers DESIGN.md §9's "parse stream-json,
// never scrape stdout": a tool call, a file edit and the final result are
// three different things and arrive as three different events.
func TestStreamIsParsedIntoTypedEvents(t *testing.T) {
	dir := projectDir(t)
	f := newFake(t, emit(lineInit, lineText, lineTools, lineDenied, lineResult("Changed two files.")))
	e := New(Options{Binary: f.bin})

	events, result, err := drain(t, e, briefFor(t, dir))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var edits, tools, notices, outputs int
	for _, ev := range events {
		switch ev.Type {
		case executor.EventFileEdit:
			edits++
		case executor.EventToolUse:
			tools++
			if ev.Tool == "Bash" && !strings.Contains(ev.Text, "go test") {
				t.Errorf("the Bash call did not carry its command: %+v", ev)
			}
		case executor.EventNotice:
			notices++
		case executor.EventOutput:
			outputs++
		}
	}
	if edits != 2 {
		t.Errorf("saw %d file edits, want 2 (Edit and Write)", edits)
	}
	if tools != 1 {
		t.Errorf("saw %d plain tool calls, want 1 (Bash); an edit must not be reported as one", tools)
	}
	if notices != 1 {
		t.Errorf("saw %d notices, want 1: a denied tool is the one thing an unattended run must surface", notices)
	}
	if outputs == 0 {
		t.Error("the agent's text was not emitted")
	}

	if result == nil {
		t.Fatal("no result")
	}
	if got := result.FilesChanged; len(got) != 2 || got[0] != "/work/chunk.go" || got[1] != "/work/chunk_test.go" {
		t.Errorf("FilesChanged = %q, want both edited files in a stable order", got)
	}
	if result.Usage.OutputTokens != 340 || result.Usage.CacheWriteTokens != 1500 {
		t.Errorf("Usage = %+v, want the result event's accounting", result.Usage)
	}
	if result.Turns != 3 || result.CostUSD == 0 {
		t.Errorf("Turns/Cost = %d/%v, want the result event's values", result.Turns, result.CostUSD)
	}
	if result.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want the model that actually served the run", result.Model)
	}
	if result.SessionID != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("SessionID = %q, want the id apex generated", result.SessionID)
	}
}

// TestRunIsTeedToItsLog: DESIGN.md §9 requires the whole run on disk, and the
// brief with it — a dispatch that did the wrong thing is nearly always one
// that was asked for the wrong thing.
func TestRunIsTeedToItsLog(t *testing.T) {
	dir := projectDir(t)
	f := newFake(t, emit(lineInit, lineText, lineResult("done")))
	e := New(Options{Binary: f.bin})
	brief := briefFor(t, dir)

	if _, _, err := drain(t, e, brief); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	body, err := os.ReadFile(brief.LogPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(body)
	for _, want := range []string{
		"apex exec run abc123",
		"AI-007",
		brief.SessionID,
		"claude --resume " + brief.SessionID,
		"Do the thing",      // the brief
		`"type":"result"`,   // the stream, verbatim
		"--- success after", // the outcome
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not contain %q:\n%s", want, log)
		}
	}
}

// TestFailureIsClassified: a failed run is reported with the agent's own
// words, and a spent subscription window is not reported as a generic failure.
func TestFailureIsClassified(t *testing.T) {
	dir := projectDir(t)

	t.Run("the agent reported an error", func(t *testing.T) {
		f := newFake(t, emit(lineInit, lineFailure("the file does not exist")))
		e := New(Options{Binary: f.bin})

		_, _, err := drain(t, e, briefFor(t, dir))
		var rerr *executor.RunError
		if !errors.As(err, &rerr) {
			t.Fatalf("err = %T (%v), want *executor.RunError", err, err)
		}
		if rerr.Kind != executor.FailureUnknown {
			t.Errorf("Kind = %v, want an unclassified failure", rerr.Kind)
		}
		if !strings.Contains(rerr.Message, "the file does not exist") {
			t.Errorf("Message = %q, want the agent's own words", rerr.Message)
		}
	})

	t.Run("the subscription window is spent", func(t *testing.T) {
		f := newFake(t, emit(lineInit, lineFailure("Usage limit reached"))+"exit 1\n")
		e := New(Options{Binary: f.bin})

		_, _, err := drain(t, e, briefFor(t, dir))
		var rerr *executor.RunError
		if !errors.As(err, &rerr) || rerr.Kind != executor.FailureSessionLimit {
			t.Fatalf("err = %v, want a session limit; a dispatch that exhausted the window shared with the user's own Claude Code sessions must say so", err)
		}
		if !rerr.UserFixable() {
			t.Error("a spent window is the user's to wait out, not an internal failure")
		}
	})
}

// TestCancellationStopsTheRun: a cancelled dispatch reports why it ended
// rather than closing its channel in silence.
func TestCancellationStopsTheRun(t *testing.T) {
	dir := projectDir(t)
	f := newFake(t, emit(lineInit, lineText)+"sleep 30\n")
	e := New(Options{Binary: f.bin})

	ctx, cancel := context.WithCancel(context.Background())
	events, err := e.Run(ctx, briefFor(t, dir))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var failure error
	for ev := range events {
		if ev.Type == executor.EventOutput {
			cancel() // the agent is alive; walk away
		}
		if ev.Type == executor.EventError {
			failure = ev.Err
		}
	}
	cancel()

	var rerr *executor.RunError
	if !errors.As(failure, &rerr) || rerr.Kind != executor.FailureCancelled {
		t.Fatalf("err = %v, want a cancelled run", failure)
	}
}

// TestAvailableRefusesAMissingBinary: Available() is load-bearing, not
// decoration (DESIGN.md §9).
func TestAvailableRefusesAMissingBinary(t *testing.T) {
	e := New(Options{Binary: filepath.Join(t.TempDir(), "absent")})

	err := e.Available()
	var unavailable *executor.UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("Available() = %v, want *executor.UnavailableError", err)
	}
	if !unavailable.UserFixable() || !strings.Contains(err.Error(), "install") {
		t.Errorf("err = %q, want it to name the fix", err)
	}

	// And a dispatch stops there rather than discovering it mid-run.
	if _, runErr := e.Run(context.Background(), briefFor(t, projectDir(t))); runErr == nil {
		t.Error("Run started with no binary to run")
	}
}

// TestBypassIsOptInAndExplicit: DESIGN.md §9 keeps bypassPermissions a per-run
// choice. The default must be the narrower mode, and asking for the wider one
// must actually change the command line rather than being quietly ignored.
func TestBypassIsOptInAndExplicit(t *testing.T) {
	dir := projectDir(t)

	for mode, want := range map[string]string{
		"":                   DefaultPermissionMode,
		BypassPermissionMode: BypassPermissionMode,
	} {
		f := newFake(t, emit(lineInit, lineResult("done")))
		e := New(Options{Binary: f.bin, PermissionMode: mode})
		if _, _, err := drain(t, e, briefFor(t, dir)); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		args := f.args(t)
		i := slices.Index(args, "--permission-mode")
		if i < 0 || i+1 >= len(args) || args[i+1] != want {
			t.Errorf("PermissionMode %q produced argv %q, want --permission-mode %s", mode, args, want)
		}
		// Whatever the mode, nothing may prompt: no terminal is watching.
		if j := slices.Index(args, "--permission-prompts"); j < 0 || args[j+1] != "none" {
			t.Errorf("argv = %q, want --permission-prompts none in every mode", args)
		}
	}
}
