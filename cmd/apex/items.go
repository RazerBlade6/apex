package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/RazerBlade6/apex/internal/store"
)

func newItemsCmd() *cobra.Command {
	var status string

	cmd := &cobra.Command{
		Use:   "items",
		Short: "List action items, grouped by project",
		Long: `List every action item apex has recorded, grouped by the project it belongs
to and newest first within each group.

--status narrows the list to one lifecycle state: proposed, accepted,
in_progress, in_review, done, or dismissed.

This reads only the database. It talks to no model and costs nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runItems(cmd.Context(), cmd.OutOrStdout(), status)
		},
	}

	cmd.Flags().StringVar(&status, "status", "",
		"show only items with this status ("+strings.Join(itemStatuses, ", ")+")")
	return cmd
}

// itemStatuses is the lifecycle from DESIGN.md §7, in order.
var itemStatuses = []string{
	store.ItemProposed, store.ItemAccepted, store.ItemInProgress,
	store.ItemInReview, store.ItemDone, store.ItemDismissed,
}

func runItems(ctx context.Context, w io.Writer, status string) error {
	if status != "" && !contains(itemStatuses, status) {
		return &badStatusError{Status: status}
	}

	sess, err := openSession(ctx, false)
	if err != nil {
		return err
	}
	defer sess.Close()

	items, err := sess.Store.ListActionItems(ctx, store.ActionItemFilter{Status: status})
	if err != nil {
		return err
	}
	projects, err := sess.Store.ListProjects(ctx)
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, p := range projects {
		names[p.Slug] = p.Name
	}

	if len(items) == 0 {
		if status != "" {
			fmt.Fprintf(w, "No action items with status %q.\n", status)
		} else {
			fmt.Fprintln(w, "No action items yet.  apex review   to generate some.")
		}
		return nil
	}

	// Grouped by project, because that is how the work is actually done, but
	// the IDs stay globally sequential (DESIGN.md §7) so an item keeps its
	// name when a project is renamed.
	grouped := map[string][]store.ActionItem{}
	for _, item := range items {
		grouped[item.ProjectSlug] = append(grouped[item.ProjectSlug], item)
	}
	slugs := make([]string, 0, len(grouped))
	for slug := range grouped {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	for i, slug := range slugs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		name := names[slug]
		if name == "" {
			name = slug
		}
		fmt.Fprintf(w, "%s\n", name)

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, item := range grouped[slug] {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				item.ID, item.Status, dash(item.Effort), truncate(item.Title, 72))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	fmt.Fprintf(w, "\n%d item(s).  apex show <id>   for one in full\n", len(items))
	return nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if i := strings.LastIndexByte(cut, ' '); i > limit/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// humanAge renders a timestamp as an age, which is what a reader of a digest
// or an item actually wants to know.
func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minute(s) ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hour(s) ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d day(s) ago", int(d.Hours()/24))
	}
}

type badStatusError struct{ Status string }

func (e *badStatusError) Error() string {
	return fmt.Sprintf("%q is not an action item status\n  one of: %s",
		e.Status, strings.Join(itemStatuses, ", "))
}

func (e *badStatusError) UserFixable() bool { return true }
