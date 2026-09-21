package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/provider/anthropic"
	"github.com/RazerBlade6/apex/internal/provider/openai"
)

func TestNames(t *testing.T) {
	names := Names()
	for _, want := range []string{anthropic.Name, openai.Name} {
		if !slices.Contains(names, want) {
			t.Errorf("Names() = %v, want it to include %q", names, want)
		}
	}
	// Every provider config.Validate accepts must be constructible, or a
	// config that passes validation fails at the first call instead.
	for _, want := range config.ValidProviders {
		if !slices.Contains(names, want) {
			t.Errorf("config accepts provider %q but the registry cannot build it", want)
		}
	}
}

func TestBuild(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		cfg      provider.Config
		wantName string
		wantErr  string
	}{
		{
			name:     "anthropic",
			provider: anthropic.Name,
			cfg:      provider.Config{APIKey: "k", Model: "claude-opus-5"},
			wantName: anthropic.Name,
		},
		{
			name:     "openai",
			provider: openai.Name,
			cfg:      provider.Config{APIKey: "k", Model: "gpt-5"},
			wantName: openai.Name,
		},
		{
			name:     "an unknown vendor",
			provider: "cohere",
			cfg:      provider.Config{APIKey: "k"},
			wantErr:  "not implemented",
		},
		{
			name:     "no key",
			provider: anthropic.Name,
			cfg:      provider.Config{Model: "claude-opus-5"},
			wantErr:  "no API key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Build(tt.provider, tt.cfg)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Build(%q) succeeded, want an error mentioning %q", tt.provider, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %q, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build(%q): %v", tt.provider, err)
			}
			if p.Name() != tt.wantName {
				t.Errorf("Name() = %q, want %q", p.Name(), tt.wantName)
			}
		})
	}
}

func TestUnknownProviderErrorIsUserFixable(t *testing.T) {
	_, err := Build("cohere", provider.Config{APIKey: "k"})
	var unknown *UnknownProviderError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %T, want *UnknownProviderError", err)
	}
	if !unknown.UserFixable() {
		t.Error("a config naming an unimplemented provider is the user's to fix")
	}
	if !strings.Contains(err.Error(), anthropic.Name) {
		t.Errorf("err = %q, want it to list what is available", err)
	}
}

// fakeAnthropic answers one streamed request with a trivial message.
func fakeAnthropic(t *testing.T, seen *http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		start, _ := json.Marshal(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg", "type": "message", "role": "assistant",
				"model": "claude-opus-5", "content": []any{},
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
			},
		})
		fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", start)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewResolvesTheKeyAndHonoursOptions(t *testing.T) {
	// A mock keyring: nothing here touches the real macOS keychain.
	keyring.MockInit()
	const secret = "sk-ant-registry-test"
	if err := keyring.Set("apex:anthropic", config.KeychainUser, secret); err != nil {
		t.Fatalf("seed keychain: %v", err)
	}

	var seen http.Header
	srv := fakeAnthropic(t, &seen)

	p, err := New(context.Background(),
		config.ModelRoute{Provider: "anthropic", Model: "claude-opus-5", Effort: "high"},
		WithBaseURL(srv.URL), WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != anthropic.Name {
		t.Fatalf("Name() = %q, want %q", p.Name(), anthropic.Name)
	}

	events, err := p.Stream(context.Background(), provider.Request{
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "ping"}},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for event := range events {
		if event.Type == provider.EventError {
			t.Fatalf("stream failed: %v", event.Err)
		}
	}

	// WithBaseURL sent the request to the fake server, and the key the
	// registry resolved is the one that travelled.
	if got := seen.Get("x-api-key"); got != secret {
		t.Errorf("x-api-key = %q, want the key resolved from the keychain", got)
	}
}

func TestNewReportsAMissingKey(t *testing.T) {
	keyring.MockInit()
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, err := New(context.Background(), config.ModelRoute{Provider: "anthropic", Model: "claude-opus-5"})
	var missing *config.MissingKeyError
	if !errors.As(err, &missing) {
		t.Fatalf("err = %T (%v), want *config.MissingKeyError", err, err)
	}
	// The message has to name both places Apex looked, since that is the fix.
	for _, want := range []string{"apex:anthropic", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

func TestNewRefusesAnUnknownProviderBeforeTouchingTheKeychain(t *testing.T) {
	_, err := New(context.Background(), config.ModelRoute{Provider: "cohere", Model: "x"})
	var unknown *UnknownProviderError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %T (%v), want *UnknownProviderError", err, err)
	}
}
