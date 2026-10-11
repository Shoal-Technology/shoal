package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVersionCmd prints the build metadata set by the release ldflags.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the shoal version, commit and build date",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "shoal %s (commit %s, built %s)\n", version, commit, date)
		},
	}
}
