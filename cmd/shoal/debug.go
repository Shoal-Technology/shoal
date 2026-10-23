package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Shoal-Technology/shoal/internal/resource"
	"github.com/Shoal-Technology/shoal/internal/runtime"
	"github.com/Shoal-Technology/shoal/internal/runtime/containerd"
)

func newDebugCmd(c *cli) *cobra.Command {
	rf := &runtimeFlags{}
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Low-level commands that talk to the local container runtime directly",
	}
	rf.register(cmd.PersistentFlags())
	withRuntime := func(fn func(ctx context.Context, rt containerd.Client, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			rt, err := rf.open(cmd.Context())
			if err != nil {
				return err
			}
			defer rt.Close()
			return fn(cmd.Context(), rt, args)
		}
	}
	cmd.AddCommand(
		newDebugRunCmd(withRuntime),
		&cobra.Command{
			Use:   "ps",
			Short: "List containers",
			Args:  cobra.NoArgs,
			RunE: withRuntime(func(ctx context.Context, rt containerd.Client, _ []string) error {
				cs, err := rt.List(ctx)
				if err != nil {
					return err
				}
				w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(w, "ID\tIMAGE\tSTATE\tRESTARTS\tIP\tSTARTED\tEXIT")
				for _, s := range cs {
					started, exit := "-", "-"
					if !s.StartedAt.IsZero() {
						started = time.Since(s.StartedAt).Round(time.Second).String() + " ago"
					}
					if s.State == runtime.StateExited {
						exit = strconv.Itoa(s.ExitCode)
					}
					ip := s.IP
					if ip == "" {
						ip = "-"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", s.ID, s.Image, s.State, max(s.Starts-1, 0), ip, started, exit)
				}
				return w.Flush()
			}),
		},
		newDebugLogsCmd(withRuntime),
		newDebugStopCmd(withRuntime),
		&cobra.Command{
			Use:   "rm ID...",
			Short: "Force-remove containers (kills them if running)",
			Args:  cobra.MinimumNArgs(1),
			RunE: withRuntime(func(ctx context.Context, rt containerd.Client, args []string) error {
				for _, id := range args {
					if err := rt.Remove(ctx, id); err != nil {
						return err
					}
				}
				return nil
			}),
		},
	)
	return cmd
}

type runtimeRunE = func(fn func(ctx context.Context, rt containerd.Client, args []string) error) func(*cobra.Command, []string) error

func newDebugRunCmd(withRuntime runtimeRunE) *cobra.Command {
	var (
		id, image, cpu, memory string
		env, publish           []string
	)
	cmd := &cobra.Command{
		Use:   "run --id ID --image IMAGE [flags] [-- COMMAND...]",
		Short: "Pull, create and start a container",
		RunE: withRuntime(func(ctx context.Context, rt containerd.Client, args []string) error {
			spec := runtime.ContainerSpec{ID: id, Image: image, Command: args}
			for _, e := range env {
				if !strings.Contains(e, "=") {
					return fmt.Errorf("invalid --env %q: want KEY=VALUE", e)
				}
				spec.Env = append(spec.Env, e)
			}
			for _, p := range publish {
				pm, err := parsePublish(p)
				if err != nil {
					return err
				}
				spec.Ports = append(spec.Ports, pm)
			}
			var err error
			if cpu != "" {
				if spec.CPUMillis, err = resource.ParseCPU(cpu); err != nil {
					return err
				}
			}
			if memory != "" {
				if spec.MemoryBytes, err = resource.ParseMemory(memory); err != nil {
					return err
				}
			}
			if err := rt.EnsureImage(ctx, image, nil); err != nil {
				return err
			}
			if err := rt.Create(ctx, spec); err != nil {
				return err
			}
			if err := rt.Start(ctx, id); err != nil {
				return err
			}
			st, err := rt.Status(ctx, id)
			if err != nil {
				return err
			}
			fmt.Printf("%s %s ip=%s\n", st.ID, st.State, st.IP)
			return nil
		}),
	}
	cmd.Flags().StringVar(&id, "id", "", "container ID (required)")
	cmd.Flags().StringVar(&image, "image", "", "image reference (required)")
	cmd.Flags().StringArrayVarP(&env, "env", "e", nil, "environment variable KEY=VALUE (repeatable)")
	cmd.Flags().StringArrayVarP(&publish, "publish", "p", nil, "publish a port HOST:CONTAINER[/udp] (repeatable)")
	cmd.Flags().StringVar(&cpu, "cpu", "", "CPU limit, e.g. 500m")
	cmd.Flags().StringVar(&memory, "memory", "", "memory limit, e.g. 128Mi")
	_ = cmd.MarkFlagRequired("id")    // only fails for unknown flag names
	_ = cmd.MarkFlagRequired("image") // only fails for unknown flag names
	return cmd
}

func parsePublish(s string) (runtime.PortMapping, error) {
	spec, proto, _ := strings.Cut(s, "/")
	if proto == "" {
		proto = "tcp"
	}
	host, ctr, ok := strings.Cut(spec, ":")
	if !ok {
		return runtime.PortMapping{}, fmt.Errorf("invalid --publish %q: want HOST:CONTAINER[/udp]", s)
	}
	hp, err1 := strconv.ParseUint(host, 10, 16)
	cp, err2 := strconv.ParseUint(ctr, 10, 16)
	if err1 != nil || err2 != nil || hp == 0 || cp == 0 || (proto != "tcp" && proto != "udp") {
		return runtime.PortMapping{}, fmt.Errorf("invalid --publish %q: want HOST:CONTAINER[/udp]", s)
	}
	return runtime.PortMapping{HostPort: int32(hp), ContainerPort: int32(cp), Protocol: proto}, nil
}

func newDebugLogsCmd(withRuntime runtimeRunE) *cobra.Command {
	var (
		follow, timestamps bool
		tail               int
	)
	cmd := &cobra.Command{
		Use:   "logs ID",
		Short: "Print a container's output",
		Args:  cobra.ExactArgs(1),
		RunE: withRuntime(func(ctx context.Context, rt containerd.Client, args []string) error {
			return rt.Logs(ctx, args[0], runtime.LogOptions{Follow: follow, Tail: tail}, func(e runtime.LogEntry) error {
				out := os.Stdout
				if e.Stream == "stderr" {
					out = os.Stderr
				}
				if timestamps {
					_, err := fmt.Fprintf(out, "%s %s\n", e.Time.Format(time.RFC3339Nano), e.Line)
					return err
				}
				_, err := fmt.Fprintln(out, e.Line)
				return err
			})
		}),
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new output until the container exits")
	cmd.Flags().BoolVarP(&timestamps, "timestamps", "t", false, "show timestamps")
	cmd.Flags().IntVar(&tail, "tail", -1, "number of recent lines to show (-1 = all)")
	return cmd
}

func newDebugStopCmd(withRuntime runtimeRunE) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "stop ID...",
		Short: "Stop containers gracefully (SIGTERM, then SIGKILL after --timeout)",
		Args:  cobra.MinimumNArgs(1),
		RunE: withRuntime(func(ctx context.Context, rt containerd.Client, args []string) error {
			for _, id := range args {
				if err := rt.Stop(ctx, id, timeout); err != nil {
					return err
				}
			}
			return nil
		}),
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "grace period before SIGKILL")
	return cmd
}
