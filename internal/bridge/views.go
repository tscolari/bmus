package bridge

import (
	"encoding/json"
	"time"

	"github.com/tscolari/bmus/internal/store"
)

// Views: the JSON shapes of crew members, orders, hails and log entries,
// shared by the RPC results (rpc.go) and the console's API (console.go).
type (
	// CrewView is a crew member. Harness and Model are resolved
	// (resolveHarness, resolveModel without an order), with where each
	// came from: SourceCrew, SourceDivision or SourceDefault (empty: sdk,
	// and the harness's own model). They follow the roster the bridge runs
	// with now; a tour keeps the harness it was provisioned with.
	CrewView struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Rank          string `json:"rank"`
		Division      string `json:"division"`
		Spec          string `json:"spec"`
		Status        string `json:"status"`
		CurrentBeam   string `json:"current_beam"`
		TourNo        int    `json:"tour_no"`
		DossierPath   string `json:"dossier_path"`
		Harness       string `json:"harness"`
		HarnessSource string `json:"harness_source"`
		Model         string `json:"model"`
		ModelSource   string `json:"model_source"`
	}

	// OrderView is an order. ProgressPct and ProgressNote are its
	// mission's latest progress report (#141), omitted without one;
	// ProgressNote is also omitted when the report has no note.
	// OpenChildren counts its child orders (ParentID) not yet ended: a
	// split order waits on them (#155). After are the orders it waits on
	// (#142), WaitingOn those of them not done yet; both always present.
	// ParkedAt is set (UTC) while its
	// mission is parked, waiting for a resume (#167).
	//
	// BudgetTokens and BudgetTurns are the order's own budget, 0 for none,
	// as order.create and order.budget take it. LimitTokens and LimitTurns
	// are its current limits (#120): its budget resolved for its assignee
	// (resolveBudget), each field times 1 + BudgetExtended, the extensions
	// granted; 0 for unlimited. BudgetSource says where each resolved
	// field comes from. Usage is what its beams reported, summed.
	OrderView struct {
		ID             string       `json:"id"`
		Title          string       `json:"title"`
		Body           string       `json:"body"`
		Division       string       `json:"division"`
		Priority       int          `json:"priority"`
		State          string       `json:"state"`
		Assignee       string       `json:"assignee"`
		ParentID       string       `json:"parent_id"`
		Repos          []OrderRepo  `json:"repos"`
		Branch         string       `json:"branch"`
		BudgetTokens   int64        `json:"budget_tokens"`
		BudgetTurns    int64        `json:"budget_turns"`
		LimitTokens    int64        `json:"limit_tokens"`
		LimitTurns     int64        `json:"limit_turns"`
		BudgetSource   BudgetSource `json:"budget_source"`
		BudgetExtended int64        `json:"budget_extended"`
		Usage          UsageView    `json:"usage"`
		CreatedBy      string       `json:"created_by"`
		Model          string       `json:"model"`
		Source         string       `json:"source,omitempty"`
		ProgressPct    *float64     `json:"progress_pct,omitempty"`
		ProgressNote   string       `json:"progress_note,omitempty"`
		OpenChildren   int          `json:"open_children"`
		After          []string     `json:"after"`
		WaitingOn      []string     `json:"waiting_on"`
		ParkedAt       *time.Time   `json:"parked_at,omitempty"`
	}

	// UsageView is an order's usage, summed over its beams (#120).
	UsageView struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		Turns        int64 `json:"turns"`
	}

	// HailView is a hail. CreatedAt is omitted on a hail from before #147,
	// DecidedAt and OriginalDecidedBy until it is decided. Confidence is
	// Command's, on a hail Command decided; CountermandUntil is set on a
	// decision the captain may countermand until then (Command's, or the
	// captain timeout's default_option), CountermandedAt once they last did
	// (#153).
	HailView struct {
		ID        string          `json:"id"`
		OrderID   string          `json:"order_id"`
		CrewID    string          `json:"crew_id"`
		Category  string          `json:"category"`
		Question  string          `json:"question"`
		Options   []string        `json:"options"`
		Context   json.RawMessage `json:"context"`
		Route     string          `json:"route"`
		Status    string          `json:"status"`
		DecidedBy string          `json:"decided_by"`
		Decision  string          `json:"decision"`
		Rationale string          `json:"rationale"`

		CreatedAt         *time.Time `json:"created_at,omitempty"`
		DecidedAt         *time.Time `json:"decided_at,omitempty"`
		OriginalDecidedBy string     `json:"original_decided_by,omitempty"`
		Confidence        *float64   `json:"confidence,omitempty"`
		CountermandUntil  *time.Time `json:"countermand_until,omitempty"`
		CountermandedAt   *time.Time `json:"countermanded_at,omitempty"`
	}

	// LogEntryView is a captain's log entry.
	LogEntryView struct {
		ID      int64           `json:"id"`
		TS      time.Time       `json:"ts"`
		Kind    string          `json:"kind"`
		CrewID  string          `json:"crew_id"`
		OrderID string          `json:"order_id"`
		Text    string          `json:"text"`
		Payload json.RawMessage `json:"payload"`
	}
)

func (d *Daemon) crewView(c store.Crew) CrewView {
	v := CrewView{ID: c.ID, Name: c.Name, Rank: c.Rank, Division: c.Division, Spec: c.Spec, Status: c.Status,
		CurrentBeam: c.CurrentBeam, TourNo: c.TourNo, DossierPath: c.DossierPath}
	v.Harness, v.HarnessSource = d.resolveHarness(c)
	v.Model, v.ModelSource = d.resolveModel(store.Order{}, c)
	return v
}

// orderView is order o's view. Its budget is resolved for its assignee,
// whose division a directed order without one takes (resolveBudget).
func (d *Daemon) orderView(o store.Order) OrderView {
	repos := make([]OrderRepo, len(o.Repos))
	for i, r := range o.Repos {
		repos[i] = OrderRepo{Name: r.Name, BaseRef: r.BaseRef}
	}
	b, src := d.resolveBudget(o, store.Crew{ID: o.Assignee, Division: o.AssigneeDivision})
	limit, _ := budgetFor(o, b, nil, "")
	return OrderView{ID: o.ID, Title: o.Title, Body: o.Body, Division: o.Division,
		Priority: o.Priority, State: o.State, Assignee: o.Assignee, ParentID: o.ParentID, Repos: repos,
		Branch: o.Branch, BudgetTokens: o.BudgetTokens, BudgetTurns: o.BudgetTurns,
		LimitTokens: limit.Tokens, LimitTurns: limit.Turns, BudgetSource: src,
		BudgetExtended: o.BudgetExtended,
		Usage:          UsageView{InputTokens: o.Used.InputTokens, OutputTokens: o.Used.OutputTokens, Turns: o.Used.Turns},
		CreatedBy:      o.CreatedBy, Model: o.Model,
		Source: o.Source, ProgressPct: o.ProgressPct, ProgressNote: o.ProgressNote, OpenChildren: o.OpenChildren,
		After: orEmpty(o.After), WaitingOn: orEmpty(o.WaitingOn), ParkedAt: utc(o.ParkedAt)}
}

func hailView(h store.Hail) HailView {
	opts := h.Options
	if opts == nil {
		opts = []string{}
	}
	return HailView{ID: h.ID, OrderID: h.OrderID, CrewID: h.CrewID, Category: h.Category, Question: h.Question,
		Options: opts, Context: h.Context, Route: h.Route, Status: h.Status, DecidedBy: h.DecidedBy,
		Decision: h.Decision, Rationale: h.Rationale, CreatedAt: utc(h.CreatedAt), DecidedAt: utc(h.DecidedAt),
		OriginalDecidedBy: h.OriginalDecidedBy, Confidence: h.Confidence, CountermandUntil: utc(h.CountermandUntil),
		CountermandedAt: utc(h.CountermandedAt)}
}

// orEmpty is ids, or an empty slice for nil: a JSON array either way.
func orEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// utc returns t in UTC, or nil for nil.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// logEntryView is e's view. A payload that isn't valid JSON is null:
// one bad row must not fail a log.tail or end a console stream.
func logEntryView(e store.LogEntry) LogEntryView {
	payload := e.Payload
	if !json.Valid(payload) {
		payload = nil
	}
	return LogEntryView{ID: e.ID, TS: e.TS.UTC(), Kind: e.Kind,
		CrewID: e.CrewID, OrderID: e.OrderID, Text: e.Text, Payload: payload}
}
