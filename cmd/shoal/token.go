package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	"example.com/shoal/api/v1alpha1"
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
	)
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a token that lets nodes join the cluster",
		Long: `Create a join token and print the command that joins a node with it.

Anyone holding the token can add a node until it expires, so keep it secret
and prefer short lifetimes or --max-uses.`,
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
	cmd.AddCommand(create)
	return cmd
}
