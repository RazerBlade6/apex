// Package claudecli runs Apex's advisor loop through a Claude Pro or Max
// subscription by shelling out to the `claude` CLI (DESIGN.md §8,
// "Subscription-backed inference").
//
// It is the same pattern as the builder loop one layer up: Apex owns the
// judgment, an existing tool owns the transport. Nothing about the Provider
// interface changes — this is a third implementation of it, not a fourth
// capability.
//
// # Never pass --bare
//
// --bare reads as the obvious flag here. It is the CLI's "use me as a raw
// inference engine" mode: skip hooks, plugins, CLAUDE.md discovery. Its own
// help then says Anthropic auth becomes "strictly ANTHROPIC_API_KEY or
// apiKeyHelper via --settings (OAuth and keychain are never read)".
//
// That is the exact mechanism this package exists to use. With --bare, a user
// with a subscription and no API key gets an auth failure, and a user with
// both silently pays for API tokens while believing they are spending
// subscription quota. --restricted is the correct isolation flag: it strips
// the code-running tools and ignores user, project and local settings files
// while leaving auth working normally.
//
// TestArgvNeverPassesBare exists to keep that true.
//
// # What the CLI cannot do
//
//   - Request.MaxTokens has no flag. The CLI decides the output limit from
//     the model. It is accepted and ignored rather than approximated.
//   - Request.CacheSystem has no flag either. The CLI manages its own cache
//     breakpoints, and the capture documented in internal/claudecmd shows it
//     doing so: a trivial prompt reported 16,440 cache-creation tokens
//     without being asked.
//   - Multi-turn history is flattened into one prompt; see renderPrompt.
package claudecli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/RazerBlade6/apex/internal/provider"
)

// Name is the value that appears as `provider = "claude-cli"` in config.toml.
const Name = "claude-cli"

// maxPromptBytes bounds what is handed to the CLI on the command line.
//
// The system prompt carries identity context and every project digest, and
// argv is not unbounded: macOS caps it around a megabyte, and exceeding it
// fails with E2BIG from deep inside exec rather than with anything a user
// could act on. This is the message that says what actually happened.
const maxPromptBytes = 400 << 10

// killGrace is how long a cancelled process has to die before os/exec stops
// waiting on its pipes. The kill is a SIGKILL to the process group, so this
// is a backstop against an unkillable child, not a graceful shutdown window.
const killGrace = 5 * time.Second

// Provider runs inference through the claude CLI.
//
// It is safe for concurrent use: every field is set once in New and read-only
// afterwards, and each call owns its own process. M4's bounded worker pool
// for parallel digest refresh can therefore share one Provider across
// workers — which it must, because process startup is the cost that makes a
// pool necessary in the first place (DESIGN.md §8).
type Provider struct {
	bin string
	cfg provider.Config
}

// New builds the provider, resolving the claude binary once.
//
// Resolution happens here rather than per call so that a missing CLI is
// reported when the provider is constructed — which is what `apex doctor`
// and `registry.New` both want — instead of at the first token of the first
// digest.
func New(cfg provider.Config) (*Provider, error) {
	bin, err := resolveBinary(cfg.Binary)
	if err != nil {
		return nil, err
	}
	return &Provider{bin: bin, cfg: cfg}, nil
}

// Name identifies the provider.
func (p *Provider) Name() string { return Name }

// Binary reports the resolved path of the CLI, for `apex doctor`.
func (p *Provider) Binary() string { return p.bin }

// argv builds the command line. schema is nil for Stream.
//
// Everything about this function is one decision repeated: give the CLI the
// least authority that still answers the question. Apex wants text back, not
// a coding agent.
func (p *Provider) argv(req provider.Request, schema json.RawMessage) ([]string, string, error) {
	req = p.cfg.Apply(req).WithDefaults()
	if err := req.Validate(); err != nil {
		return nil, "", err
	}
	prompt := renderPrompt(req.Messages)
	system := strings.TrimSpace(req.System)
	if n := len(prompt) + len(system) + len(schema); n > maxPromptBytes {
		return nil, "", &provider.Error{
			Provider: Name, Op: "argv", Kind: provider.KindRequest,
			Message: fmt.Sprintf("the prompt is %d bytes, over the %d-byte limit this provider passes on the command line", n, maxPromptBytes),
		}
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--include-partial-messages",
		// The CLI refuses stream-json without it: "When using --print,
		// --output-format=stream-json requires --verbose". It does not make
		// the output chattier in this mode; it is the price of the format.
		"--verbose",

		// Isolation. --restricted removes the tools that run commands or
		// code and ignores user, project and local settings files;
		// --strict-mcp-config skips every MCP server the user has
		// configured; --disable-slash-commands skips their skills. Together
		// these stop an advisor call from inheriting whatever a developer
		// machine happens to have installed.
		"--restricted",
		"--strict-mcp-config",
		"--disable-slash-commands",

		// No tools at all. DESIGN.md §8 is explicit that v1 has no tool use:
		// the advisor loop reads context Apex assembled and writes text.
		// --restricted alone still leaves Read, Write, Edit and friends
		// available, and an advisor call has no business editing files in
		// whatever directory apex was launched from.
		"--tools", "",

		// Advisor prompts carry PROFILE.md, SKILLS.md and every digest.
		// There is no reason to leave copies of them in ~/.claude/projects:
		// Apex never resumes these sessions, and the transcript would be a
		// second, unmanaged copy of the user's context.
		"--no-session-persistence",
	}

	// NEVER add --bare here. See the package comment: it disables the OAuth
	// and keychain auth that this entire provider exists to use.

	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if system != "" {
		args = append(args, "--system-prompt", system)
	}
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	}

	// The prompt goes last, as a positional argument.
	args = append(args, prompt)
	return args, prompt, nil
}

// renderPrompt flattens a conversation into the single prompt string the CLI
// takes.
//
// A lone user message — which is every advisor, review and ideas call — is
// passed through untouched. Multi-turn history is rendered as a labelled
// transcript, which is a real approximation: the CLI's own multi-turn input
// (--input-format stream-json) exists and would be more faithful, but it
// turns a one-shot subprocess into a protocol, and only M6's chat view needs
// it. This is the smaller thing that works, marked as such.
func renderPrompt(messages []provider.Message) string {
	if len(messages) == 1 && messages[0].Role == provider.RoleUser {
		return messages[0].Content
	}
	var b strings.Builder
	for _, m := range messages {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		switch m.Role {
		case provider.RoleAssistant:
			b.WriteString("Assistant: ")
		default:
			b.WriteString("User: ")
		}
		b.WriteString(m.Content)
	}
	return b.String()
}

// Stream sends a request and returns a channel closed exactly once, when the
// response completes or fails.
func (p *Provider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	args, _, err := p.argv(req, nil)
	if err != nil {
		return nil, err
	}
	ch := make(chan provider.Event, 8)
	go p.run(ctx, args, ch)
	return ch, nil
}

// run owns the channel. The deferred close is the only one in the package, so
// the channel is closed exactly once whichever way this returns.
func (p *Provider) run(ctx context.Context, args []string, ch chan<- provider.Event) {
	defer close(ch)

	out, err := p.invoke(ctx, "stream", args, func(text string) bool {
		return provider.Emit(ctx, ch, provider.Event{Type: provider.EventTextDelta, Text: text})
	})
	if err != nil {
		provider.EmitFinal(ctx, ch, provider.Event{Type: provider.EventError, Err: err})
		return
	}
	// A stream that carried no partial messages still produced text, in the
	// assembled assistant message. Emitting it here rather than dropping it
	// keeps Stream working if --include-partial-messages ever stops being
	// honoured, without double-emitting when it is.
	if !out.sawDelta && out.text != "" {
		if !provider.Emit(ctx, ch, provider.Event{Type: provider.EventTextDelta, Text: out.text}) {
			return
		}
	}
	usage := out.usage
	provider.EmitFinal(ctx, ch, provider.Event{Type: provider.EventDone, Usage: &usage})
}

// Structured constrains the response with --json-schema and unmarshals it.
//
// DESIGN.md §8 requires schemas written to the intersection of both vendors'
// accepted subsets — object root, every property required,
// additionalProperties false. Nothing is checked here: a schema the CLI
// rejects comes back as a classified request failure, which is the same
// answer the key-based adapters give.
func (p *Provider) Structured(ctx context.Context, req provider.Request, schema json.RawMessage, out any) (provider.Usage, error) {
	if out == nil {
		return provider.Usage{}, fmt.Errorf("claudecli: Structured needs a destination to unmarshal into")
	}
	var probe map[string]any
	if err := json.Unmarshal(schema, &probe); err != nil {
		return provider.Usage{}, fmt.Errorf("claudecli: output schema is not a JSON object: %w", err)
	}

	args, _, err := p.argv(req, schema)
	if err != nil {
		return provider.Usage{}, err
	}
	result, perr := p.invoke(ctx, "structured", args, nil)
	if perr != nil {
		// invoke hands back what the run reported before it failed, so a
		// call that was billed and then errored still accounts for itself.
		return result.usage, perr
	}
	usage := result.usage

	// The result event carries the final answer; the accumulated deltas are
	// the same text and stand in only if the run ended without one.
	body := strings.TrimSpace(result.result)
	if body == "" {
		body = strings.TrimSpace(result.text)
	}
	if body == "" {
		return usage, &provider.Error{
			Provider: Name, Op: "structured", Kind: provider.KindUnknown,
			Message: "the run produced no output to decode",
		}
	}
	if err := json.Unmarshal([]byte(body), out); err == nil {
		return usage, nil
	}
	// One tolerance, deliberately narrow: a model that wraps its JSON in a
	// markdown fence despite the schema. Anything else is a real failure and
	// is reported as one.
	if unfenced, ok := unfence(body); ok {
		if err := json.Unmarshal([]byte(unfenced), out); err == nil {
			return usage, nil
		}
	}
	return usage, &provider.Error{
		Provider: Name, Op: "structured", Kind: provider.KindUnknown,
		Message: provider.Truncate("the response was not the JSON the schema asked for: " + body),
	}
}

// unfence strips a ```json ... ``` wrapper, reporting whether there was one.
func unfence(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s, false
	}
	_, rest, found := strings.Cut(s, "\n")
	if !found {
		return s, false
	}
	if i := strings.LastIndex(rest, "```"); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest), true
}

// outcome is what one completed run produced.
type outcome struct {
	text     string // text assembled from deltas, or the assistant message
	result   string // the result event's payload
	usage    provider.Usage
	model    string // the model the CLI actually used
	sawDelta bool
	// apiKeySource is what the init event reported. "none" means the run was
	// served by the subscription; anything else means it was not.
	apiKeySource string
}

// invoke runs one claude process and parses its stream.
//
// emit, when non-nil, receives each text delta and reports whether the
// consumer is still there; false abandons the run. Structured passes nil,
// because it wants the whole answer and nothing incremental.
func (p *Provider) invoke(ctx context.Context, op string, args []string, emit func(string) bool) (*outcome, *provider.Error) {
	cmd := exec.CommandContext(ctx, p.bin, args...)
	// Nothing is piped in. Left unset, a child that decided to read stdin
	// would inherit Apex's, and an advisor call would hang on a terminal.
	cmd.Stdin = nil

	var stderr tailBuffer
	cmd.Stderr = &stderr

	configureProcessGroup(cmd)
	// exec.CommandContext's default cancel signals the process it started.
	// This signals the whole group, so a claude that spawned children leaves
	// none behind.
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = killGrace

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &provider.Error{
			Provider: Name, Op: op, Kind: provider.KindUnknown,
			Message: provider.Truncate(err.Error()), Err: err,
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, startError(op, err)
	}

	out := &outcome{}
	var (
		body      strings.Builder
		limit     *rateLimitInfo
		resultLn  *line
		abandoned bool
	)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	for scanner.Scan() {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var ln line
		if err := json.Unmarshal([]byte(raw), &ln); err != nil {
			// A line Apex cannot parse is not a reason to fail a run that is
			// otherwise working: the CLI is free to add event types. The
			// result event is what decides the outcome.
			continue
		}

		switch ln.Type {
		case "system":
			if ln.Subtype == "init" {
				out.apiKeySource = ln.APIKeySource
				if ln.Model != "" {
					out.model = ln.Model
				}
			}
		case "stream_event":
			if ln.Event == nil {
				continue
			}
			switch ln.Event.Type {
			case "message_start":
				if ln.Event.Message != nil {
					if ln.Event.Message.Model != "" {
						out.model = ln.Event.Message.Model
					}
					if u := ln.Event.Message.Usage; u != nil {
						out.usage = usageOf(u)
					}
				}
			case "content_block_delta":
				d := ln.Event.Delta
				if d == nil || d.Type != "text_delta" || d.Text == "" {
					continue
				}
				out.sawDelta = true
				body.WriteString(d.Text)
				if emit != nil && !emit(d.Text) {
					abandoned = true
				}
			case "message_delta":
				if u := ln.Event.Usage; u != nil {
					out.usage = usageOf(u)
				}
			}
		case "assistant":
			if ln.Message == nil {
				continue
			}
			if ln.Message.Model != "" {
				out.model = ln.Message.Model
			}
			if u := ln.Message.Usage; u != nil {
				out.usage = usageOf(u)
			}
			if !out.sawDelta {
				for _, block := range ln.Message.Content {
					if block.Type == "text" {
						body.WriteString(block.Text)
					}
				}
			}
		case "rate_limit_event":
			if ln.RateLimitInfo != nil {
				limit = ln.RateLimitInfo
			}
		case "result":
			copied := ln
			resultLn = &copied
			if u := ln.Usage; u != nil {
				out.usage = usageOf(u)
			}
			if out.model == "" {
				out.model = copied.ServingModel()
			}
			out.result = ln.Text()
		}

		if abandoned {
			break
		}
	}
	scanErr := scanner.Err()

	if abandoned {
		// The consumer walked away. Nothing will read the rest of stdout, so
		// the child would block on a full pipe forever; kill it before
		// waiting on it.
		_ = killProcessGroup(cmd)
	}
	waitErr := cmd.Wait()

	out.text = body.String()
	// Every usage block the CLI emits is anonymous; the serving model is
	// reported separately, by the init, message_start and assistant events and
	// by the result event's modelUsage. Stamping it here means Usage.Model
	// carries what actually served the run rather than the alias that was
	// asked for — "opus" resolves to a dated id, and DESIGN.md §7 records the
	// model on every digest and action item.
	out.usage.Model = out.model
	// The outcome is returned even on failure: it carries whatever usage the
	// run reported before it went wrong, and a call that was billed should
	// say so (DESIGN.md §18).
	if perr := classify(ctx, op, out, resultLn, limit, waitErr, scanErr, &stderr); perr != nil {
		return out, perr
	}
	return out, nil
}
