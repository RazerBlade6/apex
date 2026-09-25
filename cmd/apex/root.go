package main

import (
	"github.com/spf13/cobra"
)

// version is the build version, overridable at link time:
//
//	go build -ldflags "-X main.version=1.2.3" ./cmd/apex
var version = "1.2.0"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "apex",
		Short: "Personal project management and development hub",
		Long: `Apex holds context about you and your projects, generates action items
and project ideas, and dispatches implementation work to a coding agent.

Run with no arguments it opens the terminal interface: chat with your whole
portfolio in context, browse action items and dispatch one, and review the
registry with its digests. Every view is also a command, so the same work is
scriptable and cron-able.

This build implements milestones M1 through M6: the CLI skeleton,
configuration, credentials, storage, environment verification, the context
layer, the provider adapters — including claude-cli, which runs inference
through a Claude subscription rather than an API key — the advisor loop, the
executor that closes it, and the TUI.

Six commands reach a model. sync, review and ideas run the advisor loop, which
emits text. do and start run the builder loop, which writes code: they
dispatch a brief to a coding agent working inside the project directory, under
an exclusive per-project lock, and spend Claude Code subscription quota rather
than an API key. The chat view reaches a model per turn, and spends one extra
cheap call on a turn that looks like it says something durable about you.
doctor reaches a model only with --probe; items, show, config and profile read
local state and cost nothing.`,
		Version:       version,
		SilenceUsage:  true, // a runtime failure is not a usage error
		SilenceErrors: true, // main formats errors itself
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI(cmd.Context(), nil, nil)
		},
	}

	root.SetVersionTemplate("apex {{.Version}}\n")
	root.AddCommand(
		newDoctorCmd(), newConfigCmd(), newSyncCmd(),
		newReviewCmd(), newIdeasCmd(), newItemsCmd(), newShowCmd(),
		newDoCmd(), newStartCmd(), newProfileCmd(),
	)
	return root
}
