package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
)

// projectFlag is --project, bound on the root command (ADR 0023, pass B1).
var projectFlag string

// currentProject is the project the command works in, set before every
// command runs (see root's PersistentPreRunE). Empty means none was chosen.
var currentProject string

// projectEnv is the environment variable that chooses the project.
const projectEnv = "SHOAL_PROJECT"

// defaultProjectFile returns the file that `shoal project use` writes: the
// CLI's default project, in ~/.shoal/project.
func defaultProjectFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".shoal", "project"), nil
}

// resolveProject returns the project a command works in. The order is the
// flag, then the environment, then the default file. An empty result means no
// project was chosen: a create then goes to default, and a lookup by name
// searches every project.
func resolveProject(flag, env, file string) (string, error) {
	switch {
	case flag != "":
		return flag, nil
	case env != "":
		return env, nil
	case file == "":
		return "", nil
	}
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the default project %s: %w", file, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// chooseProject sets currentProject from the flag, the environment and the
// default file.
func chooseProject() error {
	file, err := defaultProjectFile()
	if err != nil {
		file = "" // no home directory: no default file
	}
	p, err := resolveProject(projectFlag, os.Getenv(projectEnv), file)
	if err != nil {
		return err
	}
	currentProject = p
	return nil
}

func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Create, list, delete and choose projects (ADR 0023)",
	}
	cmd.AddCommand(newProjectCreateCmd(), newProjectLsCmd(), newProjectDeleteCmd(), newProjectUseCmd())
	return cmd
}

func newProjectCreateCmd() *cobra.Command {
	var (
		cf          clientFlags
		displayName string
		description string
	)
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a project (cluster-admin only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			obj := &v1alpha1.Object{Kind: &v1alpha1.Object_Project{Project: &v1alpha1.Project{
				ApiVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindProject,
				Metadata:   &v1alpha1.ObjectMeta{Name: args[0]},
				Spec:       &v1alpha1.ProjectSpec{DisplayName: displayName, Description: description},
			}}}
			return applyObjects(cmd, &cf, []*v1alpha1.Object{obj})
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVar(&displayName, "display-name", "", "name shown in the portal (at most 200 bytes)")
	cmd.Flags().StringVar(&description, "description", "", "what the project is for (at most 200 bytes)")
	return cmd
}

func newProjectLsCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List projects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindProject})
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDISPLAY NAME\tDESCRIPTION")
			for _, o := range resp.GetItems() {
				p := o.GetProject()
				fmt.Fprintf(w, "%s\t%s\t%s\n", p.GetMetadata().GetName(), p.GetSpec().GetDisplayName(), p.GetSpec().GetDescription())
			}
			return w.Flush()
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newProjectDeleteCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete an empty project (cluster-admin only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			if _, err := c.control.Delete(ctx, &v1alpha1.DeleteRequest{Kind: v1alpha1.KindProject, Name: args[0]}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "project/%s deleted\n", args[0])
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newProjectUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use NAME",
		Short: "Make NAME the default project of this CLI (~/.shoal/project)",
		Long: "Writes the CLI's default project. --project and SHOAL_PROJECT take " +
			"precedence over it. The server is not contacted.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, err := defaultProjectFile()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(file, []byte(args[0]+"\n"), 0o600); err != nil {
				return fmt.Errorf("write %s: %w", file, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "default project: %s\n", args[0])
			return nil
		},
	}
}

func newQuotaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Set and list the ResourceQuota of a project (ADR 0023, section 7)",
	}
	cmd.AddCommand(newQuotaSetCmd(), newQuotaLsCmd())
	return cmd
}

func newQuotaSetCmd() *cobra.Command {
	var (
		cf        clientFlags
		cpu       string
		memory    string
		storage   string
		workloads = -1
	)
	cmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Set a ResourceQuota in the chosen project; unset fields are unlimited",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec := &v1alpha1.ResourceQuotaSpec{Cpu: cpu, Memory: memory, Storage: storage}
			if workloads >= 0 {
				n := int32(workloads)
				spec.Workloads = &n
			}
			obj := &v1alpha1.Object{Kind: &v1alpha1.Object_ResourceQuota{ResourceQuota: &v1alpha1.ResourceQuota{
				ApiVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindResourceQuota,
				Metadata:   &v1alpha1.ObjectMeta{Name: args[0], Project: currentProject},
				Spec:       spec,
			}}}
			return applyObjects(cmd, &cf, []*v1alpha1.Object{obj})
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVar(&cpu, "cpu", "", "CPU for all Workloads, e.g. 4 or 2000m")
	cmd.Flags().StringVar(&memory, "memory", "", "memory for all Workloads, e.g. 8Gi")
	cmd.Flags().StringVar(&storage, "storage", "", "total Volume size, e.g. 100Gi")
	cmd.Flags().IntVar(&workloads, "workloads", -1, "number of Workloads (unset is unlimited)")
	return cmd
}

func newQuotaLsCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the ResourceQuotas of the chosen project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindResourceQuota, Project: currentProject})
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PROJECT\tNAME\tCPU\tMEMORY\tWORKLOADS\tSTORAGE")
			for _, o := range resp.GetItems() {
				q := o.GetResourceQuota()
				n := "unlimited"
				if q.GetSpec().Workloads != nil {
					n = fmt.Sprint(q.GetSpec().GetWorkloads())
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", q.GetMetadata().GetProject(), q.GetMetadata().GetName(),
					orUnlimited(q.GetSpec().GetCpu()), orUnlimited(q.GetSpec().GetMemory()), n, orUnlimited(q.GetSpec().GetStorage()))
			}
			return w.Flush()
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func orUnlimited(s string) string {
	if s == "" {
		return "unlimited"
	}
	return s
}

// applyObjects sends objects to the server with Apply and prints the results,
// as `shoal apply` does.
func applyObjects(cmd *cobra.Command, cf *clientFlags, objs []*v1alpha1.Object) error {
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
}
