// Package config loads Apex configuration from ~/.apex/config.toml and
// resolves provider API keys from the system keychain or the environment.
//
// API keys are never read from, or written to, the config file. See keys.go.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Default file and directory permissions for ~/.apex. State is single-user, so
// nothing below the root is group- or world-readable.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// ModelRoute selects the provider and model used for one task type.
type ModelRoute struct {
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	Effort   string `toml:"effort"`
}

// Models is the per-task-type model routing table (DESIGN.md §8).
type Models struct {
	Advisor ModelRoute `toml:"advisor"`
	Chat    ModelRoute `toml:"chat"`
	Digest  ModelRoute `toml:"digest"`
}

// Store selects the persistence backend (DESIGN.md §7).
type Store struct {
	Backend      string `toml:"backend"`       // local | turso
	Database     string `toml:"database"`      // Turso database name; auth token in Keychain
	SyncInterval string `toml:"sync_interval"` // Go duration, e.g. "5m"
}

// Sync is the digest refresh policy (DESIGN.md §13).
type Sync struct {
	Mode     string `toml:"mode"`     // explicit | on_start | scheduled
	Schedule string `toml:"schedule"` // daily | weekly; read only when mode = scheduled

	// DigestWorkers bounds how many digests `apex sync` generates at once.
	//
	// This is an addition to §13, and it exists because of §8: a claude-cli
	// route is a subprocess per call, so one goroutine per project would
	// start one `claude` per project. The cost is not only CPU — the
	// subscription window is shared with the user's own Claude Code sessions,
	// and a twelve-project portfolio refreshing at full width is the fastest
	// way to spend it. The default is deliberately small; a user on an API
	// key can raise it.
	DigestWorkers int `toml:"digest_workers"`
}

// Executor selects the builder-loop dispatch target (DESIGN.md §9).
//
// Model and Effort are an addition M5 made for a reason worth stating: the
// builder loop is routed separately from every [models.*] slot, because it is
// not a provider call at all. DESIGN.md §9 is explicit that it runs on the
// Claude Code subscription rather than on an API key, so putting it in the
// model routing table would invite a user to point it at an API key that
// would never be read. §9's verified argv shows `--model opus --effort high`,
// which is what these default to; `apex do --model sonnet` overrides them for
// one run.
type Executor struct {
	Default string `toml:"default"`
	Model   string `toml:"model"`
	Effort  string `toml:"effort"`

	// InheritUserConfig lets a dispatched agent pick up the user's own
	// Claude Code configuration: the global ~/.claude/CLAUDE.md, personal
	// skills, hooks, plugins and settings.
	//
	// It defaults to false, and the default is the decision in DESIGN.md §9:
	// a dispatch inherits the *project* CLAUDE.md and not the user's global
	// one. Both real M5 dispatches volunteered that they were departing from
	// a convention in ~/.claude/CLAUDE.md, so personal conventions were
	// silently shaping every Apex run — and some of them are actively wrong
	// here ("delegate code generation to a sub-agent" is redundant advice for
	// a process that *is* the delegation layer).
	//
	// The field is named for what it turns on rather than off so that its
	// absence from an older config.toml means the safe value. M6 settled the
	// mechanism: see internal/executor/claudecode/memory.go for what a
	// dispatch does and does not inherit either way.
	InheritUserConfig bool `toml:"inherit_user_config"`
}

// Config is the whole of ~/.apex/config.toml.
type Config struct {
	Models   Models   `toml:"models"`
	Store    Store    `toml:"store"`
	Sync     Sync     `toml:"sync"`
	Executor Executor `toml:"executor"`

	// path records where this config was loaded from. Not serialized.
	path string `toml:"-"`
}

// Path returns the file this config was loaded from, or "" if it was
// constructed in memory.
func (c *Config) Path() string { return c.path }

// Valid enumerations, exported so `apex doctor` can report what was expected.
var (
	ValidBackends  = []string{"local", "turso"}
	ValidSyncModes = []string{"explicit", "on_start", "scheduled"}
	ValidSchedules = []string{"daily", "weekly"}
	// ValidProviders includes claude-cli, the subscription-backed provider
	// (DESIGN.md §8). It has no API key: see CredentialKindOf.
	ValidProviders = []string{"anthropic", "openai", "claude-cli"}
	ValidEfforts   = []string{"low", "medium", "high", "xhigh", "max"}
	ValidExecutors = []string{"claudecode"}
)

// Digest worker bounds. The default is conservative on purpose: see
// Sync.DigestWorkers. The ceiling is not a performance judgement but a
// guardrail — beyond it a sync is contending with the machine and, on a
// subscription route, with the user's own session window.
const (
	DefaultDigestWorkers = 2
	MaxDigestWorkers     = 16
)

// Default returns the built-in configuration. Every model slot defaults to
// claude-opus-5 (DESIGN.md §8): downgrading the digest slot is a user decision,
// not a default.
func Default() Config {
	return Config{
		Models: Models{
			Advisor: ModelRoute{Provider: "anthropic", Model: "claude-opus-5", Effort: "high"},
			Chat:    ModelRoute{Provider: "anthropic", Model: "claude-opus-5", Effort: "medium"},
			Digest:  ModelRoute{Provider: "anthropic", Model: "claude-opus-5", Effort: "low"},
		},
		Store: Store{
			Backend:      "local",
			Database:     "",
			SyncInterval: "5m",
		},
		Sync: Sync{
			Mode:          "explicit",
			Schedule:      "daily",
			DigestWorkers: DefaultDigestWorkers,
		},
		Executor: Executor{
			Default: "claudecode",
			Model:   "opus",
			Effort:  "high",
		},
	}
}

// Root returns the Apex state directory, ~/.apex, honouring APEX_HOME when set.
func Root() (string, error) {
	if custom := os.Getenv("APEX_HOME"); custom != "" {
		return filepath.Clean(custom), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".apex"), nil
}

// FilePath returns the path of config.toml inside the Apex state directory.
func FilePath() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "config.toml"), nil
}

// Layout enumerates the directories Apex expects under its state root
// (DESIGN.md §5). Paths are relative to Root.
var Layout = []string{
	"context",
	filepath.Join("logs", "exec"),
	"locks",
}

// EnsureLayout creates the state directory tree if it does not exist and
// reports the directories it had to create.
func EnsureLayout(ctx context.Context) (created []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := Root()
	if err != nil {
		return nil, err
	}
	for _, rel := range append([]string{"."}, Layout...) {
		dir := filepath.Join(root, rel)
		if _, statErr := os.Stat(dir); statErr == nil {
			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return created, fmt.Errorf("stat %s: %w", dir, statErr)
		}
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return created, fmt.Errorf("create %s: %w", dir, err)
		}
		created = append(created, dir)
	}
	return created, nil
}

// DatabasePath returns the path of the local SQLite file.
func DatabasePath() (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "apex.db"), nil
}

// LockPath returns the path of a machine-local lock file (DESIGN.md §10).
func LockPath(name string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "locks", name+".lock"), nil
}

// ExecLogPath returns the path of one dispatch's log file (DESIGN.md §5):
// ~/.apex/logs/exec/<run-id>.log. The run id is Apex's own hex identifier, so
// it never contains a path separator.
func ExecLogPath(runID string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "logs", "exec", runID+".log"), nil
}

// Load reads ~/.apex/config.toml, filling any absent field from Default. If the
// file does not exist it is created with the default configuration, so a first
// run leaves the user with something to edit.
func Load(ctx context.Context) (*Config, error) {
	path, err := FilePath()
	if err != nil {
		return nil, err
	}
	return LoadFrom(ctx, path)
}

// LoadFrom is Load against an explicit path. Used by tests and by any future
// --config flag.
func LoadFrom(ctx context.Context, path string) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cfg := Default()
	cfg.path = path

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := writeFile(path, &cfg); err != nil {
			return nil, err
		}
		return &cfg, nil
	case err != nil:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	// Unmarshalling onto the defaults leaves absent keys at their default
	// value, so an old or partial config file keeps working.
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.path = path
	return &cfg, nil
}

// Save writes the config back to the path it was loaded from.
func (c *Config) Save(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.path == "" {
		return errors.New("config: no path recorded; use SaveTo")
	}
	return writeFile(c.path, c)
}

// SaveTo writes the config to an explicit path.
func (c *Config) SaveTo(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeFile(path, c)
}

func writeFile(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("create config directory for %s: %w", path, err)
	}
	body, err := c.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, body, filePerm); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

const configHeader = `# Apex configuration. See DESIGN.md.
# API keys are NEVER stored here: they live in the macOS Keychain
# (apex:anthropic, apex:openai) or in ANTHROPIC_API_KEY / OPENAI_API_KEY.
#
# A route may instead set provider = "claude-cli" to run through a Claude Pro
# or Max subscription. It needs no key; it needs the claude CLI installed and
# logged in (claude auth login). Its quota is shared with your own Claude Code
# usage, so the high-volume digest slot is usually better on an API key.

`

// Marshal renders the config as the TOML that would be written to disk.
func (c *Config) Marshal() ([]byte, error) {
	body, err := toml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return append([]byte(configHeader), body...), nil
}

// SyncIntervalDuration parses Store.SyncInterval.
func (c *Config) SyncIntervalDuration() (time.Duration, error) {
	d, err := time.ParseDuration(c.Store.SyncInterval)
	if err != nil {
		return 0, fmt.Errorf("store.sync_interval %q: %w", c.Store.SyncInterval, err)
	}
	return d, nil
}

// Providers returns the distinct providers referenced by the model routing
// table, so callers know which API keys actually matter.
func (c *Config) Providers() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range []ModelRoute{c.Models.Advisor, c.Models.Chat, c.Models.Digest} {
		if r.Provider == "" || seen[r.Provider] {
			continue
		}
		seen[r.Provider] = true
		out = append(out, r.Provider)
	}
	return out
}

// Validate reports every problem with the config at once, rather than the first
// one, so `apex doctor` can print a complete list.
func (c *Config) Validate() []error {
	var errs []error

	for name, r := range map[string]ModelRoute{
		"models.advisor": c.Models.Advisor,
		"models.chat":    c.Models.Chat,
		"models.digest":  c.Models.Digest,
	} {
		if err := oneOf(name+".provider", r.Provider, ValidProviders); err != nil {
			errs = append(errs, err)
		}
		if r.Model == "" {
			errs = append(errs, fmt.Errorf("%s.model is empty", name))
		}
		if r.Effort != "" {
			if err := oneOf(name+".effort", r.Effort, ValidEfforts); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if err := oneOf("store.backend", c.Store.Backend, ValidBackends); err != nil {
		errs = append(errs, err)
	}
	if c.Store.Backend == "turso" && c.Store.Database == "" {
		errs = append(errs, errors.New(`store.database is required when store.backend = "turso"`))
	}
	if _, err := c.SyncIntervalDuration(); err != nil {
		errs = append(errs, err)
	}

	if err := oneOf("sync.mode", c.Sync.Mode, ValidSyncModes); err != nil {
		errs = append(errs, err)
	}
	if c.Sync.DigestWorkers < 1 || c.Sync.DigestWorkers > MaxDigestWorkers {
		errs = append(errs, fmt.Errorf("sync.digest_workers = %d is not between 1 and %d",
			c.Sync.DigestWorkers, MaxDigestWorkers))
	}
	if c.Sync.Mode == "scheduled" {
		if err := oneOf("sync.schedule", c.Sync.Schedule, ValidSchedules); err != nil {
			errs = append(errs, err)
		}
	}

	if err := oneOf("executor.default", c.Executor.Default, ValidExecutors); err != nil {
		errs = append(errs, err)
	}
	if c.Executor.Model == "" {
		errs = append(errs, errors.New("executor.model is empty"))
	}
	if c.Executor.Effort != "" {
		if err := oneOf("executor.effort", c.Executor.Effort, ValidEfforts); err != nil {
			errs = append(errs, err)
		}
	}

	// Sort is avoided deliberately: map iteration order above is random, so
	// callers that care about stability should sort the strings themselves.
	return errs
}

func oneOf(field, value string, allowed []string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	if value == "" {
		return fmt.Errorf("%s is empty (expected one of %v)", field, allowed)
	}
	return fmt.Errorf("%s = %q is not one of %v", field, value, allowed)
}
