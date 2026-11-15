package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/tscolari/bmus/internal/bridge"
	"github.com/tscolari/bmus/internal/regs"
)

func newHailsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "hails", Short: "List, answer and countermand hails from the crew"}

	var (
		status   string
		listJSON bool
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List hails (open ones by default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			args := bridge.HailsListArgs{Status: status}
			if status == "all" {
				args.Status = ""
			}
			var res bridge.HailsListResult
			if err := a.call(cmd.Context(), "hails.list", args, &res); err != nil {
				return err
			}
			if listJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			return printHails(cmd.OutOrStdout(), res.Hails)
		},
	}
	list.Flags().StringVar(&status, "status", "open", "open, decided, timed_out or all")
	list.Flags().BoolVar(&listJSON, "json", false, "print JSON")

	var (
		option     int
		decision   string
		answerJSON bool
	)
	answer := &cobra.Command{
		Use:   "answer <hail-id> (--option <n> | --decision <text>)",
		Short: "Answer an open hail with one of its options or a free-form decision",
		Long: `Answer an open hail with one of its options (numbered from 0, as hails list
shows them) or a free-form decision. With both, the decision text is sent
along with the option.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			args := bridge.HailsAnswerArgs{HailID: pos[0], Decision: decision}
			if cmd.Flags().Changed("option") {
				args.Option = &option
			}
			if args.Option == nil && decision == "" {
				return errors.New("pass --option <n> or --decision <text>")
			}
			var h bridge.HailView
			if err := a.call(cmd.Context(), "hails.answer", args, &h); err != nil {
				return err
			}
			if answerJSON {
				return printJSON(cmd.OutOrStdout(), h)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %s\n", h.ID, h.Status, h.Decision)
			return nil
		},
	}
	answer.Flags().IntVar(&option, "option", 0, "the chosen option's number")
	answer.Flags().StringVar(&decision, "decision", "", "the decision, in words")
	answer.Flags().BoolVar(&answerJSON, "json", false, "print the decided hail as JSON")

	var (
		cmOption             int
		cmDecision, cmReason string
		cmJSON               bool
	)
	countermand := &cobra.Command{
		Use:   "countermand <hail-id> (--option <n> | --decision <text>) [--reason <text>]",
		Short: "Override a decision Command or a captain timeout made, within its countermand window",
		Long: `Override a decision made without the captain (Command's answer, or the
default option a captain timeout took) while its countermand window is
open: hails list --json shows it as countermand_until. --option and
--decision are as for hails answer. The agent already acted on the first
answer, so its running order is sent a correction carrying the new
decision and the reason.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			args := bridge.HailsCountermandArgs{HailID: pos[0], Decision: cmDecision, Reason: cmReason}
			if cmd.Flags().Changed("option") {
				args.Option = &cmOption
			}
			if args.Option == nil && cmDecision == "" {
				return errors.New("pass --option <n> or --decision <text>")
			}
			var h bridge.HailsCountermandResult
			if err := a.call(cmd.Context(), "hails.countermand", args, &h); err != nil {
				return err
			}
			if cmJSON {
				return printJSON(cmd.OutOrStdout(), h)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s countermanded: %s\n", h.ID, h.Decision)
			if h.Note != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "note: %s\n", h.Note)
			}
			return nil
		},
	}
	countermand.Flags().IntVar(&cmOption, "option", 0, "the chosen option's number")
	countermand.Flags().StringVar(&cmDecision, "decision", "", "the decision, in words")
	countermand.Flags().StringVar(&cmReason, "reason", "", "why, for the log and the agent")
	countermand.Flags().BoolVar(&cmJSON, "json", false, "print the hail as JSON")

	var (
		since       string
		metricsJSON bool
	)
	metrics := &cobra.Command{
		Use:   "metrics [--since <duration>]",
		Short: "Report the auto-resolved share, captain overrides and time-to-answer",
		Long: `Report, over the hails created in the period: how many were decided, the
share decided first by policy or Command (auto-resolved), the share the
captain countermanded (overrides), and the median and p90 time from hail
to decision (nearest-rank). Hails closed unanswered (parked, recalled,
belayed, withdrawn) count in the total only.

--since is a duration back from now, in Go's units plus d for days ("7d",
"1d12h", "36h"), or an RFC 3339 time.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, err := parseSince(since, time.Now())
			if err != nil {
				return err
			}
			var res bridge.HailsMetricsResult
			if err := a.call(cmd.Context(), "hails.metrics", bridge.HailsMetricsArgs{Since: &from}, &res); err != nil {
				return err
			}
			if metricsJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			return printHailMetrics(cmd.OutOrStdout(), res)
		},
	}
	metrics.Flags().StringVar(&since, "since", "7d", `how far back: a duration ("7d", "36h") or an RFC 3339 time`)
	metrics.Flags().BoolVar(&metricsJSON, "json", false, "print JSON")

	var (
		keep             bool
		discard, restore []string
		reviewJSON       bool
	)
	review := &cobra.Command{
		Use:   "review <hail-id> [--keep | --discard <ref>...] [--restore <key>...]",
		Short: "Show a findings review, or answer it",
		Long: `Show a tracker_review hail's batch: the issues filed from an order's
findings, and the findings Command's triage dropped or filing failed.

With flags, answer it: --keep keeps every issue, --discard <ref> closes
that issue as not planned (repeatable; the others are kept), and
--restore <key> files a dropped or failed finding (repeatable). The
answer goes through hails answer, which checks it against the batch.
hails answer --option 0 keeps every issue and --option 1 discards every
one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, pos []string) error {
			if keep && len(discard) > 0 {
				return errors.New("pass --keep or --discard, not both")
			}
			var res bridge.HailsListResult
			if err := a.call(cmd.Context(), "hails.list", bridge.HailsListArgs{}, &res); err != nil {
				return err
			}
			i := slices.IndexFunc(res.Hails, func(h bridge.HailView) bool { return h.ID == pos[0] })
			if i < 0 {
				return fmt.Errorf("no hail %s", pos[0])
			}
			h := res.Hails[i]
			if h.Category != regs.CategoryTrackerReview {
				return fmt.Errorf("hail %s is %s, not a findings review (tracker_review)", h.ID, dash(h.Category))
			}
			if !keep && len(discard) == 0 && len(restore) == 0 {
				if reviewJSON {
					return printJSON(cmd.OutOrStdout(), h)
				}
				return printReview(cmd.OutOrStdout(), h)
			}
			args := bridge.HailsAnswerArgs{HailID: h.ID, Decision: reviewDecision(discard, restore)}
			var got bridge.HailView
			if err := a.call(cmd.Context(), "hails.answer", args, &got); err != nil {
				return err
			}
			if reviewJSON {
				return printJSON(cmd.OutOrStdout(), got)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %s\n", got.ID, got.Status, got.Decision)
			return nil
		},
	}
	review.Flags().BoolVar(&keep, "keep", false, "keep every issue")
	review.Flags().StringArrayVar(&discard, "discard", nil, "an issue to discard (repeatable)")
	review.Flags().StringArrayVar(&restore, "restore", nil, "a dropped or failed finding's key to restore (repeatable)")
	review.Flags().BoolVar(&reviewJSON, "json", false, "print the hail as JSON")

	cmd.AddCommand(list, answer, countermand, metrics, review)
	return cmd
}

// reviewDecision is the tracker_review answer hails review sends: keep,
// or discard <ref>..., then "; restore <key>..." when there are any.
func reviewDecision(discard, restore []string) string {
	s := "keep"
	if len(discard) > 0 {
		s = "discard " + strings.Join(discard, " ")
	}
	if len(restore) > 0 {
		s += "; restore " + strings.Join(restore, " ")
	}
	return s
}

// printReview prints tracker_review hail h: its question, then one row per
// issue, dropped finding and failed finding (N, REF/KEY, STATE, TITLE,
// WHY), then its decision once it has one. Titles and reasons are the
// crew's and Command's text: oneLine strips their control characters.
func printReview(out io.Writer, h bridge.HailView) error {
	var rc bridge.ReviewContext
	if len(h.Context) > 0 {
		if err := json.Unmarshal(h.Context, &rc); err != nil {
			return fmt.Errorf("hail %s: review context: %w", h.ID, err)
		}
	}
	fmt.Fprintf(out, "%s %s: %s\n", h.ID, h.Status, oneLine(h.Question))
	w := newTable(out)
	fmt.Fprintln(w, "N\tREF/KEY\tSTATE\tTITLE\tWHY")
	n := 0
	row := func(id, state, title, why string) {
		n++
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", n, oneLine(id), state, dash(oneLine(title)), dash(oneLine(why)))
	}
	for _, is := range rc.Issues {
		row(is.Ref, "filed", is.Title, "")
	}
	for _, f := range rc.Dropped {
		why := f.Why
		if f.DuplicateOf != "" && why != "" {
			why = "duplicate of " + f.DuplicateOf + "; " + why
		} else if f.DuplicateOf != "" {
			why = "duplicate of " + f.DuplicateOf
		}
		row(f.Key, "dropped", f.Title, why)
	}
	for _, f := range rc.Failed {
		row(f.Key, "failed", f.Title, f.Error)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if h.Decision != "" {
		fmt.Fprintf(out, "decision: %s\n", oneLine(h.Decision))
	}
	return nil
}

// sinceDays matches a --since with a leading day count: "7d", "1d12h".
var sinceDays = regexp.MustCompile(`^(\d+)d(.*)$`)

// parseSince reads hails metrics' --since: an RFC 3339 time, or a positive
// duration back from now, as time.ParseDuration takes it plus a leading
// whole number of days ("7d", "1d12h": a day is 24h).
func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	var d time.Duration
	rest := s
	if m := sinceDays.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil || days > 100*365 {
			return time.Time{}, fmt.Errorf("--since %q: too many days", s)
		}
		d, rest = time.Duration(days)*24*time.Hour, m[2]
	}
	if rest != "" {
		more, err := time.ParseDuration(rest)
		if err != nil || more < 0 {
			return time.Time{}, fmt.Errorf(`--since %q: want a duration such as "7d" or "36h", or an RFC 3339 time`, s)
		}
		d += more
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("--since %q: want a positive duration", s)
	}
	return now.Add(-d), nil
}

// printHailMetrics writes res as a two-column table.
func printHailMetrics(w io.Writer, res bridge.HailsMetricsResult) error {
	t := newTable(w)
	pct := func(rate *float64, n int) string {
		if rate == nil {
			return "-"
		}
		return fmt.Sprintf("%.1f%% (%d of %d)", *rate*100, n, res.Decided)
	}
	ms := func(v *int64) string {
		if v == nil {
			return "-"
		}
		return time.Duration(*v * int64(time.Millisecond)).String()
	}
	by := res.ByOriginalDecider
	fmt.Fprintf(t, "since\t%s\n", res.Since.Format(time.RFC3339))
	fmt.Fprintf(t, "hails\t%d\n", res.Total)
	fmt.Fprintf(t, "decided\t%d\n", res.Decided)
	deciders := make([]string, 0, len(by))
	for k := range by {
		deciders = append(deciders, k)
	}
	slices.SortFunc(deciders, func(a, b string) int {
		return cmp.Or(cmp.Compare(deciderRank(a), deciderRank(b)), strings.Compare(a, b))
	})
	for _, k := range deciders {
		fmt.Fprintf(t, "  first by %s\t%d\n", dash(k), by[k])
	}
	fmt.Fprintf(t, "auto-resolved\t%s\n", pct(res.AutoResolvedShare, by["policy"]+by["command"]))
	fmt.Fprintf(t, "captain overrides\t%s\n", pct(res.CaptainOverrides, res.Countermanded))
	fmt.Fprintf(t, "time-to-answer median\t%s\n", ms(res.MedianTimeToAnswerMS))
	fmt.Fprintf(t, "time-to-answer p90\t%s\n", ms(res.P90TimeToAnswerMS))
	return t.Flush()
}

// deciderRank lists policy, command and captain first, in that order.
func deciderRank(d string) int {
	switch d {
	case "policy":
		return 0
	case "command":
		return 1
	case "captain":
		return 2
	}
	return 3
}

// oneLine keeps a table row on one line, and keeps text from the crew
// (a finding's title in the log, say) from moving the terminal: control
// characters (C0, DEL and C1, escapes included) become spaces before the
// whitespace is collapsed.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)), " ")
}

// printHails prints hails as hails list's table. A bridge-raised hail
// (#152) has no order or crew member: both print as -.
func printHails(out io.Writer, hails []bridge.HailView) error {
	w := newTable(out)
	fmt.Fprintln(w, "ID\tORDER\tCREW\tCATEGORY\tSTATUS\tQUESTION\tOPTIONS\tDECISION")
	for _, h := range hails {
		opts := make([]string, len(h.Options))
		for i, o := range h.Options {
			opts[i] = fmt.Sprintf("%d=%s", i, o)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", h.ID, dash(h.OrderID), dash(h.CrewID), dash(h.Category),
			h.Status, oneLine(h.Question), dash(strings.Join(opts, " ")), dash(oneLine(h.Decision)))
	}
	return w.Flush()
}
