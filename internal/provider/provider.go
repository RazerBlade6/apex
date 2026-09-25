// Package provider is Apex's abstraction over the model vendors (DESIGN.md §8).
//
// Because the builder loop is dispatched to an external coding agent, Apex
// needs exactly two capabilities from a provider: streaming text and
// structured output. There is deliberately no tool use here — that plumbing
// lives inside the dispatched agent, which is what keeps this interface small
// enough that Go's weak sum types never become a problem.
//
// The vendor adapters live in the subpackages anthropic/ and openai/. The
// factory that turns a config [models.*] entry into a Provider lives in
// registry/, one level down, because a factory in this package would have to
// import the adapters that import it.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Role is the author of a message. There is no system role: the system prompt
// is a field on Request, as both vendors model it.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one conversational turn.
type Message struct {
	Role    Role
	Content string
}

// Request is one call to a provider.
//
// System carries the stable prefix — identity context, then digests — and the
// volatile part of the prompt belongs in Messages. That ordering is what makes
// prompt caching work (DESIGN.md §8); Prompt assembles it.
type Request struct {
	System    string
	Messages  []Message
	Model     string
	MaxTokens int
	Effort    string // low|medium|high|xhigh|max; Anthropic only, ignored elsewhere

	// CacheSystem asks the provider to place a prompt-cache breakpoint at the
	// end of System, so identity context and digests are cached and only the
	// messages vary between calls. Providers that cache automatically (OpenAI)
	// ignore it.
	//
	// This field is an addition to the struct in DESIGN.md §8: the spec
	// requires the breakpoint to be placed "after the digests", and a
	// vendor-neutral request needs somewhere to say so.
	CacheSystem bool
}

// EventType tags an Event. A stream ends with exactly one EventDone or one
// EventError, and never both.
type EventType int

const (
	EventTextDelta EventType = iota
	EventDone
	EventError
)

func (t EventType) String() string {
	switch t {
	case EventTextDelta:
		return "text_delta"
	case EventDone:
		return "done"
	case EventError:
		return "error"
	default:
		return fmt.Sprintf("event(%d)", int(t))
	}
}

// Usage is what one call cost.
//
// CacheReadTokens is load-bearing rather than decorative: it is the only way
// to confirm the cache prefix in DESIGN.md §8 is actually being hit. A run of
// calls that reports zero cache reads means something volatile crept into the
// prefix.
//
// CacheWriteTokens is not redundant with it, and M4 added it because M3.5 made
// the gap concrete: one trivial `say hi` through claude-cli reported 16,440
// cache-creation tokens and zero cache reads, and Apex had nowhere to put the
// number. Reads alone cannot tell a cache that works from a prefix just
// unstable enough to be re-written every call — in that failure mode reads
// stay at zero and writes stay high, which is the expensive case and looks
// identical to "not cached yet". Cache writes are billed at a premium, so this
// is the field that catches the mistake.
//
// Model is which model actually served the request, which is not always the
// one that was asked for: an alias ("opus" through the CLI) or a fallback
// resolves to something else. DESIGN.md §7 gives digests.model and
// action_items.generated_by to provenance, and without this field Apex could
// only ever record its own intent.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	// Model is empty when the provider did not report one; nothing here
	// invents it from the request, because a recorded guess is worse than a
	// recorded blank.
	Model string
}

// Event is one item on a provider's stream.
type Event struct {
	Type  EventType
	Text  string
	Usage *Usage // populated on EventDone
	Err   error  // populated on EventError
}

// Provider is a model vendor.
type Provider interface {
	Name() string

	// Stream returns a channel closed when the response completes or errors.
	//
	// The channel is closed exactly once. Every implementation stops when ctx
	// is cancelled, so a caller that abandons the channel must cancel ctx or
	// the sending goroutine stays parked on its last send.
	Stream(ctx context.Context, req Request) (<-chan Event, error)

	// Structured constrains output to a JSON schema, unmarshals into out,
	// and reports what the call cost.
	//
	// The Usage return is M5's, and DESIGN.md §18 argued for it at length
	// before it existed. Stream reported usage on EventDone and Structured
	// reported none, which meant the two calls carrying the *largest* prompt
	// Apex ever sends — review and ideas, which hold the identity context
	// plus every digest — were the only ones whose token cost was invisible,
	// including the cache-write number §8 proved was worth having. It also
	// forced an inconsistency in provenance: digests.model recorded the
	// serving model from Usage.Model while action_items.generated_by could
	// only record the route's, so one column said "claude-sonnet-5" and the
	// other said "sonnet" for the same run.
	//
	// Usage is returned even on failure, so a call that was billed and then
	// failed to decode still reports what it spent. A provider that cannot
	// say returns the zero value; nothing here invents one.
	Structured(ctx context.Context, req Request, schema json.RawMessage, out any) (Usage, error)
}

// Config is what the registry hands an adapter: the credential, the endpoint,
// and the defaults from one [models.*] entry.
type Config struct {
	// APIKey is injected rather than read from the environment by the adapter,
	// so key resolution stays in internal/config (DESIGN.md §13) and tests can
	// pass a throwaway value.
	APIKey string

	// BaseURL overrides the vendor endpoint. Empty means the vendor default.
	// It exists so tests can point an adapter at an httptest server; nothing
	// in Apex sets it in production.
	BaseURL string

	// Binary overrides the executable a subprocess-backed provider runs. It
	// is the claude-cli analogue of BaseURL — tests point it at a fake, and
	// nothing in Apex sets it in production — with one difference: a user
	// whose claude lives somewhere unusual has a legitimate reason to set it,
	// which is why it is config rather than a test-only hook.
	//
	// Empty means the adapter looks the binary up itself. HTTP-backed
	// adapters ignore it.
	Binary string

	// Model and Effort are the defaults from the [models.*] entry, used for
	// any Request that does not name its own.
	Model  string
	Effort string

	// MaxRetries overrides the SDK's retry count. nil keeps the SDK default
	// (two retries on 429, 5xx, and connection failures). `apex doctor
	// --probe` sets it to zero so a rate limit is reported rather than
	// silently waited out.
	MaxRetries *int
}

// Apply fills a request's Model and Effort from the route defaults. A request
// that names either wins, so one Provider can serve a caller that wants
// something other than its configured default.
func (c Config) Apply(req Request) Request {
	if req.Model == "" {
		req.Model = c.Model
	}
	if req.Effort == "" {
		req.Effort = c.Effort
	}
	return req
}

// DefaultMaxTokens is used when a Request does not set one. It is large
// because every adapter streams (DESIGN.md §8: non-streaming requests with a
// large max_tokens hit HTTP timeouts), so there is no reason to be stingy.
const DefaultMaxTokens = 16000

// Validate reports a request that cannot be sent, before any HTTP call is
// made. Adapters call it after Apply.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("provider: request has no model")
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("provider: request has no messages")
	}
	for i, m := range r.Messages {
		switch m.Role {
		case RoleUser, RoleAssistant:
		default:
			return fmt.Errorf("provider: message %d has role %q (want %q or %q)",
				i, m.Role, RoleUser, RoleAssistant)
		}
		if strings.TrimSpace(m.Content) == "" {
			return fmt.Errorf("provider: message %d is empty", i)
		}
	}
	if r.MaxTokens < 0 {
		return fmt.Errorf("provider: max tokens is negative (%d)", r.MaxTokens)
	}
	return nil
}

// WithDefaults returns the request with MaxTokens filled in if it was unset.
func (r Request) WithDefaults() Request {
	if r.MaxTokens == 0 {
		r.MaxTokens = DefaultMaxTokens
	}
	return r
}

// Emit sends an event unless ctx is done, and reports whether it landed. It is
// how adapters write text deltas: a plain send would park forever if the
// consumer walked away, which is exactly the goroutine leak the interface
// promises not to have.
func Emit(ctx context.Context, ch chan<- Event, ev Event) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// EmitFinal sends a terminal event — EventDone or EventError — and reports
// whether it landed.
//
// It differs from Emit in one case that matters: when the stream ended
// *because* ctx was cancelled, ctx.Done() is already closed, so Emit would
// drop the error and the consumer would see the channel close with no
// explanation. A consumer that is still reading deserves to be told why the
// stream stopped, so the send is attempted without consulting ctx first. It
// falls back to Emit's behaviour if the buffer is full, which is the case
// where nobody is reading anyway.
func EmitFinal(ctx context.Context, ch chan<- Event, ev Event) bool {
	select {
	case ch <- ev:
		return true
	default:
	}
	return Emit(ctx, ch, ev)
}
