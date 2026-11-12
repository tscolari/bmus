package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testEntry is the captain's log entry tests pass to a mutating method
// when the entry itself isn't what they check.
var testEntry = LogEntry{Kind: "test", Text: "test"}

// withoutTestEntries returns entries without the ones logged with testEntry.
func withoutTestEntries(entries []LogEntry) []LogEntry {
	var out []LogEntry
	for _, e := range entries {
		if e.Kind != testEntry.Kind {
			out = append(out, e)
		}
	}
	return out
}

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), DBFile))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// An order's repos keep their list order, through GetOrder and ListOrders,
// and a name appears once per order.
func TestOrderRepos(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	repos := []OrderRepo{{Name: "web", BaseRef: "main"}, {Name: "api", BaseRef: "v1.2"}}
	id1, err := s.InsertOrder(ctx, Order{Title: "two", Repos: repos}, testEntry)
	require.NoError(t, err)
	id2, err := s.InsertOrder(ctx, Order{Title: "none"}, testEntry)
	require.NoError(t, err)
	o, err := s.GetOrder(ctx, id1)
	require.NoError(t, err)
	require.Equal(t, repos, o.Repos)
	o, err = s.GetOrder(ctx, id2)
	require.NoError(t, err)
	require.Empty(t, o.Repos)
	all, err := s.ListOrders(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, repos, all[0].Repos)
	require.Empty(t, all[1].Repos)

	_, err = s.InsertOrder(ctx, Order{Title: "dup", Repos: []OrderRepo{{Name: "web", BaseRef: "a"}, {Name: "web", BaseRef: "b"}}}, testEntry)
	require.ErrorContains(t, err, "UNIQUE")
	all, err = s.ListOrders(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2, "the failed insert left no order behind")
}

// A database made before order_repos existed gains it on Open, and its
// orders read back with no repos.
func TestOpenAddsOrderRepos(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), DBFile)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE orders (
		id TEXT PRIMARY KEY, title TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', division TEXT NOT NULL DEFAULT '',
		priority INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL, assignee TEXT NOT NULL DEFAULT '',
		parent_id TEXT NOT NULL DEFAULT '', base_ref TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
		budget_tokens INTEGER NOT NULL DEFAULT 0, created_by TEXT NOT NULL DEFAULT '');
		INSERT INTO orders (id, title, state, base_ref) VALUES ('ORD-0001', 'old', 'done', 'main');`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s, err := Open(ctx, path)
	require.NoError(t, err)
	defer s.Close()
	o, err := s.GetOrder(ctx, "ORD-0001")
	require.NoError(t, err)
	require.Equal(t, "old", o.Title)
	require.Empty(t, o.Repos)
	id, err := s.InsertOrder(ctx, Order{Title: "new", Repos: []OrderRepo{{Name: "app", BaseRef: "main"}}}, testEntry)
	require.NoError(t, err)
	o, err = s.GetOrder(ctx, id)
	require.NoError(t, err)
	require.Equal(t, []OrderRepo{{Name: "app", BaseRef: "main"}}, o.Repos)
}

// A database made before the inbox was keyed by tour (#22) can't be
// upgraded in place: Open refuses it, naming the file to delete.
func TestOpenRejectsInboxWithoutTour(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFile)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE inbox (
		crew_id TEXT NOT NULL, seq INTEGER NOT NULL, received_at TIMESTAMP NOT NULL,
		PRIMARY KEY (crew_id, seq))`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(t.Context(), path)
	require.ErrorContains(t, err, "predates the tour-scoped inbox (#22); delete "+path)
}

// So is one made before beams.tour_no (#97): its beams would all read as
// tour 0's, and a later tour could reuse one.
func TestOpenRejectsBeamsWithoutTour(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFile)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE beams (
		uuid TEXT PRIMARY KEY, alias TEXT NOT NULL, crew_id TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(t.Context(), path)
	require.ErrorContains(t, err, "predates the tour-numbered beams (#97); delete "+path)
}

// A database made before crew.model and orders.model existed (#58) gains
// both on Open, as it does the later order columns (#100, #141), empty for
// existing rows, and opening it again leaves them as they are.
func TestOpenAddsModelColumns(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), DBFile)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE crew (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, rank TEXT NOT NULL DEFAULT '', division TEXT NOT NULL DEFAULT '',
		spec TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'on_deck', current_beam TEXT NOT NULL DEFAULT '',
		tour_no INTEGER NOT NULL DEFAULT 0, dossier_path TEXT NOT NULL DEFAULT '', harness TEXT NOT NULL DEFAULT 'sdk');
		INSERT INTO crew (id, name) VALUES ('sorell', 'Sorell');
		CREATE TABLE orders (
		id TEXT PRIMARY KEY, title TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', division TEXT NOT NULL DEFAULT '',
		priority INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL, assignee TEXT NOT NULL DEFAULT '',
		parent_id TEXT NOT NULL DEFAULT '', base_ref TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
		budget_tokens INTEGER NOT NULL DEFAULT 0, created_by TEXT NOT NULL DEFAULT '');
		INSERT INTO orders (id, title, state) VALUES ('ORD-0001', 'old', 'done');
		CREATE TABLE hails (
		id TEXT PRIMARY KEY, order_id TEXT NOT NULL REFERENCES orders(id), crew_id TEXT NOT NULL REFERENCES crew(id),
		category TEXT NOT NULL DEFAULT '', question TEXT NOT NULL, options TEXT NOT NULL DEFAULT '[]',
		context TEXT NOT NULL DEFAULT '{}', route TEXT NOT NULL DEFAULT 'captain', status TEXT NOT NULL DEFAULT 'open',
		decided_by TEXT NOT NULL DEFAULT '', decision TEXT NOT NULL DEFAULT '', rationale TEXT NOT NULL DEFAULT '',
		confidence REAL, countermand_until TIMESTAMP);
		INSERT INTO hails (id, order_id, crew_id, question, status, decided_by, decision)
		VALUES ('H-0001', 'ORD-0001', 'sorell', '?', 'decided', 'captain', 'yes');`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	for range 2 {
		s, err := Open(ctx, path)
		require.NoError(t, err)
		c, err := s.GetCrew(ctx, "sorell")
		require.NoError(t, err)
		require.Equal(t, Crew{ID: "sorell", Name: "Sorell", Status: CrewOnDeck, Harness: "sdk"}, c)
		o, err := s.GetOrder(ctx, "ORD-0001")
		require.NoError(t, err)
		require.Empty(t, o.Model)
		require.Nil(t, o.ProgressPct, "the progress columns (#141) are added empty")
		require.Empty(t, o.Source, "the source column (#54) is added empty")
		h, err := s.GetHail(ctx, "H-0001")
		require.NoError(t, err)
		require.Equal(t, "captain", h.DecidedBy)
		require.Nil(t, h.CreatedAt, "the hail timestamps (#147) are added NULL")
		require.Nil(t, h.DecidedAt)
		require.Empty(t, h.OriginalDecidedBy)
		require.NoError(t, s.Close())
	}

	s, err := Open(ctx, path)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "tuvok", Name: "Tuvok", Harness: "echo", Model: "m1"}, testEntry))
	c, err := s.GetCrew(ctx, "tuvok")
	require.NoError(t, err)
	require.Equal(t, "echo", c.Harness)
	require.Equal(t, "m1", c.Model)
	id, err := s.InsertOrder(ctx, Order{Title: "new", Assignee: "tuvok", Model: "m2", Source: "tracker:o/r#12"}, testEntry)
	require.NoError(t, err)
	o, err := s.GetOrder(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "m2", o.Model)
	require.Equal(t, "tracker:o/r#12", o.Source)
}

func TestCrewOrderClaim(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)

	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Rank: "lieutenant", Division: "engineering"}, testEntry))
	c, err := s.GetCrew(ctx, "sorell")
	require.NoError(t, err)
	require.Equal(t, CrewOnDeck, c.Status)
	require.Empty(t, c.Harness, "an empty harness stays unset")

	id1, err := s.InsertOrder(ctx, Order{Title: "first", Assignee: "sorell", Repos: []OrderRepo{{Name: "app", BaseRef: "main"}}}, testEntry)
	require.NoError(t, err)
	require.Equal(t, "ORD-0001", id1)
	id2, err := s.InsertOrder(ctx, Order{Title: "second", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	require.Equal(t, "ORD-0002", id2)
	pool, err := s.InsertOrder(ctx, Order{Title: "unassigned"}, testEntry)
	require.NoError(t, err)

	o, err := s.GetOrder(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, OrderPool, o.State)

	got, found, err := s.ClaimOldestDirectedOrder(ctx, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, id1, got)

	o, err = s.GetOrder(ctx, id1)
	require.NoError(t, err)
	require.Equal(t, OrderAway, o.State)
	require.Equal(t, "first", o.Title)
	require.Equal(t, []OrderRepo{{Name: "app", BaseRef: "main"}}, o.Repos)

	// Claiming again before finishing (e.g. a replayed step) returns the
	// order already away, not the next directed one.
	got, found, err = s.ClaimOldestDirectedOrder(ctx, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, id1, got)
	o, err = s.GetOrder(ctx, id2)
	require.NoError(t, err)
	require.Equal(t, OrderDirected, o.State)

	_, _, err = s.SettleOrder(ctx, id1, OrderDone, testEntry, nil)
	require.NoError(t, err)
	got, found, err = s.ClaimOldestDirectedOrder(ctx, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, id2, got)

	_, _, err = s.SettleOrder(ctx, id2, OrderFailed, testEntry, nil)
	require.NoError(t, err)
	_, found, err = s.ClaimOldestDirectedOrder(ctx, "sorell", testEntry)
	require.NoError(t, err)
	require.False(t, found)

	orders, err := s.ListOrders(ctx)
	require.NoError(t, err)
	require.Len(t, orders, 3)
	require.Equal(t, OrderDone, orders[0].State)
	require.Equal(t, OrderFailed, orders[1].State)

	require.NoError(t, s.SetCrewStatus(ctx, "sorell", CrewAway, "beam-1", testEntry))
	crew, err := s.ListCrew(ctx)
	require.NoError(t, err)
	require.Len(t, crew, 1)
	require.Equal(t, CrewAway, crew[0].Status)
	require.Equal(t, "beam-1", crew[0].CurrentBeam)

	require.ErrorIs(t, s.SetCrewStatus(ctx, "nobody", CrewAway, "", testEntry), sql.ErrNoRows)

	require.NoError(t, s.UpdateCrewStatus(ctx, "sorell", CrewHailing, testEntry))
	cr, err := s.GetCrew(ctx, "sorell")
	require.NoError(t, err)
	require.Equal(t, CrewHailing, cr.Status)
	require.Equal(t, "beam-1", cr.CurrentBeam, "status-only update keeps the beam")
	require.ErrorIs(t, s.UpdateCrewStatus(ctx, "nobody", CrewAway, testEntry), sql.ErrNoRows)

	exp := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	b := BeamRecord{UUID: "beam-1", Alias: "beam-1", CrewID: "sorell", TourNo: 3, ExpiresAt: exp}
	require.NoError(t, s.InsertBeam(ctx, b, testEntry))
	require.NoError(t, s.InsertBeam(ctx, b, testEntry), "recording a beam again is a no-op")
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "kirk", Name: "Kirk"}, testEntry))
	other := b
	other.CrewID = "kirk"
	require.ErrorIs(t, s.InsertBeam(ctx, other, testEntry), ErrBeamTaken)
	beams, err := s.ListBeams(ctx, "sorell")
	require.NoError(t, err)
	require.Len(t, beams, 1)
	require.Equal(t, "beam-1", beams[0].Alias)
	require.Equal(t, 3, beams[0].TourNo)
	require.True(t, exp.Equal(beams[0].ExpiresAt))

	_, err = s.GetCrew(ctx, "nobody")
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// Directed claims take the highest priority first, then the oldest (#96).
func TestClaimDirectedByPriority(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	var ids []string
	for _, p := range []int{PriorityRoutine, PriorityYellow, PriorityRed, PriorityYellow} {
		id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell", Priority: p}, testEntry)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	var got []string
	for {
		id, found, err := s.ClaimOldestDirectedOrder(ctx, "sorell", testEntry)
		require.NoError(t, err)
		if !found {
			break
		}
		got = append(got, id)
		_, _, err = s.SettleOrder(ctx, id, OrderDone, testEntry, nil)
		require.NoError(t, err)
	}
	require.Equal(t, []string{ids[2], ids[1], ids[3], ids[0]}, got)
}

// Pool claims (#96): the division's highest-priority pool order, then the
// oldest, assigned to the claimer and away; it is then the crew member's
// order away for the directed claim.
func TestClaimPoolOrder(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Division: "ops"}, testEntry))
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "kirk", Name: "Kirk", Status: CrewDismissed, Division: "ops"}, testEntry))
	insert := func(o Order) string {
		id, err := s.InsertOrder(ctx, o, testEntry)
		require.NoError(t, err)
		return id
	}
	routine := insert(Order{Title: "routine", Division: "ops"})
	insert(Order{Title: "science", Division: "science", Priority: PriorityRed})
	insert(Order{Title: "no division", Priority: PriorityRed})
	directed := insert(Order{Title: "directed", Division: "ops", Assignee: "kirk", Priority: PriorityRed})
	red := insert(Order{Title: "red", Division: "ops", Priority: PriorityRed})

	for _, c := range []struct{ crew, division string }{{"sorell", ""}, {"kirk", "ops"}, {"nobody", "ops"}} {
		_, found, err := s.ClaimOldestPoolOrder(ctx, c.crew, c.division, testEntry)
		require.NoError(t, err)
		require.False(t, found, "%s in %q", c.crew, c.division)
	}

	got, found, err := s.ClaimOldestPoolOrder(ctx, "sorell", "ops", LogEntry{Kind: "order", Text: "claimed"})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, red, got)
	o, err := s.GetOrder(ctx, red)
	require.NoError(t, err)
	require.Equal(t, OrderAway, o.State)
	require.Equal(t, "sorell", o.Assignee)
	c, err := s.GetCrew(ctx, "sorell")
	require.NoError(t, err)
	require.Equal(t, CrewAway, c.Status)
	entries, err := s.ListLog(ctx, 0)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	require.Equal(t, "claimed", last.Text)
	require.Equal(t, red, last.OrderID)

	// A re-run of the claim (directed first) returns it as the order away.
	got, found, err = s.ClaimOldestDirectedOrder(ctx, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, red, got)

	_, _, err = s.SettleOrder(ctx, red, OrderDone, testEntry, nil)
	require.NoError(t, err)
	got, found, err = s.ClaimOldestPoolOrder(ctx, "sorell", "ops", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, routine, got)
	o, err = s.GetOrder(ctx, directed)
	require.NoError(t, err)
	require.Equal(t, OrderDirected, o.State, "a directed order isn't in the pool")
	_, _, err = s.SettleOrder(ctx, routine, OrderDone, testEntry, nil)
	require.NoError(t, err)
	_, found, err = s.ClaimOldestPoolOrder(ctx, "sorell", "ops", testEntry)
	require.NoError(t, err)
	require.False(t, found)
}

// A pool order with parked_at set (a recalled order, #101) keeps it when
// claimed, so the claimer resumes its mission.
func TestClaimPoolOrderKeepsParked(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Division: "ops"}, testEntry))
	id, err := s.InsertOrder(ctx, Order{Title: "t", Division: "ops"}, testEntry)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE orders SET parked_at = ? WHERE id = ?`, time.Now(), id)
	require.NoError(t, err)
	got, found, err := s.ClaimOldestPoolOrder(ctx, "sorell", "ops", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, id, got)
	o, err := s.GetOrder(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, o.ParkedAt)
}

func TestHails(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)

	h1, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Category: "scope_change", Question: "Widen?",
		Options: []string{"Widen scope", "Stay on target"}, Context: json.RawMessage(`{"files":["a.go"]}`)}, testEntry)
	require.NoError(t, err)
	require.Equal(t, "H-0001", h1)

	// While h1 is open, inserting again for the same order returns it.
	again, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "Widen?"}, testEntry)
	require.NoError(t, err)
	require.Equal(t, h1, again)

	// Deciding twice with the same decision is a no-op; a different one fails.
	require.NoError(t, s.MarkHailDecided(ctx, h1, "captain", "Stay on target", "keep it small", testEntry))
	require.NoError(t, s.MarkHailDecided(ctx, h1, "captain", "Stay on target", "keep it small", testEntry))
	require.ErrorIs(t, s.MarkHailDecided(ctx, h1, "captain", "Widen scope", "", testEntry), ErrHailClosed)

	h2, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "Push?"}, testEntry)
	require.NoError(t, err)
	require.Equal(t, "H-0002", h2)

	h, err := s.GetHail(ctx, h1)
	require.NoError(t, err)
	require.Equal(t, HailDecided, h.Status)
	require.Equal(t, "captain", h.Route)
	require.Equal(t, []string{"Widen scope", "Stay on target"}, h.Options)
	require.JSONEq(t, `{"files":["a.go"]}`, string(h.Context))
	require.Equal(t, "Stay on target", h.Decision)
	require.Nil(t, h.Confidence)

	// A timed-out hail can't be decided.
	timedOut, err := s.TimeOutOpenHail(ctx, h2, LogEntry{Kind: "hail", CrewID: "sorell", OrderID: orderID, Text: "closed"})
	require.NoError(t, err)
	require.True(t, timedOut)
	require.ErrorIs(t, s.MarkHailDecided(ctx, h2, "captain", "yes", "", testEntry), ErrHailClosed)

	// Missing hails are sql.ErrNoRows.
	require.ErrorIs(t, s.MarkHailDecided(ctx, "H-9999", "captain", "yes", "", testEntry), sql.ErrNoRows)

	// Hails must reference an existing order.
	_, err = s.InsertHail(ctx, Hail{OrderID: "ORD-9999", CrewID: "sorell", Question: "?"}, testEntry)
	require.Error(t, err)

	// ListHails filters by status; empty lists all, in creation order.
	h3, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "Merge?", Options: []string{"yes", "no"}}, testEntry)
	require.NoError(t, err)
	ids := func(status string) []string {
		hs, err := s.ListHails(ctx, status)
		require.NoError(t, err)
		var out []string
		for _, h := range hs {
			out = append(out, h.ID)
		}
		return out
	}
	require.Equal(t, []string{h1, h2, h3}, ids(""))
	require.Equal(t, []string{h3}, ids(HailOpen))
	require.Equal(t, []string{h1}, ids(HailDecided))
	require.Equal(t, []string{h2}, ids(HailTimedOut))
	all, err := s.ListHails(ctx, "")
	require.NoError(t, err)
	require.Equal(t, h, all[0], "ListHails reads rows as GetHail does")

	// OpenHailForOrder finds the order's open hail, and nothing once it closes.
	open, found, err := s.OpenHailForOrder(ctx, orderID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, h3, open.ID)
	require.Equal(t, []string{"yes", "no"}, open.Options)
	_, found, err = s.OpenHailForOrder(ctx, "ORD-9999")
	require.NoError(t, err)
	require.False(t, found)
	entry := LogEntry{Kind: "hail", CrewID: "sorell", OrderID: orderID, Text: "closed"}
	closed, err := s.TimeOutOpenHail(ctx, h3, entry)
	require.NoError(t, err)
	require.True(t, closed)
	_, found, err = s.OpenHailForOrder(ctx, orderID)
	require.NoError(t, err)
	require.False(t, found)

	// TimeOutOpenHail closes and logs only an open hail.
	for _, id := range []string{h3, h1} { // timed out, decided
		closed, err = s.TimeOutOpenHail(ctx, id, entry)
		require.NoError(t, err)
		require.False(t, closed)
	}
	_, err = s.TimeOutOpenHail(ctx, "H-9999", entry)
	require.ErrorIs(t, err, sql.ErrNoRows)
	entries, err := s.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Len(t, withoutTestEntries(entries), 2, "h2's and h3's")
	got, err := s.GetHail(ctx, h1)
	require.NoError(t, err)
	require.Equal(t, HailDecided, got.Status)
}

func TestListLogLimit(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	for i := range 5 {
		_, err := s.AppendLog(ctx, LogEntry{Kind: "order", Text: strconv.Itoa(i)})
		require.NoError(t, err)
	}
	texts := func(since int64, limit int) []string {
		es, err := s.ListLogLimit(ctx, since, limit)
		require.NoError(t, err)
		var out []string
		for _, e := range es {
			out = append(out, e.Text)
		}
		return out
	}
	require.Equal(t, []string{"0", "1", "2", "3", "4"}, texts(0, 0))
	require.Equal(t, []string{"0", "1"}, texts(0, 2), "the oldest entries above since")
	require.Equal(t, []string{"2", "3"}, texts(2, 2))
	require.Empty(t, texts(5, 2))
}

func TestDecideOpenHail(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	id, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "?"}, testEntry)
	require.NoError(t, err)

	// fn failing rolls the decision back.
	boom := errors.New("boom")
	require.ErrorIs(t, s.DecideOpenHail(ctx, id, "captain", "yes", "", testEntry, func(*sql.Tx) error { return boom }), boom)
	h, err := s.GetHail(ctx, id)
	require.NoError(t, err)
	require.Equal(t, HailOpen, h.Status)

	ran := false
	require.NoError(t, s.DecideOpenHail(ctx, id, "captain", "yes", "", testEntry, func(*sql.Tx) error { ran = true; return nil }))
	require.True(t, ran)
	// Not idempotent: even the same decision is refused once decided.
	require.ErrorIs(t, s.DecideOpenHail(ctx, id, "captain", "yes", "", testEntry, nil), ErrHailClosed)
	// MarkHailDecided with the same arguments still confirms it.
	require.NoError(t, s.MarkHailDecided(ctx, id, "captain", "yes", "", testEntry))
	require.ErrorIs(t, s.DecideOpenHail(ctx, "H-9999", "captain", "yes", "", testEntry, nil), sql.ErrNoRows)
}

// #147: a hail records when it was raised and first decided, and its
// first decider, which a later decision (a countermand's, simulated here by
// reopening the hail) doesn't change.
func TestHailTimestamps(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	raised := time.Date(2026, 10, 29, 9, 0, 0, 0, time.FixedZone("x", 3600))
	id, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "?"},
		LogEntry{TS: raised, Kind: "hail", Text: "raised"})
	require.NoError(t, err)
	h, err := s.GetHail(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, h.CreatedAt)
	require.True(t, raised.Equal(*h.CreatedAt), "created_at is the entry's TS: %v", h.CreatedAt)
	require.Nil(t, h.DecidedAt)
	require.Empty(t, h.OriginalDecidedBy)

	require.NoError(t, s.DecideOpenHail(ctx, id, "captain", "yes", "", LogEntry{Kind: "hail", Text: "decided"}, nil))
	h, err = s.GetHail(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, h.DecidedAt)
	require.Equal(t, "captain", h.OriginalDecidedBy)
	entries, err := s.ListLog(ctx, 0)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	require.Equal(t, "decided", last.Text)
	require.True(t, last.TS.Equal(*h.DecidedAt), "decided_at is the log entry's ts: %v, %v", h.DecidedAt, last.TS)
	first := *h.DecidedAt

	_, err = s.db.ExecContext(ctx, `UPDATE hails SET status = ? WHERE id = ?`, HailOpen, id)
	require.NoError(t, err)
	later := first.Add(time.Minute)
	require.NoError(t, s.DecideOpenHail(ctx, id, "command", "no", "", LogEntry{TS: later, Kind: "hail", Text: "again"}, nil))
	h, err = s.GetHail(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "command", h.DecidedBy)
	require.Equal(t, "captain", h.OriginalDecidedBy, "set once, at the first decision")
	require.True(t, first.Equal(*h.DecidedAt), "decided_at too")
}

// DecideOpenHailAs writes route, confidence and countermand_until with the
// decision only when set (#149).
func TestDecideOpenHailAs(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	plain, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "?"}, testEntry)
	require.NoError(t, err)
	require.NoError(t, s.DecideOpenHailAs(ctx, plain, HailDecision{DecidedBy: "captain", Decision: "yes"}, testEntry, nil))
	h, err := s.GetHail(ctx, plain)
	require.NoError(t, err)
	require.Equal(t, "captain", h.Route, "InsertHail's default, kept")
	require.Nil(t, h.Confidence)
	require.Nil(t, h.CountermandUntil)

	full, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "?"}, testEntry)
	require.NoError(t, err)
	until := time.Date(2026, 10, 30, 9, 5, 0, 0, time.FixedZone("x", 3600))
	conf := 0.9
	require.NoError(t, s.DecideOpenHailAs(ctx, full, HailDecision{DecidedBy: "policy", Decision: "allow", Rationale: "rule",
		Route: "auto", Confidence: &conf, CountermandUntil: &until}, testEntry, nil))
	h, err = s.GetHail(ctx, full)
	require.NoError(t, err)
	require.Equal(t, HailDecided, h.Status)
	require.Equal(t, "policy", h.DecidedBy)
	require.Equal(t, "policy", h.OriginalDecidedBy)
	require.Equal(t, "rule", h.Rationale)
	require.Equal(t, "auto", h.Route)
	require.InDelta(t, 0.9, *h.Confidence, 0)
	require.True(t, until.Equal(*h.CountermandUntil), "%v", h.CountermandUntil)
	require.ErrorIs(t, s.DecideOpenHailAs(ctx, full, HailDecision{DecidedBy: "captain", Decision: "no"}, testEntry, nil), ErrHailClosed)
}

// CountermandHail takes a policy or Command decision inside its window:
// decided_by turns captain, original_decided_by stays. It refuses one past
// the window, an auto_allow decision (no window), the captain's own answer
// and an open hail, with ErrCountermandExpired (#153). MarkHailDecided
// still confirms the first decision afterwards.
func TestCountermandHail(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	decided := time.Now().Add(-time.Minute).UTC()
	until := decided.Add(5 * time.Minute)
	gone := decided.Add(30 * time.Second) // in the past already
	hail := func(dec *HailDecision) string {
		t.Helper()
		id, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "?", Options: []string{"allow", "deny"}}, testEntry)
		require.NoError(t, err)
		if dec != nil {
			require.NoError(t, s.DecideOpenHailAs(ctx, id, *dec, LogEntry{TS: decided, Kind: "hail", Text: "decided"}, nil))
		}
		return id
	}
	conf := 0.9
	entry := LogEntry{Kind: "hail", Text: "countermanded"}

	// Inside the window, fn runs in the transaction, and its failure rolls
	// the countermand back.
	cmd := hail(&HailDecision{DecidedBy: "command", Decision: "allow", Rationale: "fine", Route: "command",
		Confidence: &conf, CountermandUntil: &until})
	boom := errors.New("boom")
	require.ErrorIs(t, s.CountermandHail(ctx, cmd, "deny", "too risky", entry, func(*sql.Tx) error { return boom }), boom)
	h, err := s.GetHail(ctx, cmd)
	require.NoError(t, err)
	require.Equal(t, "command", h.DecidedBy)
	require.Nil(t, h.CountermandedAt)

	ran := false
	before := time.Now()
	require.NoError(t, s.CountermandHail(ctx, cmd, "deny", "too risky", entry, func(*sql.Tx) error { ran = true; return nil }))
	after := time.Now()
	require.True(t, ran)
	h, err = s.GetHail(ctx, cmd)
	require.NoError(t, err)
	require.Equal(t, HailDecided, h.Status)
	require.Equal(t, "captain", h.DecidedBy)
	require.Equal(t, "command", h.OriginalDecidedBy)
	require.Equal(t, "deny", h.Decision)
	require.Equal(t, "too risky", h.Rationale)
	require.Equal(t, "command", h.Route)
	require.InDelta(t, 0.9, *h.Confidence, 0)
	require.True(t, decided.Equal(*h.DecidedAt), "decided_at is the first decision's")
	require.True(t, until.Equal(*h.CountermandUntil))
	require.NotNil(t, h.CountermandedAt)
	require.False(t, h.CountermandedAt.Before(before) || h.CountermandedAt.After(after), "countermanded_at is taken in the transaction")
	entries, err := s.ListLog(ctx, 0)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	require.Equal(t, "countermanded", last.Text)
	require.True(t, last.TS.Equal(*h.CountermandedAt))

	// The mission's decide-hail, confirming Command's first decision after
	// the countermand, is a no-op; another decider's is still refused.
	require.NoError(t, s.MarkHailDecided(ctx, cmd, "command", "allow", "fine", testEntry))
	require.ErrorIs(t, s.MarkHailDecided(ctx, cmd, "policy", "allow", "", testEntry), ErrHailClosed)
	h, err = s.GetHail(ctx, cmd)
	require.NoError(t, err)
	require.Equal(t, "captain", h.DecidedBy)
	require.Equal(t, "deny", h.Decision)

	require.True(t, h.CountermandedAway())
	require.Nil(t, h.CountermandOption)

	// A second countermand in the same window is taken, and records the
	// option it picked (#54): option 0 with text of the captain's own
	// allows.
	zero := 0
	require.NoError(t, s.CountermandHailOption(ctx, cmd, "allow, go ahead", &zero, "", entry, nil))
	h, err = s.GetHail(ctx, cmd)
	require.NoError(t, err)
	require.Equal(t, "allow, go ahead", h.Decision)
	require.Equal(t, &zero, h.CountermandOption)
	require.False(t, h.CountermandedAway())
	require.NoError(t, s.CountermandHail(ctx, cmd, "allow", "", entry, nil))
	h, err = s.GetHail(ctx, cmd)
	require.NoError(t, err)
	require.Nil(t, h.CountermandOption, "a free-text countermand clears it")
	locked, now, err := s.GetHailLocked(ctx, cmd)
	require.NoError(t, err)
	require.Equal(t, h, locked)
	require.WithinDuration(t, time.Now(), now, time.Minute)
	require.False(t, h.CountermandedAway(), "no option: allow reads as allowing")
	// Past the window it is refused, and nothing changes.
	late := hail(&HailDecision{DecidedBy: "command", Decision: "allow", CountermandUntil: &gone})
	require.ErrorIs(t, s.CountermandHail(ctx, late, "deny", "late", entry, nil), ErrCountermandExpired)
	h, err = s.GetHail(ctx, late)
	require.NoError(t, err)
	require.Equal(t, "allow", h.Decision)
	require.Equal(t, "command", h.DecidedBy)

	// The captain timeout's default_option (policy, with a window) is taken.
	timeout := hail(&HailDecision{DecidedBy: "policy", Decision: "allow", CountermandUntil: &until})
	require.NoError(t, s.CountermandHail(ctx, timeout, "deny", "", entry, nil))

	// An auto_allow decision has no window.
	auto := hail(&HailDecision{DecidedBy: "policy", Decision: "allow", Route: "auto"})
	require.ErrorIs(t, s.CountermandHail(ctx, auto, "deny", "", entry, nil), ErrCountermandExpired)
	// Nor does the captain's own answer, even with countermand_until set.
	captain := hail(&HailDecision{DecidedBy: "captain", Decision: "allow", CountermandUntil: &until})
	require.ErrorIs(t, s.CountermandHail(ctx, captain, "deny", "", entry, nil), ErrCountermandExpired)
	// Nor an open hail.
	open := hail(nil)
	require.ErrorIs(t, s.CountermandHail(ctx, open, "deny", "", entry, nil), ErrCountermandExpired)
	require.ErrorIs(t, s.CountermandHail(ctx, "H-9999", "deny", "", entry, nil), sql.ErrNoRows)

	// ListCountermandable: the hails still inside their window at now,
	// the window's last instant included.
	ids := func(now time.Time) []string {
		t.Helper()
		hs, err := s.ListCountermandable(ctx, now)
		require.NoError(t, err)
		var out []string
		for _, h := range hs {
			out = append(out, h.ID)
		}
		return out
	}
	require.Equal(t, []string{cmd, timeout}, ids(time.Now()))
	require.Equal(t, []string{cmd, timeout}, ids(until))
	require.Equal(t, []string{cmd, late, timeout}, ids(gone))
	require.Empty(t, ids(until.Add(time.Nanosecond)))
}

// UpdateOpenHail sets the route and logs only while the hail is open, and
// TimeOutOpenHailThen runs its fn only when it closes the hail (#149).
func TestUpdateOpenHail(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	id, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "?", Route: "command"}, testEntry)
	require.NoError(t, err)
	entry := LogEntry{Kind: "hail", Text: "routed"}
	open, err := s.UpdateOpenHail(ctx, id, "captain", entry)
	require.NoError(t, err)
	require.True(t, open)
	open, err = s.UpdateOpenHail(ctx, id, "", LogEntry{Kind: "hail", Text: "reminder"})
	require.NoError(t, err)
	require.True(t, open, "the same route counts")
	h, err := s.GetHail(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "captain", h.Route, "an empty route keeps it")

	ran := 0
	fn := func(*sql.Tx) error { ran++; return nil }
	closed, err := s.TimeOutOpenHailThen(ctx, id, LogEntry{Kind: "hail", Text: "closed"}, fn)
	require.NoError(t, err)
	require.True(t, closed)
	closed, err = s.TimeOutOpenHailThen(ctx, id, LogEntry{Kind: "hail", Text: "closed"}, fn)
	require.NoError(t, err)
	require.False(t, closed)
	require.Equal(t, 1, ran)

	open, err = s.UpdateOpenHail(ctx, id, "auto", LogEntry{Kind: "hail", Text: "late"})
	require.NoError(t, err)
	require.False(t, open)
	_, err = s.UpdateOpenHail(ctx, "H-9999", "", entry)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Equal(t, []string{"routed", "reminder", "closed"}, texts(t, s))
	h, err = s.GetHail(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "captain", h.Route)

	// A failing fn rolls the close back.
	id2, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "?"}, testEntry)
	require.NoError(t, err)
	boom := errors.New("boom")
	_, err = s.TimeOutOpenHailThen(ctx, id2, LogEntry{Kind: "hail", Text: "closed"}, func(*sql.Tx) error { return boom })
	require.ErrorIs(t, err, boom)
	h, err = s.GetHail(ctx, id2)
	require.NoError(t, err)
	require.Equal(t, HailOpen, h.Status)
}

// An order and its crew member are hailing while the order's hail is open,
// and back away once the hail is decided or timed out (#134).
func TestOrderHailing(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Status: CrewAway}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell", Division: "ops", State: OrderAway}, testEntry)
	require.NoError(t, err)
	state := func() string {
		t.Helper()
		o, err := s.GetOrder(ctx, orderID)
		require.NoError(t, err)
		return o.State
	}
	hail := func() string {
		t.Helper()
		id, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "?"}, testEntry)
		require.NoError(t, err)
		return id
	}
	crewStatus := func() string {
		t.Helper()
		c, err := s.GetCrew(ctx, "sorell")
		require.NoError(t, err)
		return c.Status
	}

	h := hail()
	require.Equal(t, OrderHailing, state())
	require.NoError(t, s.MarkHailDecided(ctx, h, "captain", "yes", "", testEntry))
	require.Equal(t, OrderAway, state())

	h = hail()
	require.Equal(t, OrderHailing, state())
	require.NoError(t, s.DecideOpenHail(ctx, h, "captain", "yes", "", testEntry, nil))
	require.Equal(t, OrderAway, state())

	h = hail()
	require.Equal(t, OrderHailing, state())
	require.Equal(t, CrewHailing, crewStatus())
	closed, err := s.TimeOutOpenHail(ctx, h, testEntry)
	require.NoError(t, err)
	require.True(t, closed)
	require.Equal(t, OrderAway, state())
	require.Equal(t, CrewAway, crewStatus(), "a timed-out hail sets the crew member back away too")

	// A late hail from a mission whose order a recall moved to the pool
	// leaves the order alone (#48), and so does closing it.
	_, recalled, err := s.RecallOrder(ctx, orderID, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, recalled)
	h = hail()
	require.Equal(t, OrderPool, state())
	// Meanwhile sorell hails on its next order; closing the stale hail
	// leaves sorell and that order alone.
	next, err := s.InsertOrder(ctx, Order{Title: "t2", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)
	nextHail, err := s.InsertHail(ctx, Hail{OrderID: next, CrewID: "sorell", Question: "?"}, testEntry)
	require.NoError(t, err)
	require.Equal(t, CrewHailing, crewStatus())
	closed, err = s.TimeOutOpenHail(ctx, h, testEntry)
	require.NoError(t, err)
	require.True(t, closed)
	require.Equal(t, OrderPool, state())
	require.Equal(t, CrewHailing, crewStatus())
	o, err := s.GetOrder(ctx, next)
	require.NoError(t, err)
	require.Equal(t, OrderHailing, o.State)
	closed, err = s.TimeOutOpenHail(ctx, nextHail, testEntry)
	require.NoError(t, err)
	require.True(t, closed)
	require.Equal(t, CrewAway, crewStatus())

	// Claimed by another crew member, the order isn't the late hail's
	// crew member's: left alone.
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "spock", Name: "Spock", Division: "ops"}, testEntry))
	_, found, err := s.ClaimOldestPoolOrder(ctx, "spock", "ops", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	h = hail()
	require.Equal(t, OrderAway, state())
	require.NoError(t, s.DecideOpenHail(ctx, h, "captain", "yes", "", testEntry, nil))

	// Settled from hailing, it stays settled when its hail closes.
	h, err = s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "spock", Question: "?"}, testEntry)
	require.NoError(t, err)
	require.Equal(t, OrderHailing, state())
	current, settled, err := s.SettleOrder(ctx, orderID, OrderFailed, testEntry, nil)
	require.NoError(t, err)
	require.True(t, settled)
	require.Equal(t, OrderFailed, current)
	closed, err = s.TimeOutOpenHail(ctx, h, testEntry)
	require.NoError(t, err)
	require.True(t, closed)
	require.Equal(t, OrderFailed, state())
}

func TestCheckpointLogInbox(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)

	cp := Checkpoint{OrderID: orderID, Seq: 1, TranscriptPath: "t", HandoverPath: "h",
		Repos: []CheckpointRepo{{Name: "app", Commit: "abc123", BundlePath: "b"}, {Name: "lib", Commit: "0f0f", BundlePath: "l"}}}
	require.NoError(t, s.InsertCheckpoint(ctx, cp))
	require.NoError(t, s.InsertCheckpoint(ctx, cp), "repeating the same checkpoint is a no-op")
	for name, other := range map[string]Checkpoint{
		"another commit":  {OrderID: orderID, Seq: 1, TranscriptPath: "t", HandoverPath: "h", Repos: []CheckpointRepo{{Name: "app", Commit: "def456", BundlePath: "b"}, cp.Repos[1]}},
		"a repo fewer":    {OrderID: orderID, Seq: 1, TranscriptPath: "t", HandoverPath: "h", Repos: cp.Repos[:1]},
		"repos reordered": {OrderID: orderID, Seq: 1, TranscriptPath: "t", HandoverPath: "h", Repos: []CheckpointRepo{cp.Repos[1], cp.Repos[0]}},
		"another note":    {OrderID: orderID, Seq: 1, TranscriptPath: "t", HandoverPath: "h2", Repos: cp.Repos},
	} {
		require.ErrorIs(t, s.InsertCheckpoint(ctx, other), ErrCheckpointConflict, name)
	}
	cps, err := s.ListCheckpoints(ctx, orderID)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	cp.CreatedAt = cps[0].CreatedAt
	require.Equal(t, cp, cps[0])

	id1, err := s.AppendLog(ctx, LogEntry{Kind: "order", CrewID: "sorell", OrderID: orderID, Text: "claimed"})
	require.NoError(t, err)
	id2, err := s.AppendLog(ctx, LogEntry{Kind: "done", OrderID: orderID, Payload: json.RawMessage(`{"pct":100}`)})
	require.NoError(t, err)
	require.Greater(t, id2, id1)
	last, err := s.LastOrderLog(ctx, orderID, "done")
	require.NoError(t, err)
	require.Equal(t, id2, last.ID)
	require.JSONEq(t, `{"pct":100}`, string(last.Payload))
	_, err = s.LastOrderLog(ctx, orderID, "fail")
	require.ErrorIs(t, err, sql.ErrNoRows)

	// Stored times round-trip and are readable by SQLite's date functions.
	var (
		ts      time.Time
		tsEpoch int64
	)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT ts, unixepoch(ts) FROM log WHERE id = ?`, id1).Scan(&ts, &tsEpoch))
	require.WithinDuration(t, time.Now(), ts, time.Minute)
	require.Equal(t, ts.Unix(), tsEpoch)

	at := time.Date(2026, 10, 16, 9, 12, 3, 0, time.UTC)
	require.NoError(t, s.InsertCheckpoint(ctx, Checkpoint{OrderID: orderID, Seq: 2, CreatedAt: at}))
	var (
		created      time.Time
		createdEpoch int64
	)
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT created_at, unixepoch(created_at) FROM checkpoints WHERE order_id = ? AND seq = 2`,
		orderID).Scan(&created, &createdEpoch))
	require.True(t, at.Equal(created), "got %v", created)
	require.Equal(t, at.Unix(), createdEpoch)

	seen, err := s.SeenInbox(ctx, "sorell", 0, 7)
	require.NoError(t, err)
	require.False(t, seen)
	inserted, err := s.InsertInboxSeq(ctx, "sorell", 0, 7)
	require.NoError(t, err)
	require.True(t, inserted)
	inserted, err = s.InsertInboxSeq(ctx, "sorell", 0, 7)
	require.NoError(t, err)
	require.False(t, inserted)
	seen, err = s.SeenInbox(ctx, "sorell", 0, 7)
	require.NoError(t, err)
	require.True(t, seen)
	seen, err = s.SeenInbox(ctx, "sorell", 1, 7)
	require.NoError(t, err)
	require.False(t, seen, "per tour")
	inserted, err = s.InsertInboxSeq(ctx, "sorell", 1, 7)
	require.NoError(t, err)
	require.True(t, inserted, "per tour")
}

func TestConcurrentInsertOrder(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)

	const n = 20
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { ids[i], errs[i] = s.InsertOrder(ctx, Order{Title: strconv.Itoa(i)}, testEntry) })
	}
	wg.Wait()

	want := make([]string, n)
	for i := range n {
		require.NoError(t, errs[i])
		want[i] = fmt.Sprintf("ORD-%04d", i+1)
	}
	require.ElementsMatch(t, want, ids)
}

func TestReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), DBFile)
	s, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	require.NoError(t, s.Close())

	s, err = Open(ctx, path)
	require.NoError(t, err)
	defer s.Close()
	_, err = s.GetCrew(ctx, "sorell")
	require.NoError(t, err)

	var mode string
	require.NoError(t, s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode))
	require.Equal(t, "wal", mode)
}

func TestOpenSpecialCharsInPath(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "we?ird%41 #home")
	path := filepath.Join(dir, DBFile)
	s, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	var mode string
	require.NoError(t, s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode))
	require.Equal(t, "wal", mode, "query parameters still apply")
	require.NoError(t, s.Close())

	_, err = os.Stat(path)
	require.NoError(t, err, "database created at the literal path")
	entries, err := os.ReadDir(filepath.Dir(dir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing created at a truncated path")
}

func TestAcquireLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", LockFile)
	lock, err := AcquireLock(path)
	require.NoError(t, err)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	pid := strconv.Itoa(os.Getpid())
	require.Equal(t, pid, strings.TrimSpace(string(b)))

	_, err = AcquireLock(path)
	require.ErrorIs(t, err, ErrLocked)
	require.Contains(t, err.Error(), "pid "+pid)

	require.NoError(t, lock.Close())
	lock, err = AcquireLock(path)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
}

func TestContiguousInboxSeq(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	contig := func(crew string) uint64 {
		t.Helper()
		n, err := s.ContiguousInboxSeq(ctx, crew, 0)
		require.NoError(t, err)
		return n
	}
	record := func(crew string, seqs ...uint64) {
		t.Helper()
		for _, seq := range seqs {
			_, err := s.RecordInbox(ctx, crew, 0, seq, nil)
			require.NoError(t, err)
		}
	}

	require.Equal(t, uint64(0), contig("a"))
	record("a", 2, 3)
	require.Equal(t, uint64(0), contig("a"), "seq 1 missing")
	record("a", 1)
	require.Equal(t, uint64(3), contig("a"))
	record("a", 5, 6, 9)
	require.Equal(t, uint64(3), contig("a"), "gap at 4")
	record("a", 4)
	require.Equal(t, uint64(6), contig("a"))
	record("b", 1)
	require.Equal(t, uint64(1), contig("b"), "per crew")
	require.Equal(t, uint64(6), contig("a"))

	// A new tour's beam numbers from seq 1 again: its seqs are its own.
	n, err := s.ContiguousInboxSeq(ctx, "a", 1)
	require.NoError(t, err)
	require.Equal(t, uint64(0), n, "per tour")
	recorded, err := s.RecordInbox(ctx, "a", 1, 1, nil)
	require.NoError(t, err)
	require.True(t, recorded, "seq 1 of tour 1 isn't tour 0's")
	n, err = s.ContiguousInboxSeq(ctx, "a", 1)
	require.NoError(t, err)
	require.Equal(t, uint64(1), n)
	require.Equal(t, uint64(6), contig("a"))
}

func TestRecordInboxIsAtomicWithItsCallback(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)

	// A failing callback rolls back both the inbox row and its log entry.
	boom := fmt.Errorf("boom")
	recorded, err := s.RecordInbox(ctx, "a", 0, 1, func(tx InboxTx) error {
		_, err := tx.AppendLog(LogEntry{Kind: "order", CrewID: "a", Text: "rolled back"})
		require.NoError(t, err)
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.False(t, recorded)
	seen, err := s.SeenInbox(ctx, "a", 0, 1)
	require.NoError(t, err)
	require.False(t, seen)
	entries, err := s.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Empty(t, entries)

	// A successful one commits both.
	recorded, err = s.RecordInbox(ctx, "a", 0, 1, func(tx InboxTx) error {
		require.NotNil(t, tx.SQL())
		_, err := tx.AppendLog(LogEntry{Kind: "order", CrewID: "a", Text: "kept"})
		return err
	})
	require.NoError(t, err)
	require.True(t, recorded)
	entries, err = s.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "kept", entries[0].Text)

	// A repeat doesn't run the callback.
	recorded, err = s.RecordInbox(ctx, "a", 0, 1, func(InboxTx) error {
		t.Fatal("callback run for an already recorded seq")
		return nil
	})
	require.NoError(t, err)
	require.False(t, recorded)
}

func TestSettleOrder(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)
	done := LogEntry{Kind: "done", OrderID: id, Text: "done"}
	fail := LogEntry{Kind: "fail", OrderID: id, Text: "failed"}
	late := LogEntry{Kind: "order", OrderID: id, Text: "late"}

	state, settled, err := s.SettleOrder(ctx, id, OrderFailed, fail, nil)
	require.NoError(t, err)
	require.Equal(t, OrderFailed, state)
	require.True(t, settled)
	// A re-run, or another failure: nothing changes, nothing is logged.
	state, settled, err = s.SettleOrder(ctx, id, OrderFailed, fail, &late)
	require.NoError(t, err)
	require.Equal(t, OrderFailed, state)
	require.False(t, settled)
	// A late done keeps the order failed and logs late instead.
	state, settled, err = s.SettleOrder(ctx, id, OrderDone, done, &late)
	require.NoError(t, err)
	require.Equal(t, OrderFailed, state)
	require.False(t, settled)
	_, _, err = s.SettleOrder(ctx, id, OrderDone, done, nil)
	require.NoError(t, err)

	entries, err := s.ListLog(ctx, 0)
	require.NoError(t, err)
	var texts []string
	for _, e := range withoutTestEntries(entries) {
		texts = append(texts, e.Text)
	}
	require.Equal(t, []string{"failed", "late"}, texts)
	o, err := s.GetOrder(ctx, id)
	require.NoError(t, err)
	require.Equal(t, OrderFailed, o.State)
	_, _, err = s.SettleOrder(ctx, "ORD-9999", OrderDone, done, nil)
	require.Error(t, err)
}

// rowsOf returns every row of the crew, orders, hails and beams tables,
// keyed "<table>/<key>", each row's columns printed as one string.
func rowsOf(t *testing.T, s *Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	for table, key := range map[string]string{"crew": "id", "orders": "id", "hails": "id", "beams": "uuid"} {
		rows, err := s.db.QueryContext(t.Context(), `SELECT `+key+`, * FROM `+table)
		require.NoError(t, err)
		cols, err := rows.Columns()
		require.NoError(t, err)
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			require.NoError(t, rows.Scan(ptrs...))
			out[fmt.Sprintf("%s/%v", table, vals[0])] = fmt.Sprint(vals[1:]...)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return out
}

// Every store method that changes a row of crew, orders, hails or beams
// appends its captain's log entry in the same transaction (#69): each call
// that changes rows adds exactly one entry, the one it was given, naming
// every row it changed, and a call that changes nothing adds none.
func TestChangesLogInTheSameTransaction(t *testing.T) {
	// Fixtures, logged with testEntry before the call under test.
	crew := func(status string) func(*testing.T, *Store) {
		return func(t *testing.T, s *Store) {
			require.NoError(t, s.InsertCrew(t.Context(), Crew{ID: "sorell", Name: "Sorell", Status: status}, testEntry))
		}
	}
	order := func(status, state string) func(*testing.T, *Store) {
		return func(t *testing.T, s *Store) {
			crew(status)(t, s)
			_, err := s.InsertOrder(t.Context(), Order{Title: "t", Assignee: "sorell", State: state}, testEntry)
			require.NoError(t, err)
		}
	}
	poolOrder := func(t *testing.T, s *Store) {
		crew(CrewOnDeck)(t, s)
		_, err := s.InsertOrder(t.Context(), Order{Title: "t", Division: "ops"}, testEntry)
		require.NoError(t, err)
	}
	hail := func(t *testing.T, s *Store) {
		order(CrewAway, OrderAway)(t, s)
		_, err := s.InsertHail(t.Context(), Hail{OrderID: "ORD-0001", CrewID: "sorell", Question: "?"}, testEntry)
		require.NoError(t, err)
	}
	sickbayHail := func(t *testing.T, s *Store) {
		hail(t, s)
		require.NoError(t, s.UpdateCrewStatus(t.Context(), "sorell", CrewSickbay, testEntry))
	}
	// The crew member's next tour has it away on ORD-0002 after its earlier
	// order ORD-0001 settled as state (#48).
	nextTour := func(state string) func(*testing.T, *Store) {
		return func(t *testing.T, s *Store) {
			order(CrewAway, state)(t, s)
			_, err := s.InsertOrder(t.Context(), Order{Title: "t2", Assignee: "sorell", State: OrderAway}, testEntry)
			require.NoError(t, err)
		}
	}
	// ORD-0001's hail H-0001 is left open when the order fails; the next
	// tour has the crew member hailing on ORD-0002 (#48).
	nextTourHail := func(t *testing.T, s *Store) {
		hail(t, s)
		_, _, err := s.SettleOrder(t.Context(), "ORD-0001", OrderFailed, testEntry, nil)
		require.NoError(t, err)
		require.NoError(t, s.UpdateCrewStatus(t.Context(), "sorell", CrewAway, testEntry))
		_, err = s.InsertOrder(t.Context(), Order{Title: "t2", Assignee: "sorell", State: OrderAway}, testEntry)
		require.NoError(t, err)
		_, err = s.InsertHail(t.Context(), Hail{OrderID: "ORD-0002", CrewID: "sorell", Question: "?"}, testEntry)
		require.NoError(t, err)
		c, err := s.GetCrew(t.Context(), "sorell")
		require.NoError(t, err)
		require.Equal(t, CrewHailing, c.Status)
	}
	decided := func(t *testing.T, s *Store) {
		hail(t, s)
		require.NoError(t, s.MarkHailDecided(t.Context(), "H-0001", "captain", "yes", "", testEntry))
	}
	beam := BeamRecord{UUID: "beam-1", Alias: "beam-1", CrewID: "sorell"}
	beamed := func(t *testing.T, s *Store) {
		crew(CrewOnDeck)(t, s)
		require.NoError(t, s.InsertBeam(t.Context(), beam, testEntry))
	}
	noErr := func(err error) error { return err }
	noErr2 := func(_ any, err error) error { return err }
	noErr3 := func(_, _ any, err error) error { return err }

	cases := []struct {
		name    string
		setup   func(*testing.T, *Store)
		entry   LogEntry // the entry the call passes; Text is set to name
		run     func(context.Context, *Store, LogEntry) error
		changes bool     // whether the call changes rows
		same    []string // rows ("<table>/<key>") the call must leave alone
		err     error    // the error wanted, if any
	}{
		{name: "insert crew", setup: func(*testing.T, *Store) {}, entry: LogEntry{CrewID: "sorell"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, e)
			}},
		{name: "insert crew again", setup: crew(CrewOnDeck), entry: LogEntry{CrewID: "sorell"}, err: errAny,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, e)
			}},
		{name: "insert crew within limit", setup: crew(CrewOnDeck), entry: LogEntry{CrewID: "kirk"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.InsertCrewWithin(ctx, Crew{ID: "kirk", Name: "Kirk"}, 2, e)
			}},
		{name: "insert crew over limit", setup: crew(CrewOnDeck), entry: LogEntry{CrewID: "kirk"}, err: ErrRosterFull,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.InsertCrewWithin(ctx, Crew{ID: "kirk", Name: "Kirk"}, 1, e)
			}},
		{name: "insert crew past dismissed", setup: crew(CrewDismissed), entry: LogEntry{CrewID: "kirk"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.InsertCrewWithin(ctx, Crew{ID: "kirk", Name: "Kirk"}, 1, e)
			}},
		{name: "set crew status", setup: crew(CrewOnDeck), entry: LogEntry{CrewID: "sorell"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.SetCrewStatus(ctx, "sorell", CrewOnDeck, "beam-1", e)
			}},
		{name: "set crew status unchanged", setup: crew(CrewOnDeck), entry: LogEntry{CrewID: "sorell"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.SetCrewStatus(ctx, "sorell", CrewOnDeck, "", e)
			}},
		{name: "set missing crew status", setup: crew(CrewOnDeck), entry: LogEntry{CrewID: "nobody"}, err: sql.ErrNoRows,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.SetCrewStatus(ctx, "nobody", CrewOnDeck, "", e)
			}},
		{name: "update crew status", setup: crew(CrewOnDeck), entry: LogEntry{CrewID: "sorell"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.UpdateCrewStatus(ctx, "sorell", CrewSickbay, e)
			}},
		{name: "update crew status unchanged", setup: crew(CrewSickbay), entry: LogEntry{CrewID: "sorell"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.UpdateCrewStatus(ctx, "sorell", CrewSickbay, e)
			}},
		{name: "insert order", setup: crew(CrewOnDeck), entry: LogEntry{CrewID: "sorell"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr2(s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, e))
			}},
		{name: "claim order", setup: order(CrewOnDeck, ""), entry: LogEntry{CrewID: "sorell"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr3(s.ClaimOldestDirectedOrder(ctx, "sorell", e))
			}},
		{name: "claim order already away", setup: order(CrewAway, OrderAway), entry: LogEntry{CrewID: "sorell"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr3(s.ClaimOldestDirectedOrder(ctx, "sorell", e))
			}},
		{name: "claim no order", setup: crew(CrewOnDeck), entry: LogEntry{CrewID: "sorell"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr3(s.ClaimOldestDirectedOrder(ctx, "sorell", e))
			}},
		{name: "claim pool order", setup: poolOrder, entry: LogEntry{CrewID: "sorell"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr3(s.ClaimOldestPoolOrder(ctx, "sorell", "ops", e))
			}},
		{name: "claim pool order other division", setup: poolOrder, entry: LogEntry{CrewID: "sorell"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr3(s.ClaimOldestPoolOrder(ctx, "sorell", "science", e))
			}},
		{name: "settle order", setup: order(CrewAway, OrderAway), entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr3(s.SettleOrder(ctx, "ORD-0001", OrderDone, e, nil))
			}},
		{name: "settle order settled", setup: order(CrewAway, OrderDone), entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr3(s.SettleOrder(ctx, "ORD-0001", OrderDone, e, nil))
			}},
		{name: "insert hail", setup: order(CrewAway, OrderAway), entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr2(s.InsertHail(ctx, Hail{OrderID: "ORD-0001", CrewID: "sorell", Question: "?"}, e))
			}},
		// A crew member in sickbay (or dismissed) after its tour ended
		// keeps that status when a late mission hails or is answered (#43).
		{name: "insert hail crew in sickbay", setup: order(CrewSickbay, OrderAway), entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001"}, changes: true,
			same: []string{"crew/sorell"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr2(s.InsertHail(ctx, Hail{OrderID: "ORD-0001", CrewID: "sorell", Question: "?"}, e))
			}},
		{name: "decide open hail crew in sickbay", setup: sickbayHail, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001", Payload: json.RawMessage(`{"hail_id":"H-0001"}`)}, changes: true,
			same: []string{"crew/sorell"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.DecideOpenHail(ctx, "H-0001", "captain", "yes", "", e, nil)
			}},
		// A late mission hailing or answered on an order that is no longer
		// its crew member's current one (settled, or reassigned) leaves the
		// status of the crew member's next tour alone (#48).
		{name: "insert hail order settled", setup: nextTour(OrderDone), entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001"}, changes: true,
			same: []string{"crew/sorell", "orders/ORD-0001", "orders/ORD-0002"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr2(s.InsertHail(ctx, Hail{OrderID: "ORD-0001", CrewID: "sorell", Question: "?"}, e))
			}},
		{name: "insert hail order reassigned", setup: func(t *testing.T, s *Store) {
			nextTour(OrderAway)(t, s)
			_, err := s.db.ExecContext(t.Context(), `UPDATE orders SET assignee = 'spock' WHERE id = 'ORD-0001'`)
			require.NoError(t, err)
		}, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001"}, changes: true,
			same: []string{"crew/sorell", "orders/ORD-0001", "orders/ORD-0002"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr2(s.InsertHail(ctx, Hail{OrderID: "ORD-0001", CrewID: "sorell", Question: "?"}, e))
			}},
		{name: "decide open hail order settled", setup: nextTourHail, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001", Payload: json.RawMessage(`{"hail_id":"H-0001"}`)}, changes: true,
			same: []string{"crew/sorell", "orders/ORD-0001", "orders/ORD-0002"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.DecideOpenHail(ctx, "H-0001", "captain", "yes", "", e, nil)
			}},
		{name: "mark hail decided order settled", setup: nextTourHail, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001", Payload: json.RawMessage(`{"hail_id":"H-0001"}`)}, changes: true,
			same: []string{"crew/sorell", "orders/ORD-0001", "orders/ORD-0002"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.MarkHailDecided(ctx, "H-0001", "captain", "yes", "", e)
			}},
		{name: "insert hail open", setup: hail, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001"},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr2(s.InsertHail(ctx, Hail{OrderID: "ORD-0001", CrewID: "sorell", Question: "?"}, e))
			}},
		{name: "mark hail decided", setup: hail, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001", Payload: json.RawMessage(`{"hail_id":"H-0001"}`)}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.MarkHailDecided(ctx, "H-0001", "captain", "yes", "", e)
			}},
		{name: "mark hail decided again", setup: decided, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001", Payload: json.RawMessage(`{"hail_id":"H-0001"}`)},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.MarkHailDecided(ctx, "H-0001", "captain", "yes", "", e)
			}},
		{name: "decide open hail", setup: hail, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001", Payload: json.RawMessage(`{"hail_id":"H-0001"}`)}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.DecideOpenHail(ctx, "H-0001", "captain", "yes", "", e, nil)
			}},
		{name: "decide closed hail", setup: decided, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001", Payload: json.RawMessage(`{"hail_id":"H-0001"}`)}, err: ErrHailClosed,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return s.DecideOpenHail(ctx, "H-0001", "captain", "yes", "", e, nil)
			}},
		{name: "time out hail", setup: hail, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001", Payload: json.RawMessage(`{"hail_id":"H-0001"}`)}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr2(s.TimeOutOpenHail(ctx, "H-0001", e))
			}},
		{name: "time out closed hail", setup: decided, entry: LogEntry{CrewID: "sorell", OrderID: "ORD-0001", Payload: json.RawMessage(`{"hail_id":"H-0001"}`)},
			run: func(ctx context.Context, s *Store, e LogEntry) error {
				return noErr2(s.TimeOutOpenHail(ctx, "H-0001", e))
			}},
		{name: "insert beam", setup: crew(CrewOnDeck), entry: LogEntry{CrewID: "sorell"}, changes: true,
			run: func(ctx context.Context, s *Store, e LogEntry) error { return noErr(s.InsertBeam(ctx, beam, e)) }},
		{name: "insert beam again", setup: beamed, entry: LogEntry{CrewID: "sorell"},
			run: func(ctx context.Context, s *Store, e LogEntry) error { return noErr(s.InsertBeam(ctx, beam, e)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			s := openTest(t)
			tc.setup(t, s)
			before := rowsOf(t, s)
			logged, err := s.ListLog(ctx, 0)
			require.NoError(t, err)
			var last int64
			if len(logged) > 0 {
				last = logged[len(logged)-1].ID
			}

			e := tc.entry
			e.Kind, e.Text = "change", tc.name
			err = tc.run(ctx, s, e)
			switch {
			case tc.err == errAny:
				require.Error(t, err)
			case tc.err != nil:
				require.ErrorIs(t, err, tc.err)
			default:
				require.NoError(t, err)
			}

			after := rowsOf(t, s)
			var changed []string
			for k, v := range after {
				if before[k] != v {
					changed = append(changed, k)
				}
			}
			for k := range before {
				if _, ok := after[k]; !ok {
					changed = append(changed, k)
				}
			}
			entries, err := s.ListLog(ctx, last)
			require.NoError(t, err)
			if !tc.changes {
				require.Empty(t, changed, "a no-op changes no row")
				require.Empty(t, entries, "a no-op writes no log entry")
				return
			}
			require.NotEmpty(t, changed)
			for _, k := range tc.same {
				require.NotContains(t, changed, k)
				require.Contains(t, after, k)
			}
			require.Len(t, entries, 1, "one entry per change")
			got := entries[0]
			require.Equal(t, "change", got.Kind)
			require.Equal(t, tc.name, got.Text)
			var payload map[string]any
			require.NoError(t, json.Unmarshal(got.Payload, &payload))
			for _, k := range changed {
				table, id, _ := strings.Cut(k, "/")
				switch table {
				case "crew":
					require.Equal(t, id, got.CrewID, "entry names crew row %s", k)
				case "orders":
					require.Equal(t, id, got.OrderID, "entry names order row %s", k)
				case "hails":
					h, err := s.GetHail(ctx, id)
					require.NoError(t, err)
					require.Equal(t, id, payload["hail_id"], "entry names hail row %s", k)
					require.Equal(t, h.OrderID, got.OrderID, "entry names hail row %s's order", k)
				case "beams":
					var owner string
					require.NoError(t, s.db.QueryRowContext(ctx, `SELECT crew_id FROM beams WHERE uuid = ?`, id).Scan(&owner))
					require.Equal(t, owner, got.CrewID, "entry names beam row %s's crew", k)
				}
			}
		})
	}
}

// errAny marks a case of TestChangesLogInTheSameTransaction that must fail,
// with any error.
var errAny = errors.New("any error")

// SetSetting logs a change in the same transaction and nothing for a
// setting already at the value.
func TestSettings(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)

	_, err := s.GetSetting(ctx, "standing_orders")
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, s.SetSetting(ctx, "standing_orders", "on", LogEntry{Kind: "bridge", Text: "on"}))
	require.NoError(t, s.SetSetting(ctx, "standing_orders", "on", LogEntry{Kind: "bridge", Text: "on again"}))
	require.NoError(t, s.SetSetting(ctx, "standing_orders", "off", LogEntry{Kind: "bridge", Text: "off"}))
	v, err := s.GetSetting(ctx, "standing_orders")
	require.NoError(t, err)
	require.Equal(t, "off", v)

	entries, err := s.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "on", entries[0].Text)
	require.Equal(t, "off", entries[1].Text)
}

// A database's nonce is made once and kept across opens; another
// database gets another (#206).
func TestNonce(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), DBFile)
	s, err := Open(t.Context(), path)
	require.NoError(t, err)
	nonce := s.Nonce()
	require.Regexp(t, `^[0-9a-f]{16}$`, nonce)
	require.NoError(t, s.Close())
	s, err = Open(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, nonce, s.Nonce(), "kept across a restart")
	require.NoError(t, s.Close())
	require.NotEqual(t, nonce, openTest(t).Nonce(), "a fresh database")
}
