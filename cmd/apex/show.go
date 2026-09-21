package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RazerBlade6/apex/internal/store"
)

func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show one action item or idea in full",
		Long: `Print an action item (AI-014) or an idea (IDEA-007) in full, including the
body and the rationale that the list view truncates.

IDs are case-insensitive and the prefix may be omitted: 14 and ai-14 both find
AI-014.

For an action item, this also reports whether the project has changed since
the item was generated. That is what action_items.context_hash is for: an item
written against a blocker you have since resolved is worth knowing about
before you start on it.

This reads only the database. It talks to no model and costs nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
}

func runShow(ctx context.Context, w io.Writer, id string) error {
	sess, err := openSession(ctx, false)
	if err != nil {
		return err
	}
	defer sess.Close()

	width := terminalWidth()
	itemID, ideaID := normalizeID(id)

	if itemID != "" {
		item, err := sess.Store.GetActionItem(ctx, itemID)
		switch {
		case err == nil:
			return showActionItem(ctx, w, sess, item, width)
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
	}
	if ideaID != "" {
		idea, err := sess.Store.GetIdea(ctx, ideaID)
		switch {
		case err == nil:
			writeIdea(w, idea, width)
			fmt.Fprintf(w, "Proposed %s by %s.\n", humanAge(idea.CreatedAt), dash(idea.GeneratedBy))
			if idea.StartedProjectSlug != "" {
				fmt.Fprintf(w, "Started as project %s.\n", idea.StartedProjectSlug)
			}
			return nil
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
	}
	return &unknownIDError{ID: id}
}

func showActionItem(ctx context.Context, w io.Writer, sess *session, item store.ActionItem, width int) error {
	name := item.ProjectSlug
	project, err := sess.Store.GetProject(ctx, item.ProjectSlug)
	switch {
	case err == nil:
		name = project.Name
	case !errors.Is(err, store.ErrNotFound):
		return err
	}

	writeActionItem(w, item, name, width)
	fmt.Fprintf(w, "Proposed %s by %s.\n", humanAge(item.CreatedAt), dash(item.GeneratedBy))
	if !item.UpdatedAt.Equal(item.CreatedAt) {
		fmt.Fprintf(w, "Last changed %s.\n", humanAge(item.UpdatedAt))
	}

	// Provenance (DESIGN.md §7). The stored hash covers the digest set the
	// item was generated from; comparing it against the current one is the
	// whole reason the column exists.
	if item.ContextHash == "" {
		return nil
	}
	adv := sess.advisorFor()
	actx, err := adv.LoadContext(ctx)
	if err != nil {
		return err
	}
	if actx.Hash() == item.ContextHash {
		fmt.Fprintf(w, "Context %s: still current.\n", short(item.ContextHash))
		return nil
	}
	fmt.Fprintf(w, "Context %s: the portfolio has changed since this was generated.\n",
		short(item.ContextHash))
	if d, err := sess.Store.GetDigest(ctx, item.ProjectSlug); err == nil && d.GeneratedAt.After(item.CreatedAt) {
		fmt.Fprintf(w, "  %s's digest was regenerated %s, after this item.\n",
			name, d.GeneratedAt.Format(time.RFC3339))
	}
	return nil
}

// normalizeID turns whatever the user typed into the two IDs it might be.
//
// Both are returned rather than one guessed at, because the numbering is
// global but the prefixes are not interchangeable: "7" is a plausible way to
// ask for either AI-007 or IDEA-007, and looking up both and reporting which
// exists beats making the user remember which kind of thing they are after.
func normalizeID(raw string) (itemID, ideaID string) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	switch {
	case strings.HasPrefix(s, "AI-"):
		return pad("AI-", s[3:]), ""
	case strings.HasPrefix(s, "IDEA-"):
		return "", pad("IDEA-", s[5:])
	case strings.HasPrefix(s, "AI"):
		return pad("AI-", s[2:]), ""
	case strings.HasPrefix(s, "IDEA"):
		return "", pad("IDEA-", s[4:])
	default:
		return pad("AI-", s), pad("IDEA-", s)
	}
}

// pad restores the three-digit form store.nextID writes.
func pad(prefix, digits string) string {
	digits = strings.TrimSpace(digits)
	if digits == "" {
		return ""
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return ""
		}
	}
	for len(digits) < 3 {
		digits = "0" + digits
	}
	return prefix + digits
}

type unknownIDError struct{ ID string }

func (e *unknownIDError) Error() string {
	return fmt.Sprintf("no action item or idea with id %q\n"+
		"  apex items   lists the action items", e.ID)
}

func (e *unknownIDError) UserFixable() bool { return true }
