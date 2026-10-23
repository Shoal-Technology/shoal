package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/resource"
)

// newBuildCmd groups the build commands (ADR 0021). A BuildConfig is the
// recipe; a Build is one run of it.
func newBuildCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build images from git repositories",
		Long: `Build images from git repositories. A BuildConfig is the recipe; "shoal build
start" makes one run of it, a Build. The Builds of a config run one at a time,
oldest first, and the last 10 finished Builds of each config are kept. A server
with no build executor marks each Build as Error.`,
	}
	cmd.AddCommand(newBuildCreateConfigCmd(), newBuildStartCmd(), newBuildLsCmd(), newBuildLogsCmd(), newBuildCancelCmd(), newBuildWebhookURLCmd())
	return cmd
}

func newBuildCreateConfigCmd() *cobra.Command {
	var (
		cf   clientFlags
		file string
	)
	cmd := &cobra.Command{
		Use:   "create-config -f FILE",
		Short: "Create or update BuildConfigs from a YAML file (- for stdin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := readManifestFile(cmd, file)
			if err != nil {
				return err
			}
			objs, err := buildConfigObjects(data)
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
	cmd.Flags().StringVarP(&file, "file", "f", "", "manifest file with BuildConfig objects, or - for stdin (required)")
	_ = cmd.MarkFlagRequired("file") // only fails for unknown flag names
	return cmd
}

func newBuildStartCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "start CONFIG",
		Short: "Start one run of a BuildConfig",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.StartBuild(ctx, &v1alpha1.StartBuildRequest{Config: args[0]})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "build/%s created\n", resp.GetBuild())
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newBuildLsCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "ls [CONFIG]",
		Short: "List Builds, newest first, optionally only those of one BuildConfig",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindBuild})
			if err != nil {
				return err
			}
			config := ""
			if len(args) == 1 {
				config = args[0]
			}
			return printBuilds(cmd.OutOrStdout(), filterBuilds(resp.GetItems(), config), time.Now())
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newBuildLogsCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "logs BUILD",
		Short: "Show the output of a Build",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.BuildLogs(ctx, &v1alpha1.BuildLogsRequest{Build: args[0]})
			if err != nil {
				return err
			}
			return printBuildLogs(cmd.OutOrStdout(), resp.GetLines())
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newBuildCancelCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "cancel BUILD",
		Short: "Cancel a Build that has not finished",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			if _, err := c.control.CancelBuild(ctx, &v1alpha1.CancelBuildRequest{Build: args[0]}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "build/%s cancel requested\n", args[0])
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func newBuildWebhookURLCmd() *cobra.Command {
	var (
		cf      clientFlags
		baseURL string
	)
	cmd := &cobra.Command{
		Use:   "webhook-url CONFIG",
		Short: "Print the webhook URL of a BuildConfig and how to set it up in the git host",
		Long: `Print the URL that a git host calls to start a Build of CONFIG (ADR 0021,
pass C), with what to set in the host. The secret's value is never printed: it
is the webhook-secret key of the Secret named in the output.

The URL is on the portal, which listens on port 7444 by default. The git host
must reach it and trust its certificate, so give the portal a certificate that
browsers and git hosts trust (shoal server --ui-cert and --ui-key).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			obj, err := c.control.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindBuildConfig, Name: args[0]})
			if err != nil {
				return err
			}
			cfg := obj.GetBuildConfig()
			if cfg.GetSpec().GetWebhook() == nil {
				return fmt.Errorf("build/%s has no webhook: set spec.webhook in its manifest", args[0])
			}
			base := baseURL
			if base == "" {
				host := c.server
				if h, _, err := net.SplitHostPort(c.server); err == nil {
					host = h
				}
				if host == "" {
					return errors.New("the server host is unknown: pass --base-url")
				}
				base = "https://" + net.JoinHostPort(host, "7444")
			}
			url := strings.TrimSuffix(base, "/") + resource.WebhookPath(cfg.GetMetadata().GetUid())
			return printWebhookSetup(cmd.OutOrStdout(), url, cfg)
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVar(&baseURL, "base-url", "", "address of the portal as the git host reaches it, e.g. https://shoal.example.com:7444 (default: the server's host on port 7444)")
	return cmd
}

// printWebhookSetup prints the URL and what to configure for the config's
// provider. It never prints the secret: only the Secret's name and key.
func printWebhookSetup(w io.Writer, url string, cfg *v1alpha1.BuildConfig) error {
	hook := cfg.GetSpec().GetWebhook()
	secret := hook.GetSecret()
	fmt.Fprintf(w, "URL:     %s\n", url)
	fmt.Fprintf(w, "Secret:  %s, key %s\n", secret, resource.WebhookSecretKey)
	fmt.Fprintf(w, "Branch:  %s\n", resource.WebhookBranch(cfg.GetSpec()))
	fmt.Fprintf(w, "Set the secret once with: shoal secret create %s %s=<value>\n\n", secret, resource.WebhookSecretKey)
	switch hook.GetProvider() {
	case resource.WebhookGitHub:
		fmt.Fprintln(w, "GitHub (repository settings, Webhooks, Add webhook):")
		fmt.Fprintln(w, "  Payload URL: the URL above. Content type: application/json.")
		fmt.Fprintln(w, "  Secret: the value of the webhook-secret key. Events: Just the push event.")
	case resource.WebhookGitLab:
		fmt.Fprintln(w, "GitLab (project settings, Webhooks):")
		fmt.Fprintln(w, "  URL: the URL above. Secret token: the value of the webhook-secret key.")
		fmt.Fprintln(w, "  Trigger: Push events.")
	case resource.WebhookGitea:
		fmt.Fprintln(w, "Gitea (repository settings, Webhooks, Gitea):")
		fmt.Fprintln(w, "  Target URL: the URL above. Secret: the value of the webhook-secret key.")
		fmt.Fprintln(w, "  Trigger on: Push events.")
	case resource.WebhookGeneric:
		fmt.Fprintln(w, "Generic: POST a JSON body to the URL above:")
		fmt.Fprintf(w, "  {\"ref\": \"refs/heads/%s\", \"commit\": \"<full commit sha>\"}\n", resource.WebhookBranch(cfg.GetSpec()))
		fmt.Fprintln(w, "  with Content-Type application/json and the header")
		fmt.Fprintln(w, "  X-Shoal-Signature-256: sha256=<hex HMAC-SHA256 of the body, keyed with the secret value>.")
	}
	fmt.Fprintln(w, "\nRequests are limited to 1 MiB and 30 per minute per config. Other events are answered 202 and ignored.")
	return nil
}

// readManifestFile reads a manifest from a file, or from stdin for "-".
func readManifestFile(cmd *cobra.Command, file string) ([]byte, error) {
	if file == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	return data, nil
}

// buildConfigObjects decodes a manifest that must hold only BuildConfig
// objects. Each one is checked locally, so errors show before a round trip.
func buildConfigObjects(data []byte) ([]*v1alpha1.Object, error) {
	objs, err := resource.Decode(data)
	if err != nil {
		return nil, err
	}
	var errs []error
	for i, o := range objs {
		if o.GetBuildConfig() == nil {
			errs = append(errs, fmt.Errorf("object %d is not a BuildConfig: create-config takes only BuildConfig objects", i+1))
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

// filterBuilds keeps the Builds of config, or every Build when config is "".
func filterBuilds(items []*v1alpha1.Object, config string) []*v1alpha1.Object {
	var out []*v1alpha1.Object
	for _, o := range items {
		b := o.GetBuild()
		if b == nil {
			continue
		}
		if config == "" || b.GetSpec().GetConfig() == config {
			out = append(out, o)
		}
	}
	return out
}

// printBuilds writes Builds as a table, newest first.
func printBuilds(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	rows := slices.Clone(items)
	slices.SortStableFunc(rows, func(a, b *v1alpha1.Object) int {
		return b.GetBuild().GetMetadata().GetCreatedAt().AsTime().Compare(a.GetBuild().GetMetadata().GetCreatedAt().AsTime())
	})
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCONFIG\tPHASE\tCOMMIT\tIMAGE\tAGE")
	for _, o := range rows {
		b := o.GetBuild()
		st := b.GetStatus()
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			b.GetMetadata().GetName(), b.GetSpec().GetConfig(), st.GetPhase(),
			dash(shortCommit(st.GetCommit())), dash(st.GetImage()),
			age(now, b.GetMetadata().GetCreatedAt().AsTime()))
	}
	return tw.Flush()
}

// printBuildConfigs writes BuildConfigs as a table, with the newest Build of
// each.
func printBuildConfigs(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tGIT\tREF\tLAST BUILD\tPHASE\tAGE")
	for _, o := range items {
		c := o.GetBuildConfig()
		if c == nil {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			c.GetMetadata().GetName(), dash(c.GetSpec().GetGit().GetUrl()), dash(c.GetSpec().GetGit().GetRef()),
			dash(c.GetStatus().GetLastBuild()), dash(c.GetStatus().GetLastPhase()),
			age(now, c.GetMetadata().GetCreatedAt().AsTime()))
	}
	return tw.Flush()
}

// printBuildLogs writes the lines of a Build, oldest first.
func printBuildLogs(w io.Writer, lines []*v1alpha1.LogLine) error {
	for _, l := range lines {
		if _, err := fmt.Fprintln(w, l.GetLine()); err != nil {
			return err
		}
	}
	return nil
}

// shortCommit is the first 12 characters of a commit, as git shows them.
func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
