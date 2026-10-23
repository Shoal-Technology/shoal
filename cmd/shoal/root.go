package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Shoal-Technology/shoal/internal/runtime/containerd"
)

// cli holds options shared by all commands.
type cli struct {
	logLevel  string
	logFormat string
}

func newRootCmd() *cobra.Command {
	c := &cli{}
	root := &cobra.Command{
		Use:           "shoal",
		Short:         "A lightweight container orchestrator",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&c.logLevel, "log-level", "info", "log level: debug, info, warn, error")
	root.PersistentFlags().StringVar(&c.logFormat, "log-format", "text", "log format: text or json")
	root.AddCommand(
		newServerCmd(c), newAgentCmd(c),
		newApplyCmd(), newGetCmd(), newDescribeCmd(), newLogsCmd(), newRestartCmd(), newDeleteCmd(), newTokenCmd(), newUICmd(),
		newClusterCmd(), newBackupCmd(), newLabelCmd(), newVolumeCmd(),
		newDeployCmd(), newAppCmd(), newDBCmd(), newExplainCmd(), newSecretCmd(), newRegistryCmd(),
		newDebugCmd(c), newInitCmd(), newUninstallCmd(), newVersionCmd(),
	)
	return root
}

func (c *cli) logger() (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.logLevel)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q", c.logLevel)
	}
	opts := &slog.HandlerOptions{Level: level}
	switch c.logFormat {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	default:
		return nil, fmt.Errorf("invalid --log-format %q", c.logFormat)
	}
}

// runtimeFlags configure the local containerd runtime.
type runtimeFlags struct {
	opts containerd.Options
	// volumeBackend is loop (default) or dir; see ADR 0016, section 1.2.
	volumeBackend string
}

func (f *runtimeFlags) register(fs *pflag.FlagSet) {
	fs.StringVar(&f.opts.Address, "containerd-address", "/run/containerd/containerd.sock", "containerd socket")
	fs.StringVar(&f.opts.Namespace, "containerd-namespace", "shoal", "containerd namespace for managed containers")
	fs.StringVar(&f.opts.StateDir, "state-dir", "/var/lib/shoal", "directory for logs and runtime state")
	fs.StringVar(&f.opts.PodCIDR, "pod-cidr", "", "IPv4 subnet for containers on this node (standalone mode, default 10.250.0.0/24; with a server, the server assigns it)")
	fs.StringVar(&f.opts.CNIBinDir, "cni-bin-dir", "/opt/cni/bin", "directory containing CNI plugins")
	fs.StringVar(&f.opts.BridgeName, "bridge-name", "shoal0", "Linux bridge for containers on this node")
	fs.StringVar(&f.volumeBackend, "volume-backend", "loop", "volume backend: loop (ext4 image, size enforced; falls back to dir with a warning if loop devices are missing) or dir (plain directory, no size limit)")
}

func (f *runtimeFlags) open(ctx context.Context) (containerd.Client, error) {
	// One CNI network per namespace, so several agents can share a host.
	f.opts.NetworkName = f.opts.Namespace
	rt, err := containerd.New(ctx, f.opts)
	if err != nil {
		if os.Geteuid() != 0 {
			return nil, fmt.Errorf("%w (hint: the runtime needs root)", err)
		}
		return nil, err
	}
	return rt, nil
}
