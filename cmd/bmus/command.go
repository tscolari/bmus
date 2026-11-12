package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/bridge"
)

func newCommandCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "command", Short: "Manage Command, the agent that answers routed hails"}
	guidance := &cobra.Command{
		Use:   "guidance",
		Short: "Set or show the captain's guidance for Command",
		Long: `Set or show the captain's guidance for Command.

The guidance is free text every consult of Command carries, at most 4096
bytes. It keeps its value across restarts.`,
	}
	printGuidance := func(cmd *cobra.Command, asJSON bool, res bridge.CommandGuidanceResult) error {
		if asJSON {
			return printJSON(cmd.OutOrStdout(), res)
		}
		if res.Guidance != "" {
			fmt.Fprintln(cmd.OutOrStdout(), res.Guidance)
		}
		return nil
	}
	var setJSON, showJSON bool
	set := &cobra.Command{
		Use:   "set <text>",
		Short: "Set the guidance; an empty text clears it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			var res bridge.CommandGuidanceResult
			if err := a.call(cmd.Context(), "command.guidance.set", bridge.CommandGuidanceSetArgs{Guidance: &pos[0]}, &res); err != nil {
				return err
			}
			return printGuidance(cmd, setJSON, res)
		},
	}
	set.Flags().BoolVar(&setJSON, "json", false, "print JSON")
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the guidance; nothing when none is set",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res bridge.CommandGuidanceResult
			if err := a.call(cmd.Context(), "command.guidance.get", nil, &res); err != nil {
				return err
			}
			return printGuidance(cmd, showJSON, res)
		},
	}
	show.Flags().BoolVar(&showJSON, "json", false, "print JSON")
	guidance.AddCommand(set, show)
	cmd.AddCommand(guidance)
	return cmd
}
