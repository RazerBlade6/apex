package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/provider/claudecli"
	"github.com/RazerBlade6/apex/internal/provider/registry"
)

// fakeClaudeBin writes a script standing in for the claude binary. Nothing in
// this file runs the real CLI: the whole point of M3.5's tests is that they
// cost no subscription quota.
func fakeClaudeBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

// claudeCLIConfig routes the advisor slot at the subscription provider and
// leaves the rest on API keys, which is the split DESIGN.md §8 recommends.
func claudeCLIConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := probeConfig(t)
	cfg.Models.Advisor = config.ModelRoute{Provider: claudecli.Name, Model: "opus", Effort: "high"}
	return cfg
}

func TestClaudeAuthCheckDistinguishesItsStates(t *testing.T) {
	const loggedIn = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty",` +
		`"email":"user@example.com","subscriptionType":"pro"}`

	tests := []struct {
		name       string
		body       string
		missing    bool // point doctor at a path with no binary
		wantLevel  level
		wantDetail []string
		wantFix    []string
	}{
		{
			name:       "logged in on a subscription",
			body:       "printf '%s\\n' '" + loggedIn + "'",
			wantLevel:  levelOK,
			wantDetail: []string{"logged in", "user@example.com", "pro"},
		},
		{
			name:       "installed but logged out",
			body:       `printf '%s\n' '{"loggedIn":false}'` + "\nexit 1",
			wantLevel:  levelFail,
			wantDetail: []string{"not logged in"},
			wantFix:    []string{"claude auth login"},
		},
		{
			name:       "not installed",
			missing:    true,
			wantLevel:  levelFail,
			wantDetail: []string{"not found"},
			wantFix:    []string{"install the Claude Code CLI"},
		},
		{
			name:       "logged in with an API key rather than the subscription",
			body:       `printf '%s\n' '{"loggedIn":true,"authMethod":"apiKey"}'`,
			wantLevel:  levelWarn,
			wantDetail: []string{"not a claude.ai subscription"},
			wantFix:    []string{"claude auth login"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "absent")
			if !tt.missing {
				bin = fakeClaudeBin(t, tt.body)
			}

			r := &report{}
			checkClaudeAuth(context.Background(), r, claudeCLIConfig(t), doctorOptions{claudeBin: bin})

			got := findCheck(t, r, "claude auth")
			if got.Level != tt.wantLevel {
				t.Errorf("level = %v, want %v (detail: %s)", got.Level, tt.wantLevel, got.Detail)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail = %q, want it to mention %q", got.Detail, want)
				}
			}
			for _, want := range tt.wantFix {
				if !strings.Contains(got.Fix, want) {
					t.Errorf("fix = %q, want it to mention %q", got.Fix, want)
				}
			}
		})
	}
}

// TestClaudeAuthCheckIsSkippedWithoutAClaudeCLIRoute keeps doctor from
// telling a key-only user to log into a CLI they do not use for inference.
func TestClaudeAuthCheckIsSkippedWithoutAClaudeCLIRoute(t *testing.T) {
	r := &report{}
	checkClaudeAuth(context.Background(), r, probeConfig(t), doctorOptions{
		claudeBin: filepath.Join(t.TempDir(), "absent"),
	})
	for _, c := range r.checks {
		if c.Name == "claude auth" {
			t.Fatalf("doctor checked the CLI login with no claude-cli route: %+v", c)
		}
	}
}

// TestKeyChecksNeverMentionClaudeCLI is DESIGN.md §13's guarantee, checked at
// the surface the user actually reads.
func TestKeyChecksNeverMentionClaudeCLI(t *testing.T) {
	keyring.MockInit()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	r := &report{}
	checkKeys(context.Background(), r, claudeCLIConfig(t))

	for _, c := range r.checks {
		if strings.Contains(c.Name, claudecli.Name) {
			t.Errorf("the key table has a row for %q: %+v", claudecli.Name, c)
		}
		if strings.Contains(c.Detail, claudecli.Name) || strings.Contains(c.Fix, claudecli.Name) {
			t.Errorf("a key check mentioned %q: %+v", claudecli.Name, c)
		}
	}
}

// TestProbeDistinguishesSubscriptionFailures is --probe's half of the
// milestone: four outcomes, four fixes, and a session limit that is never
// dressed up as an API rate limit.
func TestProbeDistinguishesSubscriptionFailures(t *testing.T) {
	const (
		initLine = `{"type":"system","subtype":"init","session_id":"S","model":"opus","apiKeySource":"none"}`
		okResult = `{"type":"result","subtype":"success","is_error":false,"result":"pong",` +
			`"usage":{"input_tokens":3,"output_tokens":1},"api_error_status":null}`
		limitEvent = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected",` +
			`"resetsAt":1789996200,"rateLimitType":"five_hour"}}`
		limitResult = `{"type":"result","subtype":"error","is_error":true,` +
			`"result":"Claude AI usage limit reached","api_error_status":null}`
	)
	emitLine := func(lines ...string) string {
		var b strings.Builder
		for _, l := range lines {
			b.WriteString("printf '%s\\n' '" + l + "'\n")
		}
		return b.String()
	}

	tests := []struct {
		name       string
		body       string
		missing    bool
		wantLevel  level
		wantDetail []string
		wantFix    []string
	}{
		{
			name:       "the subscription answers",
			body:       emitLine(initLine, `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}}`, okResult),
			wantLevel:  levelOK,
			wantDetail: []string{"answered"},
		},
		{
			name:       "not installed",
			missing:    true,
			wantLevel:  levelFail,
			wantDetail: []string{"nothing to probe with", "not found"},
			wantFix:    []string{"install the Claude Code CLI"},
		},
		{
			name:       "logged out",
			body:       "printf 'Error: not logged in\\n' >&2\nexit 1",
			wantLevel:  levelFail,
			wantDetail: []string{"not logged in"},
			wantFix:    []string{"claude auth login"},
		},
		{
			name:       "session limit reached",
			body:       emitLine(initLine, limitEvent, limitResult) + "exit 1",
			wantLevel:  levelFail,
			wantDetail: []string{"session limit reached", "five_hour"},
			wantFix: []string{
				"NOT an API rate limit",
				"your own Claude Code sessions",
				`provider = "anthropic"`,
			},
		},
		{
			name:       "some other failure",
			body:       "printf 'segfault\\n' >&2\nexit 139",
			wantLevel:  levelFail,
			wantDetail: []string{"segfault"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "absent")
			if !tt.missing {
				bin = fakeClaudeBin(t, tt.body)
			}
			cfg := claudeCLIConfig(t)

			r := &report{}
			checkProbe(context.Background(), r, cfg, doctorOptions{
				probe: true,
				newProvider: func(ctx context.Context, route config.ModelRoute) (provider.Provider, error) {
					return registry.Build(route.Provider, provider.Config{
						Binary: bin, Model: route.Model, Effort: route.Effort,
					})
				},
			})

			got := findCheck(t, r, claudecli.Name+" probe")
			if got.Level != tt.wantLevel {
				t.Errorf("level = %v, want %v (detail: %s)", got.Level, tt.wantLevel, got.Detail)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail = %q, want it to mention %q", got.Detail, want)
				}
			}
			for _, want := range tt.wantFix {
				if !strings.Contains(got.Fix, want) {
					t.Errorf("fix = %q, want it to mention %q", got.Fix, want)
				}
			}
			// The one thing a session limit must never be called.
			if strings.Contains(got.Detail, "rate limited") {
				t.Error("a subscription failure was reported as an API rate limit")
			}
			// And no advice about API keys for a route that has none.
			if strings.Contains(got.Fix, "security add-generic-password") {
				t.Errorf("fix = %q, want no keychain advice for a subscription route", got.Fix)
			}
		})
	}
}
