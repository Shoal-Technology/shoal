package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// noteStream marks server notes (e.g. an unreachable node) among log lines.
const noteStream = "shoal"

func newLogsCmd() *cobra.Command {
	var (
		cf         clientFlags
		replica    string
		follow     bool
		tail       int32
		timestamps bool
	)
	cmd := &cobra.Command{
		Use:   "logs WORKLOAD",
		Short: "Print the logs of a workload's replicas",
		Long: `Print the logs of a workload's replicas, fetched from their nodes.

Lines are prefixed with the replica name unless --replica is given. Without
--follow the lines of all replicas are merged in time order; with it they are
shown as they arrive, until interrupted.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			// A follow runs until interrupted; anything else gets the timeout.
			ctx, cancel := c.ctx(cmd.Context())
			if follow {
				cancel()
				ctx, cancel = context.WithCancel(cmd.Context())
			}
			defer cancel()
			stream, err := c.control.Logs(ctx, &v1alpha1.LogsRequest{Workload: args[0], Replica: replica, Follow: follow, Tail: tail})
			if err != nil {
				return err
			}
			for {
				l, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					if follow && cmd.Context().Err() != nil {
						return nil // interrupted
					}
					return err
				}
				printLogLine(cmd.OutOrStdout(), cmd.ErrOrStderr(), l, replica == "", timestamps)
			}
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVar(&replica, "replica", "", "only this replica")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new output")
	cmd.Flags().Int32Var(&tail, "tail", -1, "recent lines per replica (-1 = all)")
	cmd.Flags().BoolVarP(&timestamps, "timestamps", "t", false, "show timestamps")
	return cmd
}

func printLogLine(stdout, stderr io.Writer, l *v1alpha1.LogLine, prefix, timestamps bool) {
	out := stdout
	if l.GetStream() == "stderr" {
		out = stderr
	}
	var head string
	if l.GetStream() == noteStream {
		out, head = stderr, "shoal: "
	}
	if prefix {
		head += "[" + l.GetReplica() + "] "
	}
	if timestamps {
		head += l.GetTime().AsTime().Local().Format(time.RFC3339Nano) + " "
	}
	fmt.Fprintln(out, head+l.GetLine())
}
