package advisor

import (
	"context"
	"fmt"
	"strings"

	"github.com/RazerBlade6/apex/internal/provider"
	"github.com/RazerBlade6/apex/internal/store"
)

// reviewInstructions is the volatile half of an `apex review` call: it goes in
// the user message, after the cache breakpoint, because only the identity
// context and the digests belong in the cached prefix (DESIGN.md §8).
const reviewInstructions = `Review the portfolio above and propose the action items that would move it
forward most right now.

Rules:
- Propose at most %d items in total, across all projects. Fewer is better than
  padding: an item that is not worth doing this week is noise.
- Ground every item in something the digests actually say. If a digest names a
  blocker, that is the strongest candidate there is.
- Set "project" to a project's slug exactly as it appears in parentheses in
  the portfolio above. Do not invent a project.
- "title" is one imperative line. "body" says what to do and how you would
  know it was done. "rationale" says why now, referring to the digest.
- Prefer the item that unblocks something over the item that polishes
  something.

Action items already open, which you must NOT propose again:
%s`

// maxReviewItems bounds one review. It is a prompt instruction rather than a
// schema constraint because JSON Schema's maxItems is outside the strict
// subset every vendor accepts, and because a hard cut would drop the model's
// own ordering rather than its worst suggestions.
const maxReviewItems = 8

// reviewResponse is the schema's shape.
type reviewResponse struct {
	Items []proposedItem `json:"items"`
}

type proposedItem struct {
	Project   string `json:"project"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Rationale string `json:"rationale"`
	Effort    string `json:"effort"`
}

// ReviewResult is what one `apex review` produced.
type ReviewResult struct {
	// Inserted are the action items written to the database, in the order
	// the model proposed them.
	Inserted []store.ActionItem
	// Duplicates are titles that matched an item already open. Reported, not
	// hidden: a model proposing the same thing twice is a signal about the
	// portfolio, not an error.
	Duplicates []string
	// UnknownProjects are items naming a project no digest covers. They are
	// skipped rather than attached to a guess.
	UnknownProjects []string
	// ContextHash is the provenance stamp every inserted item carries.
	ContextHash string
	// Usage is what the call cost. It exists because Structured now reports
	// it (DESIGN.md §18): this is the largest prompt Apex ever sends —
	// identity context plus every digest — and until M5 it was the one call
	// whose token and cache accounting was invisible.
	Usage provider.Usage
	// Model is the model that actually served the call, which is what every
	// inserted item records as generated_by.
	Model string
}

// Review generates action items across the portfolio and records the new ones.
//
// The flow is DESIGN.md §14: load identity and digests, one Structured call,
// deduplicate against existing non-dismissed items, insert as proposed with a
// context hash.
//
// Deduplication happens in Go rather than by asking the model nicely. The
// prompt does list what is already open — which is what stops the model
// wasting its output on repeats — but a model that ignores the list must not
// be able to fill the table with duplicates.
func (a *Advisor) Review(ctx context.Context, actx *Context) (*ReviewResult, error) {
	if len(actx.Digests) == 0 {
		return nil, ErrNoDigests
	}

	existing, err := a.Store.ListActionItems(ctx, store.ActionItemFilter{})
	if err != nil {
		return nil, err
	}
	open := openTitles(existing)

	schema, err := schemaBytes(actionItemsSchema)
	if err != nil {
		return nil, err
	}

	route := a.Config.Models.Advisor
	p, err := a.providerFor(ctx, route)
	if err != nil {
		return nil, err
	}

	instruction := fmt.Sprintf(reviewInstructions, maxReviewItems, renderOpenItems(existing))
	req := actx.Prompt(instruction).Build(route.Model, route.Effort, listMaxTokens)

	var decoded reviewResponse
	usage, err := p.Structured(ctx, req, schema, &decoded)
	if err != nil {
		return nil, err
	}

	// The serving model, not the route's alias. M4 could only record what it
	// asked for, so action_items.generated_by said "opus" while
	// digests.model said the dated id the CLI resolved it to — two columns,
	// two answers, one run (DESIGN.md §18). The route's model is still the
	// fallback, because a recorded blank is worse than a recorded intent.
	model := usage.Model
	if model == "" {
		model = route.Model
	}

	result := &ReviewResult{ContextHash: actx.Hash(), Usage: usage, Model: model}
	now := a.now()
	seen := map[string]bool{}

	for _, item := range decoded.Items {
		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}
		slug, ok := resolveProject(actx, item.Project)
		if !ok {
			result.UnknownProjects = append(result.UnknownProjects, item.Project)
			continue
		}
		key := slug + "\x00" + normalizeTitle(title)
		// Both halves matter: the first catches a repeat of something
		// already open, the second catches the model proposing the same
		// item twice within one response.
		if open[key] || seen[key] {
			result.Duplicates = append(result.Duplicates, title)
			continue
		}
		seen[key] = true

		id, err := a.Store.NextActionItemID(ctx)
		if err != nil {
			return nil, err
		}
		row := store.ActionItem{
			ID:          id,
			ProjectSlug: slug,
			Title:       title,
			Body:        strings.TrimSpace(item.Body),
			Rationale:   strings.TrimSpace(item.Rationale),
			Effort:      normalizeEffort(item.Effort),
			Status:      store.ItemProposed,
			CreatedAt:   now,
			UpdatedAt:   now,
			GeneratedBy: model,
			ContextHash: result.ContextHash,
		}
		if err := a.Store.InsertActionItem(ctx, row); err != nil {
			return nil, err
		}
		result.Inserted = append(result.Inserted, row)
	}
	return result, nil
}

// ErrNoDigests reports an advisor call with nothing to reason over. It is
// user-fixable and the fix is always the same command.
var ErrNoDigests = &NoDigestsError{}

// NoDigestsError is returned when no project digest has been generated yet.
type NoDigestsError struct{}

func (e *NoDigestsError) Error() string {
	return "no project digests exist yet\n" +
		"  register projects in PROJECTS.md, then run: apex sync"
}

func (e *NoDigestsError) UserFixable() bool { return true }

// openTitles indexes the items a new proposal must not duplicate: everything
// that is not dismissed.
//
// A dismissed item is deliberately not in the index. The user said no to it
// once; if the portfolio has moved and the model raises it again, that is the
// advisor doing its job, and hiding it would make a dismissal permanent in a
// way nothing told the user it would be. A `done` item IS in the index,
// because re-proposing finished work is noise.
func openTitles(items []store.ActionItem) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		if item.Status == store.ItemDismissed {
			continue
		}
		out[item.ProjectSlug+"\x00"+normalizeTitle(item.Title)] = true
	}
	return out
}

// renderOpenItems lists what is already open, for the prompt.
func renderOpenItems(items []store.ActionItem) string {
	var b strings.Builder
	for _, item := range items {
		if item.Status == store.ItemDismissed {
			continue
		}
		fmt.Fprintf(&b, "- [%s] %s: %s (%s)\n",
			item.ProjectSlug, item.ID, item.Title, item.Status)
	}
	if b.Len() == 0 {
		return "(none: this is the first review)"
	}
	return strings.TrimRight(b.String(), "\n")
}

// resolveProject maps whatever the model wrote into a slug that exists in the
// context it was given.
//
// The schema asks for the slug and the prompt shows it, so the exact match is
// the normal path. The name and the slugified name are accepted too, because
// a model handed "ScholarRAG (scholarrag)" sometimes returns the half a human
// would. Anything else is reported to the user, never attached to the nearest
// project: an action item on the wrong project is worse than one that did not
// land.
func resolveProject(actx *Context, want string) (string, bool) {
	want = strings.TrimSpace(want)
	if want == "" {
		return "", false
	}
	covered := map[string]bool{}
	for _, d := range actx.Digests {
		covered[d.ProjectSlug] = true
	}
	if covered[want] {
		return want, true
	}
	for slug := range covered {
		p, ok := actx.Projects[slug]
		if ok && strings.EqualFold(p.Name, want) {
			return slug, true
		}
	}
	if normalized := normalizeSlug(want); covered[normalized] {
		return normalized, true
	}
	return "", false
}

// normalizeSlug mirrors project.Slug without importing it: internal/project
// is about the filesystem and the registry, and this package only needs the
// string transformation.
func normalizeSlug(name string) string {
	return strings.ReplaceAll(normalizeTitle(name), " ", "-")
}

// normalizeEffort keeps action_items.effort to the three values DESIGN.md §7
// documents, rather than trusting an enum the vendor may or may not have
// enforced.
func normalizeEffort(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "small", "medium", "large":
		return strings.ToLower(strings.TrimSpace(s))
	default:
		return ""
	}
}
