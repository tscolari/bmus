package main

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/store"
)

func newCrewCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "crew", Short: "Manage the crew"}

	var (
		args   bridge.CrewAddArgs
		asJSON bool
	)
	add := &cobra.Command{
		Use:   "add <id>",
		Short: "Add a crew member and start its first tour (provisions a beam)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			args.ID = pos[0]
			if args.Harness != "" && !slices.Contains(bridge.Harnesses, args.Harness) {
				return fmt.Errorf("--harness: want one of %s, got %q", strings.Join(bridge.Harnesses, ", "), args.Harness)
			}
			if args.Dossier != "" {
				abs, err := filepath.Abs(args.Dossier)
				if err != nil {
					return err
				}
				args.Dossier = abs
			}
			var c bridge.CrewView
			if err := a.call(cmd.Context(), "crew.add", args, &c); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), c)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "crew %s added (%s); tour started, provisioning a beam\n", c.ID, c.Status)
			return nil
		},
	}
	add.Flags().StringVar(&args.Name, "name", "", "display name (required)")
	add.Flags().StringVar(&args.Rank, "rank", "", "rank")
	add.Flags().StringVar(&args.Division, "division", "", "division")
	add.Flags().StringVar(&args.Spec, "spec", "", "specialty")
	add.Flags().StringVar(&args.Dossier, "dossier", "", "dossier file")
	add.Flags().StringVar(&args.Harness, "harness", "",
		"harness the crew member's beams run, fixed for a tour: "+strings.Join(bridge.Harnesses, ", ")+" (default: the division's in the roster, else serve's, sdk)")
	add.Flags().StringVar(&args.Model, "model", "",
		"model the crew member's orders run on unless an order names one (default: the division's in the roster, else the harness's own); passed to the harness as is")
	add.Flags().BoolVar(&asJSON, "json", false, "print the crew member as JSON")
	add.MarkFlagRequired("name")

	var listJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List the crew",
		Long: `List the crew. HARNESS and MODEL are resolved, each followed by its source:
(crew) the crew member's own, (division) its division's default in the roster,
or (default) neither: serve's sdk harness, or the harness's own model (-). An
order's own model overrides MODEL for that order. If the roster changed since a
crew member's beam was provisioned, HARNESS shows what its next tour will get.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res bridge.CrewListResult
			if err := a.call(cmd.Context(), "crew.list", nil, &res); err != nil {
				return err
			}
			if listJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			w := newTable(cmd.OutOrStdout())
			fmt.Fprintln(w, "ID\tNAME\tRANK\tDIVISION\tHARNESS\tMODEL\tSTATUS\tBEAM\tTOUR")
			for _, c := range res.Crew {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Name, dash(c.Rank), dash(c.Division),
					sourced(c.Harness, c.HarnessSource, "sdk"), sourced(c.Model, c.ModelSource, "-"),
					c.Status, beamCell(c), strconv.Itoa(c.TourNo))
			}
			return w.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "print JSON")

	dismiss := crewActionCmd(a, "dismiss", "crew.dismiss",
		"Dismiss a crew member: its tour ends, its running or parked order fails, and its beam is removed",
		map[string]string{
			bridge.OutcomeDone:      "crew %s dismissed\n",
			bridge.OutcomeRequested: "crew %s dismissed; its tour is ending and will remove its beam\n",
		})
	reenergize := crewActionCmd(a, "reenergize", "crew.reenergize",
		"Give a crew member in sickbay a new tour and beam; it resumes its parked order",
		map[string]string{
			bridge.OutcomeRequested: "re-energize of crew %s requested; its new tour is provisioning a beam\n",
			bridge.OutcomeUnderWay:  "re-energize of crew %s already under way\n",
		})

	cmd.AddCommand(add, list, dismiss, reenergize)
	return cmd
}

// sourced is a resolved harness or model followed by its source, e.g.
// "echo (division)"; def stands in for an empty value.
func sourced(v, source, def string) string {
	return cmp.Or(v, def) + " (" + source + ")"
}

// crewActionCmd is `bmus crew <use> <id>`, calling rpc with the id and
// printing the message for its outcome (with the id) or, with --json, the
// result.
func crewActionCmd(a *app, use, rpc, short string, says map[string]string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			var res bridge.CrewActionResult
			if err := a.call(cmd.Context(), rpc, bridge.CrewIDArgs{ID: pos[0]}, &res); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			say, ok := says[res.Outcome]
			if !ok {
				say = "crew %s: " + res.Outcome + "\n"
			}
			fmt.Fprintf(cmd.OutOrStdout(), say, res.ID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the crew member as JSON")
	return cmd
}

// beamCell is crew list's BEAM: the current beam, "provisioning" for a
// crew member beaming in without one yet (#106), else "-".
func beamCell(c bridge.CrewView) string {
	if c.CurrentBeam == "" && c.Status == store.CrewBeamingIn {
		return "provisioning"
	}
	return dash(c.CurrentBeam)
}
