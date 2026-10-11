package main

import (
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"

	"example.com/shoal/internal/appspec"
	"example.com/shoal/internal/deploy"
)

func newDeployCmd() *cobra.Command {
	var (
		cf        clientFlags
		file      string
		dryRun    bool
		prune     bool
		pruneData bool
		wait      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "deploy [-f shoal.yaml]",
		Short: "Deploy an app from shoal.yaml",
		Long: `Deploy an app from shoal.yaml (ADR 0017).

The file is expanded into the objects of the app (labelled app=<name>) and
applied as one batch. --dry-run prints what would change and writes nothing.
--prune deletes labelled objects that the file no longer has; Volumes,
Databases and Secrets are kept unless --prune-data is also given. Deploy then
waits until the workload has its replicas ready, the database is Ready and
the route is Ready. --wait=0 skips the wait.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				file = args[0]
			}
			if pruneData && !prune {
				return errors.New("--prune-data needs --prune")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			app, err := appspec.Parse(data)
			if err != nil {
				return err
			}
			objs, err := appspec.Expand(app)
			if err != nil {
				return err
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = deploy.Run(cmd.Context(), c.control, deploy.Options{
				App:       app.Name,
				Desired:   objs,
				DryRun:    dryRun,
				Prune:     prune,
				PruneData: pruneData,
				Wait:      wait,
				Out:       cmd.OutOrStdout(),
			})
			return err
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVarP(&file, "file", "f", "shoal.yaml", "app spec to deploy")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would change; write nothing")
	cmd.Flags().BoolVar(&prune, "prune", false, "delete labelled objects the spec no longer has")
	cmd.Flags().BoolVar(&pruneData, "prune-data", false, "with --prune, also delete Volumes, Databases and Secrets")
	cmd.Flags().DurationVar(&wait, "wait", 5*time.Minute, "how long to wait for readiness (0 = do not wait)")
	return cmd
}
