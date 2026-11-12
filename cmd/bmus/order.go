package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
)

// orderFile is the YAML `bmus order create` reads. Unknown fields are an
// error.
type orderFile struct {
	Title        string          `yaml:"title"`
	Division     string          `yaml:"division"`
	Assignee     string          `yaml:"assignee"`
	Priority     string          `yaml:"priority"`
	Repos        []orderFileRepo `yaml:"repos"`
	BudgetTokens int64           `yaml:"budget_tokens"`
	Body         string          `yaml:"body"`
	Model        string          `yaml:"model"`
	Source       string          `yaml:"source"`
}

// orderFileRepo is an entry of an order file's repos: a repo registered on
// the bridge (`bridge run --repo`) and the git ref the work starts from.
type orderFileRepo struct {
	Name    string `yaml:"name"`
	BaseRef string `yaml:"base_ref"`
}

// orderPriorities maps an order file's priority to the store's. Empty is
// routine.
var orderPriorities = map[string]int{
	"":        store.PriorityRoutine,
	"routine": store.PriorityRoutine,
	"yellow":  store.PriorityYellow,
	"red":     store.PriorityRed,
}

// parseOrder strictly decodes an order YAML file (one document).
func parseOrder(data []byte) (bridge.OrderCreateArgs, error) {
	var o orderFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&o); err != nil {
		if errors.Is(err, io.EOF) {
			return bridge.OrderCreateArgs{}, errors.New("empty order file")
		}
		return bridge.OrderCreateArgs{}, err
	}
	// Only empty documents may follow, e.g. after a trailing "---".
	for {
		var extra any
		err := dec.Decode(&extra)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || extra != nil {
			return bridge.OrderCreateArgs{}, errors.New("an order file holds one YAML document")
		}
	}
	if _, ok := tracker.SourceRef(o.Source); !ok && strings.TrimSpace(o.Title) == "" {
		return bridge.OrderCreateArgs{}, errors.New("title is required (or a tracker source)")
	}
	if o.Assignee == "" && o.Division == "" {
		return bridge.OrderCreateArgs{}, errors.New("assignee or division is required")
	}
	priority, ok := orderPriorities[o.Priority]
	if !ok {
		return bridge.OrderCreateArgs{}, fmt.Errorf("unknown priority %q (want routine, yellow or red)", o.Priority)
	}
	if o.BudgetTokens < 0 {
		return bridge.OrderCreateArgs{}, errors.New("budget_tokens must not be negative")
	}
	// The bridge checks the repos against its registry, and that Phase 1
	// orders name exactly one.
	repos := make([]bridge.OrderRepo, len(o.Repos))
	for i, r := range o.Repos {
		repos[i] = bridge.OrderRepo{Name: r.Name, BaseRef: r.BaseRef}
	}
	return bridge.OrderCreateArgs{Title: o.Title, Body: o.Body, Division: o.Division, Assignee: o.Assignee,
		Priority: priority, Repos: repos, BudgetTokens: o.BudgetTokens, Model: o.Model,
		Source: o.Source}, nil
}

func newOrderCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "order", Short: "Manage orders"}

	var createJSON bool
	create := &cobra.Command{
		Use:   "create <order.yaml>",
		Short: "Create an order from a YAML file and print its id",
		Long: `Create an order from a YAML file and print its id.

The file's fields: title (required), assignee (a crew id), division, repos
(required), priority, budget_tokens, body, model and source. Unknown fields
are an error. model is the model the order runs on, overriding the
assignee's (` + "`crew add --model`" + `); the harness checks it when the order
starts.

source says where the order comes from, free-form. On a bridge with a
tracker (<home>/tracker.yaml), ` + "`source: tracker:<ref>`" + ` (` + "`tracker:#12`" + ` or
` + "`tracker:owner/name#12`" + ` on GitHub) reads the item when the order is created:
its text goes into the body, after the file's own body, and its title is
the order's when the file gives none. Without a tracker such an order is
refused.

An order with an assignee is directed to that crew member. One with only a
division goes to the division's pool, which its crew members claim from
while standing orders are on (` + "`bmus standing-orders on`" + `). priority is
routine (the default), yellow or red: claims take the highest first, then
the oldest, and an open red order puts the bridge on red alert.

repos lists the repos the order works on, each {name, base_ref}: a repo
registered with ` + "`bmus bridge run --repo <name>=<url>`" + ` and the git ref the work
starts from (a branch of the repo's remote, a tag or a commit id). Phase 1
orders take exactly one:

  repos:
    - name: app
      base_ref: main`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			data, err := os.ReadFile(pos[0])
			if err != nil {
				return err
			}
			args, err := parseOrder(data)
			if err != nil {
				return fmt.Errorf("%s: %w", pos[0], err)
			}
			var res bridge.OrderCreateResult
			if err := a.call(cmd.Context(), "order.create", args, &res); err != nil {
				return err
			}
			if createJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.ID)
			return nil
		},
	}
	create.Flags().BoolVar(&createJSON, "json", false, "print JSON")

	var listJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List orders",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res bridge.OrderListResult
			if err := a.call(cmd.Context(), "order.list", nil, &res); err != nil {
				return err
			}
			if listJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			w := newTable(cmd.OutOrStdout())
			fmt.Fprintln(w, "ID\tSTATE\tPROGRESS\tASSIGNEE\tDIVISION\tTITLE")
			for _, o := range res.Orders {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", o.ID, stateCell(o), progressCell(o.ProgressPct), dash(o.Assignee), dash(o.Division), o.Title)
			}
			return w.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "print JSON")

	belay := orderActionCmd(a, "belay", "order.belay",
		"Abandon an order: its mission ends it belayed and stops its beam's run",
		`Abandon an order that is away, hailing or recalled (its mission parked). Its
mission settles it belayed and, if a beam is running it, sends the beam the
belay; nothing is checkpointed. An order no crew member has claimed is
withdrawn instead (`+"`bmus order withdraw`"+`). A split order, whose mission has
ended, is belayed at once.

The belay cascades to the order's child orders: those not claimed yet are
withdrawn, those in flight are belayed, and those already ended are left
alone. The output says how many open child orders it reaches.`,
		belayLine)
	recall := orderActionCmd(a, "recall", "order.recall",
		"Return an order to its division's pool from its last checkpoint",
		`Return an order that is away or hailing to its division's pool. Its beam
writes a checkpoint and stops; any crew member of the division may then claim
the order and resume it from that checkpoint, the recalled one included. An
order without a division has no pool to return to. An order parked by a watch
change or beam loss can be recalled only while its crew member is in sickbay.`,
		func(v bridge.OrderView) string { return "order " + v.ID + " recall requested" })
	withdraw := orderActionCmd(a, "withdraw", "order.withdraw",
		"Take back an order no crew member has claimed yet",
		`Take back an order no crew member has claimed yet (pool or directed). A
recalled order still has its parked mission: belay it instead.`,
		func(v bridge.OrderView) string { return "order " + v.ID + " withdrawn" })

	var (
		routeJSON          bool
		routeTo, routeDivs string
	)
	route := &cobra.Command{
		Use:   "route <id>",
		Short: "Give an unclaimed order another assignee or division",
		Long: `Give an order no crew member has claimed yet (pool or directed, a recalled order
included) another assignee or division. --assignee directs it to that crew
member; --assignee "" returns it to its division's pool. --division sets its
division ("" clears it). At least one is required, and the order must keep an
assignee or a division.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			args := bridge.OrderRouteArgs{ID: pos[0]}
			if cmd.Flags().Changed("assignee") {
				args.Assignee = &routeTo
			}
			if cmd.Flags().Changed("division") {
				args.Division = &routeDivs
			}
			if args.Assignee == nil && args.Division == nil {
				return errors.New("--assignee or --division is required")
			}
			var res bridge.OrderView
			if err := a.call(cmd.Context(), "order.route", args, &res); err != nil {
				return err
			}
			if routeJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			if res.Assignee != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "order %s routed to %s\n", res.ID, res.Assignee)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "order %s routed to the %s pool\n", res.ID, res.Division)
			}
			return nil
		},
	}
	route.Flags().StringVar(&routeTo, "assignee", "", "crew id to direct the order to; empty for the division's pool")
	route.Flags().StringVar(&routeDivs, "division", "", "the order's division; empty to clear it")
	route.Flags().BoolVar(&routeJSON, "json", false, "print the order as JSON")

	var (
		pushJSON, pushForce bool
		pushTarget          string
	)
	push := &cobra.Command{
		Use:   "push <id>",
		Short: "Push a done order's work to its repo's remote",
		Long: `Push a done order's work from the bridge's clone to each repo's registered
URL, with your own git configuration and credentials: bmus/<order> goes to
--target-branch (default bmus/<order>). The push is refused unless the order is
done and its work was fetched into the bridge's clone at the reported commit.
--force overwrites a target branch that has diverged; without it git refuses a
non-fast-forward push. Repos are pushed in order, and the first failure stops
the push. Each push is logged.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			args := bridge.OrderPushArgs{ID: pos[0], TargetBranch: pushTarget, Force: pushForce}
			var res bridge.OrderPushResult
			if err := a.call(cmd.Context(), "order.push", args, &res); err != nil {
				var re *bridge.RPCError
				if errors.As(err, &re) && re.Code == bridge.CodeConflict && !pushForce {
					return fmt.Errorf("%w\n(the target branch has diverged; --force overwrites it)", err)
				}
				return err
			}
			if pushJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			for _, r := range res.Repos {
				fmt.Fprintf(cmd.OutOrStdout(), "order %s: repo %s pushed to %s at %s\n", res.ID, r.Name, res.TargetBranch, r.Commit)
			}
			return nil
		},
	}
	push.Flags().StringVar(&pushTarget, "target-branch", "", "branch to push to (default bmus/<order>)")
	push.Flags().BoolVar(&pushForce, "force", false, "overwrite the target branch even if it has diverged")
	push.Flags().BoolVar(&pushJSON, "json", false, "print JSON")

	cmd.AddCommand(create, list, belay, recall, withdraw, route, push)
	return cmd
}

// orderActionCmd is `bmus order <use> <id> [--reason]`, calling rpc with
// the id and reason and printing say (with the id) or, with --json, the
// order.
func orderActionCmd[T any](a *app, use, rpc, short, long string, say func(T) string) *cobra.Command {
	var (
		asJSON bool
		reason string
	)
	cmd := &cobra.Command{
		Use:   use + " <id>",
		Short: short,
		Long:  long,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			var res T
			if err := a.call(cmd.Context(), rpc, bridge.OrderActionArgs{ID: pos[0], Reason: reason}, &res); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintln(cmd.OutOrStdout(), say(res))
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, for the captain's log")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the order as JSON")
	return cmd
}

// belayLine is `bmus order belay`'s output: "order ORD-0001 belay
// requested", or "order ORD-0001 belayed" for a split order the call
// belayed itself (res.Belayed, not the state: a mission may settle its
// order before the reply reads it), with ", and its 2 open child orders"
// when the belay reaches any (#146). The CLI has no confirm prompt: the
// count is in the output.
func belayLine(res bridge.OrderBelayResult) string {
	line := "order " + res.ID + " belay requested"
	if res.Belayed {
		line = "order " + res.ID + " belayed"
	}
	switch {
	case res.Children == 1:
		line += ", and its 1 open child order"
	case res.Children > 1:
		line += fmt.Sprintf(", and its %d open child orders", res.Children)
	}
	return line
}

// progressCell shows an order's progress as a table cell: "40%", or "-"
// without a report (#141). The pct is clamped to 0-100, as the console
// does, and floored: 99.6 isn't done.
func progressCell(pct *float64) string {
	if pct == nil {
		return "-"
	}
	return fmt.Sprintf("%d%%", int(math.Floor(min(100, max(0, *pct)))))
}

// stateCell is an order's STATE cell: its state, with a split order's
// open children, "split (2 open)" (#155).
func stateCell(o bridge.OrderView) string {
	if o.State == store.OrderSplit {
		return fmt.Sprintf("%s (%d open)", o.State, o.OpenChildren)
	}
	return o.State
}
