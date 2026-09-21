package main

import (
	"github.com/spf13/cobra"
)

// version is the build version, overridable at link time:
//
//	go build -ldflags "-X main.version=1.2.3" ./cmd/apex
var version = "0.1.0-dev (M4)"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "apex",
		Short: "Personal project management and development hub",
		Long: `Apex holds context about you and your projects, generates action items
and project ideas, and dispatches implementation work to a coding agent.

This build implements milestones M1 through M4: the CLI skeleton,
configuration, credentials, storage, environment verification, the context
layer, the provider adapters — including claude-cli, which runs inference
through a Claude subscription rather than an API key — and the advisor loop
itself: digest generation, action items, and project ideas. do, start, and
the TUI arrive later.

Three commands reach a model: sync generates digests, review generates action
items, and ideas proposes projects. doctor does so only with --probe. items
and show read the database and cost nothing.`,
		Version:       version,
		SilenceUsage:  true, // a runtime failure is not a usage error
		SilenceErrors: true, // main formats errors itself
		RunE: func(cmd *cobra.Command, args []string) error {
			// The bare `apex` invocation launches the TUI from M6 onwards.
			// Until then, showing help beats a silent no-op.
			return cmd.Help()
		},
	}

	root.SetVersionTemplate("apex {{.Version}}\n")
	root.AddCommand(
		newDoctorCmd(), newConfigCmd(), newSyncCmd(),
		newReviewCmd(), newIdeasCmd(), newItemsCmd(), newShowCmd(),
	)
	return root
}
