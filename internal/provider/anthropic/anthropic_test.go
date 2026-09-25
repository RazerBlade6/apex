package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RazerBlade6/apex/internal/provider"
)

// testKey is what every test authenticates with. It is deliberately
// distinctive so a test can assert it never turns up in an error message.
const testKey = "sk-ant-test-NEVER-LOG-THIS"

// frame renders one SSE frame the way the Messages API does.
func frame(event string, data any) string {
	body, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, body)
}

// textStream is a complete, well-formed response: a message with the given
// text deltas and the usage the API would report.
func textStream(deltas ...string) string {
	var b strings.Builder
	b.WriteString(frame("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_test", "type": "message", "role": "assistant",
			"model": "claude-opus-5", "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens": 120, "output_tokens": 1,
				"cache_read_input_tokens": 96, "cache_creation_input_tokens": 0,
			},
		},
	}))
	b.WriteString(frame("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))
	for _, d := range deltas {
		b.WriteString(frame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": d},
		}))
	}
	b.WriteString(frame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}))
	b.WriteString(frame("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 42},
	}))
	b.WriteString(frame("message_stop", map[string]any{"type": "message_stop"}))
	return b.String()
}

// fakeAPI is an httptest server standing in for the Messages API. It records
// the last request body so tests can assert on what was actually sent.
type fakeAPI struct {
	*httptest.Server
	body map[string]any
	hdr  http.Header
}

func newFakeAPI(t *testing.T, handler http.HandlerFunc) *fakeAPI {
	t.Helper()
	api := &fakeAPI{}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		api.hdr = r.Header.Clone()
		api.body = map[string]any{}
		_ = json.Unmarshal(raw, &api.body)
		handler(w, r)
	}))
	t.Cleanup(api.Close)
	return api
}

// streamOK answers with a fixed SSE body.
func streamOK(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func newTestProvider(t *testing.T, api *fakeAPI, opts ...func(*provider.Config)) *Provider {
	t.Helper()
	noRetries := 0
	cfg := provider.Config{
		APIKey:     testKey,
		BaseURL:    api.URL,
		Model:      "claude-opus-5",
		Effort:     "high",
		MaxRetries: &noRetries,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// drain reads a stream to completion and reports what came out. It also
// enforces the contract: the channel closes, and exactly one terminal event
// arrives.
func drain(t *testing.T, events <-chan provider.Event) (text string, usage *provider.Usage, err error) {
	t.Helper()
	var b strings.Builder
	terminal := 0
	for event := range events {
		switch event.Type {
		case provider.EventTextDelta:
			if terminal > 0 {
				t.Error("a text delta arrived after the stream ended")
			}
			b.WriteString(event.Text)
		case provider.EventDone:
			terminal++
			usage = event.Usage
		case provider.EventError:
			terminal++
			err = event.Err
		}
	}
	if terminal != 1 {
		t.Errorf("the stream produced %d terminal events, want exactly 1", terminal)
	}
	// A closed channel yields the zero value forever; receiving again proves
	// the close happened rather than the range ending some other way.
	if _, open := <-events; open {
		t.Error("the channel was not closed")
	}
	return b.String(), usage, err
}

func userRequest(text string) provider.Request {
	return provider.Request{
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: text}},
		MaxTokens: 64,
	}
}

func TestStreamHappyPath(t *testing.T) {
	api := newFakeAPI(t, streamOK(textStream("Hel", "lo, ", "world")))
	p := newTestProvider(t, api)

	events, err := p.Stream(context.Background(), userRequest("hi"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, usage, streamErr := drain(t, events)
	if streamErr != nil {
		t.Fatalf("stream failed: %v", streamErr)
	}
	if text != "Hello, world" {
		t.Errorf("text = %q, want %q", text, "Hello, world")
	}
	if usage == nil {
		t.Fatal("EventDone carried no usage")
	}
	// message_start reports the input side, message_delta the output side.
	if usage.InputTokens != 120 || usage.OutputTokens != 42 {
		t.Errorf("usage = %+v, want 120 in / 42 out", usage)
	}
	// M4 needs this to confirm the cache prefix is actually being hit.
	if usage.CacheReadTokens != 96 {
		t.Errorf("CacheReadTokens = %d, want 96", usage.CacheReadTokens)
	}
	if p.Name() != Name {
		t.Errorf("Name() = %q, want %q", p.Name(), Name)
	}
}

func TestStreamRequestShape(t *testing.T) {
	tests := []struct {
		name  string
		model string
		req   provider.Request
		check func(t *testing.T, body map[string]any)
	}{
		{
			name:  "model ids carry no date suffix",
			model: "claude-opus-5",
			req:   userRequest("hi"),
			check: func(t *testing.T, body map[string]any) {
				if body["model"] != "claude-opus-5" {
					t.Errorf("model = %v, want claude-opus-5", body["model"])
				}
			},
		},
		{
			name:  "thinking is adaptive, never budget_tokens",
			model: "claude-opus-5",
			req:   userRequest("hi"),
			check: func(t *testing.T, body map[string]any) {
				thinking, ok := body["thinking"].(map[string]any)
				if !ok {
					t.Fatalf("thinking = %v, want an object", body["thinking"])
				}
				if thinking["type"] != "adaptive" {
					t.Errorf("thinking.type = %v, want adaptive", thinking["type"])
				}
				if _, present := thinking["budget_tokens"]; present {
					t.Error("budget_tokens was sent; Opus 5 rejects it")
				}
			},
		},
		{
			name:  "effort is nested in output_config",
			model: "claude-opus-5",
			req:   provider.Request{Messages: userRequest("hi").Messages, Effort: "low", MaxTokens: 64},
			check: func(t *testing.T, body map[string]any) {
				if _, topLevel := body["effort"]; topLevel {
					t.Error("effort was sent top-level; it belongs in output_config")
				}
				cfg, ok := body["output_config"].(map[string]any)
				if !ok {
					t.Fatalf("output_config = %v, want an object", body["output_config"])
				}
				if cfg["effort"] != "low" {
					t.Errorf("output_config.effort = %v, want low", cfg["effort"])
				}
			},
		},
		{
			name:  "the cache breakpoint lands on the last system block",
			model: "claude-opus-5",
			req: provider.Request{
				System:      "identity\n\ndigests",
				Messages:    userRequest("hi").Messages,
				MaxTokens:   64,
				CacheSystem: true,
			},
			check: func(t *testing.T, body map[string]any) {
				system, ok := body["system"].([]any)
				if !ok || len(system) == 0 {
					t.Fatalf("system = %v, want a non-empty array", body["system"])
				}
				last, ok := system[len(system)-1].(map[string]any)
				if !ok {
					t.Fatalf("system block = %v, want an object", system[len(system)-1])
				}
				control, ok := last["cache_control"].(map[string]any)
				if !ok {
					t.Fatalf("cache_control = %v, want an object", last["cache_control"])
				}
				if control["type"] != "ephemeral" {
					t.Errorf("cache_control.type = %v, want ephemeral", control["type"])
				}
			},
		},
		{
			name:  "no breakpoint is placed when none was asked for",
			model: "claude-opus-5",
			req: provider.Request{
				System:    "identity",
				Messages:  userRequest("hi").Messages,
				MaxTokens: 64,
			},
			check: func(t *testing.T, body map[string]any) {
				system := body["system"].([]any)
				last := system[len(system)-1].(map[string]any)
				if _, present := last["cache_control"]; present {
					t.Error("an unrequested cache breakpoint was placed")
				}
			},
		},
		{
			name:  "haiku takes neither adaptive thinking nor effort",
			model: "claude-haiku-4-5",
			req:   provider.Request{Messages: userRequest("hi").Messages, Effort: "low", MaxTokens: 64},
			check: func(t *testing.T, body map[string]any) {
				if _, present := body["thinking"]; present {
					t.Error("adaptive thinking was sent to haiku, which rejects it")
				}
				if cfg, present := body["output_config"].(map[string]any); present {
					if _, hasEffort := cfg["effort"]; hasEffort {
						t.Error("effort was sent to haiku, which rejects it")
					}
				}
			},
		},
		{
			name:  "roles round-trip",
			model: "claude-opus-5",
			req: provider.Request{
				Messages: []provider.Message{
					{Role: provider.RoleUser, Content: "one"},
					{Role: provider.RoleAssistant, Content: "two"},
					{Role: provider.RoleUser, Content: "three"},
				},
				MaxTokens: 64,
			},
			check: func(t *testing.T, body map[string]any) {
				messages, ok := body["messages"].([]any)
				if !ok || len(messages) != 3 {
					t.Fatalf("messages = %v, want 3", body["messages"])
				}
				want := []string{"user", "assistant", "user"}
				for i, w := range want {
					got := messages[i].(map[string]any)["role"]
					if got != w {
						t.Errorf("messages[%d].role = %v, want %v", i, got, w)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, streamOK(textStream("ok")))
			p := newTestProvider(t, api, func(c *provider.Config) { c.Model = tt.model })

			events, err := p.Stream(context.Background(), tt.req)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if _, _, streamErr := drain(t, events); streamErr != nil {
				t.Fatalf("stream failed: %v", streamErr)
			}
			// Every request streams: a large max_tokens on a non-streaming
			// request would race the HTTP timeout (DESIGN.md §8).
			if api.body["stream"] != true {
				t.Errorf("stream = %v, want true", api.body["stream"])
			}
			tt.check(t, api.body)
		})
	}
}

func TestStreamMidStreamError(t *testing.T) {
	// A well-formed stream that fails partway: the API answered 200 and only
	// then gave up.
	body := strings.SplitAfter(textStream("partial"), "\n\n")
	head := strings.Join(body[:3], "")
	api := newFakeAPI(t, streamOK(head+frame("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "overloaded_error", "message": "Overloaded"},
	})))
	p := newTestProvider(t, api)

	events, err := p.Stream(context.Background(), userRequest("hi"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text, usage, streamErr := drain(t, events)
	if streamErr == nil {
		t.Fatal("the stream reported success after an error frame")
	}
	if usage != nil {
		t.Errorf("usage = %+v, want none on a failed stream", usage)
	}
	if text != "partial" {
		t.Errorf("text = %q, want the deltas that arrived before the error", text)
	}

	var perr *provider.Error
	if !errors.As(streamErr, &perr) {
		t.Fatalf("err = %T, want *provider.Error", streamErr)
	}
	// Auth and validation already passed, so this is the vendor's failure and
	// it is worth retrying.
	if perr.Kind != provider.KindServer {
		t.Errorf("Kind = %v, want %v", perr.Kind, provider.KindServer)
	}
	if !perr.Retryable() {
		t.Error("an overloaded_error is retryable")
	}
	if !strings.Contains(perr.Message, "overloaded_error") {
		t.Errorf("Message = %q, want the vendor's own error body", perr.Message)
	}
}

func TestStreamHTTPFailures(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantKind      provider.Kind
		wantRetryable bool
	}{
		{
			name:     "a rejected key",
			status:   http.StatusUnauthorized,
			body:     `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			wantKind: provider.KindAuth,
		},
		{
			name:          "a rate limit",
			status:        http.StatusTooManyRequests,
			body:          `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			wantKind:      provider.KindRateLimit,
			wantRetryable: true,
		},
		{
			name:     "a bad request",
			status:   http.StatusBadRequest,
			body:     `{"type":"error","error":{"type":"invalid_request_error","message":"model: unknown"}}`,
			wantKind: provider.KindRequest,
		},
		{
			name:     "an unknown model",
			status:   http.StatusNotFound,
			body:     `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`,
			wantKind: provider.KindRequest,
		},
		{
			name:          "a provider outage",
			status:        http.StatusInternalServerError,
			body:          `{"type":"error","error":{"type":"api_error","message":"internal"}}`,
			wantKind:      provider.KindServer,
			wantRetryable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			})
			p := newTestProvider(t, api)

			events, err := p.Stream(context.Background(), userRequest("hi"))
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			_, _, streamErr := drain(t, events)
			if streamErr == nil {
				t.Fatal("a failing request reported success")
			}

			var perr *provider.Error
			if !errors.As(streamErr, &perr) {
				t.Fatalf("err = %T, want *provider.Error", streamErr)
			}
			if perr.Kind != tt.wantKind {
				t.Errorf("Kind = %v, want %v", perr.Kind, tt.wantKind)
			}
			if perr.StatusCode != tt.status {
				t.Errorf("StatusCode = %d, want %d", perr.StatusCode, tt.status)
			}
			if perr.Retryable() != tt.wantRetryable {
				t.Errorf("Retryable() = %v, want %v", perr.Retryable(), tt.wantRetryable)
			}
			if !perr.UserFixable() {
				t.Error("a classified failure is the user's to act on")
			}
			// The key travels in a header and must never come back out in a
			// message the user (or a log) sees.
			if strings.Contains(perr.Error(), testKey) {
				t.Fatal("the API key leaked into an error message")
			}
		})
	}
}

func TestStreamContextCancellationMidStream(t *testing.T) {
	sent := make(chan struct{})
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		body := strings.SplitAfter(textStream("first"), "\n\n")
		fmt.Fprint(w, strings.Join(body[:3], ""))
		w.(http.Flusher).Flush()
		close(sent)
		<-r.Context().Done() // hold the stream open until the client gives up
	})
	p := newTestProvider(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := p.Stream(ctx, userRequest("hi"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Read the delta that did arrive, then cancel mid-stream.
	first, ok := <-events
	if !ok || first.Type != provider.EventTextDelta || first.Text != "first" {
		t.Fatalf("first event = %+v, want the text delta", first)
	}
	<-sent
	cancel()

	var terminal provider.Event
	count := 0
	for event := range events {
		if event.Type != provider.EventTextDelta {
			terminal = event
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d terminal events after cancellation, want 1", count)
	}
	if terminal.Type != provider.EventError {
		t.Fatalf("terminal event = %v, want an error", terminal.Type)
	}
	if !errors.Is(terminal.Err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", terminal.Err)
	}
	var perr *provider.Error
	if errors.As(terminal.Err, &perr) && perr.Kind != provider.KindCancelled {
		t.Errorf("Kind = %v, want %v", perr.Kind, provider.KindCancelled)
	}
}

func TestStreamAbandonedChannelDoesNotLeak(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// More deltas than the channel buffer holds, so the sender is
		// guaranteed to be parked on a send with nobody reading.
		for i := 0; i < 64; i++ {
			fmt.Fprint(w, frame("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": "msg", "type": "message", "role": "assistant",
					"model": "claude-opus-5", "content": []any{},
					"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
				},
			}))
			fmt.Fprint(w, frame("content_block_start", map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""},
			}))
			fmt.Fprint(w, frame("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": "x"},
			}))
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	p := newTestProvider(t, api)

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := p.Stream(ctx, userRequest("hi")); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Let the sender fill the buffer and block.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// The goroutine parks on a send; cancelling ctx is what releases it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if runtime.NumGoroutine() <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d, want no more than the %d before the stream",
				runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStructured(t *testing.T) {
	type plan struct {
		Title  string `json:"title"`
		Effort string `json:"effort"`
	}
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {"title": {"type": "string"}, "effort": {"type": "string"}},
		"required": ["title", "effort"],
		"additionalProperties": false
	}`)

	t.Run("a schema-constrained response is parsed", func(t *testing.T) {
		api := newFakeAPI(t, streamOK(textStream(`{"title":"Fix `, `chunking","effort":"medium"}`)))
		p := newTestProvider(t, api)

		var got plan
		if _, err := p.Structured(context.Background(), userRequest("plan"), schema, &got); err != nil {
			t.Fatalf("Structured: %v", err)
		}
		if got.Title != "Fix chunking" || got.Effort != "medium" {
			t.Errorf("got = %+v, want the parsed record", got)
		}

		// The schema goes in output_config.format, not the deprecated
		// top-level output_format (DESIGN.md §8).
		if _, deprecated := api.body["output_format"]; deprecated {
			t.Error("the deprecated output_format field was sent")
		}
		cfg, ok := api.body["output_config"].(map[string]any)
		if !ok {
			t.Fatalf("output_config = %v, want an object", api.body["output_config"])
		}
		format, ok := cfg["format"].(map[string]any)
		if !ok {
			t.Fatalf("output_config.format = %v, want an object", cfg["format"])
		}
		if format["type"] != "json_schema" {
			t.Errorf("format.type = %v, want json_schema", format["type"])
		}
		if _, ok := format["schema"].(map[string]any); !ok {
			t.Errorf("format.schema = %v, want the schema object", format["schema"])
		}
		if api.body["stream"] != true {
			t.Error("a structured call must stream like any other")
		}
	})

	t.Run("a non-JSON response is a parse failure, not silent success", func(t *testing.T) {
		api := newFakeAPI(t, streamOK(textStream("sorry, no")))
		p := newTestProvider(t, api)

		var got plan
		_, err := p.Structured(context.Background(), userRequest("plan"), schema, &got)
		if err == nil {
			t.Fatal("Structured accepted a response that was not JSON")
		}
		if !strings.Contains(err.Error(), "decode structured response") {
			t.Errorf("err = %q, want it to name the decode step", err)
		}
	})

	t.Run("an empty response is reported", func(t *testing.T) {
		api := newFakeAPI(t, streamOK(textStream()))
		p := newTestProvider(t, api)

		var got plan
		if _, err := p.Structured(context.Background(), userRequest("plan"), schema, &got); err == nil {
			t.Fatal("Structured accepted an empty response")
		}
	})

	t.Run("an HTTP failure is classified like any other", func(t *testing.T) {
		api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error"}}`)
		})
		p := newTestProvider(t, api)

		var got plan
		_, err := p.Structured(context.Background(), userRequest("plan"), schema, &got)
		var perr *provider.Error
		if !errors.As(err, &perr) {
			t.Fatalf("err = %T (%v), want *provider.Error", err, err)
		}
		if perr.Kind != provider.KindAuth || perr.Op != "structured" {
			t.Errorf("err = %+v, want an auth failure on the structured op", perr)
		}
		if strings.Contains(perr.Error(), testKey) {
			t.Fatal("the API key leaked into an error message")
		}
	})

	t.Run("a schema that is not an object is refused before any call", func(t *testing.T) {
		api := newFakeAPI(t, streamOK(textStream("{}")))
		p := newTestProvider(t, api)

		var got plan
		if _, err := p.Structured(context.Background(), userRequest("plan"), json.RawMessage(`["nope"]`), &got); err == nil {
			t.Fatal("a non-object schema was accepted")
		}
	})
}

func TestNewRejectsAnEmptyKey(t *testing.T) {
	if _, err := New(provider.Config{Model: "claude-opus-5"}); err == nil {
		t.Fatal("New accepted an empty API key")
	}
}

func TestStreamValidatesBeforeSending(t *testing.T) {
	api := newFakeAPI(t, streamOK(textStream("ok")))
	p := newTestProvider(t, api)

	if _, err := p.Stream(context.Background(), provider.Request{}); err == nil {
		t.Fatal("Stream accepted a request with no messages")
	}
	if api.body != nil {
		t.Error("an invalid request was sent to the API anyway")
	}
}

func TestCapabilitiesFor(t *testing.T) {
	tests := []struct {
		model            string
		adaptiveThinking bool
		effort           bool
	}{
		{model: "claude-opus-5", adaptiveThinking: true, effort: true},
		{model: "claude-sonnet-5", adaptiveThinking: true, effort: true},
		{model: "claude-fable-5-1", adaptiveThinking: true, effort: true},
		{model: "claude-opus-4-8", adaptiveThinking: true, effort: true},
		{model: "claude-sonnet-4-6", adaptiveThinking: true, effort: true},
		{model: "claude-haiku-4-5", adaptiveThinking: false, effort: false},
		{model: "claude-sonnet-4-5", adaptiveThinking: false, effort: false},
		{model: "claude-opus-4-5", adaptiveThinking: false, effort: true},
		{model: "claude-3-5-haiku", adaptiveThinking: false, effort: false},
		// An id released after this code was written is assumed modern: a new
		// Opus silently losing thinking is the worse of the two failures.
		{model: "claude-opus-6", adaptiveThinking: true, effort: true},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := capabilitiesFor(tt.model)
			if got.adaptiveThinking != tt.adaptiveThinking || got.effort != tt.effort {
				t.Errorf("capabilitiesFor(%q) = %+v, want thinking=%v effort=%v",
					tt.model, got, tt.adaptiveThinking, tt.effort)
			}
		})
	}
}

func TestAPIKeyIsSentAsAHeaderAndNowhereElse(t *testing.T) {
	api := newFakeAPI(t, streamOK(textStream("ok")))
	p := newTestProvider(t, api)

	events, err := p.Stream(context.Background(), userRequest("hi"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if _, _, streamErr := drain(t, events); streamErr != nil {
		t.Fatalf("stream failed: %v", streamErr)
	}
	if api.hdr.Get("x-api-key") != testKey {
		t.Errorf("x-api-key = %q, want the injected key", api.hdr.Get("x-api-key"))
	}
	body, _ := json.Marshal(api.body)
	if strings.Contains(string(body), testKey) {
		t.Error("the key appeared in the request body")
	}
}
