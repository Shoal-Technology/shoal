package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/security"
	"github.com/kaiserern/shoal/internal/server"
)

// clientFlags configure how CLI commands reach the server.
type clientFlags struct {
	config  string
	server  string
	timeout time.Duration
}

func (f *clientFlags) register(fs *pflag.FlagSet) {
	fs.StringVar(&f.config, "config", "", "client credentials (default: $SHOAL_CONFIG, ~/.shoal/config, then the server's admin.conf)")
	fs.StringVar(&f.server, "server", "", "server host:port (overrides the address in the credentials)")
	fs.DurationVar(&f.timeout, "timeout", 30*time.Second, "request timeout")
}

// configPath picks the first credentials file that exists.
func (f *clientFlags) configPath() (string, error) {
	if f.config != "" {
		return f.config, nil
	}
	if p := os.Getenv("SHOAL_CONFIG"); p != "" {
		return p, nil
	}
	candidates := []string{server.AdminConfigPath("/var/lib/shoal/server")}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append([]string{filepath.Join(home, ".shoal", "config")}, candidates...)
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no client credentials found: pass --config, set SHOAL_CONFIG, " +
		"or copy the server's admin.conf to ~/.shoal/config (run as root to use /var/lib/shoal/server/admin.conf directly)")
}

type client struct {
	conn    *grpc.ClientConn
	control v1alpha1.ControlClient
	server  string
	timeout time.Duration
}

func (f *clientFlags) connect() (*client, error) {
	path, err := f.configPath()
	if err != nil {
		return nil, err
	}
	cfg, err := security.LoadClientConfig(path)
	if err != nil {
		return nil, err
	}
	p, err := cfg.Parse()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	addr := cfg.Server
	if f.server != "" {
		addr = f.server
	}
	// A write sent to a follower fails with the leader's address; the
	// interceptor retries it once there (ADR 0014).
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(p.TLS())),
		grpc.WithUnaryInterceptor(server.LeaderRetry(p.TLS())))
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	return &client{conn: conn, control: v1alpha1.NewControlClient(conn), server: addr, timeout: f.timeout}, nil
}

func (c *client) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, c.timeout)
}

func (c *client) Close() error { return c.conn.Close() }
