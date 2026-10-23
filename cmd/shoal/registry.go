package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Shoal-Technology/shoal/internal/registry"
	"github.com/Shoal-Technology/shoal/internal/security"
)

// newRegistryCmd is `shoal registry`: the embedded OCI registry (ADR 0020).
func newRegistryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "list images in the embedded registry, and set up docker push",
	}
	cmd.AddCommand(newRegistryLsCmd(), newRegistryLoginCmd())
	return cmd
}

// registryFlags locate the registry and the admin credentials.
type registryFlags struct {
	cf       clientFlags
	registry string
}

func (f *registryFlags) register(fs *pflag.FlagSet) {
	fs.StringVar(&f.registry, "registry", "", "registry host:port (the server's --registry-listen address)")
	f.cf.register(fs)
}

// load returns the admin credentials and the registry address.
func (f *registryFlags) load() (*security.ClientConfig, string, error) {
	if f.registry == "" {
		return nil, "", errors.New("pass --registry host:port (the server's --registry-listen address)")
	}
	path, err := f.cf.configPath()
	if err != nil {
		return nil, "", err
	}
	cfg, err := security.LoadClientConfig(path)
	if err != nil {
		return nil, "", err
	}
	return cfg, f.registry, nil
}

// httpClient returns a client that trusts the cluster CA and presents the
// admin certificate, which is what the registry needs for the tag listing.
func httpClient(cfg *security.ClientConfig) (*http.Client, error) {
	p, err := cfg.Parse()
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(p.CA)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      pool,
		Certificates: []tls.Certificate{p.TLSCert},
	}}}, nil
}

func newRegistryLsCmd() *cobra.Command {
	var rf registryFlags
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "list the repositories and tags, with the digest each tag points to",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, addr, err := rf.load()
			if err != nil {
				return err
			}
			hc, err := httpClient(cfg)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), rf.cf.timeout)
			defer cancel()
			var tags []registry.Tag
			if err := getJSON(ctx, hc, "https://"+addr+registry.TagsPath, &tags); err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "REPOSITORY\tTAG\tDIGEST")
			for _, t := range tags {
				fmt.Fprintf(w, "%s\t%s\t%s\n", t.Repository, t.Tag, t.Digest)
			}
			return w.Flush()
		},
	}
	rf.register(cmd.Flags())
	return cmd
}

func getJSON(ctx context.Context, hc *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry: %s returned %s (is --registry right, and are these admin credentials?)", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func newRegistryLoginCmd() *cobra.Command {
	var rf registryFlags
	var to string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "write the CA and admin certificate for docker push, and print the steps",
		Long: `Writes ca.crt, client.cert and client.key (mode 0600) to a directory and prints
the commands that install them where docker looks for a registry's certificates.
Docker authenticates to the registry with the client certificate, so no
password is needed. The admin certificate can push any repository: keep the
directory private, and delete it when you are done.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, addr, err := rf.load()
			if err != nil {
				return err
			}
			dir := to
			if dir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				dir = filepath.Join(home, ".shoal", "registry", strings.NewReplacer(":", "_", "[", "", "]", "").Replace(addr))
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			files := map[string]string{"ca.crt": cfg.CACertificate, "client.cert": cfg.Certificate, "client.key": cfg.Key}
			for name, data := range files {
				if err := security.WriteFileAtomic(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
					return err
				}
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "wrote ca.crt, client.cert and client.key to %s\n\n", dir)
			fmt.Fprintf(out, "On the machine that pushes images (as root):\n")
			fmt.Fprintf(out, "  mkdir -p /etc/docker/certs.d/%s\n", addr)
			fmt.Fprintf(out, "  cp %s/ca.crt %s/client.cert %s/client.key /etc/docker/certs.d/%s/\n", dir, dir, dir, addr)
			fmt.Fprintf(out, "Then push with:\n  docker push %s/<namespace>/<name>:<tag>\n", addr)
			return nil
		},
	}
	rf.register(cmd.Flags())
	cmd.Flags().StringVar(&to, "to", "", "directory for the certificate files (default ~/.shoal/registry/<registry>)")
	return cmd
}
