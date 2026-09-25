package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RazerBlade6/apex/internal/store"
)

func newIdeasCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ideas",
		Short: "Propose new projects that fit you",
		Long: `Read your identity context and your existing portfolio, and propose projects
that do not exist yet.

This is the other half of the advisor loop. apex review proposes work on the
projects you have; this proposes the projects you do not. Both read the same
context, which is the point — nothing else on your machine knows both who you
are and what you have already built.

Ideas are recorded as "proposed". Ones that duplicate something already on the
backlog are reported and not recorded.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIdeas(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func runIdeas(ctx context.Context, w io.Writer) error {
	sess, err := openSession(ctx, false)
	if err != nil {
		return err
	}
	defer sess.Close()

	adv := sess.advisorFor()
	actx, err := adv.LoadContext(ctx)
	if err != nil {
		return err
	}

	warnIdentity(w, sess.Root, actx)
	if len(actx.Digests) == 0 {
		// Not an error, unlike review: a user with no projects yet is
		// precisely who wants project ideas. But with no profile and no
		// portfolio there is nothing specific to reason from, and saying so
		// costs less than letting them find out from the output.
		fmt.Fprintln(w, "No project digests exist yet, so there is nothing to reason from except")
		fmt.Fprintln(w, "whatever identity context you have written. Register projects in PROJECTS.md")
		fmt.Fprintln(w, "and run `apex sync` for ideas that account for what you have already built.")
		fmt.Fprintln(w)
	} else {
		reportDigestAge(w, actx)
	}

	route := sess.Config.Models.Advisor
	fmt.Fprintf(w, "Proposing projects through %s/%s...\n\n", route.Provider, route.Model)

	result, err := adv.Ideas(ctx, actx)
	if err != nil {
		return err
	}

	if len(result.Inserted) == 0 {
		fmt.Fprintln(w, "No new ideas.")
	}
	width := terminalWidth()
	for _, idea := range result.Inserted {
		writeIdea(w, idea, width)
	}

	if len(result.Inserted) > 0 {
		fmt.Fprintf(w, "%s · %s\n", dash(result.Model), describeUsage(result.Usage))
		fmt.Fprintf(w, "Recorded %d idea(s) as proposed.\n", len(result.Inserted))
		fmt.Fprintln(w, "apex show <id>   for one in full")
	}
	if len(result.Duplicates) > 0 {
		fmt.Fprintf(w, "\nSkipped %d proposal(s) already on the backlog:\n", len(result.Duplicates))
		for _, title := range result.Duplicates {
			fmt.Fprintf(w, "  %s\n", title)
		}
	}
	return nil
}

func writeIdea(w io.Writer, idea store.Idea, width int) {
	header := fmt.Sprintf("%s  %s", idea.ID, idea.Title)
	fmt.Fprintln(w, header)
	fmt.Fprintln(w, strings.Repeat("-", min(len(header), width)))
	fmt.Fprintf(w, "%s\n\n", idea.Status)

	if idea.Pitch != "" {
		renderMarkdown(w, idea.Pitch, "", width)
		fmt.Fprintln(w)
	}
	if idea.Rationale != "" {
		fmt.Fprintln(w, "Why you:")
		renderMarkdown(w, idea.Rationale, "  ", width)
		fmt.Fprintln(w)
	}
}
