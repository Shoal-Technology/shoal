package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/install"
)

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage join tokens",
	}
	var (
		cf          clientFlags
		ttl         time.Duration
		maxUses     int32
		description string
		bundle      bool
		out         string
	)
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a token that lets nodes join the cluster",
		Long: `Create a join token and print the command that joins a node with it.

Anyone holding the token can add a node until it expires, so keep it secret
and prefer short lifetimes or --max-uses.

With --bundle --out FILE the token is written to FILE as a join bundle
(shoal-join/v1, mode 0600) for ` + "`shoal init --join FILE`" + ` on a new host.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.CreateJoinToken(ctx, &v1alpha1.CreateJoinTokenRequest{
				Ttl: durationpb.New(ttl), MaxUses: maxUses, Description: description,
			})
			if err != nil {
				return err
			}
			uses := "unlimited uses"
			if maxUses > 0 {
				uses = fmt.Sprintf("%d use(s)", maxUses)
			}
			if bundle {
				if out == "" {
					return fmt.Errorf("--bundle needs --out FILE")
				}
				expires := resp.ExpiresAt.AsTime().UTC().Format(time.RFC3339)
				data, err := json.MarshalIndent(install.Bundle{
					Format: install.BundleFormat, Server: c.server, Token: resp.Token,
					CAHash: resp.CaHash, ExpiresAt: expires,
				}, "", "  ")
				if err != nil {
					return err
				}
				if err := writeSecretFile(out, append(data, '\n')); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote join bundle %s (mode 0600, valid until %s, %s).\n"+
					"It holds a join token: treat it as a secret. On the new host run:\n"+
					"  sudo shoal init --join %s\n", out, expires, uses, out)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Join a node by running this on it (as root):\n\n"+
				"  shoal agent --server %s --token %s --ca-hash %s\n\n"+
				"The token is valid until %s for %s.\n",
				c.server, resp.Token, resp.CaHash, resp.ExpiresAt.AsTime().Local().Format(time.RFC3339), uses)
			return nil
		},
	}
	cf.register(create.Flags())
	create.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "how long the token is valid (1m to 720h)")
	create.Flags().Int32Var(&maxUses, "max-uses", 0, "number of nodes that may join with it (0 = unlimited)")
	create.Flags().StringVar(&description, "description", "", "note stored with the token")
	create.Flags().BoolVar(&bundle, "bundle", false, "write a join bundle for `shoal init --join` instead of printing the join command")
	create.Flags().StringVar(&out, "out", "", "file for --bundle (created with mode 0600; must not exist)")
	cmd.AddCommand(create)
	return cmd
}
