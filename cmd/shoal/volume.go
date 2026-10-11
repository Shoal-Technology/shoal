package main

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kaiserern/shoal/internal/volume"
)

// newVolumeCmd groups the node-local volume commands (ADR 0016).
func newVolumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Manage node-local volumes on this node",
	}
	var stateDir string
	purge := &cobra.Command{
		Use:   "purge NAME",
		Short: "Delete the retained data of a deleted volume",
		Long: `Delete the data that a deleted volume kept (retention Retain). Run this on the
node that holds the data: the data lives on that node's disk, not in the
server. Only entries named NAME-<digits> are removed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			removed, err := volume.Purge(filepath.Join(stateDir, "volumes"), args[0])
			if err != nil {
				return err
			}
			if len(removed) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no retained data for volume %q on this node\n", args[0])
				return nil
			}
			for _, r := range removed {
				fmt.Fprintf(cmd.OutOrStdout(), "purged %s\n", r)
			}
			return nil
		},
	}
	purge.Flags().StringVar(&stateDir, "state-dir", "/var/lib/shoal", "the agent's state directory (same as the agent's --state-dir)")
	cmd.AddCommand(purge)
	return cmd
}
