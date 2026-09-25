package advisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// Chat is the free-form surface (DESIGN.md §1, §12): the same identity context
// and the same portfolio digests as `review` and `ideas`, with a conversation
// on top.
//
// It streams rather than collecting, because it is the one advisor call a
// person watches happen. DESIGN.md §12's TUI drains the returned channel with
// a tea.Cmd that re-issues itself; a non-interactive caller can hand the same
// channel to provider.Collect.
//
// Persistence is deliberate and narrow. The sessions and messages tables have
// existed since M1 (DESIGN.md §7) with nothing writing to them; this is what
// they were for. The session row is created on the first message rather than
// in NewChat, so a TUI opened and closed without a word leaves nothing behind.

// chatInstructions is the system section that makes this a conversation rather
// than a report.
const chatInstructions = `You are Apex, a personal project-management, design and development hub.

You hold two things nothing else holds at once: a model of this specific user,
and a digest of every project they have. Answer from both. When a question
touches their portfolio, reason across all of it rather than one project at a
time — that cross-project view is the only thing here that another tool could
not give them.

Be direct and concrete. Markdown is rendered, so use it, but keep answers
short unless depth was asked for. If the context does not contain something
you need, say so and say which command would fill the gap (apex sync for
digests, editing PROFILE.md or SKILLS.md for identity) rather than guessing.

You cannot edit files or run commands from here. When the answer is work
rather than words, say what the action item would be; the user dispatches it
with apex do.`

// chatHistoryTurns bounds how much conversation is re-sent.
//
// It is a cost bound, not a quality one. The prompt already carries the
// identity context and every digest, and DESIGN.md §8's measurement is that
// prompt caching does not work on the subscription transport — so every turn
// re-sends the whole prefix at full price. An unbounded history would grow
// that on top, and the last eight exchanges are what a conversation actually
// refers back to.
const chatHistoryTurns = 16

// Chat is one conversation.
type Chat struct {
	a    *Advisor
	actx *Context

	// SessionID identifies the row in `sessions`. It is empty until the
	// first message is sent.
	SessionID string
	// History is the conversation so far, oldest first.
	History []provider.Message

	created bool
}

// NewChat starts a conversation over an already-loaded context.
//
// Nothing is written and no provider is built: a chat that is opened and never
// used costs nothing at all.
func (a *Advisor) NewChat(actx *Context) *Chat {
	return &Chat{a: a, actx: actx}
}

// Send records the user's message and streams the reply.
//
// The returned channel follows the provider contract exactly: it is closed
// once, and a caller that abandons it must cancel ctx. Complete must be called
// with the assembled reply once the stream ends, which is what appends the
// assistant turn to both the history and the database.
func (c *Chat) Send(ctx context.Context, input string) (<-chan provider.Event, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("advisor: there is nothing to send")
	}
	if err := c.ensureSession(ctx, input); err != nil {
		return nil, err
	}

	route := c.a.Config.Models.Chat
	p, err := c.a.providerFor(ctx, route)
	if err != nil {
		return nil, err
	}

	prompt := c.actx.Prompt(input)
	prompt.Identity = append([]provider.Section{{Title: "Who you are", Body: chatInstructions}}, prompt.Identity...)
	prompt.History = c.window()
	req := prompt.Build(route.Model, route.Effort, provider.DefaultMaxTokens)

	events, err := p.Stream(ctx, req)
	if err != nil {
		return nil, err
	}

	// The user's turn is recorded before the reply arrives, so a stream that
	// fails halfway still leaves the question in the transcript.
	c.History = append(c.History, provider.Message{Role: provider.RoleUser, Content: input})
	if err := c.append(ctx, store.RoleUser, input, "", nil); err != nil {
		return events, err
	}
	return events, nil
}

// Complete records the assistant's reply once the stream has ended.
//
// A reply that arrived empty — a cancelled stream, an error after zero deltas
// — is not recorded, because an empty assistant turn in the history would be
// re-sent on every later call as though the model had chosen to say nothing.
func (c *Chat) Complete(ctx context.Context, reply string, usage provider.Usage) error {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return nil
	}
	c.History = append(c.History, provider.Message{Role: provider.RoleAssistant, Content: reply})
	model := usage.Model
	if model == "" {
		model = c.a.Config.Models.Chat.Model
	}
	return c.append(ctx, store.RoleAssistant, reply, model, &usage)
}

// window returns the history that is re-sent, oldest first, bounded by
// chatHistoryTurns.
func (c *Chat) window() []provider.Message {
	if len(c.History) <= chatHistoryTurns {
		out := make([]provider.Message, len(c.History))
		copy(out, c.History)
		return out
	}
	out := make([]provider.Message, chatHistoryTurns)
	copy(out, c.History[len(c.History)-chatHistoryTurns:])
	return out
}

// ensureSession creates the sessions row on the first message.
func (c *Chat) ensureSession(ctx context.Context, first string) error {
	if c.created {
		return nil
	}
	id, err := newSessionID()
	if err != nil {
		return err
	}
	row := store.Session{ID: id, Title: sessionTitle(first), StartedAt: c.a.now()}
	if err := c.a.Store.InsertSession(ctx, row); err != nil {
		return err
	}
	c.SessionID = id
	c.created = true
	return nil
}

// append writes one turn to the messages table.
func (c *Chat) append(ctx context.Context, role, content, model string, usage *provider.Usage) error {
	msg := store.Message{
		SessionID: c.SessionID,
		Role:      role,
		Content:   content,
		CreatedAt: c.a.now(),
		Model:     model,
	}
	if usage != nil {
		in, out := usage.InputTokens, usage.OutputTokens
		msg.InputTokens, msg.OutputTokens = &in, &out
	}
	_, err := c.a.Store.AppendMessage(ctx, msg)
	return err
}

// sessionTitle is the first message, shortened, which is how a transcript is
// recognised in a list later.
func sessionTitle(first string) string {
	first = strings.Join(strings.Fields(first), " ")
	const limit = 60
	if len(first) <= limit {
		return first
	}
	cut := first[:limit]
	if i := strings.LastIndexByte(cut, ' '); i > limit/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// newSessionID returns a short random identifier for a chat session.
//
// Unlike executor.NewSessionID this is not a UUID: nothing outside Apex ever
// sees it, and no external tool parses its shape. The randomness is what
// matters, and a failure to read it is fatal rather than papered over with a
// timestamp, because two sessions sharing an id would interleave two
// transcripts in one row set.
func newSessionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("advisor: generate a session id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// StaleDigests reports which digests are older than the given age, so the chat
// surface can say the portfolio it is reasoning over is out of date.
//
// DESIGN.md §13: "Commands that depend on digests report their age when they
// run against stale data, so the predictable default never silently misleads."
// The default sync mode is explicit, which means this is the normal state on
// any machine the user has not synced today.
func (c *Context) StaleDigests(now time.Time, age time.Duration) []store.Digest {
	var out []store.Digest
	for _, d := range c.Digests {
		if now.Sub(d.GeneratedAt) > age {
			out = append(out, d)
		}
	}
	return out
}
