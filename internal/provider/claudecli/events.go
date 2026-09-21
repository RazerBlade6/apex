package claudecli

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/RazerBlade6/apex/internal/provider"
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
// Two things follow from that capture and are load-bearing below.
//
// The stream carries the text twice: once as content_block_delta events and
// again as the assembled `assistant` message. Emitting both would double
// every response, so deltas win and the assistant message is only a fallback
// for a stream that carried no partials.
//
// `apiKeySource: "none"` is how a subscription-backed run identifies itself.
// A claude-cli route whose init event names an API key source is being billed
// to an API account, not to the subscription, which is worth saying out loud
// rather than letting a user discover it later.

// line is one newline-delimited object on claude's stdout. Fields that Apex
// does not act on are left undecoded rather than modelled, so an added field
// upstream is silently ignored instead of breaking the parse.
type line struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	Model        string `json:"model"`
	APIKeySource string `json:"apiKeySource"`
	SessionID    string `json:"session_id"`

	// stream_event
	Event *apiEvent `json:"event"`

	// assistant / user
	Message *apiMessage `json:"message"`

	// rate_limit_event
	RateLimitInfo *rateLimitInfo `json:"rate_limit_info"`

	// result
	IsError bool `json:"is_error"`
	// Result is raw because the CLI documents no type for it. It is a string
	// on the run captured above; decoding it as one unconditionally would
	// turn any other shape into a parse failure at exactly the moment — a
	// failed run — when the message matters most.
	Result         json.RawMessage `json:"result"`
	Usage          *cliUsage       `json:"usage"`
	APIErrorStatus *int            `json:"api_error_status"`
	TerminalReason string          `json:"terminal_reason"`
	StopReason     string          `json:"stop_reason"`
}

// apiEvent is the Anthropic streaming event the CLI passes through verbatim
// inside a stream_event line.
type apiEvent struct {
	Type    string      `json:"type"`
	Delta   *apiDelta   `json:"delta"`
	Message *apiMessage `json:"message"`
	Usage   *cliUsage   `json:"usage"`
}

type apiDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type apiMessage struct {
	Model   string         `json:"model"`
	Content []contentBlock `json:"content"`
	Usage   *cliUsage      `json:"usage"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// cliUsage is the token accounting the CLI reports.
//
// CacheCreationInputTokens has no home in provider.Usage as M3 defined it —
// the struct has InputTokens, OutputTokens and CacheReadTokens and nothing
// else — so it is decoded and dropped. That is a real loss and is recorded in
// the milestone notes rather than papered over: DESIGN.md §8 argues cache
// *writes* are the number that catches an unstable prefix, and this provider
// can see them.
//
// The CLI also reports total_cost_usd and a per-model breakdown. Neither has
// a field on provider.Usage, and neither is invented into one.
type cliUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// rateLimitInfo is the subscription's usage window.
type rateLimitInfo struct {
	Status        string `json:"status"`
	ResetsAt      int64  `json:"resetsAt"` // unix seconds
	RateLimitType string `json:"rateLimitType"`

	UnifiedWindows map[string]struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    int64   `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

// permissive reports whether this rate_limit_event describes a window that is
// still usable.
//
// Only "allowed" has ever been observed, so everything else is treated
// conservatively — but see classify: a non-permissive status is only ever
// read on a run that already failed. A successful stream is never reported as
// limited, which keeps an unobserved status value such as a soft warning from
// turning a working call into a false session limit.
func (r *rateLimitInfo) permissive() bool {
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

// resets returns the window's reset time, or the zero time when the CLI did
// not report one. Nothing here estimates a reset it was not told.
func (r *rateLimitInfo) resets() time.Time {
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

// window names the limit that was hit, for a message the user can act on.
func (r *rateLimitInfo) window() string {
	if r == nil {
		return ""
	}
	return r.RateLimitType
}

// text decodes a result event's payload. A JSON string decodes to its value;
// anything else is handed back as the raw JSON, so a failed run's explanation
// survives even in a shape this code did not anticipate.
func (l *line) text() string {
	if len(l.Result) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(l.Result, &s); err == nil {
		return s
	}
	return string(l.Result)
}

// usage converts the CLI's accounting to Apex's.
func (u *cliUsage) usage() provider.Usage {
	if u == nil {
		return provider.Usage{}
	}
	return provider.Usage{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CacheReadInputTokens,
	}
}

// tailBufferMax bounds what is kept from a child's stderr. A CLI that fails
// by printing a stack trace must not be able to grow Apex's memory, and only
// the end of the output ever explains the failure.
const tailBufferMax = 8 << 10

// tailBuffer keeps the last tailBufferMax bytes written to it. os/exec writes
// from a single goroutine, so it needs no lock of its own.
type tailBuffer struct {
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > tailBufferMax {
		p = p[len(p)-tailBufferMax:]
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailBufferMax {
		t.buf = t.buf[len(t.buf)-tailBufferMax:]
	}
	return n, nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
