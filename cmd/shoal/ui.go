package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

func newUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Manage the web portal",
	}
	var cf clientFlags
	reset := &cobra.Command{
		Use:   "reset-password",
		Short: "Set a new random password for the web portal's admin user",
		Long: `Set a new random password for the web portal's admin user and print it.
Everyone logged in to the portal is logged out.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.ResetUIPassword(ctx, &v1alpha1.ResetUIPasswordRequest{})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "New web portal password for admin: %s\n", resp.GetPassword())
			return nil
		},
	}
	cf.register(reset.Flags())
	cmd.AddCommand(reset)
	return cmd
}
