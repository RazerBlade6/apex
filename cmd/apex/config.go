package main

import (
	"errors"
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/RazerBlade6/apex/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show resolved configuration and key sources",
		Long: `Print the effective configuration and report where each API key resolved
from. Key values are never printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			cfg, err := config.Load(ctx)
			if err != nil {
				return err
			}

			body, err := cfg.Marshal()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "# %s\n\n", cfg.Path())
			fmt.Fprintf(out, "%s\n", body)

			if errs := cfg.Validate(); len(errs) > 0 {
				msgs := make([]string, 0, len(errs))
				for _, e := range errs {
					msgs = append(msgs, e.Error())
				}
				sort.Strings(msgs)
				fmt.Fprintln(out, "# problems")
				for _, m := range msgs {
					fmt.Fprintf(out, "#   %s\n", m)
				}
				fmt.Fprintln(out)
			}

			fmt.Fprintln(out, "# api keys")
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "# PROVIDER\tSOURCE\tORIGIN")
			for _, provider := range config.ValidProviders {
				key, err := config.ResolveKey(ctx, provider)
				if err != nil {
					var missing *config.MissingKeyError
					if errors.As(err, &missing) {
						fmt.Fprintf(tw, "# %s\t%s\t%s or $%s\n",
							provider, "not found", missing.Service, missing.EnvVar)
						continue
					}
					return err
				}
				// key.Value() is deliberately never printed.
				fmt.Fprintf(tw, "# %s\t%s\t%s\n", provider, key.Source, key.Origin)
			}
			return tw.Flush()
		},
	}
	return cmd
}
