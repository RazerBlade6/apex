package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/contextfs"
	"github.com/RazerBlade6/apex/internal/project"
	"github.com/RazerBlade6/apex/internal/store"
)

func newReviewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "review [project]",
		Short: "Generate action items across the portfolio",
		Long: `Load your identity context and every cached project digest, reason over the
whole portfolio in one call, and record the action items that come back.

New items are recorded as "proposed" and stamped with a hash of the context
they were generated from, so apex can later tell you an item was written
against a state the project has since moved past.

Items that duplicate something already open are reported and not recorded.
Naming a project narrows the reasoning to that project's digest alone, which
is cheaper and worse: the advisor's most valuable work is the comparison
across projects.

This reads digests, never working trees, so it takes no project lock and stays
responsive during a dispatch. Run apex sync first if the digests are stale —
sync is what refreshes them.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var only string
			if len(args) == 1 {
				only = args[0]
			}
			return runReview(cmd.Context(), cmd.OutOrStdout(), only)
		},
	}
}

func runReview(ctx context.Context, w io.Writer, only string) error {
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

	if only != "" {
		slug, err := resolveProjectArg(ctx, sess.Store, only)
		if err != nil {
			return err
		}
		actx = actx.Filter(slug)
		if len(actx.Digests) == 0 {
			return &noDigestForProjectError{Name: only}
		}
	}

	warnIdentity(w, sess.Root, actx)
	reportDigestAge(w, actx)

	route := sess.Config.Models.Advisor
	fmt.Fprintf(w, "Reviewing %d project(s) through %s/%s...\n\n",
		len(actx.Digests), route.Provider, route.Model)

	result, err := adv.Review(ctx, actx)
	if err != nil {
		return err
	}

	if len(result.Inserted) == 0 {
		fmt.Fprintln(w, "No new action items.")
	}
	width := terminalWidth()
	for _, item := range result.Inserted {
		writeActionItem(w, item, actx.ProjectName(item.ProjectSlug), width)
	}

	// The call's cost, which M5's Structured signature made reportable for
	// the first time (DESIGN.md §18). This is the largest prompt Apex sends,
	// so it is the one whose cache behaviour is most worth watching.
	fmt.Fprintf(w, "%s · %s\n", dash(result.Model), describeUsage(result.Usage))

	if len(result.Inserted) > 0 {
		fmt.Fprintf(w, "Recorded %d action item(s) as proposed (context %s).\n",
			len(result.Inserted), short(result.ContextHash))
		fmt.Fprintln(w, "apex items   to list them   ·   apex show <id>   for one in full")
	}
	if len(result.Duplicates) > 0 {
		fmt.Fprintf(w, "\nSkipped %d proposal(s) that duplicate an item already open:\n", len(result.Duplicates))
		for _, title := range result.Duplicates {
			fmt.Fprintf(w, "  %s\n", title)
		}
	}
	if len(result.UnknownProjects) > 0 {
		fmt.Fprintf(w, "\nSkipped %d proposal(s) naming a project that is not in the portfolio: %s\n",
			len(result.UnknownProjects), strings.Join(result.UnknownProjects, ", "))
	}
	return nil
}

// writeActionItem renders one item the way `review` and `show` both want it.
func writeActionItem(w io.Writer, item store.ActionItem, projectName string, width int) {
	header := fmt.Sprintf("%s  %s", item.ID, item.Title)
	fmt.Fprintln(w, header)
	fmt.Fprintln(w, strings.Repeat("-", min(len(header), width)))

	meta := []string{projectName, item.Status}
	if item.Effort != "" {
		meta = append(meta, item.Effort)
	}
	fmt.Fprintf(w, "%s\n\n", strings.Join(meta, " · "))

	if item.Body != "" {
		renderMarkdown(w, item.Body, "", width)
		fmt.Fprintln(w)
	}
	if item.Rationale != "" {
		fmt.Fprintln(w, "Why now:")
		renderMarkdown(w, item.Rationale, "  ", width)
		fmt.Fprintln(w)
	}
}

// warnIdentity says plainly that the output is generic when PROFILE.md and
// SKILLS.md do not exist.
//
// This is the state the command ships into: neither file is written on this
// machine. The honest thing is to name the gap before the output rather than
// let a user read a generic review as a personalised one — and the fix is one
// file away, which is worth saying while they are looking at the result.
func warnIdentity(w io.Writer, root string, actx *advisor.Context) {
	missing := actx.MissingIdentity()
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(w, "No %s in %s.\n", strings.Join(missing, " or "), contextfs.ContextDir(root))
	fmt.Fprintln(w, "Apex knows your projects but nothing about you, so this will be generic")
	fmt.Fprintln(w, "advice about the code rather than advice for you. Writing those two files")
	fmt.Fprintln(w, "is the single highest-leverage thing you can do for everything downstream.")
	fmt.Fprintln(w)
}

// reportDigestAge tells the user when they are reasoning over stale context.
//
// DESIGN.md §13 requires this of every command that depends on digests: the
// default sync mode is explicit, so digests are exactly as old as the last
// time the user ran sync, and a predictable default must not silently
// mislead.
func reportDigestAge(w io.Writer, actx *advisor.Context) {
	if len(actx.Digests) == 0 {
		return
	}
	oldest := actx.Digests[0].GeneratedAt
	for _, d := range actx.Digests {
		if d.GeneratedAt.Before(oldest) {
			oldest = d.GeneratedAt
		}
	}
	fmt.Fprintf(w, "Digests last generated %s.  apex sync   to refresh them.\n\n",
		humanAge(oldest))
}

// resolveProjectArg turns a command-line project name into a slug.
func resolveProjectArg(ctx context.Context, st *store.Store, name string) (string, error) {
	slug := project.Slug(name)
	if _, err := st.GetProject(ctx, slug); err == nil {
		return slug, nil
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if strings.EqualFold(p.Name, name) {
			return p.Slug, nil
		}
	}
	return "", &unknownRegisteredProjectError{Name: name}
}

type unknownRegisteredProjectError struct{ Name string }

func (e *unknownRegisteredProjectError) Error() string {
	return fmt.Sprintf("no registered project named %q\n"+
		"  register it in PROJECTS.md, then run: apex sync", e.Name)
}

func (e *unknownRegisteredProjectError) UserFixable() bool { return true }

type noDigestForProjectError struct{ Name string }

func (e *noDigestForProjectError) Error() string {
	return fmt.Sprintf("%s has no cached digest yet\n  run: apex sync %s", e.Name, e.Name)
}

func (e *noDigestForProjectError) UserFixable() bool { return true }
