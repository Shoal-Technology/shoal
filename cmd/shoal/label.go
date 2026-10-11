package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kaiserern/shoal/api/v1alpha1"
)

// newLabelCmd sets and removes node labels. shoal.dev/edge=true makes a node
// an ingress edge (ADR 0015).
func newLabelCmd() *cobra.Command {
	var cf clientFlags
	cmd := &cobra.Command{
		Use:   "label node NAME KEY=VALUE|KEY- ...",
		Short: "Set or remove labels on a node",
		Long: `Set or remove labels on a node.

KEY=VALUE sets a label and KEY- removes it. shoal.dev/edge=true makes the
node an ingress edge: it listens on --edge-http and --edge-https and serves
the Routes. Give at least two edge nodes and point DNS at them.`,
		Example: "  shoal label node edge-1 shoal.dev/edge=true\n  shoal label node edge-1 shoal.dev/edge-",
		Args:    cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "node" {
				return fmt.Errorf("only nodes have labels: want `shoal label node NAME KEY=VALUE`")
			}
			set := map[string]string{}
			var remove []string
			for _, a := range args[2:] {
				if k, v, ok := strings.Cut(a, "="); ok {
					set[k] = v
					continue
				}
				if k, ok := strings.CutSuffix(a, "-"); ok && k != "" {
					remove = append(remove, k)
					continue
				}
				return fmt.Errorf("%q: want KEY=VALUE to set or KEY- to remove", a)
			}
			c, err := cf.connect()
			if err != nil {
				return err
			}
			defer c.Close()
			ctx, cancel := c.ctx(cmd.Context())
			defer cancel()
			resp, err := c.control.SetNodeLabels(ctx, &v1alpha1.SetNodeLabelsRequest{Node: args[1], Set: set, Remove: remove})
			if err != nil {
				return err
			}
			var parts []string
			for _, k := range slices.Sorted(maps.Keys(resp.GetLabels())) {
				parts = append(parts, k+"="+resp.GetLabels()[k])
			}
			fmt.Fprintf(cmd.OutOrStdout(), "node/%s labels: %s\n", args[1], dash(strings.Join(parts, " ")))
			return nil
		},
	}
	cf.register(cmd.Flags())
	return cmd
}
