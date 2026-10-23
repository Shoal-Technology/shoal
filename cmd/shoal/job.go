package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// newJobCmd groups the job commands (ADR 0022). A Job is one job of a pipeline
// run.
func newJobCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "job",
		Short: "Read the jobs of pipeline runs",
	}
	cmd.AddCommand(newJobLogsCmd())
	return cmd
}

func newJobLogsCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "logs JOB",
		Short: "Print the output of a job, oldest line first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.JobLogs(ctx, &v1alpha1.JobLogsRequest{Job: args[0]})
			if err != nil {
				return fmt.Errorf("logs of job %s: %w", args[0], err)
			}
			return printBuildLogs(cmd.OutOrStdout(), resp.GetLines())
		},
	}
	cf.register(cmd.Flags())
	return cmd
}
