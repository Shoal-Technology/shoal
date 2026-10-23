package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/Shoal-Technology/shoal/api/v1alpha1"
	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/tenancy"
)

// kindAliases maps the names users type to API kinds.
var kindAliases = map[string]string{
	"workload": v1alpha1.KindWorkload, "workloads": v1alpha1.KindWorkload, "wl": v1alpha1.KindWorkload,
	"node": v1alpha1.KindNode, "nodes": v1alpha1.KindNode, "no": v1alpha1.KindNode,
	"replica": v1alpha1.KindReplica, "replicas": v1alpha1.KindReplica, "rp": v1alpha1.KindReplica,
	"secret": v1alpha1.KindSecret, "secrets": v1alpha1.KindSecret,
	"config": v1alpha1.KindConfig, "configs": v1alpha1.KindConfig, "cfg": v1alpha1.KindConfig,
	"service": v1alpha1.KindService, "services": v1alpha1.KindService, "svc": v1alpha1.KindService,
	"route": v1alpha1.KindRoute, "routes": v1alpha1.KindRoute,
	"volume": v1alpha1.KindVolume, "volumes": v1alpha1.KindVolume, "vol": v1alpha1.KindVolume,
	"database": v1alpha1.KindDatabase, "databases": v1alpha1.KindDatabase, "db": v1alpha1.KindDatabase,
	"buildconfig": v1alpha1.KindBuildConfig, "buildconfigs": v1alpha1.KindBuildConfig, "bc": v1alpha1.KindBuildConfig,
	"build": v1alpha1.KindBuild, "builds": v1alpha1.KindBuild,
	"environment": v1alpha1.KindEnvironment, "environments": v1alpha1.KindEnvironment, "env": v1alpha1.KindEnvironment,
	"job": v1alpha1.KindJob, "jobs": v1alpha1.KindJob,
}

func parseKind(s string) (string, error) {
	if k, ok := kindAliases[strings.ToLower(s)]; ok {
		return k, nil
	}
	return "", fmt.Errorf("unknown resource type %q (want workloads, replicas, nodes, services, routes, secrets or configs)", s)
}

func newApplyCmd() *cobra.Command {
	var (
		cf   clientFlags
		file string
	)
	cmd := &cobra.Command{
		Use:   "apply -f FILE",
		Short: "Create or update objects from a YAML file (- for stdin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var data []byte
			var err error
			if file == "-" {
				data, err = io.ReadAll(cmd.InOrStdin())
			} else {
				data, err = os.ReadFile(file)
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", file, err)
			}
			objs, err := resource.Decode(data)
			if err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			// Validate locally too, for errors without a round trip.
			var errs []error
			for _, o := range objs {
				if kind, name, _, err := resource.Prepare(o); err != nil {
					errs = append(errs, fmt.Errorf("%s %q: %w", strings.ToLower(kind), name, err))
				}
			}
			if err := errors.Join(errs...); err != nil {
				return err
			}

			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			// --project (or the default project) applies to every project-scoped
			// object that does not name one. A manifest that names another
			// project is refused (ADR 0023, pass B1).
			for _, o := range objs {
				kind, _, _, err := resource.Prepare(o)
				if err != nil || !tenancy.ProjectScoped(kind) || currentProject == "" {
					continue
				}
				switch have := resource.ObjectProject(o); {
				case have == "":
					resource.SetObjectProject(o, currentProject)
				case have != currentProject:
					return fmt.Errorf("%s %q names project %q, but the project is %q: change one of them", strings.ToLower(kind), resource.ObjectName(o), have, currentProject)
				}
			}
			resp, err := c.control.Apply(ctx, &v1alpha1.ApplyRequest{Objects: objs})
			for _, r := range resp.GetResults() {
				fmt.Fprintf(cmd.OutOrStdout(), "%s/%s %s\n", strings.ToLower(r.Kind), r.Name, r.Action)
			}
			return err
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVarP(&file, "file", "f", "", "manifest file, or - for stdin (required)")
	_ = cmd.MarkFlagRequired("file") // only fails for unknown flag names
	return cmd
}

func newGetCmd() *cobra.Command {
	var (
		cf       clientFlags
		output   string
		workload string
		selector string
	)
	cmd := &cobra.Command{
		Use:   "get workloads|replicas|nodes|services|routes|volumes|databases|secrets|configs [NAME]",
		Short: "List objects or show one",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := parseKind(args[0])
			if err != nil {
				return err
			}
			if output != "" && output != "yaml" && output != "json" {
				return fmt.Errorf("invalid --output %q: want yaml or json", output)
			}
			reqs, err := resource.ParseSelector(selector)
			if err != nil {
				return err
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			var items []*v1alpha1.Object
			if len(args) == 2 {
				obj, err := c.control.Get(ctx, &v1alpha1.GetRequest{Kind: kind, Name: args[1], Project: currentProject})
				if err != nil {
					return err
				}
				items = []*v1alpha1.Object{obj}
			} else {
				resp, err := c.control.List(ctx, &v1alpha1.ListRequest{Kind: kind, Project: currentProject})
				if err != nil {
					return err
				}
				items = resp.GetItems()
			}
			if workload != "" {
				items = slices.DeleteFunc(items, func(o *v1alpha1.Object) bool {
					return o.GetReplica().GetSpec().GetWorkload() != workload
				})
			}
			if len(reqs) > 0 {
				items = slices.DeleteFunc(items, func(o *v1alpha1.Object) bool {
					return !resource.Matches(reqs, resource.ObjectLabels(o))
				})
			}
			out := cmd.OutOrStdout()
			switch output {
			case "yaml":
				return printYAML(out, items)
			case "json":
				return printJSON(out, items)
			}
			if len(items) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "No %ss found.\n", strings.ToLower(kind))
				return nil
			}
			switch kind {
			case v1alpha1.KindNode:
				return printNodes(out, items, time.Now())
			case v1alpha1.KindReplica:
				return printReplicas(out, items, time.Now(), true)
			case v1alpha1.KindService:
				return printServices(out, items, time.Now())
			case v1alpha1.KindRoute:
				return printRoutes(out, items, time.Now())
			case v1alpha1.KindVolume:
				return printVolumes(out, items, time.Now())
			case v1alpha1.KindDatabase:
				return printDatabases(out, items, time.Now())
			case v1alpha1.KindBuildConfig:
				return printBuildConfigs(out, items, time.Now())
			case v1alpha1.KindBuild:
				return printBuilds(out, items, time.Now())
			case v1alpha1.KindSecret, v1alpha1.KindConfig:
				return printData(out, items, time.Now())
			}
			return printWorkloads(out, items, time.Now())
		},
	}
	cf.register(cmd.Flags())
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: yaml or json (default: table)")
	cmd.Flags().StringVarP(&workload, "workload", "w", "", "only replicas of this workload")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "only objects matching labels: app=x, app!=x, key, !key (comma-separated)")
	return cmd
}

func newDescribeCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "describe workload NAME",
		Short: "Show a workload with its replicas",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if kind, err := parseKind(args[0]); err != nil {
				return err
			} else if kind != v1alpha1.KindWorkload {
				return fmt.Errorf("describe supports workloads only")
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			obj, err := c.control.Get(ctx, &v1alpha1.GetRequest{Kind: v1alpha1.KindWorkload, Name: args[1], Project: currentProject})
			if err != nil {
				return err
			}
			reps, err := c.control.List(ctx, &v1alpha1.ListRequest{Kind: v1alpha1.KindReplica})
			if err != nil {
				return err
			}
			items := slices.DeleteFunc(reps.GetItems(), func(o *v1alpha1.Object) bool {
				return o.GetReplica().GetSpec().GetWorkload() != args[1]
			})
			return describeWorkload(cmd.OutOrStdout(), obj.GetWorkload(), items, time.Now())
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func describeWorkload(w io.Writer, wl *v1alpha1.Workload, replicas []*v1alpha1.Object, now time.Time) error {
	s, st, m := wl.GetSpec(), wl.GetStatus(), wl.GetMetadata()
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "Name:\t%s\n", m.GetName())
	fmt.Fprintf(tw, "Image:\t%s\n", s.GetImage())
	if len(s.GetCommand()) > 0 {
		fmt.Fprintf(tw, "Command:\t%s\n", strings.Join(s.GetCommand(), " "))
	}
	fmt.Fprintf(tw, "Replicas:\t%d desired, %d ready\n", s.GetReplicas(), st.GetReadyReplicas())
	fmt.Fprintf(tw, "Generation:\t%d (observed %d)\n", m.GetGeneration(), st.GetObservedGeneration())
	if ro := st.GetRollout(); ro != nil {
		fmt.Fprintf(tw, "Rollout:\t%s, %d of %d replicas up to date\n", ro.GetPhase(), st.GetUpdatedReplicas(), s.GetReplicas())
		if ro.GetMessage() != "" {
			fmt.Fprintf(tw, "\t%s\n", ro.GetMessage())
		}
		if ro.GetStableGeneration() != 0 {
			fmt.Fprintf(tw, "Stable generation:\t%d (%s)\n", ro.GetStableGeneration(), ro.GetStableTemplate().GetImage())
		}
	}
	fmt.Fprintf(tw, "Update strategy:\tmaxUnavailable %d, progress deadline %ds\n",
		s.GetUpdate().GetMaxUnavailable(), s.GetUpdate().GetProgressDeadlineSeconds())
	if hc := s.GetHealthCheck(); hc != nil {
		probe := fmt.Sprintf("tcp :%d", hc.GetTcp().GetPort())
		if h := hc.GetHttp(); h != nil {
			probe = fmt.Sprintf("http :%d%s", h.GetPort(), h.GetPath())
		}
		fmt.Fprintf(tw, "Health check:\t%s every %ds (timeout %ds, %d failures, initial delay %ds)\n", probe,
			hc.GetIntervalSeconds(), hc.GetTimeoutSeconds(), hc.GetFailureThreshold(), hc.GetInitialDelaySeconds())
	}
	if r := s.GetResources(); r.GetCpu() != "" || r.GetMemory() != "" {
		fmt.Fprintf(tw, "Resources:\tcpu %s, memory %s\n", dash(r.GetCpu()), dash(r.GetMemory()))
	}
	var ports []string
	for _, p := range s.GetPorts() {
		port := fmt.Sprintf("%d/%s", p.GetContainerPort(), p.GetProtocol())
		if p.GetHostPort() != 0 {
			port = fmt.Sprintf("%d->%s", p.GetHostPort(), port)
		}
		ports = append(ports, port)
	}
	if len(ports) > 0 {
		fmt.Fprintf(tw, "Ports:\t%s\n", strings.Join(ports, ", "))
	}
	for _, e := range s.GetEnv() {
		fmt.Fprintf(tw, "Env:\t%s\n", e.GetName()) // names only: values may be secret
	}
	fmt.Fprintf(tw, "Restart policy:\t%s\n", s.GetRestartPolicy())
	fmt.Fprintf(tw, "Created:\t%s (%s ago)\n", m.GetCreatedAt().AsTime().Local().Format(time.RFC3339), age(now, m.GetCreatedAt().AsTime()))
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nReplicas:")
	if len(replicas) == 0 {
		fmt.Fprintln(w, "  (none)")
		return nil
	}
	return printReplicas(w, replicas, now, false)
}

func newDeleteCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "delete workload|replica|node|service|route|secret|config NAME",
		Short: "Delete an object",
		Long: `Delete an object.

Deleting a workload removes its replicas and their containers. Deleting a
replica makes the server replace it (possibly on another node). Deleting a
node removes it from the cluster: its replicas move to other nodes and its
agent is refused until it joins again with a new token.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := parseKind(args[0])
			if err != nil {
				return err
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			if _, err := c.control.Delete(ctx, &v1alpha1.DeleteRequest{Kind: kind, Name: args[1], Project: currentProject}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s/%s deleted\n", strings.ToLower(kind), args[1])
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

func printYAML(w io.Writer, items []*v1alpha1.Object) error {
	for i, o := range items {
		y, err := resource.EncodeYAML(resource.Inner(o))
		if err != nil {
			return err
		}
		if i > 0 {
			fmt.Fprintln(w, "---")
		}
		if _, err := w.Write(y); err != nil {
			return err
		}
	}
	return nil
}

func printJSON(w io.Writer, items []*v1alpha1.Object) error {
	opts := protojson.MarshalOptions{Multiline: true, Indent: "  "}
	for _, o := range items {
		b, err := opts.Marshal(resource.Inner(o))
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, string(b)); err != nil {
			return err
		}
	}
	return nil
}

func printWorkloads(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tIMAGE\tREADY\tUP-TO-DATE\tROLLOUT\tGENERATION\tAGE")
	for _, o := range items {
		wl := o.GetWorkload()
		st := wl.GetStatus()
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%d\t%s\t%d\t%s\n", wl.GetMetadata().GetName(), wl.GetSpec().GetImage(),
			st.GetReadyReplicas(), wl.GetSpec().GetReplicas(), st.GetUpdatedReplicas(), dash(st.GetRollout().GetPhase()),
			wl.GetMetadata().GetGeneration(), age(now, wl.GetMetadata().GetCreatedAt().AsTime()))
	}
	return tw.Flush()
}

func printReplicas(w io.Writer, items []*v1alpha1.Object, now time.Time, withWorkload bool) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if withWorkload {
		fmt.Fprint(tw, "WORKLOAD\t")
	}
	fmt.Fprintln(tw, "NAME\tNODE\tSTATUS\tREADY\tRESTARTS\tGEN\tIP\tAGE\tMESSAGE")
	for _, o := range items {
		r := o.GetReplica()
		st := r.GetStatus()
		if withWorkload {
			fmt.Fprintf(tw, "%s\t", r.GetSpec().GetWorkload())
		}
		ready := "no"
		if st.GetReady() && st.GetTemplateGeneration() == r.GetSpec().GetTemplateGeneration() {
			ready = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n", r.GetMetadata().GetName(), dash(r.GetSpec().GetNode()),
			dash(st.GetPhase()), ready, st.GetRestarts(), r.GetSpec().GetTemplateGeneration(), dash(st.GetIp()),
			age(now, r.GetMetadata().GetCreatedAt().AsTime()), oneLine(st.GetMessage()))
	}
	return tw.Flush()
}

func printServices(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCLUSTER-IP\tPORTS\tSELECTOR\tENDPOINTS\tAGE")
	for _, o := range items {
		s := o.GetService()
		var ports []string
		for _, p := range s.GetSpec().GetPorts() {
			ports = append(ports, fmt.Sprintf("%d->%d/%s", p.GetPort(), p.GetTargetPort(), p.GetProtocol()))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", s.GetMetadata().GetName(), dash(s.GetStatus().GetClusterIp()),
			strings.Join(ports, ","), s.GetSpec().GetSelector(), len(s.GetStatus().GetEndpoints()),
			age(now, s.GetMetadata().GetCreatedAt().AsTime()))
	}
	return tw.Flush()
}

func printRoutes(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tHOST\tPATH\tSERVICE\tPORT\tTLS\tSTATUS\tMESSAGE\tCERT\tCERT EXPIRES\tAGE")
	for _, o := range items {
		r := o.GetRoute()
		state := r.GetStatus().GetState()
		if state == "" {
			state = "Pending" // the controller has not reconciled it yet
		}
		expires := "-"
		if na := r.GetStatus().GetCertNotAfter(); na != nil {
			expires = na.AsTime().Format("2006-01-02")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", r.GetMetadata().GetName(), r.GetSpec().GetHost(),
			r.GetSpec().GetPathPrefix(), r.GetSpec().GetService(), r.GetSpec().GetPort(), r.GetSpec().GetTls(),
			state, dash(r.GetStatus().GetMessage()), dash(r.GetStatus().GetCertState()), expires,
			age(now, r.GetMetadata().GetCreatedAt().AsTime()))
	}
	return tw.Flush()
}

// printVolumes lists Volumes: the node each is bound to and the Workload that
// mounts it.
func printVolumes(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSIZE\tNODE\tSTATE\tWORKLOAD\tRETENTION\tQUOTA\tAGE")
	for _, o := range items {
		v := o.GetVolume()
		state := v.GetStatus().GetState()
		if state == "" {
			state = "Pending" // the controller has not reconciled it yet
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.GetMetadata().GetName(), v.GetSpec().GetSize(),
			dash(v.GetStatus().GetNode()), state, dash(v.GetStatus().GetAttachedWorkload()),
			v.GetSpec().GetRetention(), dash(v.GetStatus().GetQuota()),
			age(now, v.GetMetadata().GetCreatedAt().AsTime()))
	}
	return tw.Flush()
}

// printDatabases lists Databases: engine, version, storage, phase and endpoint.
func printDatabases(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tENGINE\tVERSION\tSIZE\tPHASE\tENDPOINT\tAGE\tMESSAGE")
	for _, o := range items {
		d := o.GetDatabase()
		phase := d.GetStatus().GetPhase()
		if phase == "" {
			phase = "Pending" // the controller has not reconciled it yet
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.GetMetadata().GetName(), dash(d.GetSpec().GetEngine()),
			dash(d.GetSpec().GetVersion()), dash(d.GetSpec().GetSize()), phase, dash(d.GetStatus().GetEndpoint()),
			age(now, d.GetMetadata().GetCreatedAt().AsTime()), oneLine(d.GetStatus().GetMessage()))
	}
	return tw.Flush()
}

// printData lists Secrets or Configs with their keys (never secret values).
func printData(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKEYS\tAGE")
	for _, o := range items {
		var meta *v1alpha1.ObjectMeta
		var data map[string]string
		if s := o.GetSecret(); s != nil {
			meta, data = s.GetMetadata(), s.GetSpec().GetData()
		} else {
			meta, data = o.GetConfig().GetMetadata(), o.GetConfig().GetSpec().GetData()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", meta.GetName(), dash(strings.Join(slices.Sorted(maps.Keys(data)), ",")), age(now, meta.GetCreatedAt().AsTime()))
	}
	return tw.Flush()
}

func newRestartCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "restart workload NAME",
		Short: "Replace a workload's replicas with a rolling update",
		Long: `Replace a workload's replicas with a rolling update (respecting
update.maxUnavailable and health checks). Use it to pick up changed Secret or
Config values in environment variables, or to retry a rolled-back release.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if kind, err := parseKind(args[0]); err != nil {
				return err
			} else if kind != v1alpha1.KindWorkload {
				return fmt.Errorf("only workloads can be restarted")
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			res, err := c.control.RestartWorkload(ctx, &v1alpha1.RestartWorkloadRequest{Name: args[1], Project: currentProject})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "workload/%s %s\n", res.GetName(), res.GetAction())
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}

// oneLine keeps table rows on one line and reasonably short.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

func printNodes(w io.Writer, items []*v1alpha1.Object, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tADDRESS\tCPU\tMEMORY\tVERSION\tLAST HEARTBEAT\tAGE")
	for _, o := range items {
		n := o.GetNode()
		st := n.GetStatus()
		last := "never"
		if hb := st.GetLastHeartbeat(); hb != nil {
			last = age(now, hb.AsTime()) + " ago"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", n.GetMetadata().GetName(), dash(st.GetPhase()), dash(st.GetAddress()),
			cpu(st.GetCapacity().GetCpuMillis()), memory(st.GetCapacity().GetMemoryBytes()), dash(st.GetAgentVersion()),
			last, age(now, n.GetMetadata().GetCreatedAt().AsTime()))
	}
	return tw.Flush()
}

func age(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0:
		return "0s"
	case d < 2*time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < 2*time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func cpu(milli int64) string {
	if milli == 0 {
		return "-"
	}
	if milli%1000 == 0 {
		return fmt.Sprint(milli / 1000)
	}
	return fmt.Sprintf("%dm", milli)
}

func memory(b int64) string {
	if b == 0 {
		return "-"
	}
	units := []string{"Ki", "Mi", "Gi", "Ti"}
	v, u := float64(b), ""
	for _, next := range units {
		if v < 1024 {
			break
		}
		v, u = v/1024, next
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + u
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
