package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"example.com/shoal/internal/agent"
	"example.com/shoal/internal/server"
)

func newServerCmd(c *cli) *cobra.Command {
	var (
		dataDir, listen, advertise string
		clusterCIDR, serviceCIDR   string
		keyFile                    string
		uiListen, uiCert, uiKey    string
		uiNames                    []string
		withAgent                  bool
		rf                         runtimeFlags
		nf                         networkFlags
		nodeName                   string
		stopTimeout                time.Duration
	)
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the control plane",
		Long: `Run the control plane server.

On first start it creates the cluster CA and state database in --data-dir and
writes admin credentials for the CLI to <data-dir>/admin.conf. Create a join
token with ` + "`shoal token create`" + ` to add nodes.

With --with-agent the server also runs an agent for this machine, which
joins without a token: a complete single-node cluster in one process.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := c.logger()
			if err != nil {
				return err
			}
			if advertise == "" {
				if advertise, err = defaultAdvertise(listen); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			srv, err := server.New(ctx, server.Config{DataDir: dataDir, AdvertiseAddress: advertise, Log: log,
				ClusterCIDR: clusterCIDR, ServiceCIDR: serviceCIDR, EncryptionKeyFile: keyFile})
			if err != nil {
				return err
			}
			defer srv.Close()
			lis, err := net.Listen("tcp", listen)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", listen, err)
			}
			log.Info("server starting", "version", version, "dataDir", dataDir, "caHash", srv.CAHash())

			var wg sync.WaitGroup
			defer wg.Wait()
			if uiListen != "" {
				stop, err := servePortal(ctx, srv, log, uiListen, uiCert, uiKey, uiNames, &wg)
				if err != nil {
					lis.Close()
					return err
				}
				defer stop()
			}
			if withAgent {
				// In the background: the agent's setup asks this very server
				// for the node's network settings, so Serve must be running.
				wg.Add(1)
				go func() {
					defer wg.Done()
					a, closeRT, err := localAgent(ctx, srv, &rf, &nf, log, nodeName, stopTimeout)
					if err != nil {
						if ctx.Err() == nil {
							log.Error("local agent failed to start", "err", err)
						}
						return
					}
					defer closeRT() // only after the agent has stopped using it
					if err := a.Run(ctx); err != nil {
						log.Error("local agent failed", "err", err)
					}
				}()
			}
			err = srv.Serve(ctx, lis)
			log.Info("server stopped")
			return err
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/var/lib/shoal/server", "directory for the state database and cluster PKI")
	cmd.Flags().StringVar(&listen, "listen", ":7443", "address to listen on")
	cmd.Flags().StringVar(&advertise, "advertise-address", "", "host:port that agents and the CLI use to reach this server (default: first non-loopback IPv4 and the --listen port)")
	cmd.Flags().StringVar(&clusterCIDR, "cluster-cidr", "10.250.0.0/16", "container subnets: one /24 per node is taken from it")
	cmd.Flags().StringVar(&serviceCIDR, "service-cidr", "10.96.0.0/16", "virtual IPs for Services")
	cmd.Flags().StringVar(&keyFile, "encryption-key-file", "", "key that encrypts Secrets at rest (default <data-dir>/secret.key, created on first start; back it up)")
	cmd.Flags().BoolVar(&withAgent, "with-agent", false, "also run an agent for this machine (single-node cluster)")
	cmd.Flags().StringVar(&uiListen, "ui-listen", ":7444", "address of the web portal (HTTPS; empty disables it)")
	cmd.Flags().StringVar(&uiCert, "ui-cert", "", "certificate file for the portal (default: issued by the cluster CA)")
	cmd.Flags().StringVar(&uiKey, "ui-key", "", "private key file for --ui-cert")
	cmd.Flags().StringSliceVar(&uiNames, "ui-name", nil, "extra host names or IPs for the portal's certificate (repeatable)")
	cmd.Flags().StringVar(&nodeName, "node-name", "", "name of the local node with --with-agent (default: host name)")
	cmd.Flags().DurationVar(&stopTimeout, "stop-timeout", 10*time.Second, "grace period before SIGKILL (local agent)")
	rf.register(cmd.Flags())
	nf.register(cmd.Flags())
	return cmd
}

// servePortal serves the web portal over HTTPS until ctx is done; stop
// shuts it down.
func servePortal(ctx context.Context, srv *server.Server, log *slog.Logger, listen, certFile, keyFile string, names []string, wg *sync.WaitGroup) (func(), error) {
	tc, err := srv.PortalTLS(certFile, keyFile, names)
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("portal: listen on %s: %w", listen, err)
	}
	hs := &http.Server{
		Handler: srv.PortalHandler(), TLSConfig: tc,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug), // TLS handshake noise from untrusting browsers
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := hs.ServeTLS(lis, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("portal stopped", "err", err)
		}
	}()
	log.Info("web portal listening", "url", "https://"+portalHost(listen, srv)+"/")
	return func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx) // log streams are cut after the timeout
	}, nil
}

func portalHost(listen string, srv *server.Server) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		if h, _, err := net.SplitHostPort(srv.AdvertiseAddress()); err == nil {
			host = h
		}
	}
	return net.JoinHostPort(host, port)
}

// localAgent prepares the in-process agent of `server --with-agent`. Its
// credentials are issued directly by the server the first time.
func localAgent(ctx context.Context, srv *server.Server, rf *runtimeFlags, nf *networkFlags, log *slog.Logger, nodeName string, stopTimeout time.Duration) (*agent.Connected, func(), error) {
	creds := func(path string) error {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return nil // already joined
		}
		name := nodeName
		if name == "" {
			var err error
			if name, err = agent.DefaultNodeName(); err != nil {
				return err
			}
		}
		c, err := srv.LocalNodeCredentials(ctx, name)
		if err != nil {
			return err
		}
		return c.Save(path)
	}
	return startConnected(ctx, rf, nf, log.With("component", "agent"), agent.ConnectedConfig{
		NodeName: nodeName, Reconciler: agent.Config{StopTimeout: stopTimeout},
	}, creds)
}

// defaultAdvertise derives the address to advertise from the listen address:
// its host if specific, otherwise the first non-loopback IPv4 address.
func defaultAdvertise(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("invalid --listen %q: %w", listen, err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", fmt.Errorf("invalid --listen %q: port must be numeric", listen)
	}
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return listen, nil
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("list interface addresses: %w", err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return net.JoinHostPort(n.IP.String(), port), nil
		}
	}
	return "", errors.New("no non-loopback IPv4 address found; set --advertise-address")
}
