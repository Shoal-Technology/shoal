package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/pipeline"
	"github.com/Shoal-Technology/shoal/internal/resource"
)

// newPipelineCmd groups the pipeline commands (ADR 0022, pass A). A Pipeline is
// the definition for a repository; a run reads shoal-ci.yml at a commit.
func newPipelineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pipeline",
		Short: "Run pipelines declared in shoal-ci.yml",
		Long: `Run pipelines declared in shoal-ci.yml at the root of a repository. A Pipeline
is the definition for one repository; "shoal pipeline run" makes one run of it,
reading the file at a commit. Pass A runs jobs through the pipeline controller
only: no container starts until a runner is configured.`,
	}
	cmd.AddCommand(newPipelineValidateCmd(), newPipelineCreateCmd(), newPipelineRunCmd(),
		newPipelineLsCmd(), newPipelineRunsCmd(), newPipelineCancelCmd(), newPipelinePlayCmd(),
		newPipelineApproveCmd(), newPipelineRejectCmd())
	return cmd
}

func newPipelineValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "validate FILE",
		Aliases: []string{"lint"},
		Short:   "Check a shoal-ci.yml without a server (- for stdin)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := readManifestFile(cmd, args[0])
			if err != nil {
				return err
			}
			src, err := pipeline.Parse(data)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: ok (%d stages, %d jobs)\n", args[0], len(src.GetStages()), len(src.GetJobs()))
			return nil
		},
	}
}

func newPipelineCreateCmd() *cobra.Command {
	var (
		cf   clientFlags
		file string
	)
	cmd := &cobra.Command{
		Use:   "create -f FILE",
		Short: "Create or update Pipelines from a YAML file (- for stdin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := readManifestFile(cmd, file)
			if err != nil {
				return err
			}
			objs, err := pipelineObjects(data)
			if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.Apply(ctx, &v1alpha1.ApplyRequest{Objects: objs})
			for _, r := range resp.GetResults() {
				fmt.Fprintf(cmd.OutOrStdout(), "%s/%s %s\n", strings.ToLower(r.Kind), r.Name, r.Action)
			}
			return err
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVarP(&file, "file", "f", "", "manifest file with Pipeline objects, or - for stdin (required)")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func newPipelineRunCmd() *cobra.Command {
	var (
		cf  clientFlags
		ref string
	)
	cmd := &cobra.Command{
		Use:   "run PIPELINE",
		Short: "Start a run of a pipeline, reading shoal-ci.yml from the checkout here",
		Long: `Start a run of a pipeline. The file is read from the git repository in the
current directory, at --ref (default: the pipeline's ref), and sent to the server
with the commit it was read at and the paths the commit changed. Run it from a
checkout of the pipeline's repository.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			obj, err := c.control.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindPipeline, Name: args[0], Project: currentProject})
			if err != nil {
				return err
			}
			spec := obj.GetPipeline().GetSpec()
			if ref == "" {
				ref = spec.GetRef()
			}
			path := spec.GetPath()
			if path == "" {
				path = resource.DefaultPipelinePath
			}
			commit, err := gitOutput("rev-parse", "--verify", ref+"^{commit}")
			if err != nil {
				return err
			}
			commit = strings.TrimSpace(commit)
			file, err := gitOutput("show", commit+":"+path)
			if err != nil {
				return fmt.Errorf("read %s at %s: %w", path, ref, err)
			}
			changed := changedPaths(commit)
			resp, err := c.control.StartPipelineRun(ctx, &v1alpha1.StartPipelineRunRequest{Project: currentProject,
				Pipeline: args[0], Ref: ref, Commit: commit, File: file, ChangedPaths: changed,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pipelinerun/%s created\n", resp.GetRun())
			return nil
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVar(&ref, "ref", "", "branch, tag or commit to read (default: the pipeline's ref)")
	return cmd
}

func newPipelineLsCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List Pipelines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindPipeline, Project: currentProject})
			if err != nil {
				return err
			}
			return printPipelines(cmd.OutOrStdout(), resp.GetItems(), time.Now())
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newPipelineRunsCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "runs [PIPELINE]",
		Short: "List pipeline runs, newest first, optionally only those of one pipeline",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindPipelineRun, Project: currentProject})
			if err != nil {
				return err
			}
			pipelineName := ""
			if len(args) == 1 {
				pipelineName = args[0]
			}
			return printPipelineRuns(cmd.OutOrStdout(), filterPipelineRuns(resp.GetItems(), pipelineName), time.Now())
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newPipelineCancelCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "cancel RUN",
		Short: "Cancel a pipeline run that has not finished",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			if _, err := c.control.CancelPipelineRun(ctx, &v1alpha1.CancelPipelineRunRequest{Run: args[0], Project: currentProject}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pipelinerun/%s cancel requested\n", args[0])
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newPipelinePlayCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "play JOB",
		Short: "Start a manual job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			if _, err := c.control.PlayJob(ctx, &v1alpha1.PlayJobRequest{Job: args[0], Project: currentProject}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "job/%s play requested\n", args[0])
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

// newPipelineApproveCmd records an approval of a job at an environment's gate
// (ADR 0022, section 10). The approver is the admin key until projects have roles.
func newPipelineApproveCmd() *cobra.Command {
	return newPipelineDecideCmd("approve RUN JOB", "Approve a job that waits at an environment's approval gate", false)
}

// newPipelineRejectCmd records a rejection of a job at a gate. The job ends
// Cancelled.
func newPipelineRejectCmd() *cobra.Command {
	return newPipelineDecideCmd("reject RUN JOB", "Reject a job that waits at an approval gate; it ends Cancelled", true)
}

// newPipelineDecideCmd is approve or reject. Both name a run and one of its jobs.
func newPipelineDecideCmd(use, short string, reject bool) *cobra.Command {
	var cf clientFlags
	var comment string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			job := resource.JobName(args[0], args[1])
			if reject {
				if _, err := c.control.RejectJob(ctx, &v1alpha1.RejectJobRequest{Job: job, Comment: comment, Project: currentProject}); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "job/%s rejected\n", job)
				return nil
			}
			if _, err := c.control.ApproveJob(ctx, &v1alpha1.ApproveJobRequest{Job: job, Comment: comment, Project: currentProject}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "job/%s approved\n", job)
			return nil
		},
	}
	cmd.Flags().StringVar(&comment, "comment", "", "a note recorded with the decision (at most 500 bytes)")
	cf.register(cmd.Flags())
	return cmd
}

// gitOutput runs git and returns its standard output. A failure carries git's
// own message.
func gitOutput(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// changedPaths returns the paths a commit changed against its first parent.
// A commit with no parent, or no git, changes nothing as far as rules know.
func changedPaths(commit string) []string {
	out, err := gitOutput("diff", "--name-only", commit+"^", commit)
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

// pipelineObjects decodes a manifest that must hold only Pipeline objects. Each
// one is checked locally, so errors show before a round trip.
func pipelineObjects(data []byte) ([]*v1alpha1.Object, error) {
	objs, err := resource.Decode(data)
	if err != nil {
		return nil, err
	}
	var errs []error
	for i, o := range objs {
		if o.GetPipeline() == nil {
			errs = append(errs, fmt.Errorf("object %d is not a Pipeline: create takes only Pipeline objects", i+1))
			continue
		}
		if kind, name, _, err := resource.Prepare(o); err != nil {
			errs = append(errs, fmt.Errorf("%s %q: %w", strings.ToLower(kind), name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return objs, nil
}

// filterPipelineRuns keeps the runs of pipeline, or every run when it is "".
func filterPipelineRuns(items []*v1alpha1.Object, pipelineName string) []*v1alpha1.Object {
	var out []*v1alpha1.Object
	for _, o := range items {
		r := o.GetPipelineRun()
		if r == nil {
			continue
		}
		if pipelineName == "" || r.GetSpec().GetPipeline() == pipelineName {
			out = append(out, o)
		}
	}
	return out
}

// printPipelines writes Pipelines as a table.
func printPipelines(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tREPO\tREF\tPATH\tLAST RUN\tPHASE\tAGE")
	for _, o := range items {
		p := o.GetPipeline()
		if p == nil {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.GetMetadata().GetName(), dash(p.GetSpec().GetRepo()), dash(p.GetSpec().GetRef()), dash(p.GetSpec().GetPath()),
			dash(p.GetStatus().GetLastRun()), dash(p.GetStatus().GetLastPhase()),
			age(now, p.GetMetadata().GetCreatedAt().AsTime()))
	}
	return tw.Flush()
}

// printPipelineRuns writes runs as a table, newest first.
func printPipelineRuns(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	rows := slices.Clone(items)
	slices.SortStableFunc(rows, func(a, b *v1alpha1.Object) int {
		return b.GetPipelineRun().GetMetadata().GetCreatedAt().AsTime().Compare(a.GetPipelineRun().GetMetadata().GetCreatedAt().AsTime())
	})
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPIPELINE\tTRIGGER\tCOMMIT\tPHASE\tAGE\tMESSAGE")
	for _, o := range rows {
		r := o.GetPipelineRun()
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.GetMetadata().GetName(), r.GetSpec().GetPipeline(), dash(r.GetSpec().GetTrigger()),
			dash(shortCommit(r.GetSpec().GetCommit())), dash(r.GetStatus().GetPhase()),
			age(now, r.GetMetadata().GetCreatedAt().AsTime()), dash(r.GetStatus().GetMessage()))
	}
	return tw.Flush()
}
