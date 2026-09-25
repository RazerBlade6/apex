package claudecmd

import (
	"encoding/json"
	"strings"
	"time"
)

// This file holds the wire shape of `claude --output-format stream-json`.
//
// None of it was written from memory. It was captured on 2026-09-21 from
// claude 2.1.278 by running exactly one real invocation:
//
//	claude -p --output-format stream-json --include-partial-messages \
//	       --verbose --restricted --strict-mcp-config \
//	       --disable-slash-commands --model sonnet 'say hi'
//
// which produced, in order:
//
//	{"type":"system","subtype":"init","model":"claude-sonnet-5",
//	 "apiKeySource":"none","session_id":"...","tools":[...]}
//	{"type":"system","subtype":"status","status":"requesting"}
//	{"type":"stream_event","event":{"type":"message_start","message":{...,"usage":{...}}}}
//	{"type":"stream_event","event":{"type":"content_block_start","index":0,
//	 "content_block":{"type":"text","text":""}}}
//	{"type":"stream_event","event":{"type":"content_block_delta","index":0,
//	 "delta":{"type":"text_delta","text":"Hi!"}}}
//	   ... one per delta ...
//	{"type":"assistant","message":{"content":[{"type":"text","text":"Hi! ..."}],"usage":{...}}}
//	{"type":"stream_event","event":{"type":"content_block_stop","index":0}}
//	{"type":"stream_event","event":{"type":"message_delta",
//	 "delta":{"stop_reason":"end_turn"},"usage":{...}}}
//	{"type":"stream_event","event":{"type":"message_stop"}}
//	{"type":"rate_limit_event","rate_limit_info":{"status":"allowed",
//	 "resetsAt":1789996200,"rateLimitType":"five_hour",
//	 "unifiedWindows":{"five_hour":{"utilization":0.26,"resetsAt":...},
//	                   "seven_day":{"utilization":0.13,"resetsAt":...}}}}
//	{"type":"result","subtype":"success","is_error":false,
//	 "result":"Hi! What can I help you with today?","usage":{...},
//	 "modelUsage":{"claude-sonnet-5":{...}},"api_error_status":null,
//	 "terminal_reason":"completed","total_cost_usd":0.065904}
//
// Three things follow from that capture and are load-bearing below.
//
// The stream carries the text twice: once as content_block_delta events and
// again as the assembled `assistant` message. Emitting both would double
// every response, so deltas win and the assistant message is only a fallback
// for a stream that carried no partials.
//
// `apiKeySource: "none"` is how a subscription-backed run identifies itself.
// A run whose init event names an API key source is being billed to an API
// account, not to the subscription, which is worth saying out loud rather
// than letting a user discover it later.
//
// The capture was made with every tool disabled, so it shows no tool_use or
// tool_result blocks. M5's executor runs the same CLI with tools ENABLED, so
// ContentBlock below models them from the Anthropic message format the CLI
// passes through verbatim. Those fields are the one part of this file that is
// not from the capture, and the executor treats a block it cannot make sense
// of as nothing rather than guessing.

// Line is one newline-delimited object on claude's stdout. Fields that Apex
// does not act on are left undecoded rather than modelled, so an added field
// upstream is silently ignored instead of breaking the parse.
type Line struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	Model        string   `json:"model"`
	APIKeySource string   `json:"apiKeySource"`
	SessionID    string   `json:"session_id"`
	Tools        []string `json:"tools"`
	CWD          string   `json:"cwd"`

	// stream_event
	Event *APIEvent `json:"event"`

	// assistant / user
	Message *APIMessage `json:"message"`

	// rate_limit_event
	RateLimitInfo *RateLimitInfo `json:"rate_limit_info"`

	// result
	IsError bool `json:"is_error"`
	// Result is raw because the CLI documents no type for it. It is a string
	// on the run captured above; decoding it as one unconditionally would
	// turn any other shape into a parse failure at exactly the moment — a
	// failed run — when the message matters most.
	Result json.RawMessage `json:"result"`
	Usage  *Usage          `json:"usage"`
	// ModelUsage is the per-model cost breakdown, keyed by the model id the
	// CLI billed. Only its keys are read; the values are left undecoded.
	ModelUsage     map[string]json.RawMessage `json:"modelUsage"`
	APIErrorStatus *int                       `json:"api_error_status"`
	TerminalReason string                     `json:"terminal_reason"`
	StopReason     string                     `json:"stop_reason"`
	NumTurns       int                        `json:"num_turns"`
	DurationMS     int64                      `json:"duration_ms"`
	TotalCostUSD   float64                    `json:"total_cost_usd"`
}

// APIEvent is the Anthropic streaming event the CLI passes through verbatim
// inside a stream_event line.
type APIEvent struct {
	Type    string      `json:"type"`
	Delta   *APIDelta   `json:"delta"`
	Message *APIMessage `json:"message"`
	Usage   *Usage      `json:"usage"`
}

// APIDelta is one incremental piece of a content block.
type APIDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// APIMessage is an assembled assistant or user message.
type APIMessage struct {
	Role    string         `json:"role"`
	Model   string         `json:"model"`
	Content []ContentBlock `json:"content"`
	Usage   *Usage         `json:"usage"`
}

// ContentBlock is one block of a message.
//
// text blocks are what the provider reads. tool_use and tool_result blocks
// are what the executor reads: they are how a run says it called a tool, and
// what the tool answered. A permission denial arrives as a tool_result with
// IsError set, which is why that field is modelled rather than dropped —
// `--permission-prompts none` means a denial is the expected outcome of
// anything Apex did not authorise, and a run that quietly did nothing is
// worse than one that says what it was refused.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result. Content is raw because the CLI sends a string for some
	// tools and a block array for others.
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Usage is the token accounting the CLI reports.
//
// CacheCreationInputTokens matters: DESIGN.md §8 argues cache *writes* are the
// number that catches an unstable prompt prefix, and this transport is the one
// that shows them most clearly — a trivial prompt reported 16,440 of them.
//
// The CLI also reports total_cost_usd, which lives on Line rather than here
// because it is a property of the whole run and not of one message.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// RateLimitInfo is the subscription's usage window.
type RateLimitInfo struct {
	Status        string `json:"status"`
	ResetsAt      int64  `json:"resetsAt"` // unix seconds
	RateLimitType string `json:"rateLimitType"`

	UnifiedWindows map[string]struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    int64   `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

// Permissive reports whether this rate_limit_event describes a window that is
// still usable.
//
// Only "allowed" has ever been observed, so everything else is treated
// conservatively — but both callers read a non-permissive status only on a run
// that already failed. A successful stream is never reported as limited, which
// keeps an unobserved status value such as a soft warning from turning a
// working call into a false session limit.
func (r *RateLimitInfo) Permissive() bool {
	if r == nil {
		return true
	}
	s := strings.ToLower(strings.TrimSpace(r.Status))
	switch {
	case s == "":
		return true
	case strings.Contains(s, "allow"), strings.Contains(s, "warn"), s == "ok":
		return true
	default:
		return false
	}
}

// Resets returns the window's reset time, or the zero time when the CLI did
// not report one. Nothing here estimates a reset it was not told.
func (r *RateLimitInfo) Resets() time.Time {
	if r == nil {
		return time.Time{}
	}
	best := r.ResetsAt
	if best == 0 {
		for _, w := range r.UnifiedWindows {
			if w.ResetsAt > best {
				best = w.ResetsAt
			}
		}
	}
	if best <= 0 {
		return time.Time{}
	}
	return time.Unix(best, 0)
}

// Window names the limit that was hit, for a message the user can act on.
func (r *RateLimitInfo) Window() string {
	if r == nil {
		return ""
	}
	return r.RateLimitType
}

// Text decodes a result event's payload. A JSON string decodes to its value;
// anything else is handed back as the raw JSON, so a failed run's explanation
// survives even in a shape this code did not anticipate.
func (l *Line) Text() string {
	if len(l.Result) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(l.Result, &s); err == nil {
		return s
	}
	return string(l.Result)
}

// ServingModel returns the model a result event's modelUsage breakdown names.
//
// The init and message events normally say it first and this never has to run.
// It exists for the case they do not: modelUsage is keyed by the model id the
// CLI actually billed, so it answers the question DESIGN.md §8 asks Usage.Model
// to answer — which model served this — even when nothing earlier in the stream
// did. More than one key means the run was served by more than one model, and
// naming one of them would be a guess, so it reports nothing.
func (l *Line) ServingModel() string {
	if len(l.ModelUsage) != 1 {
		return ""
	}
	for name := range l.ModelUsage {
		return name
	}
	return ""
}

// MaxLineBytes bounds one line of stdout.
//
// The assembled `assistant` message arrives as a single line and grows with
// the response, so the default 64KiB bufio.Scanner limit is not enough. An
// executor run makes this sharper than the provider does: a tool_result
// carrying a whole file is one line too.
const MaxLineBytes = 32 << 20

// TailBufferMax bounds what is kept from a child's stderr. A CLI that fails by
// printing a stack trace must not be able to grow Apex's memory, and only the
// end of the output ever explains the failure.
const TailBufferMax = 8 << 10

// TailBuffer keeps the last TailBufferMax bytes written to it. os/exec writes
// from a single goroutine, so it needs no lock of its own.
type TailBuffer struct {
	buf []byte
}

func (t *TailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > TailBufferMax {
		p = p[len(p)-TailBufferMax:]
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > TailBufferMax {
		t.buf = t.buf[len(t.buf)-TailBufferMax:]
	}
	return n, nil
}

func (t *TailBuffer) String() string { return string(t.buf) }
