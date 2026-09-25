package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/RazerBlade6/apex/internal/claudecmd"
	"github.com/RazerBlade6/apex/internal/executor"
	"github.com/RazerBlade6/apex/internal/provider"
)

// editingTools are the tools whose use means a file changed.
//
// DESIGN.md §9 asks the executor to distinguish tool calls from file edits,
// and this set is that distinction. It is a deny-nothing list: a tool not
// named here is reported as an ordinary tool call, so an unknown tool is
// under-reported rather than misreported as an edit that did not happen.
var editingTools = map[string]bool{
	"Write":        true,
	"Edit":         true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

// toolInput is the handful of fields Apex reads out of a tool call. Every
// other field the tool takes is left undecoded: this is for describing the
// call in a terminal, not for re-executing it.
type toolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Path         string `json:"path"`
	Pattern      string `json:"pattern"`
	Command      string `json:"command"`
	Description  string `json:"description"`
	URL          string `json:"url"`
}

// state accumulates one run as its stream arrives.
type state struct {
	result    *executor.Result
	seenFiles map[string]bool
	// emit reports whether the consumer is still there; false abandons the
	// run.
	emit func(executor.Event) bool

	sawDelta bool
	text     strings.Builder

	limit      *claudecmd.RateLimitInfo
	resultLine *claudecmd.Line
	// apiKeySource is what the init event reported. "none" means the run was
	// served by the subscription; anything else means it was not, which is
	// worth a notice rather than silence.
	apiKeySource string
}

// consume folds one stream line into the state and emits whatever it means.
// It returns false when the consumer has gone away.
func (s *state) consume(ln *claudecmd.Line) bool {
	switch ln.Type {
	case "system":
		if ln.Subtype != "init" {
			return true
		}
		if ln.SessionID != "" {
			s.result.SessionID = ln.SessionID
		}
		if ln.Model != "" {
			s.result.Model = ln.Model
		}
		s.apiKeySource = ln.APIKeySource
		// DESIGN.md §9: "the builder loop runs on the Claude Code
		// subscription, not on an API key". apiKeySource "none" is how a
		// subscription-backed run identifies itself, so anything else means
		// this dispatch is being billed to an API account — which the user
		// should hear now rather than discover on an invoice.
		if src := strings.TrimSpace(s.apiKeySource); src != "" && src != "none" {
			return s.emit(executor.Event{
				Type: executor.EventNotice,
				Text: "this run is authenticated by an API key (" + src + "), not the Claude subscription, so it is billed rather than drawn from your plan",
			})
		}

	case "stream_event":
		return s.consumeStreamEvent(ln)

	case "assistant":
		return s.consumeAssistant(ln)

	case "user":
		return s.consumeToolResults(ln)

	case "rate_limit_event":
		if ln.RateLimitInfo != nil {
			s.limit = ln.RateLimitInfo
		}

	case "result":
		copied := *ln
		s.resultLine = &copied
		if u := ln.Usage; u != nil {
			s.result.Usage = usageOf(u)
		}
		if s.result.Model == "" {
			s.result.Model = copied.ServingModel()
		}
		s.result.Turns = ln.NumTurns
		s.result.CostUSD = ln.TotalCostUSD
		s.result.Summary = strings.TrimSpace(ln.Text())
	}
	return true
}

func (s *state) consumeStreamEvent(ln *claudecmd.Line) bool {
	if ln.Event == nil {
		return true
	}
	switch ln.Event.Type {
	case "message_start":
		if m := ln.Event.Message; m != nil {
			if m.Model != "" {
				s.result.Model = m.Model
			}
			if m.Usage != nil {
				s.result.Usage = usageOf(m.Usage)
			}
		}
	case "content_block_delta":
		d := ln.Event.Delta
		if d == nil || d.Type != "text_delta" || d.Text == "" {
			return true
		}
		s.sawDelta = true
		s.text.WriteString(d.Text)
		return s.emit(executor.Event{Type: executor.EventOutput, Text: d.Text})
	case "message_delta":
		if u := ln.Event.Usage; u != nil {
			s.result.Usage = usageOf(u)
		}
	}
	return true
}

// consumeAssistant reads an assembled assistant message: its text (only as a
// fallback, since the deltas already carried it) and its tool calls, which
// the partial-message stream does not surface in a usable form.
func (s *state) consumeAssistant(ln *claudecmd.Line) bool {
	if ln.Message == nil {
		return true
	}
	if ln.Message.Model != "" {
		s.result.Model = ln.Message.Model
	}
	if ln.Message.Usage != nil {
		s.result.Usage = usageOf(ln.Message.Usage)
	}
	for _, block := range ln.Message.Content {
		switch block.Type {
		case "text":
			// Deltas already emitted this. Emitting it again would double
			// every line of the run's output.
			if !s.sawDelta && block.Text != "" {
				s.text.WriteString(block.Text)
				if !s.emit(executor.Event{Type: executor.EventOutput, Text: block.Text}) {
					return false
				}
			}
		case "tool_use":
			if !s.emitToolUse(block) {
				return false
			}
		}
	}
	return true
}

func (s *state) emitToolUse(block claudecmd.ContentBlock) bool {
	var in toolInput
	if len(block.Input) > 0 {
		// A tool whose input is not an object is not a reason to fail the
		// run; it is a reason to describe the call with less detail.
		_ = json.Unmarshal(block.Input, &in)
	}

	if editingTools[block.Name] {
		path := in.FilePath
		if path == "" {
			path = in.NotebookPath
		}
		if path == "" {
			path = in.Path
		}
		if path != "" && !s.seenFiles[path] {
			s.seenFiles[path] = true
			s.result.FilesChanged = append(s.result.FilesChanged, path)
		}
		return s.emit(executor.Event{
			Type: executor.EventFileEdit,
			Tool: block.Name,
			Path: path,
			Text: path,
		})
	}

	return s.emit(executor.Event{
		Type: executor.EventToolUse,
		Tool: block.Name,
		Text: describeTool(block.Name, in),
	})
}

// describeTool renders a one-line summary of a tool call for a terminal.
func describeTool(name string, in toolInput) string {
	switch {
	case in.Command != "":
		return firstLine(in.Command)
	case in.FilePath != "":
		return in.FilePath
	case in.NotebookPath != "":
		return in.NotebookPath
	case in.Pattern != "":
		return in.Pattern
	case in.Path != "":
		return in.Path
	case in.URL != "":
		return in.URL
	case in.Description != "":
		return firstLine(in.Description)
	default:
		return ""
	}
}

// consumeToolResults surfaces the tool results that went wrong.
//
// A successful result is noise — the agent's own narration covers it — but a
// failed one is the single most useful thing on the stream for an unattended
// run. `--permission-prompts none` denies rather than asks, so a permission
// the dispatch did not carry shows up here and nowhere else; without this, a
// run blocked on a denied tool would look like a run that simply chose not to
// do very much.
func (s *state) consumeToolResults(ln *claudecmd.Line) bool {
	if ln.Message == nil {
		return true
	}
	for _, block := range ln.Message.Content {
		if block.Type != "tool_result" || !block.IsError {
			continue
		}
		text := executor.Truncate(toolResultText(block.Content))
		if text == "" {
			text = "the tool reported an error and said nothing further"
		}
		if !s.emit(executor.Event{Type: executor.EventNotice, Text: text}) {
			return false
		}
	}
	return true
}

// toolResultText decodes a tool_result payload, which is a string for some
// tools and a block array for others.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []claudecmd.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, block := range blocks {
			if block.Type == "text" && block.Text != "" {
				if b.Len() > 0 {
					b.WriteString(" ")
				}
				b.WriteString(block.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

// finish returns the completed result.
func (s *state) finish() *executor.Result {
	if s.result.Summary == "" {
		s.result.Summary = strings.TrimSpace(s.text.String())
	}
	// Stable order: the terminal report and the log should not depend on
	// which file the agent happened to touch first.
	sort.Strings(s.result.FilesChanged)
	return s.result
}

// usageOf converts the CLI's accounting to Apex's.
//
// It is the same conversion internal/provider/claudecli makes. Both live on
// their own side of the provider/executor boundary rather than in claudecmd,
// because claudecmd models the wire format and deliberately depends on
// nothing else in Apex.
func usageOf(u *claudecmd.Usage) provider.Usage {
	if u == nil {
		return provider.Usage{}
	}
	return provider.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// sessionLimitPhrases are how a subscription window announces it is spent.
//
// They are the weaker of two signals: the structured rate_limit_event is
// checked first. DESIGN.md §8 records why they exist anyway — the exhausted
// form of that event has never been observed, because observing it requires
// exhausting the window, so a run that could only recognise a session limit
// through an unobserved field would recognise it never.
var sessionLimitPhrases = []string{
	"usage limit reached",
	"session limit",
	"5-hour limit",
	"five-hour limit",
	"weekly limit",
	"limit reached · resets",
}

// classify decides whether a finished run failed, and if so how.
//
// The order matters the same way it does in the provider: a cancelled context
// is not a failure of the executor, and a spent subscription window is not a
// generic failure. Everything else keeps the agent's own words.
func classify(
	ctx context.Context,
	runID string,
	st *state,
	waitErr, scanErr error,
	stderr *claudecmd.TailBuffer,
) *executor.RunError {

	// 1. The caller left, or the timeout fired. The process group was
	//    killed, so "signal: killed" in waitErr says nothing about the run.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &executor.RunError{
			Executor: Name, RunID: runID, Kind: executor.FailureCancelled,
			Message: "the dispatch was cancelled; the agent was stopped and its changes, if any, are still in the working tree",
			Err:     ctxErr,
		}
	}

	failed := waitErr != nil || scanErr != nil ||
		(st.resultLine != nil && st.resultLine.IsError) || st.resultLine == nil
	if !failed {
		return nil
	}

	detail := strings.TrimSpace(st.result.Summary)
	if tail := strings.TrimSpace(stderr.String()); tail != "" {
		if detail != "" {
			detail += "; "
		}
		detail += tail
	}
	lower := strings.ToLower(detail)

	// 2. A subscription window that is spent, read only on a run that
	//    already failed.
	if !st.limit.Permissive() || containsAny(lower, sessionLimitPhrases) {
		msg := "the Claude subscription's usage window is exhausted"
		if w := st.limit.Window(); w != "" {
			msg = "the Claude subscription's " + w + " usage window is exhausted"
		}
		if detail != "" {
			msg += ": " + detail
		}
		return &executor.RunError{
			Executor: Name, RunID: runID, Kind: executor.FailureSessionLimit,
			Message:  executor.Truncate(msg),
			ResetsAt: st.limit.Resets(),
			Err:      waitErr,
		}
	}

	// 3. Anything else, with the exit status and the agent's message intact.
	msg := detail
	var exitErr *exec.ExitError
	switch {
	case errors.As(waitErr, &exitErr):
		if msg == "" {
			msg = "no output"
		}
		msg = claudecmd.Binary + " exited " + strconv.Itoa(exitErr.ExitCode()) + ": " + msg
	case scanErr != nil:
		if msg == "" {
			msg = scanErr.Error()
		}
		msg = "reading the event stream failed: " + msg
	case st.resultLine == nil:
		if msg == "" {
			msg = "the agent produced no result event, so the run's outcome is unknown"
		}
	case msg == "":
		msg = claudecmd.Binary + " reported a failure without saying what"
	}

	err := waitErr
	if err == nil {
		err = scanErr
	}
	return &executor.RunError{
		Executor: Name, RunID: runID, Kind: executor.FailureUnknown,
		Message: executor.Truncate(msg), Err: err,
	}
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}
