package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kaiserern/shoal/api/v1alpha1"
)

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Show and change the server cluster (raft members)",
		Long: `Show and change the members of the server cluster (ADR 0014).

All three commands need admin credentials. A write sent to a follower is
retried on the leader automatically.`,
	}
	cmd.AddCommand(newClusterStatusCmd(), newClusterRemoveCmd(), newClusterJoinBundleCmd())
	return cmd
}

func newClusterStatusCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "List raft members, the leader and the indexes of the server you ask",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.ClusterStatus(ctx, &v1alpha1.ClusterStatusRequest{})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NODE ID\tRAFT ADDRESS\tVOTER\tLEADER")
			for _, m := range resp.GetMembers() {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.GetNodeId(), m.GetRaftAddress(), yesNo(m.GetVoter()), yesNo(m.GetLeader()))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out, "\nLeader API:    %s\n", orNone(resp.GetLeaderApiAddress()))
			fmt.Fprintf(out, "Answered by:   %s\n", resp.GetNodeId())
			fmt.Fprintf(out, "Commit index:  %d\n", resp.GetCommitIndex())
			fmt.Fprintf(out, "Applied index: %d\n", resp.GetAppliedIndex())
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newClusterRemoveCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "remove <node-id>",
		Short: "Remove a server from the raft cluster",
		Long: `Remove a raft member by its node ID (see "shoal cluster status").

The leader applies the removal. It refuses to remove the last voter. You can
remove the leader itself: it steps down once the change commits, and the
others elect a new leader.

The removed server is not stopped for you. Stop its shoal server process
after this command. A removed server that keeps running can still try to
campaign for leadership until it is stopped.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.RemoveServer(ctx, &v1alpha1.RemoveServerRequest{NodeId: args[0]})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s from the cluster (commit index %d).\n"+
				"Stop its shoal server now if it is still running.\n", args[0], resp.GetCommitIndex())
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newClusterJoinBundleCmd() *cobra.Command {
	var (
		cf  clientFlags
		out string
	)
	cmd := &cobra.Command{
		Use:   "join-bundle",
		Short: "Write the cluster CA and secret key to a file for a new server",
		Long: `Write a join bundle: the cluster CA certificate and key and the secret
encryption key. A new server takes it with --raft-join-bundle=<file> and then
needs no manual copy of pki/ or secret.key.

The bundle is a cluster secret. Anyone who has it can issue certificates for
the cluster and decrypt its Secrets. The file is created with mode 0600 and
the command refuses to overwrite an existing file. Move it over a private
channel, pass it to the new server, and delete it afterwards.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" {
				return fmt.Errorf("--out is required")
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.JoinBundle(ctx, &v1alpha1.JoinBundleRequest{})
			if err != nil {
				return err
			}
			if err := writeSecretFile(out, resp.GetBundle()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote join bundle %s (CA hash %s, mode 0600).\n"+
				"It holds the CA key and the secret key: treat it as a cluster secret.\n", out, resp.GetCaHash())
			return nil
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVar(&out, "out", "", "file to write (created with mode 0600; must not exist)")
	return cmd
}

// writeSecretFile creates path with mode 0600 and fails if it exists. A
// partly written file is removed.
func writeSecretFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orNone(s string) string {
	if s == "" {
		return "(none yet)"
	}
	return s
}
