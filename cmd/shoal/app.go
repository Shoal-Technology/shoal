package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/internal/appspec"
	"github.com/Shoal-Technology/shoal/internal/deploy"
)

func newAppCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Show, log, delete and configure an app deployed from shoal.yaml",
	}
	cmd.AddCommand(newAppStatusCmd(), newAppLogsCmd(), newAppDeleteCmd(), newAppSecretCmd())
	return cmd
}

func newAppStatusCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "status NAME",
		Short: "Show every object of an app, with its state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			objs, err := deploy.AppObjects(cmd.Context(), c.control, args[0])
			if err != nil {
				return err
			}
			if len(objs) == 0 {
				return fmt.Errorf("no objects labelled %s=%s", appspec.LabelApp, args[0])
			}
			out := cmd.OutOrStdout()
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "KIND\tNAME\tSTATE\tMESSAGE")
			for _, r := range deploy.StatusRows(objs) {
				msg := dash(r.Message)
				if r.Issue != "" {
					msg = r.Issue
					if r.Message != "" {
						msg = r.Message + "; " + r.Issue
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", strings.ToLower(r.Kind), r.Name, r.State, msg)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			url := deploy.URLFor(args[0], objs)
			if url == "" {
				url = "none"
			}
			fmt.Fprintf(out, "\nurl: %s\n", url)
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newAppLogsCmd() *cobra.Command {
	var (
		cf     clientFlags
		follow bool
		tail   int32
	)
	cmd := &cobra.Command{
		Use:   "logs NAME [-f] [--tail N]",
		Short: "Print the logs of an app's workload replicas",
		Long: `Print the logs of an app's workload replicas. Each line is prefixed with the
replica name. This is the same path as "shoal logs NAME".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lc := newLogsCmd()
			largs := []string{args[0], fmt.Sprintf("--tail=%d", tail)}
			if follow {
				largs = append(largs, "--follow")
			}
			if cf.config != "" {
				largs = append(largs, "--config", cf.config)
			}
			if cf.server != "" {
				largs = append(largs, "--server", cf.server)
			}
			lc.SetArgs(largs)
			lc.SetOut(cmd.OutOrStdout())
			lc.SetErr(cmd.ErrOrStderr())
			lc.SetIn(cmd.InOrStdin())
			lc.SilenceErrors = true
			lc.SilenceUsage = true
			return lc.ExecuteContext(cmd.Context())
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new output")
	cmd.Flags().Int32Var(&tail, "tail", -1, "recent lines per replica (-1 = all)")
	return cmd
}

func newAppDeleteCmd() *cobra.Command {
	var (
		cf   clientFlags
		data bool
		yes  bool
	)
	cmd := &cobra.Command{
		Use:   "delete NAME [--data] [--yes]",
		Short: "Delete every object of an app",
		Long: `Delete every object labelled app=NAME. Volumes, the Database and the Secret
are kept unless --data is given. The list is printed first; with --data the
command asks for confirmation unless --yes is given.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			out := cmd.OutOrStdout()
			plan, err := deploy.DeleteApp(cmd.Context(), c.control, args[0], deploy.DeleteOptions{
				Data: data,
				Confirm: func(p deploy.DeletePlan) bool {
					for _, r := range p.Delete {
						fmt.Fprintf(out, "delete  %s\n", r)
					}
					for _, r := range p.Keep {
						fmt.Fprintf(out, "keep    %s (data is kept; --data deletes it)\n", r)
					}
					if !data || yes {
						return true
					}
					return askYes(cmd.InOrStdin(), out, fmt.Sprintf("Delete %d objects, including data? [y/N] ", len(p.Delete)))
				},
			})
			if errors.Is(err, deploy.ErrAborted) {
				return errors.New("aborted, nothing deleted")
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "deleted %d objects\n", len(plan.Delete))
			return nil
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().BoolVar(&data, "data", false, "also delete Volumes, the Database and the Secret")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// askYes prints prompt and reports whether the answer is yes.
func askYes(in io.Reader, out io.Writer, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func newAppSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Set the values of an app's secrets",
	}
	var cf clientFlags
	set := &cobra.Command{
		Use:   "set NAME KEY=VALUE...",
		Short: "Set secret values for an app (KEY=- reads the value from stdin)",
		Long: `Set values in the Secret <name>-secrets, which the app's env reads. The
Secret is created when missing. Other keys keep their values. KEY=- reads the
value from stdin (one key per stdin). Values are never printed.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			values, err := deploy.ParseAssignments(args[1:], cmd.InOrStdin())
			if err != nil {
				return err
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			keys, err := deploy.SetSecret(cmd.Context(), c.control, args[0], values)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "secret/%s set: %s (values not shown)\n",
				appspec.SecretsName(args[0]), strings.Join(keys, ", "))
			return nil
		},
	}
	cf.register(set.Flags())
	cmd.AddCommand(set)
	return cmd
}
