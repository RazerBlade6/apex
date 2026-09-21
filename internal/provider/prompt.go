package provider

import "strings"

// Section is one titled block of prompt text.
type Section struct {
	// Title is rendered as a markdown heading. It may be empty, in which case
	// only the body is emitted.
	Title string
	Body  string
}

// Prompt assembles a request in the order prompt caching needs (DESIGN.md §8):
// identity context, then digests, then the volatile request, with the cache
// breakpoint after the digests.
//
// The ordering is the whole point. Both vendors cache on a prefix match, so
// any byte that changes invalidates everything after it. Identity context
// (PROFILE.md, SKILLS.md) changes least often, project digests change on a
// sync, and the request changes every call — so that is the order they go in,
// and the breakpoint goes at the boundary between what is stable and what is
// not.
//
// Build puts Identity and Digests in Request.System and the volatile part in
// Request.Messages, which is exactly where the breakpoint has to fall: both
// vendors render system before messages, so a breakpoint at the end of the
// system prompt caches the identity context and the digests and nothing else.
//
// Nothing here loads real context. M4 fills these fields from contextfs and
// the digests table; the ordering primitive belongs next to the provider code
// that depends on it.
type Prompt struct {
	// Identity is the stable head: PROFILE.md, SKILLS.md.
	Identity []Section
	// Digests is one section per project, still stable between calls.
	Digests []Section
	// Instruction is the volatile request. It becomes the user message.
	Instruction string
	// History is prior conversation, appended before the instruction. Chat
	// sessions use it; review and ideas do not.
	History []Message
}

// System renders the stable prefix: identity context first, then digests.
func (p Prompt) System() string {
	var b strings.Builder
	for _, s := range append(append([]Section{}, p.Identity...), p.Digests...) {
		writeSection(&b, s)
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeSection(b *strings.Builder, s Section) {
	body := strings.TrimSpace(s.Body)
	title := strings.TrimSpace(s.Title)
	if title == "" && body == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	if title != "" {
		b.WriteString("# ")
		b.WriteString(title)
		if body != "" {
			b.WriteString("\n\n")
		}
	}
	b.WriteString(body)
}

// Build renders the prompt into a Request, with the cache breakpoint placed
// after the digests.
//
// route supplies the model and effort; a caller that wants something else
// overrides the returned request's fields.
func (p Prompt) Build(model, effort string, maxTokens int) Request {
	system := p.System()
	messages := make([]Message, 0, len(p.History)+1)
	messages = append(messages, p.History...)
	if strings.TrimSpace(p.Instruction) != "" {
		messages = append(messages, Message{Role: RoleUser, Content: p.Instruction})
	}
	return Request{
		System:    system,
		Messages:  messages,
		Model:     model,
		Effort:    effort,
		MaxTokens: maxTokens,
		// Only mark a breakpoint when there is a stable prefix to cache.
		CacheSystem: system != "",
	}
}
