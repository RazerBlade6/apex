package advisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// ContextHashVersion labels the byte layout ContextHash feeds to SHA-256, for
// the same reason project.SourceHashVersion does: a changed layout must read
// as "generated against different context", not as a hash two binaries
// disagree about.
const ContextHashVersion = "apex-context-hash-v1"

// Context is everything an advisor call reasons over: the identity documents
// and every cached project digest.
//
// It is loaded once per command and reused, because the whole point of the
// ordering in DESIGN.md §8 is that this content is the stable cache prefix.
type Context struct {
	Identity contextfs.Identity
	// Digests is every cached digest, ordered by project slug.
	Digests []store.Digest
	// Projects maps a slug to its registry row, so a digest can be labelled
	// with the name the user actually wrote.
	Projects map[string]store.Project
}

// LoadContext reads the identity documents and every cached digest.
//
// A missing PROFILE.md or SKILLS.md is not an error. contextfs distinguishes
// absent from empty precisely so this layer can carry on and *say* that the
// output will be generic, rather than either failing or inventing a persona.
func (a *Advisor) LoadContext(ctx context.Context) (*Context, error) {
	identity, err := contextfs.LoadIdentity(ctx, a.Root)
	if err != nil {
		return nil, err
	}
	digests, err := a.Store.ListDigests(ctx)
	if err != nil {
		return nil, err
	}
	projects, err := a.Store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	byslug := make(map[string]store.Project, len(projects))
	for _, p := range projects {
		byslug[p.Slug] = p
	}
	return &Context{Identity: identity, Digests: digests, Projects: byslug}, nil
}

// MissingIdentity names the identity documents that do not exist or are empty.
//
// Commands print this rather than swallowing it. On a machine where neither
// file has been written — which is the state this feature ships into — every
// advisor call is reasoning from project digests alone, and a user deserves
// to know that before they judge the output.
func (c *Context) MissingIdentity() []string {
	var missing []string
	if c.Identity.Profile.Empty() {
		missing = append(missing, contextfs.ProfileFile)
	}
	if c.Identity.Skills.Empty() {
		missing = append(missing, contextfs.SkillsFile)
	}
	return missing
}

// ProjectName returns the registry name for a slug, falling back to the slug
// itself for a digest whose project row has since gone.
func (c *Context) ProjectName(slug string) string {
	if p, ok := c.Projects[slug]; ok && p.Name != "" {
		return p.Name
	}
	return slug
}

// Filter narrows the context to one project, for `apex review <project>`.
//
// The identity documents stay: a single-project review still has to know who
// is doing the work. Only the digest set shrinks, which is also what makes
// the returned Context hash to a different value — an item generated from one
// digest genuinely was not generated from the whole portfolio.
func (c *Context) Filter(slug string) *Context {
	out := &Context{Identity: c.Identity, Projects: c.Projects}
	for _, d := range c.Digests {
		if d.ProjectSlug == slug {
			out.Digests = append(out.Digests, d)
		}
	}
	return out
}

// Hash is the provenance stamp written to action_items.context_hash
// (DESIGN.md §7): it identifies the context an item was generated from, so
// Apex can later say the project has moved on.
//
// It covers the digests' source hashes rather than their bodies — two
// regenerations of one unchanged project are the same context even if the
// model phrased the digest differently — and the identity documents, which
// are as much a part of the reasoning as the digests are. Fields are
// length-prefixed for the same reason project.SourceHash does it: so no body
// can imitate the field that follows it.
func (c *Context) Hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n", ContextHashVersion)
	hashDoc(h, "profile", c.Identity.Profile)
	hashDoc(h, "skills", c.Identity.Skills)

	slugs := make([]string, 0, len(c.Digests))
	sources := make(map[string]string, len(c.Digests))
	for _, d := range c.Digests {
		slugs = append(slugs, d.ProjectSlug)
		sources[d.ProjectSlug] = d.SourceHash
	}
	sort.Strings(slugs)
	fmt.Fprintf(h, "digests:%d\n", len(slugs))
	for _, slug := range slugs {
		fmt.Fprintf(h, "%s=%s\n", slug, sources[slug])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashDoc(h interface{ Write([]byte) (int, error) }, label string, doc contextfs.Document) {
	if !doc.Present {
		fmt.Fprintf(h, "%s:absent\n", label)
		return
	}
	body := strings.ReplaceAll(doc.Body, "\r\n", "\n")
	fmt.Fprintf(h, "%s:%d\n", label, len(body))
	h.Write([]byte(body))
	h.Write([]byte("\n"))
}

// Prompt assembles the request in the order prompt caching needs: identity
// context, then digests, then the volatile instruction (DESIGN.md §8).
//
// Everything stable goes through provider.Prompt's Identity and Digests
// fields, which Build renders into Request.System with the cache breakpoint at
// its end. The instruction — which differs on every call — goes into
// Request.Messages, after the breakpoint. Getting this backwards is not a
// performance detail: it means nothing is ever cached.
func (c *Context) Prompt(instruction string) provider.Prompt {
	p := provider.Prompt{Instruction: instruction}

	if !c.Identity.Profile.Empty() {
		p.Identity = append(p.Identity, provider.Section{
			Title: "Who you are working for (PROFILE.md)",
			Body:  c.Identity.Profile.Body,
		})
	}
	if !c.Identity.Skills.Empty() {
		p.Identity = append(p.Identity, provider.Section{
			Title: "What they know (SKILLS.md)",
			Body:  c.Identity.Skills.Body,
		})
	}
	if len(p.Identity) == 0 {
		// Said plainly, in the prompt as well as in the terminal. A model
		// given no identity context and no acknowledgement of the fact will
		// cheerfully invent one.
		p.Identity = append(p.Identity, provider.Section{
			Title: "Identity context",
			Body: "None available: the user has not written " + contextfs.ProfileFile +
				" or " + contextfs.SkillsFile + " yet. Reason from the project " +
				"digests alone and do not infer a persona, a skill set, or " +
				"preferences that the digests do not show.",
		})
	}

	if len(c.Digests) == 0 {
		p.Digests = append(p.Digests, provider.Section{
			Title: "Projects",
			Body:  "No project digests are available. Run `apex sync` to generate them.",
		})
		return p
	}

	var b strings.Builder
	for _, d := range c.Digests {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %s (%s)\n\n%s", c.ProjectName(d.ProjectSlug), d.ProjectSlug,
			strings.TrimSpace(d.Body))
	}
	p.Digests = append(p.Digests, provider.Section{
		Title: "The portfolio",
		Body:  b.String(),
	})
	return p
}
