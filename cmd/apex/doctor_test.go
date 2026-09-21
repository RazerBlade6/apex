package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/provider/registry"
)

// probeKey is what the fake providers authenticate with. It is distinctive so
// a test can assert it never reaches the report.
const probeKey = "sk-ant-probe-test-NEVER-LOG-THIS"

// noRetries matches what the real probe configures: one attempt, so the
// provider's own answer is what gets reported.
var noRetries = 0

// anthropicStreamOK is the smallest well-formed Messages API stream: enough
// for the probe to conclude the key works.
func anthropicStreamOK() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		start, _ := json.Marshal(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_probe", "type": "message", "role": "assistant",
				"model": "claude-opus-5", "content": []any{},
				"usage": map[string]any{
					"input_tokens": 9, "output_tokens": 1,
					"cache_read_input_tokens": 0, "cache_creation_input_tokens": 0,
				},
			},
		})
		fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", start)
		fmt.Fprint(w, "event: content_block_start\ndata: "+
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: "+
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`+"\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: "+
			`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":4}}`+"\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
}

// jsonStatus answers every request with one status and body.
func jsonStatus(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// probeConfig writes a default config into a temp directory, so cfg.Path()
// names a real file the fix text can point at. Nothing under the user's own
// ~/.apex is read or written.
func probeConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(context.Background(), filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	return cfg
}

func findCheck(t *testing.T, r *report, name string) check {
	t.Helper()
	for _, c := range r.checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, r.checks)
	return check{}
}

// TestProbeDistinguishesFailureModes is the point of --probe: the user runs it
// the moment they add a key, so "it did not work" is not an answer. Every case
// here is a different fix.
func TestProbeDistinguishesFailureModes(t *testing.T) {
	tests := []struct {
		name string
		// handler answers the probe request. nil means the server is closed
		// before the probe runs, so nothing is listening.
		handler http.HandlerFunc
		// newProvider overrides construction entirely, for the cases that
		// fail before any request is made.
		newProvider func(ctx context.Context, route config.ModelRoute) (provider.Provider, error)
		wantLevel   level
		wantDetail  []string
		wantFix     []string
	}{
		{
			name:       "a working key",
			handler:    anthropicStreamOK(),
			wantLevel:  levelOK,
			wantDetail: []string{"claude-opus-5", "answered"},
		},
		{
			name: "no key",
			newProvider: func(ctx context.Context, route config.ModelRoute) (provider.Provider, error) {
				service, envVar, err := config.CredentialLocation(route.Provider)
				if err != nil {
					return nil, err
				}
				return nil, &config.MissingKeyError{
					Provider: route.Provider, Service: service, EnvVar: envVar,
				}
			},
			wantLevel:  levelFail,
			wantDetail: []string{"no key to probe with", "apex:anthropic", "ANTHROPIC_API_KEY"},
			wantFix:    []string{"security add-generic-password", "export ANTHROPIC_API_KEY"},
		},
		{
			name: "key rejected",
			handler: jsonStatus(http.StatusUnauthorized,
				`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`),
			wantLevel:  levelFail,
			wantDetail: []string{"key rejected", "HTTP 401", "authentication_error"},
			wantFix:    []string{"not accepted", "security add-generic-password"},
		},
		{
			name: "rate limited",
			handler: jsonStatus(http.StatusTooManyRequests,
				`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`),
			// The key is valid; the account is busy. Failing the whole report
			// for that would be wrong.
			wantLevel:  levelWarn,
			wantDetail: []string{"rate limited", "HTTP 429", "the key is valid"},
			wantFix:    []string{"does not retry"},
		},
		{
			name:       "network unreachable",
			handler:    nil,
			wantLevel:  levelFail,
			wantDetail: []string{"network unreachable", "no HTTP response"},
			wantFix:    []string{"connectivity", "never sent anywhere"},
		},
		{
			name: "a model id that does not exist",
			handler: jsonStatus(http.StatusNotFound,
				`{"type":"error","error":{"type":"not_found_error","message":"model: claude-opus-5"}}`),
			wantLevel:  levelFail,
			wantDetail: []string{"request rejected", "HTTP 404"},
			wantFix:    []string{"models.*.model", "claude-opus-5"},
		},
		{
			name: "a provider outage",
			handler: jsonStatus(http.StatusInternalServerError,
				`{"type":"error","error":{"type":"api_error","message":"internal"}}`),
			wantLevel:  levelWarn,
			wantDetail: []string{"provider error", "HTTP 500"},
			wantFix:    []string{"vendor's failure"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := probeConfig(t)
			opts := doctorOptions{probe: true, newProvider: tt.newProvider}

			if opts.newProvider == nil {
				handler := tt.handler
				if handler == nil {
					handler = func(w http.ResponseWriter, r *http.Request) {}
				}
				srv := httptest.NewServer(handler)
				if tt.handler == nil {
					// Nothing is listening on this address any more, which is
					// what an unreachable provider looks like from here.
					srv.Close()
				} else {
					t.Cleanup(srv.Close)
				}
				opts.newProvider = func(ctx context.Context, route config.ModelRoute) (provider.Provider, error) {
					return registry.Build(route.Provider, provider.Config{
						APIKey:  probeKey,
						BaseURL: srv.URL,
						Model:   route.Model,
						Effort:  route.Effort,
						// No retries, so a 429 is reported rather than
						// waited out — exactly as the real probe does.
						MaxRetries: &noRetries,
					})
				}
			}

			r := &report{}
			checkProbe(context.Background(), r, cfg, opts)

			got := findCheck(t, r, "anthropic probe")
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
			if tt.wantLevel == levelFail && !r.failed() {
				t.Error("the report did not fail on a failing probe")
			}
			if tt.wantLevel == levelWarn && r.failed() {
				t.Error("a warning was reported as a failure")
			}
			// Whatever happened, the credential does not appear in the report.
			for _, c := range r.checks {
				if strings.Contains(c.Detail, probeKey) || strings.Contains(c.Fix, probeKey) {
					t.Fatalf("the API key leaked into the %q check", c.Name)
				}
			}
		})
	}
}

// TestProbeSendsTheCheapestUsefulRequest guards the cost of a command the user
// is expected to run casually.
func TestProbeSendsTheCheapestUsefulRequest(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&raw)
		body = raw
		anthropicStreamOK()(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := probeConfig(t)
	// The configured route asks for the expensive setting; the probe must not
	// honour it.
	cfg.Models.Advisor.Effort = "max"

	r := &report{}
	checkProbe(context.Background(), r, cfg, doctorOptions{
		probe: true,
		newProvider: func(ctx context.Context, route config.ModelRoute) (provider.Provider, error) {
			return registry.Build(route.Provider, provider.Config{
				APIKey: probeKey, BaseURL: srv.URL, Model: route.Model,
				Effort: route.Effort, MaxRetries: &noRetries,
			})
		},
	})

	if got := findCheck(t, r, "anthropic probe"); got.Level != levelOK {
		t.Fatalf("probe = %v (%s), want ok", got.Level, got.Detail)
	}
	if body["max_tokens"] != float64(probeMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", body["max_tokens"], probeMaxTokens)
	}
	if cfgBlock, ok := body["output_config"].(map[string]any); ok {
		if cfgBlock["effort"] != "low" {
			t.Errorf("output_config.effort = %v, want low", cfgBlock["effort"])
		}
	} else {
		t.Errorf("output_config = %v, want an object carrying the low effort", body["output_config"])
	}
}

func TestProbeRoutesOnePerProvider(t *testing.T) {
	tests := []struct {
		name   string
		models config.Models
		want   []string
	}{
		{
			name: "three slots on one vendor probe once",
			models: config.Models{
				Advisor: config.ModelRoute{Provider: "anthropic", Model: "claude-opus-5"},
				Chat:    config.ModelRoute{Provider: "anthropic", Model: "claude-opus-5"},
				Digest:  config.ModelRoute{Provider: "anthropic", Model: "claude-haiku-4-5"},
			},
			want: []string{"anthropic"},
		},
		{
			name: "two vendors probe twice, advisor first",
			models: config.Models{
				Advisor: config.ModelRoute{Provider: "anthropic", Model: "claude-opus-5"},
				Chat:    config.ModelRoute{Provider: "openai", Model: "gpt-5"},
				Digest:  config.ModelRoute{Provider: "anthropic", Model: "claude-haiku-4-5"},
			},
			want: []string{"anthropic", "openai"},
		},
		{
			name: "the advisor model wins for its vendor",
			models: config.Models{
				Advisor: config.ModelRoute{Provider: "anthropic", Model: "claude-opus-5"},
				Chat:    config.ModelRoute{Provider: "anthropic", Model: "claude-sonnet-5"},
				Digest:  config.ModelRoute{Provider: "anthropic", Model: "claude-haiku-4-5"},
			},
			want: []string{"anthropic"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Models = tt.models
			routes := probeRoutes(&cfg)
			if len(routes) != len(tt.want) {
				t.Fatalf("routes = %+v, want %d", routes, len(tt.want))
			}
			for i, want := range tt.want {
				if routes[i].Provider != want {
					t.Errorf("routes[%d].Provider = %q, want %q", i, routes[i].Provider, want)
				}
			}
			if routes[0].Model != tt.models.Advisor.Model {
				t.Errorf("routes[0].Model = %q, want the advisor's %q",
					routes[0].Model, tt.models.Advisor.Model)
			}
		})
	}
}

// TestDoctorWithoutProbeCallsNoAPI is the other half of the contract: without
// the flag, doctor behaves exactly as it did before M3 and reaches no vendor.
func TestDoctorWithoutProbeCallsNoAPI(t *testing.T) {
	newFixture(t)
	keyring.MockInit() // never the real macOS keychain

	called := 0
	opts := doctorOptions{
		probe: false,
		newProvider: func(ctx context.Context, route config.ModelRoute) (provider.Provider, error) {
			called++
			t.Error("doctor constructed a provider without --probe")
			return nil, fmt.Errorf("must not be called")
		},
	}

	r := runDoctor(context.Background(), opts)
	if called != 0 {
		t.Errorf("newProvider was called %d times, want 0", called)
	}
	if len(r.checks) == 0 {
		t.Fatal("doctor reported nothing at all")
	}
	for _, c := range r.checks {
		if strings.Contains(c.Name, "probe") {
			t.Errorf("a probe check ran without the flag: %+v", c)
		}
	}
	// The checks that do not depend on the environment must still be there.
	findCheck(t, r, "config")
	findCheck(t, r, "migrations")
}

// TestDoctorProbeFlagIsOffByDefault guards the default: probing costs money.
func TestDoctorProbeFlagIsOffByDefault(t *testing.T) {
	cmd := newDoctorCmd()
	flag := cmd.Flags().Lookup("probe")
	if flag == nil {
		t.Fatal("doctor has no --probe flag")
	}
	if flag.DefValue != "false" {
		t.Errorf("--probe defaults to %q, want false", flag.DefValue)
	}
}
