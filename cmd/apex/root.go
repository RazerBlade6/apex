package main

import (
	"github.com/spf13/cobra"
)

// version is the build version, overridable at link time:
//
//	go build -ldflags "-X main.version=1.2.3" ./cmd/apex
var version = "0.1.0-dev (M2)"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "apex",
		Short: "Personal project management and development hub",
		Long: `Apex holds context about you and your projects, generates action items
and project ideas, and dispatches implementation work to a coding agent.

This build implements milestones M1 and M2: the CLI skeleton, configuration,
credentials, storage, environment verification, and the context layer that
apex sync keeps up to date. review, ideas, items, do, start, and the TUI
arrive in later milestones, and no build so far makes an LLM call.`,
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
	root.AddCommand(newDoctorCmd(), newConfigCmd(), newSyncCmd())
	return root
}
