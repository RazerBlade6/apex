package claudecli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Everything in this file stands in for the claude binary.
//
// Exactly one real invocation was ever made — the capture documented at the
// top of events.go — and it is the only thing in this package that ever cost
// the user's subscription quota. Every test replays that shape from a fake,
// so the suite is offline, deterministic, and free.

// fake is a shell script impersonating claude. It records the argv it was
// given so a test can assert on the command line Apex builds, which is where
// the --bare trap would show up.
type fake struct {
	bin  string
	argv string
	pid  string
}

// newFake writes an executable script whose body is sh. The script records
// argv first, so even a fake that then fails still reports what it was asked
// to do.
func newFake(t *testing.T, body string) *fake {
	t.Helper()
	return newFakeFunc(t, func(*fake) string { return body })
}

// newFakeFunc is newFake for a body that needs the fake's own paths — the
// cancellation test writes its grandchild's pid into one.
func newFakeFunc(t *testing.T, build func(*fake) string) *fake {
	t.Helper()
	dir := t.TempDir()
	f := &fake{
		bin:  filepath.Join(dir, "claude"),
		argv: filepath.Join(dir, "argv"),
		pid:  filepath.Join(dir, "child-pid"),
	}
	body := build(f)
	script := strings.Join([]string{
		"#!/bin/sh",
		": > " + quote(f.argv),
		`for a in "$@"; do printf '%s\n' "$a" >> ` + quote(f.argv) + "; done",
		body,
		"",
	}, "\n")
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return f
}

// args returns the recorded command line.
func (f *fake) args(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(f.argv)
	if err != nil {
		t.Fatalf("the fake claude recorded no argv: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		out = append(out, line)
	}
	// The trailing newline yields one empty element; an empty *argument* is
	// real (--tools ""), so only the last one is dropped.
	if n := len(out); n > 0 && out[n-1] == "" {
		out = out[:n-1]
	}
	return out
}

// quote wraps a path for /bin/sh. Test paths contain no single quotes.
func quote(s string) string { return "'" + s + "'" }

// emit renders shell that prints each line to stdout, one JSON object per
// line, exactly as the CLI does.
func emit(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		if strings.Contains(l, "'") {
			panic("fixture contains a single quote and cannot be shell-quoted: " + l)
		}
		fmt.Fprintf(&b, "printf '%%s\\n' %s\n", quote(l))
	}
	return b.String()
}

// The fixtures below are the captured shape with the volatile fields
// shortened. Field names, nesting and types are unchanged from the real run.

const (
	lineInit = `{"type":"system","subtype":"init","cwd":"/tmp","session_id":"S",` +
		`"model":"claude-opus-5","apiKeySource":"none","claude_code_version":"2.1.278",` +
		`"tools":[],"mcp_servers":[],"permissionMode":"default"}`

	lineStatus = `{"type":"system","subtype":"status","status":"requesting","session_id":"S","uuid":"u1"}`

	lineMessageStart = `{"type":"stream_event","event":{"type":"message_start","message":{` +
		`"model":"claude-opus-5","id":"msg_1","type":"message","role":"assistant","content":[],` +
		`"stop_reason":null,"usage":{"input_tokens":11,"cache_creation_input_tokens":16440,` +
		`"cache_read_input_tokens":7,"output_tokens":4}}},"session_id":"S","uuid":"u2","ttft_ms":942}`

	lineBlockStart = `{"type":"stream_event","event":{"type":"content_block_start","index":0,` +
		`"content_block":{"type":"text","text":""}},"session_id":"S","uuid":"u3"}`

	lineBlockStop = `{"type":"stream_event","event":{"type":"content_block_stop","index":0},` +
		`"session_id":"S","uuid":"u6"}`

	lineMessageDelta = `{"type":"stream_event","event":{"type":"message_delta",` +
		`"delta":{"stop_reason":"end_turn","stop_sequence":null},` +
		`"usage":{"input_tokens":11,"cache_creation_input_tokens":16440,` +
		`"cache_read_input_tokens":7,"output_tokens":14}},"session_id":"S","uuid":"u7"}`

	lineMessageStop = `{"type":"stream_event","event":{"type":"message_stop"},"session_id":"S","uuid":"u8"}`

	lineRateLimitAllowed = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed",` +
		`"resetsAt":1789996200,"rateLimitType":"five_hour","overageStatus":"rejected",` +
		`"unifiedWindows":{"five_hour":{"utilization":0.26,"resetsAt":1789996200},` +
		`"seven_day":{"utilization":0.13,"resetsAt":1790571600}}},"session_id":"S","uuid":"u9"}`

	lineRateLimitExhausted = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected",` +
		`"resetsAt":1789996200,"rateLimitType":"five_hour",` +
		`"unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":1789996200}}},` +
		`"session_id":"S","uuid":"u9"}`
)

// lineDelta is one text_delta event.
func lineDelta(text string) string {
	return `{"type":"stream_event","event":{"type":"content_block_delta","index":0,` +
		`"delta":{"type":"text_delta","text":"` + text + `"}},"session_id":"S","uuid":"ud"}`
}

// lineAssistant is the assembled assistant message the CLI emits alongside
// the deltas. Its text must not be emitted a second time.
func lineAssistant(text string) string {
	return `{"type":"assistant","message":{"model":"claude-opus-5","id":"msg_1","type":"message",` +
		`"role":"assistant","content":[{"type":"text","text":"` + text + `"}],` +
		`"usage":{"input_tokens":11,"cache_creation_input_tokens":16440,` +
		`"cache_read_input_tokens":7,"output_tokens":14}},"session_id":"S","uuid":"ua"}`
}

// lineResult is the terminal event.
func lineResult(text string) string {
	return `{"type":"result","subtype":"success","is_error":false,"duration_ms":2098,` +
		`"result":"` + text + `","session_id":"S","num_turns":1,"total_cost_usd":0.0659,` +
		`"api_error_status":null,"terminal_reason":"completed",` +
		`"usage":{"input_tokens":11,"cache_creation_input_tokens":16440,` +
		`"cache_read_input_tokens":7,"output_tokens":14},` +
		`"modelUsage":{"claude-opus-5":{"inputTokens":11,"outputTokens":14}},"uuid":"ur"}`
}

// lineErrorResult is a failed run's terminal event.
func lineErrorResult(subtype, text string, apiStatus string) string {
	return `{"type":"result","subtype":"` + subtype + `","is_error":true,"duration_ms":120,` +
		`"result":"` + text + `","session_id":"S","num_turns":1,` +
		`"api_error_status":` + apiStatus + `,"terminal_reason":"error","uuid":"ur"}`
}

// okStream is the happy path: init, deltas, assistant, result.
func okStream(text string) string {
	return emit(
		lineInit,
		lineStatus,
		lineMessageStart,
		lineBlockStart,
		lineDelta(text),
		lineAssistant(text),
		lineBlockStop,
		lineMessageDelta,
		lineMessageStop,
		lineRateLimitAllowed,
		lineResult(text),
	)
}
