package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RazerBlade6/apex/internal/advisor"
	"github.com/RazerBlade6/apex/internal/config"
	"github.com/RazerBlade6/apex/internal/contextfs"
)

func newProfileCmd() *cobra.Command {
	var forget int

	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Show what apex has learned about you, and forget any of it",
		Long: `List the observations apex has recorded about you, with the date each was
learned and the remark it came from.

Apex owns one section of PROFILE.md and SKILLS.md — a trailing "## Observed"
block — and writes nothing else in either file. Everything you wrote yourself
is untouched. Entries are added during a chat when a turn reveals something
durable, superseded when a later turn contradicts them, and capped so they
cannot grow without bound: identity context is re-sent on every call apex
makes.

--forget <n> removes one observation by the number shown here. If an
observation is right and you would rather say it in your own words, move it
into your prose above the block and forget it here.

This reads and writes only markdown. It talks to no model and costs nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProfile(cmd.Context(), cmd.OutOrStdout(), forget)
		},
	}

	cmd.Flags().IntVar(&forget, "forget", 0,
		"remove observation <n>, as numbered by `apex profile`")
	return cmd
}

func runProfile(ctx context.Context, w io.Writer, forget int) error {
	// Deliberately not openSession: this command reads two markdown files and
	// never opens the database, so refusing it against a pending migration
	// would stop the user inspecting their own identity context for a reason
	// that has nothing to do with it.
	root, err := config.Root()
	if err != nil {
		return err
	}
	if _, err := config.EnsureLayout(ctx); err != nil {
		return err
	}

	obs, err := advisor.LoadObservations(ctx, root)
	if err != nil {
		return err
	}

	if forget != 0 {
		entry, err := obs.Forget(forget)
		if err != nil {
			return err
		}
		if err := obs.Save(ctx); err != nil {
			return err
		}
		fmt.Fprintf(w, "Forgot %d: %s\n", forget, entry.Obs.Claim)
		fmt.Fprintf(w, "  removed from %s\n", identityFileName(entry.File))
		return nil
	}

	writeIdentityStatus(w, obs)

	list := obs.List()
	if len(list) == 0 {
		fmt.Fprintln(w, "\nApex has not learned anything about you yet.")
		fmt.Fprintln(w, "It records an observation when a chat turn reveals something durable.")
		fmt.Fprintln(w, "  apex          opens the chat")
		return nil
	}

	fmt.Fprintf(w, "\nObserved (%d of %d):\n", len(list), 2*contextfs.MaxObservations)
	now := time.Now()
	width := terminalWidth()
	for _, e := range list {
		fmt.Fprintf(w, "\n%2d. [%s] %s\n", e.N, e.File, strings.TrimSpace(e.Obs.Claim))
		if src := strings.TrimSpace(e.Obs.Source); src != "" {
			writeWrapped(w, "source: "+src, "      ", "      ", width)
		}
		fmt.Fprintf(w, "      learned %s\n", advisor.ObservationAge(e.Obs, now))
	}
	fmt.Fprintln(w, "\n  apex profile --forget <n>   to remove one")
	return nil
}

// writeIdentityStatus reports the two documents themselves.
//
// It is the same check `apex doctor` makes (DESIGN.md §18, item 13) and it
// belongs here too: an observation list is meaningless without knowing whether
// there is any hand-written identity context around it.
func writeIdentityStatus(w io.Writer, obs *advisor.Observations) {
	for _, f := range []struct {
		name string
		file *contextfs.IdentityFile
	}{
		{contextfs.ProfileFile, obs.Profile},
		{contextfs.SkillsFile, obs.Skills},
	} {
		switch {
		case !f.file.Present:
			fmt.Fprintf(w, "%-12s absent     %s\n", f.name, f.file.Path)
		case strings.TrimSpace(f.file.UserBody()) == "":
			fmt.Fprintf(w, "%-12s empty      %s\n", f.name, f.file.Path)
		default:
			fmt.Fprintf(w, "%-12s %-10s %s\n", f.name,
				fmt.Sprintf("%d line(s)", countLines(f.file.UserBody())), f.file.Path)
		}
	}
}

func countLines(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// identityFileName maps an observation's file tag to the document it names.
func identityFileName(file string) string {
	if file == advisor.ObservationSkills {
		return contextfs.SkillsFile
	}
	return contextfs.ProfileFile
}
