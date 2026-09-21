package advisor

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/project"
	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// digestInstructions is the digest-route system prompt.
//
// It is short on purpose. A digest is the unit the advisor loop reasons over
// (DESIGN.md §6), so its value is in being dense and current, not in being
// well written — and a long instruction would be a large share of a cheap
// call's input.
const digestInstructions = `You write project digests for Apex, a personal project-management tool.

Given one project's own documentation and its recent git activity, write a
200-300 word synthesis of where that project actually stands right now. It is
read later by a model reasoning across the user's whole portfolio at once, so
it must stand alone without the sources.

Cover, in prose and in this order:
- what the project is, in one sentence;
- what currently works;
- what is in progress or half-finished, judging from uncommitted changes and
  recent commits as much as from the documentation;
- what is blocking it, and what the stated goals are.

Rules:
- PROJECT.md is the user's own words and is authoritative. Where the git
  activity contradicts it, say so explicitly rather than choosing a side.
- Never invent a fact. If a section has nothing to report, say the sources do
  not say.
- No preamble, no headings, no bullet lists, no markdown. Plain paragraphs
  only. Begin with the project's name.`

// readmeLimit bounds how much of a README reaches the prompt. READMEs run to
// thousands of lines; the opening is what describes the project, and the rest
// is installation instructions the digest has no use for.
const readmeLimit = 6000

// projectDocLimit bounds PROJECT.md, which is the authoritative source and so
// gets far more room than the README. It is a guard against a pathological
// file, not a summarisation budget.
const projectDocLimit = 20000

// DigestSources renders one project's assembled sources as the volatile half
// of a digest prompt.
//
// It is the user message, not the system prompt, and that is the whole point:
// the system prompt holds the instructions and the identity context, which are
// byte-identical across every project in a sync, so the cache prefix survives
// from one digest to the next. Putting the project's sources in System would
// mean every call wrote a fresh cache entry and read none (DESIGN.md §8).
func DigestSources(src *project.DigestSource) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n", src.Name)

	b.WriteString("\n--- " + contextfs.ProjectFile + " ---\n")
	if src.HasProjectDoc() {
		b.WriteString(clip(src.Doc.Raw, projectDocLimit))
	} else {
		b.WriteString("(the project has none)")
	}

	b.WriteString("\n\n--- " + contextfs.ReadmeFile + " ---\n")
	if src.Readme.Present {
		b.WriteString(clip(src.Readme.Body, readmeLimit))
	} else {
		b.WriteString("(the project has none)")
	}

	b.WriteString("\n\n--- git ---\n")
	if !src.Git.IsRepo {
		b.WriteString("(not a git repository)")
	} else {
		fmt.Fprintf(&b, "HEAD %s on %s\n", orNone(src.Git.ShortHead(), "(no commits yet)"), orNone(src.Git.Branch, "(unknown)"))
		b.WriteString("\nrecent commits (newest first):\n")
		if len(src.Git.Log) == 0 {
			b.WriteString("(none)\n")
		} else {
			for _, line := range src.Git.Log {
				b.WriteString("  " + line + "\n")
			}
		}
		b.WriteString("\nuncommitted changes (git status --short):\n")
		if len(src.Git.Status) == 0 {
			b.WriteString("(none: the working tree is clean)\n")
		} else {
			for _, line := range src.Git.Status {
				b.WriteString("  " + line + "\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func orNone(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// clip truncates a body on a line boundary, saying that it did.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if i := strings.LastIndexByte(cut, '\n'); i > limit/2 {
		cut = cut[:i]
	}
	return cut + "\n\n[truncated by apex]"
}

// GenerateDigest produces one project's digest through the digest route.
//
// The returned Digest carries the source hash it was generated from, which is
// what makes the cache correct: a digest is only ever stored against the exact
// inputs it saw, so a later sync that computes the same hash can skip the call
// entirely (DESIGN.md §6).
//
// Model records what actually served the request, not what was asked for —
// which for a claude-cli route matters immediately, since "opus" is an alias
// the CLI resolves to a dated id.
func (a *Advisor) GenerateDigest(ctx context.Context, p provider.Provider, actx *Context, src *project.DigestSource) (store.Digest, provider.Usage, error) {
	route := a.Config.Models.Digest

	prompt := provider.Prompt{
		Identity:    digestIdentity(actx),
		Instruction: DigestSources(src),
	}
	req := prompt.Build(route.Model, route.Effort, digestMaxTokens)

	body, usage, err := provider.Collect(ctx, p, req)
	if err != nil {
		return store.Digest{}, usage, fmt.Errorf("generate digest for %s: %w", src.Name, err)
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return store.Digest{}, usage, fmt.Errorf("generate digest for %s: the model returned no text", src.Name)
	}

	model := usage.Model
	if model == "" {
		// digests.model is NOT NULL, and the route's model is the honest
		// fallback: it is what was asked for, and the provider declined to
		// say what answered.
		model = route.Model
	}
	return store.Digest{
		ProjectSlug: src.Slug,
		Body:        body,
		SourceHash:  src.SourceHash,
		GeneratedAt: a.now(),
		Model:       model,
	}, usage, nil
}

// digestIdentity is the stable head of a digest prompt: the instructions, then
// the identity context. It is identical for every project in a sync, which is
// exactly what the cache needs.
func digestIdentity(actx *Context) []provider.Section {
	sections := []provider.Section{{Title: "Your task", Body: digestInstructions}}
	if actx == nil {
		return sections
	}
	if !actx.Identity.Profile.Empty() {
		sections = append(sections, provider.Section{
			Title: "Who the user is (PROFILE.md)",
			Body:  actx.Identity.Profile.Body,
		})
	}
	if !actx.Identity.Skills.Empty() {
		sections = append(sections, provider.Section{
			Title: "What the user knows (SKILLS.md)",
			Body:  actx.Identity.Skills.Body,
		})
	}
	return sections
}

// DigestResult is what refreshing one project produced.
type DigestResult struct {
	Slug string
	Name string
	// Digest is the generated digest. Zero when Err is set.
	Digest store.Digest
	Usage  provider.Usage
	Err    error
}

// RefreshDigests generates and stores a digest for each source, with at most
// `workers` model calls in flight.
//
// The bound is not a nicety. DESIGN.md §8 records that a claude-cli route is
// one subprocess per call, so an unbounded fan-out over a twelve-project
// portfolio starts twelve `claude` processes at once and spends a shared
// session window as fast as the machine allows.
//
// One project's failure does not abort the rest: every source produces a
// DigestResult, and the caller reports the failures. That mirrors `apex sync`,
// where one bad project has never been allowed to stop the run. A cancelled
// context does stop it, because the user asked.
//
// Results come back in the order the sources were given, whatever order the
// workers finished in, so the report is stable.
func (a *Advisor) RefreshDigests(ctx context.Context, actx *Context, srcs []*project.DigestSource, workers int) ([]DigestResult, error) {
	results := make([]DigestResult, len(srcs))
	if len(srcs) == 0 {
		return results, nil
	}
	if workers < 1 {
		workers = 1
	}

	p, err := a.providerFor(ctx, a.Config.Models.Digest)
	if err != nil {
		return nil, err
	}

	// Generation runs in parallel; the writes do not. modernc.org/sqlite
	// serialises them anyway, and a single writer keeps the failure of one
	// project's store write attributable to that project.
	type generated struct {
		digest store.Digest
		usage  provider.Usage
		err    error
	}
	out := make([]generated, len(srcs))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	for i, src := range srcs {
		i, src := i, src
		g.Go(func() error {
			digest, usage, err := a.GenerateDigest(gctx, p, actx, src)
			out[i] = generated{digest: digest, usage: usage, err: err}
			// Errors are carried, not returned: returning one would cancel
			// gctx and take the rest of the portfolio down with it. Only a
			// cancelled context below stops the run.
			return gctx.Err()
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	for i, src := range srcs {
		results[i] = DigestResult{Slug: src.Slug, Name: src.Name, Usage: out[i].usage, Err: out[i].err}
		if out[i].err != nil {
			continue
		}
		if err := a.Store.UpsertDigest(ctx, out[i].digest); err != nil {
			results[i].Err = err
			continue
		}
		if err := a.Store.MarkProjectSynced(ctx, src.Slug, out[i].digest.GeneratedAt); err != nil {
			results[i].Err = err
			continue
		}
		results[i].Digest = out[i].digest
	}
	return results, nil
}
