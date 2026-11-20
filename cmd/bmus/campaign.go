package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/store"
)

func newCampaignCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "campaign", Short: "Run a tracker issue through work, review and landing"}

	var (
		runJSON   bool
		run       bridge.CampaignRunArgs
		maxRounds int
	)
	runCmd := &cobra.Command{
		Use:   "run --issue <ref> --repo <name> (--division <d> | --assignee <crew>)",
		Short: "Start a campaign on one tracker issue",
		Long: `Start a campaign on one tracker issue: a work order from the issue, a review
by a crew member who never worked on it, fix rounds while the review blocks,
and a push of the passing head once you allow it (its campaign_land hail).

--issue is a tracker ref (#12, or owner/name#12 on GitHub) and --repo a repo
registered with the bridge. --base is where round 1's work starts (default
main). --assignee directs round 1's work to a crew member; --division alone
pools it in that division. --review-division is where reviews go (default
science). --max-rounds caps the rounds, the implementation and its fixes,
from 1 to 10 (default 3). --land push is the only land mode. --branch is the
branch the task lands on, by default campaign/<id>/<ref with every character
outside [A-Za-z0-9._-] made '-'>.

The bridge refuses, before it writes anything: no tracker; a ref the tracker
refuses; an issue it can't find or that is closed; an issue in another
campaign's open task; a repo or base an order would be refused; an unknown
division or assignee; a review division with no crew member; max-rounds out
of range; land pr or merge; a bad branch name; and a branch that already
exists on the repo's remote.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			run.MaxRounds = &maxRounds
			var res bridge.CampaignView
			if err := a.call(cmd.Context(), "campaign.run", run, &res); err != nil {
				return err
			}
			if runJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintln(cmd.OutOrStdout(), runLine(res))
			return nil
		},
	}
	f := runCmd.Flags()
	f.StringVar(&run.Issue, "issue", "", "the tracker issue, such as #12 (required)")
	f.StringVar(&run.Repo, "repo", "", "the registered repo to work on (required)")
	f.StringVar(&run.Base, "base", "", "the ref round 1's work starts from (default main)")
	f.StringVar(&run.Division, "division", "", "the division whose pool takes the work")
	f.StringVar(&run.Assignee, "assignee", "", "the crew member round 1's work goes to")
	f.StringVar(&run.ReviewDivision, "review-division", "", "the division whose pool takes the reviews (default science)")
	f.IntVar(&maxRounds, "max-rounds", 3, "the most rounds, from 1 to 10")
	f.StringVar(&run.Land, "land", "push", "how a passing task lands: push")
	f.StringVar(&run.Branch, "branch", "", "the branch the task lands on (default campaign/<id>/<slug>)")
	f.BoolVar(&runJSON, "json", false, "print JSON")
	_ = runCmd.MarkFlagRequired("issue")
	_ = runCmd.MarkFlagRequired("repo")

	var listJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List campaigns",
		Long: `List campaigns, one row each: its state, its issue and the task's state,
round of max, repo and land branch.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res bridge.CampaignListResult
			if err := a.call(cmd.Context(), "campaign.list", nil, &res); err != nil {
				return err
			}
			if listJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			return printCampaigns(cmd.OutOrStdout(), res.Campaigns)
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "print JSON")

	var showJSON bool
	show := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a campaign, its task and its orders",
		Long: `Show a campaign, then its task (state, round of max, head and commit, open
order, open hail, and why it stalled or dropped), then one row per order.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			var res bridge.CampaignShowResult
			if err := a.call(cmd.Context(), "campaign.show", bridge.CampaignIDArgs{ID: pos[0]}, &res); err != nil {
				return err
			}
			if showJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			return printCampaign(cmd.OutOrStdout(), res)
		},
	}
	show.Flags().BoolVar(&showJSON, "json", false, "print JSON")

	var (
		belayJSON   bool
		belayReason string
	)
	belay := &cobra.Command{
		Use:   "belay <id>",
		Short: "Stop a campaign: its task drops with no stall hail",
		Long: `Stop a campaign. The bridge marks it belaying, closes its task's open hail as
timed out, and withdraws the task's open order if no crew member has claimed
it, or belays it otherwise. The task then drops itself without raising a
stall hail, and the campaign ends. Running it again on a campaign being
belayed closes anything left open; a campaign that has ended is refused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			var res bridge.CampaignBelayResult
			if err := a.call(cmd.Context(), "campaign.belay", bridge.CampaignBelayArgs{ID: pos[0], Reason: belayReason},
				&res); err != nil {
				return err
			}
			if belayJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintln(cmd.OutOrStdout(), campaignBelayLine(res))
			return nil
		},
	}
	belay.Flags().StringVar(&belayReason, "reason", "", "why, for the captain's log")
	belay.Flags().BoolVar(&belayJSON, "json", false, "print JSON")

	cmd.AddCommand(runCmd, list, show, belay)
	return cmd
}

// runLine is `bmus campaign run`'s output: "campaign C-0001 running
// o/r#12 on app from main: work for sorell, reviews in science, up to 3
// rounds, landing by push to campaign/C-0001/o-r-12".
func runLine(v bridge.CampaignView) string {
	c := store.Campaign{Land: v.Land, Repo: v.Repo, BaseRef: v.BaseRef, Division: v.Division, Assignee: v.Assignee,
		ReviewDivision: v.ReviewDivision, MaxRounds: v.MaxRounds}
	var t store.CampaignTask
	if len(v.Tasks) > 0 {
		t = store.CampaignTask{Ref: v.Tasks[0].Ref, Branch: v.Tasks[0].Branch}
	}
	return oneLine("campaign " + v.ID + " " + v.State + " " + bridge.CampaignRunText(c, t))
}

// campaignBelayLine is `bmus campaign belay`'s output: "campaign C-0001
// belaying: hail H-0007 closed, order ORD-0003 belay requested".
func campaignBelayLine(res bridge.CampaignBelayResult) string {
	var parts []string
	for _, h := range res.Hails {
		parts = append(parts, "hail "+h+" closed")
	}
	for _, o := range res.Orders {
		parts = append(parts, "order "+o.ID+" "+o.Action)
	}
	line := "campaign " + res.Campaign.ID + " " + res.Campaign.State
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	return line
}

// roundCell is a task's round of max: "2/3".
func roundCell(t bridge.CampaignTaskView) string {
	return fmt.Sprintf("%d/%d", t.Round, t.MaxRounds)
}

// printCampaigns writes `bmus campaign list`'s table, one row per
// campaign, with its task's ref, state and round (a campaign of stage 3
// has one task).
func printCampaigns(out io.Writer, cs []bridge.CampaignView) error {
	w := newTable(out)
	fmt.Fprintln(w, "ID\tSTATE\tISSUE\tTASK\tROUND\tREPO\tBRANCH")
	for _, c := range cs {
		ref, state, round, branch := "-", "-", "-", "-"
		if len(c.Tasks) > 0 {
			t := c.Tasks[0]
			ref, state, round, branch = oneLine(t.Ref), t.State, roundCell(t), oneLine(t.Branch)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.State, ref, state, round, oneLine(c.Repo), branch)
	}
	return w.Flush()
}

// printCampaign writes `bmus campaign show`: the campaign, each task, then
// one row per order. Text from the tracker or the crew (a ref, a stall's
// reason) goes through oneLine.
func printCampaign(out io.Writer, res bridge.CampaignShowResult) error {
	c := res.Campaign
	w := newTable(out)
	fmt.Fprintf(w, "campaign\t%s\n", c.ID)
	fmt.Fprintf(w, "state\t%s\n", c.State)
	fmt.Fprintf(w, "repo\t%s from %s\n", oneLine(c.Repo), oneLine(c.BaseRef))
	work := "-"
	switch {
	case c.Assignee != "" && c.Division != "":
		work = c.Assignee + " (" + c.Division + ")"
	case c.Assignee != "":
		work = c.Assignee
	case c.Division != "":
		work = c.Division + " pool"
	}
	fmt.Fprintf(w, "work\t%s\n", oneLine(work))
	fmt.Fprintf(w, "reviews\t%s pool\n", oneLine(c.ReviewDivision))
	fmt.Fprintf(w, "max rounds\t%d\n", c.MaxRounds)
	fmt.Fprintf(w, "land\t%s\n", c.Land)
	fmt.Fprintf(w, "created\t%s\n", c.CreatedAt.UTC().Format(time.RFC3339))
	if c.EndedAt != nil {
		fmt.Fprintf(w, "ended\t%s\n", c.EndedAt.UTC().Format(time.RFC3339))
	}
	for _, t := range c.Tasks {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "task\t%s\n", oneLine(t.Ref))
		fmt.Fprintf(w, "state\t%s\n", t.State)
		fmt.Fprintf(w, "round\t%d of %d\n", t.Round, t.MaxRounds)
		fmt.Fprintf(w, "branch\t%s\n", oneLine(t.Branch))
		head := "-"
		if t.Head != "" {
			head = t.Head + " at " + shortSHA(t.HeadCommit)
		}
		fmt.Fprintf(w, "head\t%s\n", head)
		fmt.Fprintf(w, "open order\t%s\n", dash(t.OpenOrder))
		fmt.Fprintf(w, "open hail\t%s\n", dash(t.OpenHail))
		fmt.Fprintf(w, "why\t%s\n", dash(oneLine(t.Why)))
		if t.LandedAt != nil {
			fmt.Fprintf(w, "landed\t%s\n", t.LandedAt.UTC().Format(time.RFC3339))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(res.Orders) == 0 {
		return nil
	}
	fmt.Fprintln(out)
	w = newTable(out)
	fmt.Fprintln(w, "ORDER\tKIND\tROUND\tSTATE\tASSIGNEE")
	for _, o := range res.Orders {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", o.ID, o.Kind, o.Round, o.State, dash(o.Assignee))
	}
	return w.Flush()
}

// shortSHA is a commit id's first 12 characters, "-" for none.
func shortSHA(c string) string {
	if c == "" {
		return "-"
	}
	return c[:min(len(c), 12)]
}
