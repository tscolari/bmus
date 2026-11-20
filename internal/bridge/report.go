package bridge

// The console's mission report (#104): a done order, the crew member and
// beams that ran it, its report, its pushes and its diff, read from the
// store, the ship's log and the bridge's clone.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// OrderReport is a done order's mission report. Crew is nil when the order
// has no assignee or the crew member's row is gone; Report is nil without
// a done entry. Beams are the aliases the order's entries name, oldest
// first, and may be empty for an order from before they were recorded.
// Cap and Repos are OrderDiff's.
type OrderReport struct {
	Order  ReportOrder    `json:"order"`
	Crew   *CrewView      `json:"crew"`
	Beams  []string       `json:"beams"`
	Report *ReportSummary `json:"report"`
	Pushes []ReportPush   `json:"pushes"`
	Cap    int            `json:"cap"`
	Repos  []RepoDiff     `json:"repos"`
}

// ReportOrder is the order's view, with when it was created (its "order
// created" entry) and done (its done entry); nil when the entry is
// missing.
type ReportOrder struct {
	OrderView
	CreatedAt *time.Time `json:"created_at"`
	DoneAt    *time.Time `json:"done_at"`
}

// ReportSummary is the crew's report, from the done entry's payload.
// Verdict is a review order's verdict (#187), as the done entry holds it
// (#182); nil for a work order, and for an entry without a verdict Check
// takes.
type ReportSummary struct {
	Summary   string         `json:"summary"`
	Artifacts []string       `json:"artifacts"`
	Verdict   *comms.Verdict `json:"verdict,omitempty"`
}

// ReportPush is one push of the order's work, from its "repo <name>
// pushed to <branch>" entry.
type ReportPush struct {
	Repo   string    `json:"repo"`
	Branch string    `json:"branch"`
	Commit string    `json:"commit"`
	Force  bool      `json:"force"`
	At     time.Time `json:"at"`
}

// OrderReport reads done order id's mission report, its diffs capped at
// limit bytes each. The order must exist and be done (doneOrder).
//
// The beams come from the entries that carry one: ship-repos and the done
// entry (#104), and each checkpoint pulled. A re-energize or a watch
// change moves an order to another beam, and its next ship-repos names
// it.
func (d *Daemon) OrderReport(ctx context.Context, id string, limit int) (OrderReport, error) {
	o, err := d.doneOrder(ctx, id)
	if err != nil {
		return OrderReport{}, err
	}
	out := OrderReport{Order: ReportOrder{OrderView: d.orderView(o)}, Beams: []string{}, Pushes: []ReportPush{}}
	if o.Assignee != "" {
		c, err := d.store.GetCrew(ctx, o.Assignee)
		switch {
		case err == nil:
			v := d.crewView(c)
			out.Crew = &v
		case !errors.Is(err, sql.ErrNoRows):
			return OrderReport{}, err
		}
	}
	entries, err := d.store.ListOrderLogKind(ctx, id, logOrder)
	if err != nil {
		return OrderReport{}, err
	}
	done, err := d.store.LastOrderLog(ctx, id, logDone)
	switch {
	case err == nil:
		entries = append(entries, done)
		var p donePayload
		if json.Unmarshal(done.Payload, &p) == nil {
			out.Report = &ReportSummary{Summary: p.Summary, Artifacts: p.Artifacts}
			if out.Report.Artifacts == nil {
				out.Report.Artifacts = []string{}
			}
			out.Report.Verdict = reportVerdict(o, p.Verdict)
		}
		at := done.TS
		out.Order.DoneAt = &at
	case !errors.Is(err, sql.ErrNoRows):
		return OrderReport{}, err
	}
	for _, e := range entries {
		var p struct {
			Beam   string `json:"beam"`
			Repo   string `json:"repo"`
			Target string `json:"target"`
			Commit string `json:"commit"`
			Force  bool   `json:"force"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.Beam != "" && !slices.Contains(out.Beams, p.Beam) {
			out.Beams = append(out.Beams, p.Beam)
		}
		switch {
		case out.Order.CreatedAt == nil && strings.HasPrefix(e.Text, "order created: "):
			at := e.TS
			out.Order.CreatedAt = &at
		case p.Repo != "" && p.Target != "" && strings.HasPrefix(e.Text, "repo "+p.Repo+" pushed to "):
			out.Pushes = append(out.Pushes, ReportPush{Repo: p.Repo, Branch: p.Target, Commit: p.Commit, Force: p.Force, At: e.TS})
		}
	}
	diff, err := d.orderDiff(ctx, o, limit)
	if err != nil {
		return OrderReport{}, err
	}
	out.Cap, out.Repos = diff.Cap, diff.Repos
	return out, nil
}

// reportVerdict is the verdict a mission report shows for o: v when o is
// a review and Check takes v, else nil. The mission stores only a verdict
// Check took, and only for a review (#182); the check keeps a hand-made
// entry from showing a verdict the bridge never accepted.
func reportVerdict(o store.Order, v *comms.Verdict) *comms.Verdict {
	if o.Kind != store.OrderKindReview || v == nil || v.Check() != nil {
		return nil
	}
	return v
}
