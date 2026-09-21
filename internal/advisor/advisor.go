// Package advisor is the advisor loop (DESIGN.md §2): it reads context,
// reasons across the whole portfolio, and emits text — digests, action items,
// project ideas.
//
// It owns no transport and no storage of its own. It composes what M2 and M3
// built: contextfs and the digests table supply the context, internal/provider
// supplies the model, and internal/store takes the results. Everything below
// is about the composition — which context goes where in the prompt, what is
// cached, what is deduplicated, and what is recorded as provenance.
//
// The builder loop is deliberately absent. Nothing here writes to a project
// directory or runs a command; that is M5's Executor.
package advisor

import (
	"context"
	"fmt"
	"time"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// ProviderFunc constructs the provider for one model route.
//
// It is a field rather than a direct call to provider/registry so that every
// test in this package runs offline against a fake. The real one is
// registry.New, wired in by cmd/apex.
type ProviderFunc func(ctx context.Context, route config.ModelRoute) (provider.Provider, error)

// Advisor composes context and a provider.
//
// One Advisor serves a whole command. Providers are built per route and
// memoised for the call that needs them, because a claude-cli provider
// resolves its binary in New and a bounded digest pool must not repeat that
// per project (DESIGN.md §8).
type Advisor struct {
	Store  *store.Store
	Config *config.Config
	// Root is the Apex state directory, ~/.apex: where the identity
	// documents live.
	Root string
	// NewProvider builds a provider for a route.
	NewProvider ProviderFunc
	// Now is the clock. Tests pin it; production leaves it nil for
	// time.Now.
	Now func() time.Time
}

// New builds an Advisor. Nothing here talks to a model or opens a file: the
// first call that needs a provider builds one.
func New(st *store.Store, cfg *config.Config, root string, newProvider ProviderFunc) *Advisor {
	return &Advisor{Store: st, Config: cfg, Root: root, NewProvider: newProvider}
}

func (a *Advisor) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// providerFor builds the provider for one route.
func (a *Advisor) providerFor(ctx context.Context, route config.ModelRoute) (provider.Provider, error) {
	if a.NewProvider == nil {
		return nil, fmt.Errorf("advisor: no provider factory was configured")
	}
	p, err := a.NewProvider(ctx, route)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("advisor: provider factory returned nothing for %s", route.Provider)
	}
	return p, nil
}

// maxTokens for the two call shapes. A digest is specified at 200–300 tokens
// (DESIGN.md §6) and the ceiling is a guardrail rather than a target; a
// structured list of action items is longer but still bounded, and the
// generous default in provider.DefaultMaxTokens is meant for chat.
const (
	digestMaxTokens = 1200
	listMaxTokens   = 8000
)
