package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"example.com/shoal/api/v1alpha1"
)

// maxBackupBytes bounds the backup a client accepts. The default gRPC limit
// (4 MiB) is small for a cluster with many records.
const maxBackupBytes = 256 << 20

func newBackupCmd() *cobra.Command {
	var (
		cf  clientFlags
		out string
	)
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Write a consistent backup of all cluster records to a file",
		Long: `Write a backup of every record in the cluster (format shoal-backup/v1).

The server takes one point-in-time copy: on the raft store it is read on the
leader after a Raft barrier, so every acknowledged write is included. The
file is JSON with the revision, the creation time, the server version and a
SHA-256 of the records. It is created with mode 0600 and the command refuses
to overwrite an existing file.

Secrets stay encrypted in the file. Restoring it needs the same secret.key
(see "shoal server --restore-from"). Restore creates a new cluster identity:
agents and admin credentials must be issued again.

Needs admin credentials.`,
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
			resp, err := c.control.Backup(ctx, &v1alpha1.BackupRequest{}, grpc.MaxCallRecvMsgSize(maxBackupBytes))
			if err != nil {
				return err
			}
			if err := writeSecretFile(out, resp.GetBackup()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote backup %s (%d records at revision %d, mode 0600).\n"+
				"It holds Secrets encrypted with the cluster secret key: keep it with that key.\n",
				out, resp.GetRecordCount(), resp.GetRevision())
			return nil
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVar(&out, "out", "", "file to write (created with mode 0600; must not exist)")
	return cmd
}
