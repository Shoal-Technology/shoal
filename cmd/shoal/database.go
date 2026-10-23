package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/database"
)

// newDBCmd groups the managed database commands (ADR 0016, pass D).
func newDBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Back up and restore managed databases",
	}
	cmd.AddCommand(newDBBackupsCmd(), newDBBackupCmd(), newDBRestoreCmd())
	return cmd
}

func newDBBackupsCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "backups DATABASE",
		Short: "List the logical backups of a database, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.DatabaseBackups(ctx, &v1alpha1.DatabaseBackupsRequest{Database: args[0], Project: currentProject})
			if err != nil {
				return err
			}
			return printBackups(cmd.OutOrStdout(), resp.GetBackups())
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newDBBackupCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "backup DATABASE",
		Short: "Take a logical backup of a database now",
		Long: `Ask for a logical backup (pg_dump, custom format) of a Ready database now. The
backup runs on the database's node; "shoal db backups DATABASE" shows when it is
ok. The backup is stored on the database's data volume.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.BackupDatabase(ctx, &v1alpha1.BackupDatabaseRequest{Database: args[0], Project: currentProject})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "backup %s %s\n", resp.GetBackup().GetId(), resp.GetBackup().GetStatus())
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newDBRestoreCmd() *cobra.Command {
	var cf clientFlags
	var from, into string
	cmd := &cobra.Command{
		Use:   "restore DATABASE --from BACKUP --into NEW",
		Short: "Restore a backup into a new database",
		Long: `Create database NEW from backup BACKUP of DATABASE. The restore loads the dump
into NEW once NEW is Ready. DATABASE is never written: its data stays as it is.
NEW must not exist. Its password is the password of DATABASE.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			source := args[0]
			if into == source {
				return fmt.Errorf("--into must be a new database, not %s", source)
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.DatabaseBackups(ctx, &v1alpha1.DatabaseBackupsRequest{Database: source, Project: currentProject})
			if err != nil {
				return err
			}
			var backup *v1alpha1.DatabaseBackupRecord
			for _, b := range resp.GetBackups() {
				if b.GetId() == from {
					backup = b
				}
			}
			switch {
			case backup == nil:
				return fmt.Errorf("backup %s of database %s not found (see shoal db backups %s)", from, source, source)
			case backup.GetStatus() != database.BackupOK:
				return fmt.Errorf("backup %s is %s, not ok", from, backup.GetStatus())
			}
			got, err := c.control.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindDatabase, Name: source, Project: currentProject})
			if err != nil {
				return err
			}
			d := got.GetDatabase()
			if d == nil {
				return fmt.Errorf("database %s not found", source)
			}
			spec := &v1alpha1.DatabaseSpec{
				Engine:      d.GetSpec().GetEngine(),
				Version:     backup.GetVersion(), // the exact minor the dump was taken from
				Size:        d.GetSpec().GetSize(),
				Resources:   d.GetSpec().GetResources(),
				RestoreFrom: from,
			}
			obj := &v1alpha1.Object{Kind: &v1alpha1.Object_Database{Database: &v1alpha1.Database{
				ApiVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindDatabase,
				Metadata: &v1alpha1.ObjectMeta{Name: into}, Spec: spec,
			}}}
			if _, err := c.control.Apply(ctx, &v1alpha1.ApplyRequest{Objects: []*v1alpha1.Object{obj}}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "database/%s restore from %s requested (see shoal get databases)\n", into, from)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "backup ID to restore (from shoal db backups)")
	cmd.Flags().StringVar(&into, "into", "", "name of the new database")
	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("into")
	cf.register(cmd.Flags())
	return cmd
}

// printBackups lists backups as a table, one per line.
func printBackups(w io.Writer, backups []*v1alpha1.DatabaseBackupRecord) error {
	if len(backups) == 0 {
		fmt.Fprintln(w, "no backups")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCREATED\tSTATUS\tREASON\tVERSION\tSIZE\tSHA256\tERROR")
	for _, b := range backups {
		created := b.GetCreatedAt().AsTime().UTC().Format(time.RFC3339)
		sum := b.GetSha256()
		if len(sum) > 12 {
			sum = sum[:12]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", b.GetId(), created, b.GetStatus(), b.GetReason(),
			b.GetVersion(), b.GetSize(), sum, oneLine(strings.TrimSpace(b.GetError())))
	}
	return tw.Flush()
}
