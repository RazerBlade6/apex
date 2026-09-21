package openai

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

// testKey is deliberately distinctive so a test can assert it never turns up
// in an error message.
const testKey = "sk-openai-test-NEVER-LOG-THIS"

// chunk renders one SSE frame the way Chat Completions does: data only, no
// event name.
func chunk(data any) string {
	body, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("data: %s\n\n", body)
}

func deltaChunk(text string) string {
	return chunk(map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk",
		"created": 1, "model": "gpt-5",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": text},
		}},
	})
}

// usageChunk is the final chunk, which is the only one that carries usage and
// only when stream_options.include_usage was set.
func usageChunk() string {
	return chunk(map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk",
		"created": 1, "model": "gpt-5", "choices": []any{},
		"usage": map[string]any{
			"prompt_tokens": 120, "completion_tokens": 42, "total_tokens": 162,
			"prompt_tokens_details": map[string]any{"cached_tokens": 96},
		},
	})
}

func textStream(deltas ...string) string {
	var b strings.Builder
	for _, d := range deltas {
		b.WriteString(deltaChunk(d))
	}
	b.WriteString(usageChunk())
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

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
		Model:      "gpt-5",
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

// drain reads a stream to completion and enforces the contract: the channel
// closes, and exactly one terminal event arrives.
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
	if usage.InputTokens != 120 || usage.OutputTokens != 42 {
		t.Errorf("usage = %+v, want 120 in / 42 out", usage)
	}
	// prompt_tokens_details.cached_tokens, which is where OpenAI reports what
	// its automatic prefix caching saved.
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
		req   provider.Request
		check func(t *testing.T, body map[string]any)
	}{
		{
			name: "usage is asked for explicitly",
			req:  userRequest("hi"),
			check: func(t *testing.T, body map[string]any) {
				opts, ok := body["stream_options"].(map[string]any)
				if !ok {
					t.Fatalf("stream_options = %v, want an object", body["stream_options"])
				}
				if opts["include_usage"] != true {
					t.Error("include_usage was not set; usage would never arrive")
				}
			},
		},
		{
			name: "the modern token cap is used",
			req:  userRequest("hi"),
			check: func(t *testing.T, body map[string]any) {
				if body["max_completion_tokens"] != float64(64) {
					t.Errorf("max_completion_tokens = %v, want 64", body["max_completion_tokens"])
				}
			},
		},
		{
			name: "system leads, then the turns in order",
			req: provider.Request{
				System: "identity and digests",
				Messages: []provider.Message{
					{Role: provider.RoleUser, Content: "one"},
					{Role: provider.RoleAssistant, Content: "two"},
				},
				MaxTokens:   64,
				CacheSystem: true,
			},
			check: func(t *testing.T, body map[string]any) {
				messages, ok := body["messages"].([]any)
				if !ok || len(messages) != 3 {
					t.Fatalf("messages = %v, want 3", body["messages"])
				}
				want := []string{"system", "user", "assistant"}
				for i, w := range want {
					if got := messages[i].(map[string]any)["role"]; got != w {
						t.Errorf("messages[%d].role = %v, want %v", i, got, w)
					}
				}
			},
		},
		{
			name: "effort is not mapped onto reasoning_effort",
			req:  provider.Request{Messages: userRequest("hi").Messages, Effort: "xhigh", MaxTokens: 64},
			check: func(t *testing.T, body map[string]any) {
				// DESIGN.md §8: effort is Anthropic-only and ignored
				// elsewhere. xhigh is not even a level OpenAI has.
				if _, present := body["reasoning_effort"]; present {
					t.Error("effort leaked into reasoning_effort")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, streamOK(textStream("ok")))
			p := newTestProvider(t, api)

			events, err := p.Stream(context.Background(), tt.req)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if _, _, streamErr := drain(t, events); streamErr != nil {
				t.Fatalf("stream failed: %v", streamErr)
			}
			if api.body["stream"] != true {
				t.Errorf("stream = %v, want true", api.body["stream"])
			}
			if api.body["model"] != "gpt-5" {
				t.Errorf("model = %v, want gpt-5", api.body["model"])
			}
			tt.check(t, api.body)
		})
	}
}

func TestStreamMidStreamError(t *testing.T) {
	api := newFakeAPI(t, streamOK(deltaChunk("partial")+chunk(map[string]any{
		"error": map[string]any{"message": "the server had a problem", "type": "server_error"},
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
		t.Errorf("text = %q, want the delta that arrived before the error", text)
	}

	var perr *provider.Error
	if !errors.As(streamErr, &perr) {
		t.Fatalf("err = %T, want *provider.Error", streamErr)
	}
	if perr.Kind != provider.KindServer || !perr.Retryable() {
		t.Errorf("Kind = %v (retryable %v), want a retryable server failure",
			perr.Kind, perr.Retryable())
	}
}

func TestStreamRefusal(t *testing.T) {
	api := newFakeAPI(t, streamOK(chunk(map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion.chunk",
		"created": 1, "model": "gpt-5",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"refusal": "I can't help with that"},
		}},
	})+"data: [DONE]\n\n"))
	p := newTestProvider(t, api)

	events, err := p.Stream(context.Background(), userRequest("hi"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_, _, streamErr := drain(t, events)
	if streamErr == nil {
		t.Fatal("a refusal was reported as success")
	}
	var perr *provider.Error
	if !errors.As(streamErr, &perr) {
		t.Fatalf("err = %T, want *provider.Error", streamErr)
	}
	if perr.Retryable() {
		t.Error("a refusal is not retryable")
	}
	if !strings.Contains(perr.Message, "refused") {
		t.Errorf("Message = %q, want it to say the model refused", perr.Message)
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
			body:     `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`,
			wantKind: provider.KindAuth,
		},
		{
			name:          "a rate limit",
			status:        http.StatusTooManyRequests,
			body:          `{"error":{"message":"Rate limit reached","type":"requests"}}`,
			wantKind:      provider.KindRateLimit,
			wantRetryable: true,
		},
		{
			name:     "an unknown model",
			status:   http.StatusNotFound,
			body:     `{"error":{"message":"The model does not exist","type":"invalid_request_error"}}`,
			wantKind: provider.KindRequest,
		},
		{
			name:     "a bad request",
			status:   http.StatusBadRequest,
			body:     `{"error":{"message":"Invalid schema","type":"invalid_request_error"}}`,
			wantKind: provider.KindRequest,
		},
		{
			name:          "a provider outage",
			status:        http.StatusBadGateway,
			body:          `{"error":{"message":"Bad gateway","type":"server_error"}}`,
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
		fmt.Fprint(w, deltaChunk("first"))
		w.(http.Flusher).Flush()
		close(sent)
		<-r.Context().Done()
	})
	p := newTestProvider(t, api)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := p.Stream(ctx, userRequest("hi"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

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
		for i := 0; i < 64; i++ {
			fmt.Fprint(w, deltaChunk("x"))
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
	time.Sleep(50 * time.Millisecond)
	cancel()

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
		if err := p.Structured(context.Background(), userRequest("plan"), schema, &got); err != nil {
			t.Fatalf("Structured: %v", err)
		}
		if got.Title != "Fix chunking" || got.Effort != "medium" {
			t.Errorf("got = %+v, want the parsed record", got)
		}

		format, ok := api.body["response_format"].(map[string]any)
		if !ok {
			t.Fatalf("response_format = %v, want an object", api.body["response_format"])
		}
		if format["type"] != "json_schema" {
			t.Errorf("response_format.type = %v, want json_schema", format["type"])
		}
		inner, ok := format["json_schema"].(map[string]any)
		if !ok {
			t.Fatalf("json_schema = %v, want an object", format["json_schema"])
		}
		// Without strict, the schema is a hint rather than a guarantee, which
		// is the entire reason for constraining the output at all.
		if inner["strict"] != true {
			t.Errorf("json_schema.strict = %v, want true", inner["strict"])
		}
		if _, ok := inner["schema"].(map[string]any); !ok {
			t.Errorf("json_schema.schema = %v, want the schema object", inner["schema"])
		}
	})

	t.Run("a non-JSON response is a parse failure, not silent success", func(t *testing.T) {
		api := newFakeAPI(t, streamOK(textStream("sorry, no")))
		p := newTestProvider(t, api)

		var got plan
		err := p.Structured(context.Background(), userRequest("plan"), schema, &got)
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
		if err := p.Structured(context.Background(), userRequest("plan"), schema, &got); err == nil {
			t.Fatal("Structured accepted an empty response")
		}
	})

	t.Run("a rejected schema is classified, not swallowed", func(t *testing.T) {
		api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"Invalid schema: additionalProperties is required","type":"invalid_request_error"}}`)
		})
		p := newTestProvider(t, api)

		var got plan
		err := p.Structured(context.Background(), userRequest("plan"), schema, &got)
		var perr *provider.Error
		if !errors.As(err, &perr) {
			t.Fatalf("err = %T (%v), want *provider.Error", err, err)
		}
		if perr.Kind != provider.KindRequest || perr.Op != "structured" {
			t.Errorf("err = %+v, want a request failure on the structured op", perr)
		}
		if !strings.Contains(perr.Message, "Invalid schema") {
			t.Errorf("Message = %q, want the vendor's explanation", perr.Message)
		}
		if strings.Contains(perr.Error(), testKey) {
			t.Fatal("the API key leaked into an error message")
		}
	})
}

func TestNewRejectsAnEmptyKey(t *testing.T) {
	if _, err := New(provider.Config{Model: "gpt-5"}); err == nil {
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
	if got := api.hdr.Get("Authorization"); got != "Bearer "+testKey {
		t.Errorf("Authorization = %q, want the injected key as a bearer token", got)
	}
	body, _ := json.Marshal(api.body)
	if strings.Contains(string(body), testKey) {
		t.Error("the key appeared in the request body")
	}
}
