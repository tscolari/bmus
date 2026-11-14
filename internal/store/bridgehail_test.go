package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A database made with the schema from before #151
// (testdata/schema-before-151.sql, schema.sql as it was) is refused, naming
// the file to delete for a fresh database. Its hails can't lose their NOT
// NULL in place.
func TestOpenRejectsHailsWithoutDeliverTo(t *testing.T) {
	t.Parallel()
	old, err := os.ReadFile(filepath.Join("testdata", "schema-before-151.sql"))
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), DBFile)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(string(old))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(t.Context(), path)
	require.ErrorContains(t, err, "bridge.db predates the bridge-raised hails (#151); delete "+path+
		" and start the bridge again, which makes a fresh database")

	// Refused before the schema ran: it gained no findings table.
	db, err = sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('findings', 'hails_deliver_to')`).Scan(&n))
	require.Zero(t, n)
}

// A new database has the #151 schema: hails.order_id and crew_id
// nullable, hails.deliver_to, and the findings table keyed by order and
// key.
func TestStage2Schema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	hails := tableColumns(t, s.db, "hails")
	require.Equal(t, "TEXT notnull=0 default=NULL", hails["order_id"])
	require.Equal(t, "TEXT notnull=0 default=NULL", hails["crew_id"])
	require.Equal(t, "TEXT notnull=0 default=NULL", hails["deliver_to"])
	require.Equal(t, map[string]string{
		"order_id":     "TEXT notnull=1 default=NULL",
		"key":          "TEXT notnull=1 default=NULL",
		"crew_id":      "TEXT notnull=1 default=NULL",
		"title":        "TEXT notnull=1 default=NULL",
		"body":         "TEXT notnull=1 default=''",
		"labels":       "TEXT notnull=1 default='[]'",
		"severity":     "TEXT notnull=1 default=''",
		"blocked_by":   "TEXT notnull=1 default='[]'",
		"received_at":  "TIMESTAMP notnull=1 default=NULL",
		"state":        "TEXT notnull=1 default='new'",
		"why":          "TEXT notnull=1 default=''",
		"duplicate_of": "TEXT notnull=1 default=''",
		"ref":          "TEXT notnull=1 default=''",
	}, tableColumns(t, s.db, "findings"))

	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	insert := `INSERT INTO findings (order_id, key, crew_id, title, received_at) VALUES (?, ?, 'sorell', 'x', ?)`
	_, err = s.db.ExecContext(ctx, insert, orderID, "flaky-test", time.Now().UTC())
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, insert, orderID, "flaky-test", time.Now().UTC())
	require.ErrorContains(t, err, "UNIQUE", "one row per order and key")
	_, err = s.db.ExecContext(ctx, insert, "ORD-9999", "other", time.Now().UTC())
	require.ErrorContains(t, err, "FOREIGN KEY", "the order must exist")
}

// InsertBridgeHail records a hail with no order or crew member. A second
// insert for the same deliver_to and category returns the first id and
// writes nothing, also once the hail is closed. Another category, or
// another raiser, is a new hail. Its log entry names the raiser's order
// and carries hail_id; no crew member or order changes status.
func TestInsertBridgeHail(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Status: CrewAway}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)
	last, err := s.LastLogID(ctx)
	require.NoError(t, err)

	created := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	h := Hail{DeliverTo: "findings/" + orderID, Category: "tracker_review", Question: "review?",
		Options: []string{"keep", "discard"}, Context: json.RawMessage(`{"issues":[]}`)}
	entry := LogEntry{TS: created, Kind: "hail", OrderID: orderID, Text: "review raised"}
	id, err := s.InsertBridgeHail(ctx, h, entry)
	require.NoError(t, err)
	again, err := s.InsertBridgeHail(ctx, h, entry)
	require.NoError(t, err)
	require.Equal(t, id, again)

	got, err := s.GetHail(ctx, id)
	require.NoError(t, err)
	require.Empty(t, got.OrderID)
	require.Empty(t, got.CrewID)
	require.Equal(t, "findings/"+orderID, got.DeliverTo)
	require.Equal(t, "tracker_review", got.Category)
	require.Equal(t, []string{"keep", "discard"}, got.Options)
	require.JSONEq(t, `{"issues":[]}`, string(got.Context))
	require.Equal(t, "captain", got.Route)
	require.Equal(t, HailOpen, got.Status)
	require.NotNil(t, got.CreatedAt)
	require.True(t, created.Equal(*got.CreatedAt))

	entries, err := s.ListLog(ctx, last)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the second insert logs nothing")
	require.Equal(t, orderID, entries[0].OrderID)
	require.Empty(t, entries[0].CrewID)
	require.JSONEq(t, `{"hail_id":"`+id+`"}`, string(entries[0].Payload))

	c, err := s.GetCrew(ctx, "sorell")
	require.NoError(t, err)
	require.Equal(t, CrewAway, c.Status, "no crew member turns hailing")
	o, err := s.GetOrder(ctx, orderID)
	require.NoError(t, err)
	require.Equal(t, OrderAway, o.State, "no order turns hailing")
	_, found, err := s.OpenHailForOrder(ctx, orderID)
	require.NoError(t, err)
	require.False(t, found, "the hail isn't the order's")

	// Closed, it still answers the re-run.
	require.NoError(t, s.DecideOpenHail(ctx, id, DeciderCaptain, "keep", "", testEntry, nil))
	again, err = s.InsertBridgeHail(ctx, h, entry)
	require.NoError(t, err)
	require.Equal(t, id, again)

	triage := h
	triage.Category = "findings_triage"
	other, err := s.InsertBridgeHail(ctx, triage, LogEntry{Kind: "hail", Text: "triage raised"})
	require.NoError(t, err)
	require.NotEqual(t, id, other)
	elsewhere := h
	elsewhere.DeliverTo = "findings/ORD-0099"
	third, err := s.InsertBridgeHail(ctx, elsewhere, LogEntry{Kind: "hail", Text: "raised"})
	require.NoError(t, err)
	require.NotContains(t, []string{id, other}, third)

	// The index holds whatever path writes the row.
	_, err = s.db.ExecContext(ctx, `INSERT INTO hails (id, deliver_to, category, question) VALUES ('H-9999', ?, ?, '?')`,
		h.DeliverTo, h.Category)
	require.ErrorContains(t, err, "UNIQUE")
}

// InsertBridgeHail refuses a hail without deliver_to or category, or with
// an order or crew member, and writes nothing.
func TestInsertBridgeHailRefusals(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	for name, h := range map[string]Hail{
		"no deliver_to": {Category: "tracker_review", Question: "?"},
		"no category":   {DeliverTo: "findings/ORD-0001", Question: "?"},
		"an order":      {DeliverTo: "findings/ORD-0001", Category: "tracker_review", OrderID: "ORD-0001"},
		"a crew member": {DeliverTo: "findings/ORD-0001", Category: "tracker_review", CrewID: "sorell"},
	} {
		_, err := s.InsertBridgeHail(ctx, h, testEntry)
		require.ErrorIs(t, err, ErrBridgeHail, name)
	}
	hails, err := s.ListHails(ctx, "")
	require.NoError(t, err)
	require.Empty(t, hails)
}

// A beam's hail beside a bridge hail is unaffected: InsertHail still finds
// the order's open hail, sets crew member and order hailing, and closing
// either hail touches only its own. Lists, the status count, metrics and
// the countermandable list read both.
func TestBridgeHailBesideBeamHail(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Status: CrewAway}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)
	since := time.Now().Add(-time.Minute)

	bridge, err := s.InsertBridgeHail(ctx, Hail{DeliverTo: "findings/" + orderID, Category: "findings_triage", Question: "triage?"},
		LogEntry{Kind: "hail", OrderID: orderID, Text: "triage raised"})
	require.NoError(t, err)
	beam, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "beam?", Options: []string{"allow", "deny"}}, testEntry)
	require.NoError(t, err)
	again, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "beam?"}, testEntry)
	require.NoError(t, err)
	require.Equal(t, beam, again)

	h, found, err := s.OpenHailForOrder(ctx, orderID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, beam, h.ID)
	require.Empty(t, h.DeliverTo)
	requireStates := func(crew, order string) {
		t.Helper()
		c, err := s.GetCrew(ctx, "sorell")
		require.NoError(t, err)
		require.Equal(t, crew, c.Status)
		o, err := s.GetOrder(ctx, orderID)
		require.NoError(t, err)
		require.Equal(t, order, o.State)
	}
	requireStates(CrewHailing, OrderHailing)

	open, err := s.ListHails(ctx, HailOpen)
	require.NoError(t, err)
	require.Len(t, open, 2)
	require.Equal(t, bridge, open[0].ID)
	require.Empty(t, open[0].OrderID)
	require.Empty(t, open[0].CrewID)
	require.Equal(t, orderID, open[1].OrderID)
	snap, err := s.ReadStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, snap.HailsOpen)

	// Closing the bridge hail leaves the beam's crew member and order
	// hailing.
	until := time.Now().Add(time.Hour)
	require.NoError(t, s.DecideOpenHailAs(ctx, bridge, HailDecision{DecidedBy: DeciderCommand, Decision: "{}", CountermandUntil: &until},
		testEntry, nil))
	requireStates(CrewHailing, OrderHailing)
	_, err = s.UpdateOpenHail(ctx, beam, "command", testEntry)
	require.NoError(t, err)
	closed, err := s.TimeOutOpenHail(ctx, beam, testEntry)
	require.NoError(t, err)
	require.True(t, closed)
	requireStates(CrewAway, OrderAway)

	all, err := s.ListHails(ctx, "")
	require.NoError(t, err)
	require.Len(t, all, 2)
	cm, err := s.ListCountermandable(ctx, time.Now())
	require.NoError(t, err)
	require.Len(t, cm, 1)
	require.Equal(t, bridge, cm[0].ID)
	require.NoError(t, s.CountermandHail(ctx, bridge, "keep", "no", testEntry, nil))
	m, err := s.HailMetrics(ctx, since)
	require.NoError(t, err)
	require.Equal(t, 2, m.Total)
	require.Equal(t, 1, m.Decided)
	require.Equal(t, 1, m.Countermanded)
	require.Equal(t, 1, m.ByOriginalDecider[DeciderCommand])
}
