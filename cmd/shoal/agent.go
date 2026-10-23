package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Shoal-Technology/shoal/internal/agent"
	"github.com/Shoal-Technology/shoal/internal/network"
	"github.com/Shoal-Technology/shoal/internal/volume"
)

func newAgentCmd(c *cli) *cobra.Command {
	var (
		rf          runtimeFlags
		nf          networkFlags
		standalone  bool
		file        string
		interval    time.Duration
		stopTimeout time.Duration

		serverAddrs                        []string
		token, caHash, nodeName, tokenFile string
		edgeHTTP, edgeHTTPS                string
		edgeCacheDir                       string
		metricsListen                      string
	)
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the node agent",
		Long: `Run the node agent.

With --server it joins the cluster (the first time, using --token and
--ca-hash from ` + "`shoal token create`" + `), keeps its credentials in
<state-dir>/agent/credentials.conf and reports to the server. Later starts
need no token. In an HA cluster, give --server once per server (or as a
comma-separated list): the agent reports to the leader, follows it when a
follower names a new one, and reconnects to the other servers when the
leader is lost. The server assigns the node's container subnet; the agent
sets up Service load balancing, DNS for Service names and the WireGuard mesh
to the other nodes.

In --standalone mode the desired replicas are read from a local YAML file
(-f) instead of a server. The file is re-read when it changes. Containers
keep running when the agent stops.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := c.logger()
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			if tokenFile != "" {
				if token, err = readTokenFile(tokenFile); err != nil {
					return err
				}
			}

			if !standalone {
				// The edge listens only while the server says this node is an
				// edge (the shoal.dev/edge label); see agent.Edge.
				ingress := agent.NewEdge(edgeHTTP, edgeHTTPS, log)
				defer ingress.Close()
				ingress.UseCache(edgeCacheDir)
				if err := ingress.LoadCache(); err != nil {
					log.Warn("edge route cache not used", "err", err)
				}
				a, closeRT, err := startConnected(ctx, &rf, &nf, log, agent.ConnectedConfig{
					Servers: serverAddrs, Token: token, CAHash: caHash, NodeName: nodeName,
					SyncInterval: interval, Reconciler: agent.Config{StopTimeout: stopTimeout},
					Edge: ingress,
				}, nil)
				if err != nil {
					return err
				}
				defer closeRT()
				if metricsListen != "" {
					// Stop the listener before waiting for it, so a failed Run cannot hang here.
					var mwg sync.WaitGroup
					defer mwg.Wait()
					mctx, stopMetrics := context.WithCancel(ctx)
					defer stopMetrics()
					if err := startMetrics(mctx, metricsListen, agentMetrics(a, rf.opts.StateDir), agentReady(a), log, &mwg); err != nil {
						return err
					}
				}
				log.Info("agent starting", "mode", "server", "node", a.Name(), "version", version)
				err = a.Run(ctx)
				log.Info("agent stopped; containers keep running")
				return err
			}

			if file == "" {
				return errors.New("--standalone requires -f <replicas file>")
			}
			if rf.opts.PodCIDR == "" {
				rf.opts.PodCIDR = "10.250.0.0/24"
			}
			rt, err := rf.open(ctx)
			if err != nil {
				return err
			}
			defer rt.Close()

			log.Info("agent starting", "mode", "standalone", "file", file, "version", version)
			r := agent.NewReconciler(rt, log, agent.Config{StopTimeout: stopTimeout})
			defer r.Close()
			s := &agent.Standalone{Path: file, Interval: interval, Reconciler: r, Log: log}
			err = s.Run(ctx)
			log.Info("agent stopped; containers keep running")
			return err
		},
	}
	rf.register(cmd.Flags())
	nf.register(cmd.Flags())
	cmd.Flags().StringSliceVar(&serverAddrs, "server", nil, "server host:port; repeat or comma-separate to list every server of an HA cluster (required to join; afterwards the saved address is used, and the agent follows the leader)")
	cmd.Flags().StringVar(&token, "token", "", "join token (first start only)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "file holding the join token (first start only); the agent reads it once and deletes it. Used by shoal init")
	cmd.Flags().StringVar(&caHash, "ca-hash", "", "expected sha256 hash of the server's CA key (first start only)")
	cmd.Flags().StringVar(&nodeName, "node-name", "", "name of this node (default: host name)")
	cmd.Flags().BoolVar(&standalone, "standalone", false, "run without a server, from a local replicas file")
	cmd.Flags().StringVarP(&file, "file", "f", "", "replicas file (standalone mode)")
	cmd.Flags().DurationVar(&interval, "sync-interval", 2*time.Second, "how often to reconcile")
	cmd.Flags().DurationVar(&stopTimeout, "stop-timeout", 10*time.Second, "grace period before SIGKILL")
	cmd.Flags().StringVar(&edgeHTTP, "edge-http", ":80", "HTTP listener used while this node is an ingress edge (empty disables)")
	cmd.Flags().StringVar(&edgeCacheDir, "edge-cache-dir", "", "keep a copy of edge routes and endpoints here, 0600, so an edge restarted while the server is down still serves HTTP routes (no certificates or keys are written; default off)")
	cmd.Flags().StringVar(&edgeHTTPS, "edge-https", ":443", "HTTPS listener used while this node is an ingress edge (empty disables)")
	cmd.Flags().StringVar(&metricsListen, "metrics-listen", "127.0.0.1:9465", "address for /metrics, /healthz and /readyz (server mode; loopback by default, empty disables)")
	cmd.MarkFlagsMutuallyExclusive("standalone", "server")
	cmd.MarkFlagsMutuallyExclusive("standalone", "token")
	cmd.MarkFlagsMutuallyExclusive("token", "token-file")
	return cmd
}

// networkFlags configure a server-mode agent's cluster networking.
type networkFlags struct {
	wgInterface string
	wgPort      int
	wgEndpoint  string
}

func (f *networkFlags) register(fs *pflag.FlagSet) {
	fs.StringVar(&f.wgInterface, "wireguard-interface", "shoal-wg", "WireGuard interface for the mesh between nodes (empty disables the mesh)")
	fs.IntVar(&f.wgPort, "wireguard-port", 51820, "UDP port of the WireGuard mesh")
	fs.StringVar(&f.wgEndpoint, "wireguard-endpoint", "", "host:port other nodes use to reach this node's mesh (default: the address the server sees)")
}

// startConnected prepares a server-mode agent: it joins (or loads its
// credentials), gets the node's network settings, sets up networking and
// opens the runtime with the node's subnet. creds, if set, writes
// credentials before loading them (server --with-agent).
func startConnected(ctx context.Context, rf *runtimeFlags, nf *networkFlags, log *slog.Logger, cfg agent.ConnectedConfig,
	creds func(path string) error) (*agent.Connected, func(), error) {
	dir := filepath.Join(rf.opts.StateDir, "agent")
	cfg.CredentialsPath = filepath.Join(dir, "credentials.conf")
	cfg.AssignmentsPath = filepath.Join(dir, "assignments.pb")
	cfg.NodeConfigPath = filepath.Join(dir, "node.conf")
	cfg.Reconciler.FilesDir = filepath.Join(dir, "files")
	// Volumes live under the state dir, so `shoal volume purge` finds them.
	volRoot := filepath.Join(rf.opts.StateDir, "volumes")
	vm, err := volume.NewManager(volRoot, rf.volumeBackend, log)
	if err != nil {
		return nil, nil, err
	}
	cfg.Reconciler.Volumes, cfg.VolumeRoot, cfg.VolumeBackend = vm, volRoot, vm.Backend()
	cfg.WireguardEndpoint = nf.wgEndpoint
	cfg.Version, cfg.Log = version, log
	if creds != nil {
		if err := creds(cfg.CredentialsPath); err != nil {
			return nil, nil, err
		}
	}
	a, err := agent.NewConnected(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	nc, err := a.NodeConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	if addr := nc.GetRegistryAddress(); addr != "" {
		// The node pulls shoal.local and <registry>/ images from the embedded registry (ADR 0020, pass B).
		rf.opts.Registry = a.Registry(addr)
	}
	netNode, err := network.NewNode(network.NodeConfig{
		PodCIDR: nc.GetPodCidr(), ClusterCIDR: nc.GetClusterCidr(), DNSDomain: nc.GetDnsDomain(),
		WireguardInterface: nf.wgInterface, WireguardPort: nf.wgPort,
		WireguardKeyPath: filepath.Join(dir, "wireguard.key"), Log: log.With("component", "network"),
	})
	if err != nil {
		return nil, nil, err
	}
	rf.opts.PodCIDR = nc.GetPodCidr() // the server's assignment wins over --pod-cidr
	rf.opts.DNSServer, rf.opts.DNSSearch = netNode.DNSServer(), []string{nc.GetDnsDomain()}
	rt, err := rf.open(ctx)
	if err != nil {
		return nil, nil, err
	}
	a.Attach(rt, netNode)
	// Runs Builds when the server labels this node a builder (ADR 0021, pass B).
	go a.RunBuilder(ctx)
	log.Info("node network", "podCIDR", nc.GetPodCidr(), "dns", netNode.DNSServer(), "wireguard", nf.wgInterface)
	return a, func() { rt.Close() }, nil
}
