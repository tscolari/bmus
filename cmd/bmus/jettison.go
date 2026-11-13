package main

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/bridge"
)

func newJettisonCmd(a *app) *cobra.Command {
	var (
		olderThan string
		yes       bool
		asJSON    bool
	)
	cmd := &cobra.Command{
		Use:     "jettison",
		Aliases: []string{"purge"},
		Short:   "Delete the bridge's branches of orders that ended a while ago",
		Long: `Delete the bmus/<order> branches in the bridge's clones of orders that
ended (done, failed, belayed or withdrawn) at least --older-than ago.

It never touches the branch of an open order, any other branch, or
anything on a remote. Without --yes it is a dry run: it lists the branches
it would delete, per repo and order. A jettisoned order's mission report
shows "branch jettisoned" in place of its diff.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := bridge.ParseAge(olderThan); err != nil {
				return fmt.Errorf("--older-than: %w", err)
			}
			var res bridge.JettisonResult
			if err := a.call(cmd.Context(), "bridge.jettison", bridge.JettisonArgs{OlderThan: olderThan, Yes: yes}, &res); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			return printJettison(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", bridge.DefaultJettisonAge,
		"keep branches of orders that ended more recently: a duration such as 36h, or days such as 7d")
	cmd.Flags().BoolVar(&yes, "yes", false, "delete the branches; without it, only list them")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// printJettison writes res's branches as a table, then a summary line.
func printJettison(w io.Writer, res bridge.JettisonResult) error {
	if len(res.Branches) > 0 {
		tw := newTable(w)
		fmt.Fprintln(tw, "REPO\tORDER\tBRANCH\tENDED")
		for _, b := range res.Branches {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.Repo, b.Order, b.Branch, b.EndedAt.UTC().Format(time.RFC3339))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, jettisonSummary(res))
	return err
}

// jettisonSummary is printJettison's last line.
func jettisonSummary(res bridge.JettisonResult) string {
	orders := map[string]bool{}
	for _, b := range res.Branches {
		orders[b.Order] = true
	}
	n := fmt.Sprintf("%s of %s", plural(len(res.Branches), "branch", "branches"), plural(len(orders), "order", "orders"))
	kept := fmt.Sprintf("%s kept", plural(res.Kept, "branch", "branches"))
	switch {
	case len(res.Branches) == 0:
		return fmt.Sprintf("nothing to jettison: no order ended over %s ago has a branch; %s", res.OlderThan, kept)
	case res.Deleted:
		return fmt.Sprintf("jettisoned %s that ended over %s ago; %s", n, res.OlderThan, kept)
	default:
		return fmt.Sprintf("would jettison %s that ended over %s ago; %s; run with --yes to delete them", n, res.OlderThan, kept)
	}
}

// plural is "1 thing" or "n things".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
