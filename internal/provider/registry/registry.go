// Package registry turns a config [models.*] entry into a constructed
// provider (DESIGN.md §8).
//
// It exists as its own package rather than as a file in internal/provider
// because the adapters import provider, so a factory inside provider that
// imported the adapters would be an import cycle. The alternative — adapters
// registering themselves from init() — trades that cycle for a blank import
// nobody remembers, and turns a forgotten wire-up into a runtime "unknown
// provider" instead of a compile error. This package is the explicit version:
// if an adapter is not wired here, the build says so.
package registry

import (
	"context"
	"fmt"
	"sort"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/provider/anthropic"
	"github.com/RazerBlade6/apex/internal/provider/claudecli"
	"github.com/RazerBlade6/apex/internal/provider/openai"
)

// constructors is every vendor Apex can talk to, keyed by the name used in
// config.toml. It is the single place a new adapter is wired in.
var constructors = map[string]func(provider.Config) (provider.Provider, error){
	anthropic.Name: func(cfg provider.Config) (provider.Provider, error) { return anthropic.New(cfg) },
	openai.Name:    func(cfg provider.Config) (provider.Provider, error) { return openai.New(cfg) },
	claudecli.Name: func(cfg provider.Config) (provider.Provider, error) { return claudecli.New(cfg) },
}

// Names lists the providers this build supports, sorted.
func Names() []string {
	out := make([]string, 0, len(constructors))
	for name := range constructors {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// UnknownProviderError reports a config entry naming a vendor this build has
// no adapter for. It is user-fixable: the fix is to edit config.toml.
type UnknownProviderError struct {
	Provider string
	Known    []string
}

func (e *UnknownProviderError) Error() string {
	return fmt.Sprintf("provider %q is not implemented in this build (known: %v)", e.Provider, e.Known)
}

// UserFixable marks a bad provider name as a config edit, not a bug.
func (e *UnknownProviderError) UserFixable() bool { return true }

// Option tunes construction.
type Option func(*provider.Config)

// WithBaseURL overrides the vendor endpoint. Tests point this at an httptest
// server; nothing in Apex sets it in production.
func WithBaseURL(url string) Option {
	return func(c *provider.Config) { c.BaseURL = url }
}

// WithMaxRetries overrides the SDK retry count. `apex doctor --probe` sets it
// to zero, so a 429 is reported as a rate limit rather than waited out behind
// the SDK's default backoff.
func WithMaxRetries(n int) Option {
	return func(c *provider.Config) { c.MaxRetries = &n }
}

// WithBinary overrides the executable a subprocess-backed provider runs. It
// is WithBaseURL's counterpart for claude-cli: tests point it at a fake, and
// a user whose claude lives off PATH can point it at the real one. The
// HTTP-backed adapters ignore it.
func WithBinary(path string) Option {
	return func(c *provider.Config) { c.Binary = path }
}

// Build constructs a provider from an explicit configuration. It resolves no
// credentials: the caller supplies the key.
func Build(name string, cfg provider.Config) (provider.Provider, error) {
	newProvider, ok := constructors[name]
	if !ok {
		return nil, &UnknownProviderError{Provider: name, Known: Names()}
	}
	p, err := newProvider(cfg)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// New constructs the provider for one model route, resolving its API key from
// the keychain or the environment (DESIGN.md §13).
//
// This is what M4 calls to ask for "the advisor provider" without knowing
// which vendor sits behind it:
//
//	p, err := registry.New(ctx, cfg.Models.Advisor)
//
// A missing key comes back as *config.MissingKeyError, whose message already
// names both places Apex looked.
//
// Not every route has a key. DESIGN.md §13 was generalised in M3.5: Apex
// resolves credentials, and claude-cli's credential is one the CLI already
// holds. Resolution is therefore skipped for it rather than attempted and
// forgiven — attempting it would produce a "missing API key" error for a
// route that will never read a key, which is the one message §13 says must
// never appear.
func New(ctx context.Context, route config.ModelRoute, opts ...Option) (provider.Provider, error) {
	if _, ok := constructors[route.Provider]; !ok {
		return nil, &UnknownProviderError{Provider: route.Provider, Known: Names()}
	}
	cfg := provider.Config{
		Model:  route.Model,
		Effort: route.Effort,
	}
	if config.UsesAPIKey(route.Provider) {
		key, err := config.ResolveKey(ctx, route.Provider)
		if err != nil {
			return nil, err
		}
		cfg.APIKey = key.Value()
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return Build(route.Provider, cfg)
}
