package provider

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestRequestValidate(t *testing.T) {
	ok := []Message{{Role: RoleUser, Content: "hello"}}

	tests := []struct {
		name    string
		req     Request
		wantErr string
	}{
		{
			name: "a complete request",
			req:  Request{Model: "claude-opus-5", Messages: ok, MaxTokens: 16},
		},
		{
			name:    "no model",
			req:     Request{Messages: ok},
			wantErr: "no model",
		},
		{
			name:    "no messages",
			req:     Request{Model: "claude-opus-5"},
			wantErr: "no messages",
		},
		{
			name:    "an empty message",
			req:     Request{Model: "claude-opus-5", Messages: []Message{{Role: RoleUser, Content: "  "}}},
			wantErr: "message 0 is empty",
		},
		{
			name:    "an unknown role",
			req:     Request{Model: "claude-opus-5", Messages: []Message{{Role: "system", Content: "x"}}},
			wantErr: `role "system"`,
		},
		{
			name:    "negative max tokens",
			req:     Request{Model: "claude-opus-5", Messages: ok, MaxTokens: -1},
			wantErr: "negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigApplyAndDefaults(t *testing.T) {
	cfg := Config{Model: "claude-opus-5", Effort: "high"}

	got := cfg.Apply(Request{}).WithDefaults()
	if got.Model != "claude-opus-5" || got.Effort != "high" {
		t.Errorf("Apply left the route defaults out: %+v", got)
	}
	if got.MaxTokens != DefaultMaxTokens {
		t.Errorf("MaxTokens = %d, want the default %d", got.MaxTokens, DefaultMaxTokens)
	}

	// A request that names its own wins, so one provider can serve a caller
	// that wants something other than the configured default.
	override := cfg.Apply(Request{Model: "claude-haiku-4-5", Effort: "low", MaxTokens: 32})
	if override.Model != "claude-haiku-4-5" || override.Effort != "low" || override.MaxTokens != 32 {
		t.Errorf("Apply overrode an explicit request: %+v", override)
	}
}

func TestEmitStopsWhenContextIsDone(t *testing.T) {
	// An unbuffered channel nobody reads from: the send can only land if the
	// consumer is there, and it is not.
	ch := make(chan Event)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if Emit(ctx, ch, Event{Type: EventTextDelta, Text: "x"}) {
		t.Error("Emit reported a send on a cancelled context")
	}

	// And it does land when someone is reading.
	live, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { <-ch }()
	if !Emit(live, ch, Event{Type: EventDone}) {
		t.Error("Emit dropped an event with a live consumer")
	}
}

func TestKindClassification(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		want          Kind
		wantRetryable bool
	}{
		{name: "unauthorized", status: 401, want: KindAuth},
		{name: "forbidden", status: 403, want: KindAuth},
		{name: "rate limited", status: 429, want: KindRateLimit, wantRetryable: true},
		{name: "bad request", status: 400, want: KindRequest},
		{name: "not found", status: 404, want: KindRequest},
		{name: "unprocessable", status: 422, want: KindRequest},
		{name: "teapot", status: 418, want: KindRequest},
		{name: "server error", status: 500, want: KindServer, wantRetryable: true},
		{name: "bad gateway", status: 502, want: KindServer, wantRetryable: true},
		{name: "success", status: 200, want: KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StatusKind(tt.status)
			if got != tt.want {
				t.Errorf("StatusKind(%d) = %v, want %v", tt.status, got, tt.want)
			}
			if got.Retryable() != tt.wantRetryable {
				t.Errorf("StatusKind(%d).Retryable() = %v, want %v",
					tt.status, got.Retryable(), tt.wantRetryable)
			}
		})
	}
}

func TestTransportKind(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Kind
	}{
		{name: "cancelled", err: context.Canceled, want: KindCancelled},
		{name: "deadline", err: context.DeadlineExceeded, want: KindCancelled},
		{name: "wrapped cancellation", err: errors.Join(errors.New("stream"), context.Canceled), want: KindCancelled},
		{name: "dial failure", err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}, want: KindNetwork},
		{name: "dns failure", err: &net.DNSError{Err: "no such host"}, want: KindNetwork},
		{name: "anything else", err: errors.New("boom"), want: KindUnknown},
		{name: "nil", err: nil, want: KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TransportKind(tt.err); got != tt.want {
				t.Errorf("TransportKind(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestErrorMessage(t *testing.T) {
	err := &Error{
		Provider: "anthropic", Op: "stream", Kind: KindAuth,
		StatusCode: 401, Message: `{"error":{"type":"authentication_error"}}`,
	}
	for _, want := range []string{"anthropic", "stream", "auth", "401", "authentication_error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to mention %q", err, want)
		}
	}
	if err.Retryable() {
		t.Error("a rejected key is not retryable")
	}
	if !err.UserFixable() {
		t.Error("a rejected key is the user's to fix")
	}

	unknown := &Error{Provider: "openai", Op: "stream", Kind: KindUnknown, Err: errors.New("boom")}
	if unknown.UserFixable() {
		t.Error("an unclassifiable failure must not be reported as a user problem")
	}
	if !errors.Is(unknown, unknown.Err) {
		t.Error("Unwrap did not expose the cause")
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("x", maxMessageLen*2)
	got := Truncate(long)
	if len([]rune(got)) != maxMessageLen+1 { // the ellipsis is one rune
		t.Errorf("Truncate produced %d runes, want %d", len([]rune(got)), maxMessageLen+1)
	}
	if short := Truncate("  hello  "); short != "hello" {
		t.Errorf("Truncate(%q) = %q", "  hello  ", short)
	}
}
