package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RazerBlade6/apex/internal/provider"
)

// newProvider builds a Provider pointed at a fake binary.
func newProvider(t *testing.T, bin string) *Provider {
	t.Helper()
	p, err := New(provider.Config{Binary: bin, Model: "opus", Effort: "high"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// ask is the request every test sends.
func ask(text string) provider.Request {
	return provider.Request{
		System:   "identity context",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: text}},
	}
}

// drain reads a stream to close and returns the assembled text, the usage
// from the single EventDone, and the error from the single EventError.
//
// It also fails the test on the contract violations that matter: more than
// one terminal event, or a channel that is not closed.
func drain(t *testing.T, events <-chan provider.Event) (string, *provider.Usage, error) {
	t.Helper()
	var (
		text      strings.Builder
		usage     *provider.Usage
		err       error
		terminals int
	)
	for ev := range events {
		switch ev.Type {
		case provider.EventTextDelta:
			text.WriteString(ev.Text)
		case provider.EventDone:
			terminals++
			usage = ev.Usage
		case provider.EventError:
			terminals++
			err = ev.Err
		}
	}
	if terminals != 1 {
		t.Errorf("stream carried %d terminal events, want exactly 1", terminals)
	}
	return text.String(), usage, err
}

func TestStreamHappyPath(t *testing.T) {
	f := newFake(t, okStream("Hello world"))
	p := newProvider(t, f.bin)

	events, err := p.Stream(context.Background(), ask("ping"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, usage, streamErr := drain(t, events)
	if streamErr != nil {
		t.Fatalf("stream failed: %v", streamErr)
	}

	// The CLI reports the text twice — as deltas and as the assembled
	// assistant message. Exactly one of them must reach the caller.
	if text != "Hello world" {
		t.Errorf("text = %q, want %q (a doubled value means the assistant message was emitted as well as the deltas)", text, "Hello world")
	}
	if usage == nil {
		t.Fatal("EventDone carried no usage")
	}
	// CacheWriteTokens and Model are M4 additions (DESIGN.md §8). The cache
	// write is the number that distinguishes working caching from a prefix
	// re-written on every call, and the model is what actually served the
	// run rather than the alias that was asked for.
	want := provider.Usage{
		InputTokens: 11, OutputTokens: 14,
		CacheReadTokens: 7, CacheWriteTokens: 16440,
		Model: "claude-opus-5",
	}
	if *usage != want {
		t.Errorf("usage = %+v, want %+v", *usage, want)
	}
}

// TestStreamWithoutPartialMessages covers a stream that carries only the
// assembled assistant message, which is what would arrive if
// --include-partial-messages ever stopped being honoured.
func TestStreamWithoutPartialMessages(t *testing.T) {
	f := newFake(t, emit(lineInit, lineAssistant("only the whole message"), lineResult("only the whole message")))
	p := newProvider(t, f.bin)

	events, err := p.Stream(context.Background(), ask("ping"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, _, streamErr := drain(t, events)
	if streamErr != nil {
		t.Fatalf("stream failed: %v", streamErr)
	}
	if text != "only the whole message" {
		t.Errorf("text = %q, want the assistant message to stand in for absent deltas", text)
	}
}

func TestStreamMidStreamError(t *testing.T) {
	body := emit(
		lineInit,
		lineMessageStart,
		lineDelta("partial answer"),
		lineErrorResult("error_during_execution", "the model stopped mid-turn", "null"),
	) + "exit 1\n"
	f := newFake(t, body)
	p := newProvider(t, f.bin)

	events, err := p.Stream(context.Background(), ask("ping"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, _, streamErr := drain(t, events)

	// The deltas that did arrive are still delivered: a caller rendering a
	// stream should keep what it was given.
	if text != "partial answer" {
		t.Errorf("text = %q, want the deltas that arrived before the failure", text)
	}
	var perr *provider.Error
	if !errors.As(streamErr, &perr) {
		t.Fatalf("err = %T (%v), want *provider.Error", streamErr, streamErr)
	}
	if !strings.Contains(perr.Message, "the model stopped mid-turn") {
		t.Errorf("message = %q, want the CLI's own explanation", perr.Message)
	}
	if strings.Contains(perr.Error(), "identity context") {
		t.Error("the error quoted the prompt; argv must never reach an error string")
	}
}

// TestStreamCancellationKillsTheProcessGroup is the orphan test.
//
// The fake starts a grandchild and reports its pid. Cancelling the context
// must leave neither process running: exec.CommandContext on its own would
// signal the script and leave the grandchild alive, holding quota nobody can
// see.
func TestStreamCancellationKillsTheProcessGroup(t *testing.T) {
	f := newFakeFunc(t, func(f *fake) string {
		return emit(lineInit, lineMessageStart, lineDelta("starting")) +
			"sleep 30 &\n" +
			"printf '%s\\n' \"$!\" > " + quote(f.pid) + "\n" +
			"wait\n"
	})
	p := newProvider(t, f.bin)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := p.Stream(ctx, ask("ping"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Wait for the grandchild to exist before cancelling, or the test proves
	// nothing about killing it.
	child := waitForPID(t, f.pid)
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range events { //nolint:revive // draining to close is the point
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the stream did not close after the context was cancelled")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(child, 0); err != nil {
			return // gone, which is the whole assertion
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d survived cancellation: the process group was not killed", child)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForPID blocks until the fake has reported the pid of the process it
// started, so cancellation is tested against something that is really
// running.
func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(body))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the fake never reported a child pid")
	return 0
}

// TestStreamClosesTheChannelExactlyOnce leans on the runtime: a second close
// panics, and a channel left open blocks the range below forever.
func TestStreamClosesTheChannelExactlyOnce(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{"success", okStream("ok")},
		{"failure", emit(lineInit, lineErrorResult("error", "boom", "null")) + "exit 2\n"},
		{"no output at all", "exit 3\n"},
		{"garbage on stdout", "printf 'not json\\n'\nexit 0\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, tt.body)
			p := newProvider(t, f.bin)
			events, err := p.Stream(context.Background(), ask("ping"))
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			for range events { //nolint:revive // draining to close is the point
			}
			// A closed channel yields immediately with ok == false. A
			// channel closed twice would already have panicked above.
			select {
			case _, ok := <-events:
				if ok {
					t.Error("the channel produced a value after closing")
				}
			case <-time.After(time.Second):
				t.Error("the channel was not closed")
			}
		})
	}
}

func TestStreamAbandonedByItsConsumer(t *testing.T) {
	f := newFake(t, okStream("a much longer answer than anyone reads"))
	p := newProvider(t, f.bin)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := p.Stream(ctx, ask("ping"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Read one event and walk away, exactly as a TUI whose user pressed
	// escape would. Cancelling is the caller's side of the contract.
	<-events
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range events { //nolint:revive // draining to close is the point
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the stream did not close after its consumer left")
	}
}

func TestStructuredUsesTheJSONSchemaFlag(t *testing.T) {
	const payload = `{\"items\":[{\"title\":\"do the thing\"}]}`
	f := newFake(t, emit(lineInit, lineDelta(payload), lineResult(payload)))
	p := newProvider(t, f.bin)

	schema := json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},"required":["items"],"additionalProperties":false}`)

	var out struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	if err := p.Structured(context.Background(), ask("propose"), schema, &out); err != nil {
		t.Fatalf("Structured: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Title != "do the thing" {
		t.Fatalf("decoded %+v, want one item titled %q", out, "do the thing")
	}

	args := f.args(t)
	i := slices.Index(args, "--json-schema")
	if i < 0 {
		t.Fatalf("argv = %q, want --json-schema", args)
	}
	if i+1 >= len(args) || args[i+1] != string(schema) {
		t.Errorf("--json-schema carried %q, want the schema verbatim", args[i+1])
	}
}

func TestStructuredRejectsOutputThatIsNotTheSchema(t *testing.T) {
	f := newFake(t, emit(lineInit, lineResult("not json at all")))
	p := newProvider(t, f.bin)

	var out map[string]any
	err := p.Structured(context.Background(), ask("propose"), json.RawMessage(`{"type":"object"}`), &out)
	if err == nil {
		t.Fatal("Structured accepted output that was not JSON")
	}
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Kind != provider.KindUnknown {
		t.Fatalf("err = %v, want an unclassified provider error", err)
	}
}

func TestSessionLimitIsNotARateLimit(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			// The structured signal: the CLI's own rate_limit_event says the
			// window is spent.
			name: "from the rate_limit_event",
			body: emit(lineInit, lineRateLimitExhausted,
				lineErrorResult("error", "the run could not start", "null")) + "exit 1\n",
		},
		{
			// The textual fallback, for the shape this package could not
			// observe without spending the user's whole window.
			name: "from the message",
			body: emit(lineInit,
				lineErrorResult("error", "Claude AI usage limit reached", "null")) + "exit 1\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, tt.body)
			p := newProvider(t, f.bin)
			events, err := p.Stream(context.Background(), ask("ping"))
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			_, _, streamErr := drain(t, events)

			var perr *provider.Error
			if !errors.As(streamErr, &perr) {
				t.Fatalf("err = %T (%v), want *provider.Error", streamErr, streamErr)
			}
			if perr.Kind != provider.KindSessionLimit {
				t.Fatalf("kind = %v, want %v", perr.Kind, provider.KindSessionLimit)
			}
			// The distinction this kind exists for.
			if perr.Kind == provider.KindRateLimit {
				t.Error("a session limit was reported as an API rate limit")
			}
			if perr.Retryable() {
				t.Error("a session limit is not retryable now: the window resets in hours")
			}
			if perr.StatusCode != 0 {
				t.Errorf("StatusCode = %d, want 0: no HTTP status was involved", perr.StatusCode)
			}
		})
	}
}

func TestSessionLimitCarriesTheResetTime(t *testing.T) {
	f := newFake(t, emit(lineInit, lineRateLimitExhausted,
		lineErrorResult("error", "stopped", "null"))+"exit 1\n")
	p := newProvider(t, f.bin)

	events, _ := p.Stream(context.Background(), ask("ping"))
	_, _, streamErr := drain(t, events)

	var perr *provider.Error
	if !errors.As(streamErr, &perr) {
		t.Fatalf("err = %T, want *provider.Error", streamErr)
	}
	if want := time.Unix(1789996200, 0); !perr.ResetsAt.Equal(want) {
		t.Errorf("ResetsAt = %v, want %v", perr.ResetsAt, want)
	}
	if !strings.Contains(perr.Message, "five_hour") {
		t.Errorf("message = %q, want it to name the window that was hit", perr.Message)
	}
}

// TestSuccessfulRunIsNeverReportedAsLimited guards the conservative reading
// of rate_limit_event: its non-exhausted values were never observed, so a run
// that worked must not be turned into a session limit by one of them.
func TestSuccessfulRunIsNeverReportedAsLimited(t *testing.T) {
	body := emit(
		lineInit,
		lineDelta("fine"),
		`{"type":"rate_limit_event","rate_limit_info":{"status":"some_unobserved_value"},"session_id":"S"}`,
		lineResult("fine"),
	)
	f := newFake(t, body)
	p := newProvider(t, f.bin)

	events, _ := p.Stream(context.Background(), ask("ping"))
	text, _, streamErr := drain(t, events)
	if streamErr != nil {
		t.Fatalf("a successful run was reported as a failure: %v", streamErr)
	}
	if text != "fine" {
		t.Errorf("text = %q, want %q", text, "fine")
	}
}

func TestLoggedOutIsAnAuthFailure(t *testing.T) {
	f := newFake(t, "printf 'Error: not logged in. Run claude auth login\\n' >&2\nexit 1\n")
	p := newProvider(t, f.bin)

	events, _ := p.Stream(context.Background(), ask("ping"))
	_, _, streamErr := drain(t, events)

	var perr *provider.Error
	if !errors.As(streamErr, &perr) {
		t.Fatalf("err = %T (%v), want *provider.Error", streamErr, streamErr)
	}
	if perr.Kind != provider.KindAuth {
		t.Fatalf("kind = %v, want %v", perr.Kind, provider.KindAuth)
	}
	if !perr.UserFixable() {
		t.Error("a logged-out CLI is the user's to fix")
	}
}

func TestBinaryNotFound(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-here")
	_, err := New(provider.Config{Binary: missing, Model: "opus"})
	if err == nil {
		t.Fatal("New succeeded with a binary that does not exist")
	}
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %T (%v), want *provider.Error", err, err)
	}
	if perr.Kind != provider.KindUnavailable {
		t.Errorf("kind = %v, want %v", perr.Kind, provider.KindUnavailable)
	}
	if !strings.Contains(perr.Error(), "claude") {
		t.Errorf("err = %q, want it to name the binary that is missing", perr)
	}
}

// TestArgvNeverPassesBare is the trap this package was written around.
//
// --bare looks like the right flag for using Claude Code as a raw inference
// engine, and its help says Anthropic auth then becomes "strictly
// ANTHROPIC_API_KEY or apiKeyHelper (OAuth and keychain are never read)". It
// turns off the exact subscription auth this provider exists to use, so a
// future edit that adds it has to fail here.
func TestArgvNeverPassesBare(t *testing.T) {
	f := newFake(t, okStream("hi"))
	p := newProvider(t, f.bin)

	events, err := p.Stream(context.Background(), ask("ping"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, events)

	for _, arg := range f.args(t) {
		if arg == "--bare" {
			t.Fatal("--bare is in the command line; it disables the OAuth and keychain auth this provider exists to use")
		}
	}
}

func TestArgvCarriesTheFlagsTheCLIRequires(t *testing.T) {
	f := newFake(t, okStream("hi"))
	p := newProvider(t, f.bin)

	events, err := p.Stream(context.Background(), ask("ping"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drain(t, events)

	args := f.args(t)
	for _, want := range []string{
		"-p",
		"--output-format",
		"stream-json",
		"--include-partial-messages",
		// Verified against claude 2.1.278: stream-json without it fails with
		// "When using --print, --output-format=stream-json requires
		// --verbose" before any inference happens.
		"--verbose",
		"--restricted",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--tools",
		"--no-session-persistence",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("argv = %q, want it to contain %q", args, want)
		}
	}

	// --tools takes an empty value, which is how the CLI is told to expose
	// no tools at all. An assertion on the flag alone would pass even if the
	// value were dropped on the way to exec.
	if i := slices.Index(args, "--tools"); i < 0 || i+1 >= len(args) || args[i+1] != "" {
		t.Errorf("argv = %q, want --tools followed by an empty value", args)
	}

	// The route's model and effort reach the CLI.
	if i := slices.Index(args, "--model"); i < 0 || args[i+1] != "opus" {
		t.Errorf("argv = %q, want --model opus from the route", args)
	}
	if i := slices.Index(args, "--effort"); i < 0 || args[i+1] != "high" {
		t.Errorf("argv = %q, want --effort high from the route", args)
	}
	if i := slices.Index(args, "--system-prompt"); i < 0 || args[i+1] != "identity context" {
		t.Errorf("argv = %q, want the system prompt passed through", args)
	}
	// --json-schema belongs to Structured alone.
	if slices.Contains(args, "--json-schema") {
		t.Error("Stream passed --json-schema; only Structured constrains the output")
	}
	// The prompt is the last argument.
	if got := args[len(args)-1]; got != "ping" {
		t.Errorf("last argument = %q, want the prompt", got)
	}
}

func TestArgvRefusesAPromptItCannotPass(t *testing.T) {
	f := newFake(t, okStream("hi"))
	p := newProvider(t, f.bin)

	_, err := p.Stream(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("x", maxPromptBytes+1)}},
	})
	if err == nil {
		t.Fatal("Stream accepted a prompt too large for the command line")
	}
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Kind != provider.KindRequest {
		t.Fatalf("err = %v, want a request-kind provider error", err)
	}
}

func TestRenderPrompt(t *testing.T) {
	single := renderPrompt([]provider.Message{{Role: provider.RoleUser, Content: "just this"}})
	if single != "just this" {
		t.Errorf("a lone user message was rewritten to %q", single)
	}
	multi := renderPrompt([]provider.Message{
		{Role: provider.RoleUser, Content: "first"},
		{Role: provider.RoleAssistant, Content: "second"},
		{Role: provider.RoleUser, Content: "third"},
	})
	for _, want := range []string{"User: first", "Assistant: second", "User: third"} {
		if !strings.Contains(multi, want) {
			t.Errorf("rendered transcript = %q, want it to contain %q", multi, want)
		}
	}
}

// TestConcurrentStreams covers the promise M4's bounded worker pool depends
// on: one Provider, many simultaneous calls.
func TestConcurrentStreams(t *testing.T) {
	f := newFake(t, okStream("parallel"))
	p := newProvider(t, f.bin)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			events, err := p.Stream(context.Background(), ask("ping"))
			if err != nil {
				errs <- err
				return
			}
			var text strings.Builder
			for ev := range events {
				if ev.Type == provider.EventTextDelta {
					text.WriteString(ev.Text)
				}
				if ev.Type == provider.EventError {
					errs <- ev.Err
				}
			}
			if text.String() != "parallel" {
				errs <- errors.New("concurrent stream produced " + text.String())
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent stream: %v", err)
	}
}

// TestNoGoroutineLeak checks that a drained stream leaves nothing running.
func TestNoGoroutineLeak(t *testing.T) {
	f := newFake(t, okStream("hi"))
	p := newProvider(t, f.bin)

	before := runtime.NumGoroutine()
	for range 5 {
		events, err := p.Stream(context.Background(), ask("ping"))
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		drain(t, events)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		if runtime.NumGoroutine() <= before+2 { // os/exec keeps a reaper or two around
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines grew from %d to %d after five drained streams",
				before, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
