package advisor

import (
	"context"
	"fmt"
	"strings"

	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// ideasInstructions is the volatile half of an `apex ideas` call.
//
// The difference from review is the whole point of the command existing
// separately: review proposes work *on* what exists, ideas proposes what does
// not exist yet. The portfolio is in the prompt for both, but here it is
// evidence about the user rather than the thing to be worked on.
const ideasInstructions = `Propose new projects that would fit this user.

These are not tasks on the existing projects — propose those with ` + "`apex review`" + `.
These are projects that do not exist yet.

Rules:
- Propose at most %d, strongest fit first. Two good ideas beat six filler ones.
- Read the portfolio as evidence about the user: what they keep building, what
  they clearly enjoy, what they have built the pieces for without assembling.
- "rationale" must argue from this user specifically — their skills, their
  existing projects, the gaps between them. An idea that would suit any
  developer is not an idea this tool should produce.
- Prefer something they could start this month over something ambitious and
  abstract. "pitch" should end with a concrete first milestone.
- Say if an idea overlaps an existing project, and say what makes it separate.

Ideas already on the backlog, which you must NOT propose again:
%s`

// maxIdeas bounds one `apex ideas` call, for the same reason maxReviewItems
// bounds a review.
const maxIdeas = 5

type ideasResponse struct {
	Ideas []proposedIdea `json:"ideas"`
}

type proposedIdea struct {
	Title     string `json:"title"`
	Pitch     string `json:"pitch"`
	Rationale string `json:"rationale"`
}

// IdeasResult is what one `apex ideas` produced.
type IdeasResult struct {
	Inserted   []store.Idea
	Duplicates []string
	// Usage and Model are what the call cost and what served it; see
	// ReviewResult, which gained both for the same reason.
	Usage provider.Usage
	Model string
}

// Ideas proposes new projects and records the new ones as proposed.
//
// It runs with an empty portfolio, unlike Review: a user with no projects yet
// is exactly who wants project ideas. The output is weaker without digests,
// and the command says so rather than refusing.
func (a *Advisor) Ideas(ctx context.Context, actx *Context) (*IdeasResult, error) {
	existing, err := a.Store.ListIdeas(ctx, "")
	if err != nil {
		return nil, err
	}
	open := map[string]bool{}
	for _, idea := range existing {
		if idea.Status == store.IdeaDismissed {
			continue
		}
		open[normalizeTitle(idea.Title)] = true
	}

	schema, err := schemaBytes(ideasSchema)
	if err != nil {
		return nil, err
	}

	route := a.Config.Models.Advisor
	p, err := a.providerFor(ctx, route)
	if err != nil {
		return nil, err
	}

	instruction := fmt.Sprintf(ideasInstructions, maxIdeas, renderOpenIdeas(existing))
	req := actx.Prompt(instruction).Build(route.Model, route.Effort, listMaxTokens)

	var decoded ideasResponse
	usage, err := p.Structured(ctx, req, schema, &decoded)
	if err != nil {
		return nil, err
	}
	model := usage.Model
	if model == "" {
		model = route.Model
	}

	result := &IdeasResult{Usage: usage, Model: model}
	now := a.now()
	seen := map[string]bool{}

	for _, idea := range decoded.Ideas {
		title := strings.TrimSpace(idea.Title)
		if title == "" {
			continue
		}
		key := normalizeTitle(title)
		if open[key] || seen[key] {
			result.Duplicates = append(result.Duplicates, title)
			continue
		}
		seen[key] = true

		id, err := a.Store.NextIdeaID(ctx)
		if err != nil {
			return nil, err
		}
		row := store.Idea{
			ID:          id,
			Title:       title,
			Pitch:       strings.TrimSpace(idea.Pitch),
			Rationale:   strings.TrimSpace(idea.Rationale),
			Status:      store.IdeaProposed,
			CreatedAt:   now,
			GeneratedBy: model,
		}
		if err := a.Store.InsertIdea(ctx, row); err != nil {
			return nil, err
		}
		result.Inserted = append(result.Inserted, row)
	}
	return result, nil
}

func renderOpenIdeas(ideas []store.Idea) string {
	var b strings.Builder
	for _, idea := range ideas {
		if idea.Status == store.IdeaDismissed {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s (%s)\n", idea.ID, idea.Title, idea.Status)
	}
	if b.Len() == 0 {
		return "(none: the backlog is empty)"
	}
	return strings.TrimRight(b.String(), "\n")
}
