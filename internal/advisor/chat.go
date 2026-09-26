package advisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

You cannot edit files or run commands from here, but you can propose action
items. The user approves them here, and any they approve land in their action
item list, from which a coding agent can be dispatched to do the work.

When the conversation arrives at concrete work on a project in the portfolio —
the user asks what to do next, agrees to a plan, or asks you to add something
to their list — end your reply with exactly one fenced block tagged
` + "`" + ProposalFence + "`" + `, holding JSON in this shape:

` + "```" + ProposalFence + `
{"items": [{"project": "<slug>", "title": "...", "body": "...", "rationale": "...", "effort": "small"}]}
` + "```" + `

- "project" is a project's slug exactly as it appears in parentheses in the
  portfolio. Never invent one. If the work belongs to a project that is not
  registered yet, say so instead and suggest registering it first.
- "title" is one imperative line under 80 characters. "body" says what to do
  and how you would know it was done, in markdown, with acceptance criteria.
  "rationale" says why now. "effort" is small, medium or large.
- At most five items, and only work concrete enough to hand to a coding agent.
  Most replies need no block at all.
- Never propose an item that is already on the list below.
- Nothing is recorded until the user approves. They see the block as a
  checklist rather than as JSON, so do not repeat its contents in prose: a
  sentence introducing the items is enough.

When the user asks for new project ideas — projects that do not exist yet,
not work on the ones above — end your reply with exactly one fenced block
tagged ` + "`" + IdeaFence + "`" + ` instead:

` + "```" + IdeaFence + `
{"ideas": [{"title": "...", "pitch": "...", "rationale": "..."}]}
` + "```" + `

- At most five, strongest fit first. "title" is a short project name; "pitch"
  says what it is and ends with a concrete first milestone; "rationale" argues
  from this user specifically — their skills, their projects, the gaps between.
- The user sees the ideas as a list to pick from, so keep your prose to a
  sentence or two. When they pick one, Apex asks you for more detail and a few
  questions, and then turns it into a project and its first action item. Do
  not tell them to run apex start.
- Never propose an idea already on their backlog, and never an existing
  project.`

// ProposalFence is the info string of the fenced block a chat reply carries
// its proposed action items in. It is a fence rather than a Structured call
// because the chat streams prose, and the proposals belong to the same reply
// as the reasoning that produced them.
const ProposalFence = "apex-items"

// maxChatProposals bounds one reply's proposals, for the same reason
// maxReviewItems bounds a review, and tighter: these arrive mid-conversation,
// one approval at a time.
const maxChatProposals = 5

// proposeInstructions is the explicit form of the same request: the user
// asked for the conversation to be turned into action items, so the answer is
// a Structured call against actionItemsSchema rather than a fenced block the
// model may or may not remember to write.
const proposeInstructions = `Turn the conversation so far into action items for the user's list.

Rules:
- Only work the conversation actually arrived at. If it has not arrived at
  anything concrete enough to hand to a coding agent, return an empty list.
- At most %d items.
- Set "project" to a project's slug exactly as it appears in parentheses in
  the portfolio. Do not invent a project.
- "title" is one imperative line. "body" says what to do and how you would
  know it was done. "rationale" says why now.
- Do not propose anything already on the list of open action items.`

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

// SetContext replaces what later turns reason over — after a project was
// started or synced elsewhere — without losing the conversation.
func (c *Chat) SetContext(actx *Context) { c.actx = actx }

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

	prompt, err := c.prompt(ctx, input)
	if err != nil {
		return nil, err
	}
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

// prompt assembles a chat request: the chat's own instructions ahead of the
// identity context, and the action items already on the list after the
// portfolio.
//
// The open items are read on every turn rather than once, because the user
// approves proposals mid-conversation, and a list the model saw at the start
// would have it proposing again what was approved three turns ago. They go in
// the stable prefix, after the digests: they change only when an item does,
// which is rarely enough to cache.
func (c *Chat) prompt(ctx context.Context, instruction string) (provider.Prompt, error) {
	existing, err := c.a.Store.ListActionItems(ctx, store.ActionItemFilter{})
	if err != nil {
		return provider.Prompt{}, err
	}
	ideas, err := c.a.Store.ListIdeas(ctx, "")
	if err != nil {
		return provider.Prompt{}, err
	}
	prompt := c.actx.Prompt(instruction)
	prompt.Identity = append([]provider.Section{{Title: "Who you are", Body: chatInstructions}}, prompt.Identity...)
	prompt.Digests = append(prompt.Digests, provider.Section{
		Title: "Action items already on the list",
		Body:  renderOpenItems(existing),
	}, provider.Section{
		Title: "Project ideas already on the backlog",
		Body:  renderOpenIdeas(ideas),
	})
	return prompt, nil
}

// Proposals is what ProposeItems produced.
type Proposals struct {
	Items []ProposedItem
	Usage provider.Usage
	// Model is the model that served the call, which is what the items
	// record as generated_by if they are approved.
	Model string
}

// ProposeItems asks for the conversation so far as action items, through a
// Structured call. Nothing is recorded: the caller shows the proposals to the
// user, and RecordItems writes the ones they approve.
//
// The request is not added to the history. It is a question about the
// conversation, not a turn in it.
func (c *Chat) ProposeItems(ctx context.Context) (*Proposals, error) {
	if len(c.History) == 0 {
		return nil, fmt.Errorf("advisor: there is no conversation to turn into action items yet")
	}
	var decoded reviewResponse
	usage, model, err := c.structured(ctx, fmt.Sprintf(proposeInstructions, maxChatProposals), actionItemsSchema, &decoded)
	if err != nil {
		return nil, err
	}
	return &Proposals{Items: capProposals(decoded.Items), Usage: usage, Model: model}, nil
}

// ExtractProposals splits a chat reply into the prose the user reads and the
// action items proposed in its fenced block. See ParseReply, which it wraps.
func ExtractProposals(reply string) (prose string, items []ProposedItem, err error) {
	r := ParseReply(reply)
	return r.Prose, r.Items, r.ItemsErr
}

// Reply is a finished chat reply taken apart: the prose the user reads, and
// whatever it proposed in fenced blocks.
type Reply struct {
	Prose string
	// Items are proposed action items, from an apex-items block.
	Items    []ProposedItem
	ItemsErr error
	// Ideas are proposed new projects, from an apex-ideas block.
	Ideas    []ProposedIdea
	IdeasErr error
}

// ParseReply splits a reply into prose and proposals.
//
// For each kind of block the last one wins, and only a closed one counts: a
// reply cut off inside its block has proposed nothing the user could act on
// with confidence, so it is reported as an error rather than half-parsed. The
// prose is returned either way, with every proposal block removed, so the JSON
// is never what the user reads.
//
// A block holds {"items": [...]} or {"ideas": [...]}, and a bare array is
// accepted too, because a model asked for one shape sometimes writes the
// shorter.
func ParseReply(reply string) Reply {
	prose, blocks := splitFences(reply, ProposalFence, IdeaFence)
	out := Reply{Prose: prose}

	if b, ok := blocks[ProposalFence]; ok {
		var wrapped reviewResponse
		out.ItemsErr = decodeBlock(b, &wrapped, &wrapped.Items, "proposed action items")
		out.Items = capProposals(wrapped.Items)
	}
	if b, ok := blocks[IdeaFence]; ok {
		var wrapped ideasResponse
		out.IdeasErr = decodeBlock(b, &wrapped, &wrapped.Ideas, "proposed project ideas")
		out.Ideas = capIdeas(wrapped.Ideas)
	}
	return out
}

// fenceBlock is one proposal block's raw body, and whether it was closed.
type fenceBlock struct {
	raw    string
	closed bool
}

// splitFences removes every block tagged with one of tags from reply,
// returning the rest as prose and the last block of each tag.
func splitFences(reply string, tags ...string) (string, map[string]fenceBlock) {
	blocks := map[string]fenceBlock{}
	var kept, body []string
	open := ""
	for _, line := range strings.Split(reply, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case open == "":
			if tag := fenceTag(trimmed, tags); tag != "" {
				open, body = tag, nil
				blocks[tag] = fenceBlock{}
				continue
			}
			kept = append(kept, line)
		case strings.HasPrefix(trimmed, "```"):
			blocks[open] = fenceBlock{raw: strings.TrimSpace(strings.Join(body, "\n")), closed: true}
			open = ""
		default:
			body = append(body, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), blocks
}

// decodeBlock unmarshals a block into its wrapped shape, falling back to a
// bare array.
func decodeBlock[T any](b fenceBlock, wrapped any, list *[]T, what string) error {
	if !b.closed {
		return fmt.Errorf("the %s were cut off before the block closed", what)
	}
	err := json.Unmarshal([]byte(b.raw), wrapped)
	if err == nil {
		return nil
	}
	var bare []T
	if json.Unmarshal([]byte(b.raw), &bare) != nil {
		return fmt.Errorf("the %s are not valid JSON: %w", what, err)
	}
	*list = bare
	return nil
}

// VisibleReply is the part of a reply still arriving that the user should
// see: everything before a proposal block has opened. drafting names what the
// block holds once one has — "action items" or "project ideas" — so the caller
// can say that something is being written without showing JSON as it streams.
//
// A last line that could still become a fence — "```apex" with the rest of the
// tag yet to arrive — is held back too, or it would flash on screen for one
// token and vanish on the next.
func VisibleReply(partial string) (visible string, drafting string) {
	lines := strings.Split(partial, "\n")
	for i, line := range lines {
		switch fenceTag(strings.TrimSpace(line), []string{ProposalFence, IdeaFence}) {
		case ProposalFence:
			return strings.Join(lines[:i], "\n"), "action items"
		case IdeaFence:
			return strings.Join(lines[:i], "\n"), "project ideas"
		}
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	if strings.HasPrefix(last, "```") &&
		(strings.HasPrefix("```"+ProposalFence, last) || strings.HasPrefix("```"+IdeaFence, last)) {
		return strings.Join(lines[:len(lines)-1], "\n"), ""
	}
	return partial, ""
}

// fenceTag returns which of tags a fence line opens, or "".
func fenceTag(trimmed string, tags []string) string {
	rest, ok := strings.CutPrefix(trimmed, "```")
	if !ok {
		return ""
	}
	rest = strings.TrimSpace(rest)
	for _, t := range tags {
		if rest == t {
			return t
		}
	}
	return ""
}

// capProposals drops untitled proposals and bounds the rest.
func capProposals(items []ProposedItem) []ProposedItem {
	var out []ProposedItem
	for _, it := range items {
		if strings.TrimSpace(it.Title) == "" {
			continue
		}
		out = append(out, it)
		if len(out) == maxChatProposals {
			break
		}
	}
	return out
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
