package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/internal/errcat"
)

func newExplainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain CODE",
		Short: "Explain an error code (E01 to E15)",
		Long: `Explain an error code shown by deploy, app status or the portal.

Example: shoal explain E07`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, ok := errcat.Get(args[0])
			if !ok {
				return fmt.Errorf("unknown error code %q: the codes are E01 to E15", args[0])
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s: %s\n\nmessage  %s\nfix      %s\n", e.Code, e.Title, e.Template, e.Hint)
			return nil
		},
	}
}
