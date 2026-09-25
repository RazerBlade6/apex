package main

import (
	"github.com/spf13/cobra"
)

// version is the build version, overridable at link time:
//
//	go build -ldflags "-X main.version=1.2.3" ./cmd/apex
var version = "0.1.0-dev (M5)"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "apex",
		Short: "Personal project management and development hub",
		Long: `Apex holds context about you and your projects, generates action items
and project ideas, and dispatches implementation work to a coding agent.

This build implements milestones M1 through M5: the CLI skeleton,
configuration, credentials, storage, environment verification, the context
layer, the provider adapters — including claude-cli, which runs inference
through a Claude subscription rather than an API key — the advisor loop, and
the executor that closes it. The TUI arrives with M6.

Five commands reach a model. sync, review and ideas run the advisor loop,
which emits text. do and start run the builder loop, which writes code: they
dispatch a brief to a coding agent working inside the project directory, under
an exclusive per-project lock, and spend Claude Code subscription quota rather
than an API key. doctor reaches a model only with --probe; items and show read
the database and cost nothing.`,
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
		newDoCmd(), newStartCmd(),
	)
	return root
}
