package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/provider/registry"
	"github.com/RazerBlade6/apex/internal/store"
)

// session is the plumbing every command past `doctor` needs: the state root,
// the config, and an open store.
//
// It exists because four commands arrived at once in M4 and each would
// otherwise repeat the same twenty lines, with four chances to forget the
// backend check or the migration policy.
type session struct {
	Root    string
	Config  *config.Config
	Store   *store.Store
	backend *store.LocalBackend
}

// Close releases the database handle.
func (s *session) Close() {
	if s.backend != nil {
		s.backend.Close() //nolint:errcheck // nothing useful to do on the way out
	}
}

// openSession prepares the state directory, loads config, and opens the store.
//
// migrate says whether this command is allowed to bring the schema forward.
// Only `apex sync` passes true. Everything else — including doctor, which
// used to migrate silently (DESIGN.md §18) — reports a pending migration and
// stops, because a forward-only, un-rollback-able schema change should happen
// when the user ran the command that owns it, not as a side effect of asking a
// question.
func openSession(ctx context.Context, migrate bool) (*session, error) {
	root, err := config.Root()
	if err != nil {
		return nil, err
	}
	if _, err := config.EnsureLayout(ctx); err != nil {
		return nil, err
	}

	cfg, err := config.Load(ctx)
	if err != nil {
		return nil, err
	}
	if problems := cfg.Validate(); len(problems) > 0 {
		return nil, &invalidConfigError{Path: cfg.Path(), Problems: problems}
	}
	if cfg.Store.Backend != "local" {
		return nil, &unsupportedBackendError{Backend: cfg.Store.Backend, ConfigPath: cfg.Path()}
	}

	dbPath, err := config.DatabasePath()
	if err != nil {
		return nil, err
	}
	backend := store.NewLocalBackend(dbPath)
	st, err := store.Open(ctx, backend)
	if err != nil {
		backend.Close() //nolint:errcheck
		return nil, err
	}
	s := &session{Root: root, Config: cfg, Store: st, backend: backend}

	if migrate {
		if err := st.Migrate(ctx); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}

	status, err := st.Status(ctx)
	if err != nil {
		s.Close()
		return nil, err
	}
	if len(status.Pending) > 0 {
		s.Close()
		names := make([]string, 0, len(status.Pending))
		for _, m := range status.Pending {
			names = append(names, fmt.Sprintf("%03d_%s", m.Version, m.Name))
		}
		return nil, &pendingMigrationsError{Names: names}
	}
	return s, nil
}

// providerFactory builds the provider for one model route.
//
// It is a variable rather than a direct call to registry.New so that the
// command tests can drive sync, review and ideas end to end without a
// credential and without a network — the same seam doctorOptions.newProvider
// gives --probe, at package level because four commands need it. Nothing in
// Apex reassigns it; only tests do, and they restore it.
var providerFactory advisor.ProviderFunc = func(ctx context.Context, route config.ModelRoute) (provider.Provider, error) {
	return registry.New(ctx, route)
}

// advisorFor builds the advisor loop for this session.
func (s *session) advisorFor() *advisor.Advisor {
	return advisor.New(s.Store, s.Config, s.Root, providerFactory)
}

// pendingMigrationsError stops a command that is not allowed to migrate.
type pendingMigrationsError struct{ Names []string }

func (e *pendingMigrationsError) Error() string {
	return fmt.Sprintf("the database schema is %d migration(s) behind this binary: %s\n"+
		"  run: apex sync", len(e.Names), strings.Join(e.Names, ", "))
}

func (e *pendingMigrationsError) UserFixable() bool { return true }

// invalidConfigError reports a config that would not load correctly, with
// every problem at once rather than the first.
type invalidConfigError struct {
	Path     string
	Problems []error
}

func (e *invalidConfigError) Error() string {
	msgs := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		msgs = append(msgs, "  "+p.Error())
	}
	// Config.Validate iterates a map, so its order is random; a stable
	// message is worth a sort.
	sort.Strings(msgs)
	return fmt.Sprintf("%s has problems:\n%s\n  run `apex doctor` for the full picture",
		e.Path, strings.Join(msgs, "\n"))
}

func (e *invalidConfigError) UserFixable() bool { return true }
