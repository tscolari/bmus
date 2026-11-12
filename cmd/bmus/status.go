package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/bridge"
)

func newStatusCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print the bridge's condition (green, yellow, red) and counters",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var s bridge.BridgeStatus
			if err := a.call(cmd.Context(), "bridge.status", nil, &s); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), s)
			}
			cond := s.Condition
			if len(s.Reasons) > 0 {
				cond += " (" + strings.Join(s.Reasons, ", ") + ")"
			}
			w := newTable(cmd.OutOrStdout())
			fmt.Fprintf(w, "condition\t%s\n", cond)
			fmt.Fprintf(w, "crew\t%d beaming in, %d on deck, %d away, %d hailing, %d in sickbay\n",
				s.Crew.BeamingIn, s.Crew.OnDeck, s.Crew.Away, s.Crew.Hailing, s.Crew.Sickbay)
			fmt.Fprintf(w, "orders\t%d open, %d away\n", s.Orders.Open, s.Orders.Away)
			fmt.Fprintf(w, "hails\t%d open\n", s.HailsOpen)
			fmt.Fprintf(w, "beams\t%d live\n", s.BeamsLive)
			standing := "off"
			if s.StandingOrders {
				standing = "on"
			}
			fmt.Fprintf(w, "standing orders\t%s\n", standing)
			fmt.Fprintf(w, "log\tlast id %d\n", s.LastLogID)
			for _, info := range s.Info {
				fmt.Fprintf(w, "info\t%s\n", info)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
