package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withAPEXHome points the package at a throwaway state directory.
func withAPEXHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APEX_HOME", dir)
	return dir
}

func TestDefaultModelRouting(t *testing.T) {
	// DESIGN.md §8: all three slots default to claude-opus-5. Downgrading the
	// digest slot is a user decision, so a changed default is a bug.
	cfg := Default()
	tests := []struct {
		slot   string
		route  ModelRoute
		effort string
	}{
		{"advisor", cfg.Models.Advisor, "high"},
		{"chat", cfg.Models.Chat, "medium"},
		{"digest", cfg.Models.Digest, "low"},
	}
	for _, tt := range tests {
		t.Run(tt.slot, func(t *testing.T) {
			if tt.route.Provider != "anthropic" {
				t.Errorf("provider = %q, want anthropic", tt.route.Provider)
			}
			if tt.route.Model != "claude-opus-5" {
				t.Errorf("model = %q, want claude-opus-5", tt.route.Model)
			}
			if tt.route.Effort != tt.effort {
				t.Errorf("effort = %q, want %q", tt.route.Effort, tt.effort)
			}
		})
	}

	if cfg.Store.Backend != "local" {
		t.Errorf("store.backend = %q, want local", cfg.Store.Backend)
	}
	if cfg.Sync.Mode != "explicit" {
		t.Errorf("sync.mode = %q, want explicit", cfg.Sync.Mode)
	}
	if cfg.Executor.Default != "claudecode" {
		t.Errorf("executor.default = %q, want claudecode", cfg.Executor.Default)
	}
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Errorf("default config does not validate: %v", errs)
	}
}

func TestLoadWritesDefaultsWhenAbsent(t *testing.T) {
	dir := withAPEXHome(t)
	ctx := context.Background()

	cfg, err := Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	path := filepath.Join(dir, "config.toml")
	if cfg.Path() != path {
		t.Errorf("Path() = %q, want %q", cfg.Path(), path)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("default config was not written: %v", err)
	}
	if !strings.Contains(string(body), "claude-opus-5") {
		t.Errorf("written config lacks model defaults:\n%s", body)
	}
	// A key must never reach disk, not even as an empty placeholder field.
	// Comment lines are exempt: the header names the environment variables
	// deliberately, as instructions to the user.
	var settings []string
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			settings = append(settings, line)
		}
	}
	joined := strings.Join(settings, "\n")
	for _, forbidden := range []string{"api_key", "API_KEY", "token", "secret", "sk-"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("written config defines %q:\n%s", forbidden, body)
		}
	}

	// Loading again must not clobber the file or change the result.
	again, err := Load(ctx)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if *again != *cfg {
		t.Errorf("second Load differs:\n got %+v\nwant %+v", *again, *cfg)
	}
}

func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	dir := withAPEXHome(t)
	path := filepath.Join(dir, "config.toml")

	// A config written by an older binary knows nothing of the other keys.
	partial := "[models.digest]\nmodel = \"claude-haiku-4-5\"\neffort = \"low\"\n"
	if err := os.WriteFile(path, []byte(partial), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Models.Digest.Model != "claude-haiku-4-5" {
		t.Errorf("digest.model = %q, want the file's value", cfg.Models.Digest.Model)
	}
	if cfg.Models.Digest.Provider != "anthropic" {
		t.Errorf("digest.provider = %q, want the default to fill in", cfg.Models.Digest.Provider)
	}
	if cfg.Models.Advisor.Model != "claude-opus-5" {
		t.Errorf("advisor.model = %q, want the default", cfg.Models.Advisor.Model)
	}
	if cfg.Store.SyncInterval != "5m" {
		t.Errorf("store.sync_interval = %q, want the default", cfg.Store.SyncInterval)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withAPEXHome(t)
	ctx := context.Background()

	original := Default()
	original.Models.Digest = ModelRoute{Provider: "openai", Model: "gpt-5", Effort: "low"}
	original.Store = Store{Backend: "turso", Database: "apex-vedant", SyncInterval: "90s"}
	original.Sync = Sync{Mode: "scheduled", Schedule: "weekly"}

	path, err := FilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := original.SaveTo(ctx, path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	loaded, err := Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	original.path = path
	if *loaded != original {
		t.Errorf("round trip changed the config:\n got %+v\nwant %+v", *loaded, original)
	}
	if d, err := loaded.SyncIntervalDuration(); err != nil || d.String() != "1m30s" {
		t.Errorf("SyncIntervalDuration() = %v, %v; want 1m30s", d, err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // substring; "" means valid
	}{
		{"defaults", func(*Config) {}, ""},
		{"bad provider", func(c *Config) { c.Models.Advisor.Provider = "mistral" }, "models.advisor.provider"},
		{"empty model", func(c *Config) { c.Models.Chat.Model = "" }, "models.chat.model"},
		{"bad effort", func(c *Config) { c.Models.Digest.Effort = "turbo" }, "models.digest.effort"},
		{"bad backend", func(c *Config) { c.Store.Backend = "postgres" }, "store.backend"},
		{"turso without database", func(c *Config) { c.Store.Backend = "turso" }, "store.database"},
		{"bad interval", func(c *Config) { c.Store.SyncInterval = "soon" }, "store.sync_interval"},
		{"bad sync mode", func(c *Config) { c.Sync.Mode = "always" }, "sync.mode"},
		{"scheduled with bad schedule", func(c *Config) {
			c.Sync.Mode = "scheduled"
			c.Sync.Schedule = "hourly"
		}, "sync.schedule"},
		{"unscheduled ignores schedule", func(c *Config) { c.Sync.Schedule = "hourly" }, ""},
		{"bad executor", func(c *Config) { c.Executor.Default = "aider" }, "executor.default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			errs := cfg.Validate()

			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("Validate() = %v, want no errors", errs)
				}
				return
			}
			var joined []string
			for _, e := range errs {
				joined = append(joined, e.Error())
			}
			if !strings.Contains(strings.Join(joined, "\n"), tt.wantErr) {
				t.Fatalf("Validate() = %v, want an error mentioning %q", errs, tt.wantErr)
			}
		})
	}
}

func TestProviders(t *testing.T) {
	cfg := Default()
	if got := cfg.Providers(); len(got) != 1 || got[0] != "anthropic" {
		t.Errorf("Providers() = %v, want [anthropic]", got)
	}
	cfg.Models.Digest.Provider = "openai"
	got := cfg.Providers()
	if len(got) != 2 || got[0] != "anthropic" || got[1] != "openai" {
		t.Errorf("Providers() = %v, want [anthropic openai]", got)
	}
}

func TestEnsureLayout(t *testing.T) {
	dir := withAPEXHome(t)
	ctx := context.Background()

	created, err := EnsureLayout(ctx)
	if err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	if len(created) == 0 {
		t.Error("EnsureLayout reported nothing created on a fresh directory")
	}
	for _, rel := range Layout {
		info, err := os.Stat(filepath.Join(dir, rel))
		if err != nil || !info.IsDir() {
			t.Errorf("%s was not created: %v", rel, err)
		}
	}

	// Second call is a no-op.
	created, err = EnsureLayout(ctx)
	if err != nil {
		t.Fatalf("second EnsureLayout: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("second EnsureLayout created %v, want nothing", created)
	}
}

func TestCancelledContext(t *testing.T) {
	withAPEXHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Load(ctx); err == nil {
		t.Error("Load with a cancelled context returned no error")
	}
	if _, err := EnsureLayout(ctx); err == nil {
		t.Error("EnsureLayout with a cancelled context returned no error")
	}
}
