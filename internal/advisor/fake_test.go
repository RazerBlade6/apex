package advisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// fakeProvider stands in for every vendor. Nothing in this package's tests
// starts a subprocess or opens a socket: the point of ProviderFunc being a
// field is that the advisor loop is testable without either.
type fakeProvider struct {
	mu sync.Mutex
	// Requests is every request that reached the provider, in order.
	Requests []provider.Request
	// Text is what Stream returns.
	Text string
	// JSON is what Structured unmarshals into out.
	JSON string
	// Err, when set, fails every call.
	Err error
	// Usage is reported on EventDone.
	Usage provider.Usage
	// InFlight records the highest number of concurrent calls observed.
	InFlight, peak int
	// Block, when non-nil, holds every call until it is closed, so a test
	// can observe concurrency.
	Block chan struct{}
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) enter(req provider.Request) {
	f.mu.Lock()
	f.Requests = append(f.Requests, req)
	f.InFlight++
	if f.InFlight > f.peak {
		f.peak = f.InFlight
	}
	f.mu.Unlock()
}

func (f *fakeProvider) leave() {
	f.mu.Lock()
	f.InFlight--
	f.mu.Unlock()
}

// Peak reports the most calls that were ever in flight at once.
func (f *fakeProvider) Peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

// Calls reports how many requests arrived.
func (f *fakeProvider) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Requests)
}

func (f *fakeProvider) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	f.enter(req)
	ch := make(chan provider.Event, 4)
	go func() {
		defer close(ch)
		defer f.leave()
		if f.Block != nil {
			select {
			case <-f.Block:
			case <-ctx.Done():
			}
		}
		if f.Err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: f.Err}
			return
		}
		ch <- provider.Event{Type: provider.EventTextDelta, Text: f.Text}
		usage := f.Usage
		ch <- provider.Event{Type: provider.EventDone, Usage: &usage}
	}()
	return ch, nil
}

func (f *fakeProvider) Structured(ctx context.Context, req provider.Request, schema json.RawMessage, out any) error {
	f.enter(req)
	defer f.leave()
	if f.Err != nil {
		return f.Err
	}
	// The schema is decoded rather than ignored so a malformed one fails
	// here, the way a vendor would reject it.
	var probe map[string]any
	if err := json.Unmarshal(schema, &probe); err != nil {
		return fmt.Errorf("fake: bad schema: %w", err)
	}
	return json.Unmarshal([]byte(f.JSON), out)
}

// harness is an Advisor wired to a fake provider and a real in-memory store.
type harness struct {
	*Advisor
	Root string
	T    *testing.T
}

// fixedNow is the clock every test runs on, so timestamps are comparable.
var fixedNow = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func newHarness(t *testing.T, fake provider.Provider) *harness {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(contextfs.ContextDir(root), 0o700); err != nil {
		t.Fatal(err)
	}

	backend := store.NewLocalBackend(filepath.Join(root, "apex.db"))
	t.Cleanup(func() { backend.Close() }) //nolint:errcheck
	st, err := store.Open(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	a := New(st, &cfg, root, func(context.Context, config.ModelRoute) (provider.Provider, error) {
		return fake, nil
	})
	a.Now = func() time.Time { return fixedNow }
	return &harness{Advisor: a, Root: root, T: t}
}

// writeIdentity creates a PROFILE.md with the given body.
func (h *harness) writeIdentity(body string) {
	h.T.Helper()
	if err := os.WriteFile(contextfs.ProfilePath(h.Root), []byte(body), 0o600); err != nil {
		h.T.Fatal(err)
	}
}

// seedProject registers a project and caches a digest for it.
func (h *harness) seedProject(slug, name, digest, sourceHash string) {
	h.T.Helper()
	ctx := context.Background()
	if err := h.Store.UpsertProject(ctx, store.Project{
		Slug: slug, Name: name, Path: "/tmp/" + slug, RegisteredAt: fixedNow,
	}); err != nil {
		h.T.Fatal(err)
	}
	if digest == "" {
		return
	}
	if err := h.Store.UpsertDigest(ctx, store.Digest{
		ProjectSlug: slug, Body: digest, SourceHash: sourceHash,
		GeneratedAt: fixedNow, Model: "fake-model",
	}); err != nil {
		h.T.Fatal(err)
	}
}
