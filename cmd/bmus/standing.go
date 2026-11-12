package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/bridge"
)

func newStandingOrdersCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "standing-orders [on|off]",
		Short: "Print standing orders' state, or turn them on or off",
		Long: `Print standing orders' state, or turn them on or off.

With standing orders on, crew members on deck take pool orders of their
division. The roster's standing_orders is only the default for a new
bridge: once set, the switch keeps its value across restarts.`,
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, pos []string) error {
			var res bridge.StandingOrdersResult
			var err error
			if len(pos) == 0 {
				err = a.call(cmd.Context(), "standing_orders.get", nil, &res)
			} else {
				on := pos[0] == "on"
				err = a.call(cmd.Context(), "standing_orders.set", bridge.StandingOrdersSetArgs{On: &on}, &res)
			}
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			state := "off"
			if res.On {
				state = "on"
			}
			fmt.Fprintln(cmd.OutOrStdout(), state)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
