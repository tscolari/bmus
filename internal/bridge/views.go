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
	// split order waits on them (#155). ParkedAt is set (UTC) while its
	// mission is parked, waiting for a resume (#167).
	OrderView struct {
		ID           string      `json:"id"`
		Title        string      `json:"title"`
		Body         string      `json:"body"`
		Division     string      `json:"division"`
		Priority     int         `json:"priority"`
		State        string      `json:"state"`
		Assignee     string      `json:"assignee"`
		ParentID     string      `json:"parent_id"`
		Repos        []OrderRepo `json:"repos"`
		Branch       string      `json:"branch"`
		BudgetTokens int64       `json:"budget_tokens"`
		CreatedBy    string      `json:"created_by"`
		Model        string      `json:"model"`
		Source       string      `json:"source,omitempty"`
		ProgressPct  *float64    `json:"progress_pct,omitempty"`
		ProgressNote string      `json:"progress_note,omitempty"`
		OpenChildren int         `json:"open_children"`
		ParkedAt     *time.Time  `json:"parked_at,omitempty"`
		// BaseCommits are a done order's repos' base commits, by name:
		// where its work starts, for the console's diff command (#4). Only
		// the console's views carry them (consoleSource.orderView).
		BaseCommits map[string]string `json:"base_commits,omitempty"`
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

func orderView(o store.Order) OrderView {
	repos := make([]OrderRepo, len(o.Repos))
	for i, r := range o.Repos {
		repos[i] = OrderRepo{Name: r.Name, BaseRef: r.BaseRef}
	}
	return OrderView{ID: o.ID, Title: o.Title, Body: o.Body, Division: o.Division,
		Priority: o.Priority, State: o.State, Assignee: o.Assignee, ParentID: o.ParentID, Repos: repos,
		Branch: o.Branch, BudgetTokens: o.BudgetTokens, CreatedBy: o.CreatedBy, Model: o.Model,
		Source: o.Source, ProgressPct: o.ProgressPct, ProgressNote: o.ProgressNote, OpenChildren: o.OpenChildren,
		ParkedAt: utc(o.ParkedAt)}
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
