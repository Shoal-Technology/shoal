package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"example.com/shoal/internal/install"
)

var (
	emailRe    = regexp.MustCompile(`^[^@\s"']+@[^@\s"']+\.[^@\s"']+$`)
	nodeNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

func newInitCmd() *cobra.Command {
	var (
		email, joinFile, rootDir, nodeName, swap string
		acmeDir, acmeCARoots                     string
		acceptTOS, edge, openFirewall            bool
		dryRun, keepBundle                       bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set this machine up as a Shoal node: containerd, server, agent and edge",
		Long: `Set this machine up as a Shoal node (ADR 0017, section 1).

First node:
  sudo shoal init --email ops@example.com --accept-tos

Joining node (the bundle comes from "shoal token create --bundle --out join.json"):
  sudo shoal init --join join.json

Init runs read-only preflight checks first and exits 2 with no changes if one
fails. It reuses a containerd 2.x already on the host, and otherwise installs
the pinned containerd, runc and CNI plugins after checking their SHA-256.
--dry-run prints every step and changes nothing. Re-running is safe: finished
steps print "ok".

Exit codes: 0 done, 1 a step failed, 2 preflight failed (nothing changed),
3 partly applied (run it again to continue).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if email != "" && !emailRe.MatchString(email) {
				return fmt.Errorf("--email %q is not an email address", email)
			}
			if nodeName != "" && !nodeNameRe.MatchString(nodeName) {
				return fmt.Errorf("--node-name %q: use lower-case letters, digits and dashes", nodeName)
			}
			var bundle *install.Bundle
			if joinFile != "" {
				if email != "" || acceptTOS {
					return fmt.Errorf("--email and --accept-tos are for the first node; a joining node runs no server")
				}
				data, err := os.ReadFile(joinFile)
				if err != nil {
					return err
				}
				if bundle, err = install.ParseBundle(data); err != nil {
					return err
				}
			}
			edgeOn := edge
			if joinFile != "" && !cmd.Flags().Changed("edge") {
				edgeOn = false // a joined node is an edge only with --edge
			}
			bin, err := selfBinary()
			if err != nil {
				return err
			}
			name, home, err := invokingUser()
			if err != nil {
				return err
			}
			h, err := install.NewLocalHost(rootDir)
			if err != nil {
				return err
			}
			opts := install.Options{
				DryRun: dryRun, Email: email, AcceptTOS: acceptTOS, NodeName: nodeName,
				Edge: edgeOn, Swap: swap, OpenFirewall: openFirewall, Join: bundle,
				Binary: bin, UserName: name, UserHome: home,
				ACMEDirectory: acmeDir, ACMECARoots: acmeCARoots, Out: cmd.OutOrStdout(),
			}
			if err := install.Init(cmd.Context(), h, opts); err != nil {
				return err
			}
			if joinFile != "" && !dryRun && !keepBundle {
				if err := os.Remove(joinFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("join bundle %s was used but not removed: %w", joinFile, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed the join bundle %s (--keep-bundle keeps it).\n", joinFile)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&email, "email", "", "contact email for the ACME account (certificates); needed with --accept-tos")
	f.BoolVar(&acceptTOS, "accept-tos", false, "accept the ACME provider's terms of service (explicit consent; never implied)")
	f.StringVar(&nodeName, "node-name", "", "name of this node (default: the host name)")
	f.BoolVar(&edge, "edge", true, "label this node as an ingress edge (shoal.dev/edge=true, ports 80 and 443)")
	f.StringVar(&swap, "swap", "", "create a swap file of this size, e.g. 1G (opt-in; never changed otherwise)")
	f.BoolVar(&openFirewall, "open-firewall", false, "allow 80/tcp and 443/tcp (and 51820/udp for --join) in ufw, when ufw is active (opt-in)")
	f.BoolVar(&dryRun, "dry-run", false, "print every step and its action; change nothing")
	f.StringVar(&joinFile, "join", "", "join bundle from `shoal token create --bundle`: set up a node that joins an existing cluster")
	f.BoolVar(&keepBundle, "keep-bundle", false, "keep the join bundle after a successful join (default: remove it)")
	f.StringVar(&rootDir, "root-dir", "", "write under this directory instead of / (tests only)")
	f.StringVar(&acmeDir, "acme-directory", "", "ACME directory for the server (tests only)")
	f.StringVar(&acmeCARoots, "acme-ca-roots", "", "PEM file of extra roots for the ACME server (tests only)")
	for _, h := range []string{"root-dir", "acme-directory", "acme-ca-roots"} {
		_ = f.MarkHidden(h)
	}
	return cmd
}

func newUninstallCmd() *cobra.Command {
	var (
		dryRun, purge, yes bool
		rootDir            string
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove what shoal init installed",
		Long: `Stop and remove the shoal units, the shoal binary and the files shoal init wrote.

containerd and the CNI plugins are removed only when init installed them and no
containers outside the shoal namespace are running. A containerd that existed
before init is never removed. /var/lib/shoal (cluster CA, Secrets, volumes) is
kept unless --purge --yes is given. ~/.shoal/config is never removed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, home, err := invokingUser()
			if err != nil {
				return err
			}
			h, err := install.NewLocalHost(rootDir)
			if err != nil {
				return err
			}
			return install.Uninstall(cmd.Context(), h, install.UninstallOptions{
				DryRun: dryRun, Purge: purge, Yes: yes, UserHome: home, Out: cmd.OutOrStdout(),
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "print what would be removed; change nothing")
	f.BoolVar(&purge, "purge", false, "also delete /var/lib/shoal: cluster CA, Secrets and volume data (needs --yes)")
	f.BoolVar(&yes, "yes", false, "confirm --purge")
	f.StringVar(&rootDir, "root-dir", "", "operate under this directory instead of / (tests only)")
	_ = f.MarkHidden("root-dir")
	return cmd
}

// selfBinary returns the bytes of the running shoal executable.
func selfBinary() ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return os.ReadFile(exe)
}

// invokingUser returns the user the admin config belongs to: SUDO_USER when
// set, otherwise the current user. Its home directory holds ~/.shoal/config.
func invokingUser() (string, string, error) {
	name := os.Getenv("SUDO_USER")
	if name == "" {
		u, err := user.Current()
		if err != nil {
			return "", "", err
		}
		name = u.Username
	}
	u, err := user.Lookup(name)
	if err != nil {
		return "", "", fmt.Errorf("look up user %s: %w", name, err)
	}
	return u.Username, u.HomeDir, nil
}

// readTokenFile reads a join token that shoal init wrote, and deletes the file
// (the token is single use). A missing file is not an error: the saved
// credentials are enough on later starts.
func readTokenFile(p string) (string, error) {
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := os.Remove(p); err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
