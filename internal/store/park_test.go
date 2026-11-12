package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// texts returns the texts of entries not logged with testEntry.
func texts(t *testing.T, s *Store) []string {
	t.Helper()
	entries, err := s.ListLog(t.Context(), 0)
	require.NoError(t, err)
	var out []string
	for _, e := range withoutTestEntries(entries) {
		out = append(out, e.Text)
	}
	return out
}

// A checkpoint's seq restarts on each beam (#100): the same seq from two
// beams are two checkpoints, LatestCheckpoint goes by insertion order and
// CheckpointsNewestFirst lists them newest first.
func TestCheckpointsPerBeam(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	id, err := s.InsertOrder(ctx, Order{Title: "t"}, testEntry)
	require.NoError(t, err)
	_, found, err := s.LatestCheckpoint(ctx, id)
	require.NoError(t, err)
	require.False(t, found)

	a := Checkpoint{OrderID: id, Beam: "b1", Seq: 5, TranscriptPath: "t1", HandoverPath: "h1",
		Repos: []CheckpointRepo{{Name: "app", Commit: "c1", BundlePath: "x1"}}}
	b := Checkpoint{OrderID: id, Beam: "b2", Seq: 2, TranscriptPath: "t2", HandoverPath: "h2",
		Repos: []CheckpointRepo{{Name: "app", Commit: "c2", BundlePath: "x2"}}}
	c := Checkpoint{OrderID: id, Beam: "b2", Seq: 5, TranscriptPath: "t3", HandoverPath: "h3",
		Repos: []CheckpointRepo{{Name: "app", Commit: "c3", BundlePath: "x3"}}}
	for _, cp := range []Checkpoint{a, b, c} {
		require.NoError(t, s.InsertCheckpoint(ctx, cp))
	}
	require.NoError(t, s.InsertCheckpoint(ctx, c), "a re-run")
	other := c
	other.HandoverPath = "elsewhere"
	require.ErrorIs(t, s.InsertCheckpoint(ctx, other), ErrCheckpointConflict)

	latest, found, err := s.LatestCheckpoint(ctx, id)
	require.NoError(t, err)
	require.True(t, found)
	latest.CreatedAt = c.CreatedAt
	require.Equal(t, c, latest)
	all, err := s.CheckpointsNewestFirst(ctx, id)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, []string{"c3", "c2", "c1"}, []string{all[0].Repos[0].Commit, all[1].Repos[0].Commit, all[2].Repos[0].Commit})
}

// A database made before the checkpoints were keyed by beam (#100) is
// refused, naming the file to delete.
func TestOpenRejectsCheckpointsWithoutBeam(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFile)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE checkpoints (order_id TEXT NOT NULL, seq INTEGER NOT NULL,
		transcript_path TEXT NOT NULL DEFAULT '', handover_path TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL, PRIMARY KEY (order_id, seq))`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = Open(t.Context(), path)
	require.ErrorContains(t, err, "predates the beam-keyed checkpoints (#100); delete "+path)
}

// ParkOrder and UnparkOrder (#100): each writes and logs once, park only an
// open order, unpark only for the assignee of an away order; SettleOrder
// clears parked_at; the claim of a parked order sets its on-deck crew member
// away.
func TestParkAndUnpark(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)

	open, err := s.ParkOrder(ctx, id, LogEntry{Kind: "order", OrderID: id, Text: "parked"})
	require.NoError(t, err)
	require.False(t, open, "a directed order isn't open")
	got, found, err := s.ClaimOldestDirectedOrder(ctx, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, id, got)

	for range 2 {
		open, err = s.ParkOrder(ctx, id, LogEntry{Kind: "order", OrderID: id, Text: "parked"})
		require.NoError(t, err)
		require.True(t, open)
	}
	o, err := s.GetOrder(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, o.ParkedAt)

	// A new tour's crew member, on deck, claims the parked order: away,
	// logged once.
	require.NoError(t, s.UpdateCrewStatus(ctx, "sorell", CrewOnDeck, testEntry))
	for range 2 {
		got, found, err = s.ClaimOldestDirectedOrder(ctx, "sorell", LogEntry{Kind: "order", Text: "claimed again"})
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, id, got)
	}
	c, err := s.GetCrew(ctx, "sorell")
	require.NoError(t, err)
	require.Equal(t, CrewAway, c.Status)

	resumed, err := s.UnparkOrder(ctx, id, "tuvok", LogEntry{Kind: "order", Text: "resumed"})
	require.NoError(t, err)
	require.False(t, resumed, "not tuvok's")
	for range 2 {
		resumed, err = s.UnparkOrder(ctx, id, "sorell", LogEntry{Kind: "order", Text: "resumed"})
		require.NoError(t, err)
		require.True(t, resumed, "a re-run reads resumed too")
	}
	o, err = s.GetOrder(ctx, id)
	require.NoError(t, err)
	require.Nil(t, o.ParkedAt)

	_, err = s.ParkOrder(ctx, id, LogEntry{Kind: "order", Text: "parked again"})
	require.NoError(t, err)
	_, settled, err := s.SettleOrder(ctx, id, OrderDone, LogEntry{Kind: "done", Text: "done"}, nil)
	require.NoError(t, err)
	require.True(t, settled)
	o, err = s.GetOrder(ctx, id)
	require.NoError(t, err)
	require.Nil(t, o.ParkedAt, "settling clears parked_at")
	resumed, err = s.UnparkOrder(ctx, id, "sorell", testEntry)
	require.NoError(t, err)
	require.False(t, resumed)
	open, err = s.ParkOrder(ctx, id, testEntry)
	require.NoError(t, err)
	require.False(t, open)

	require.Equal(t, []string{"parked", "claimed again", "resumed", "parked again", "done"}, texts(t, s))
}

// The backstop's SettleAssignedOrder leaves an order that moved to another
// crew member, or whose mission is parked, alone.
func TestSettleAssignedOrder(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)

	state, settled, err := s.SettleAssignedOrder(ctx, id, "tuvok", OrderFailed, testEntry, true)
	require.NoError(t, err)
	require.False(t, settled)
	require.Equal(t, OrderAway, state)
	_, err = s.ParkOrder(ctx, id, testEntry)
	require.NoError(t, err)
	_, settled, err = s.SettleAssignedOrder(ctx, id, "sorell", OrderFailed, testEntry, false)
	require.NoError(t, err)
	require.False(t, settled, "parked")
	_, err = s.db.ExecContext(ctx, `UPDATE orders SET parked_at = NULL WHERE id = ?`, id)
	require.NoError(t, err)
	state, settled, err = s.SettleAssignedOrder(ctx, id, "sorell", OrderFailed, testEntry, false)
	require.NoError(t, err)
	require.True(t, settled)
	require.Equal(t, OrderFailed, state)
	_, _, err = s.SettleAssignedOrder(ctx, id, "", OrderFailed, testEntry, false)
	require.Error(t, err)

	// A mission known to be over: its parked order fails all the same.
	dead, err := s.InsertOrder(ctx, Order{Title: "d", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)
	_, err = s.ParkOrder(ctx, dead, testEntry)
	require.NoError(t, err)
	state, settled, err = s.SettleAssignedOrder(ctx, dead, "sorell", OrderFailed, testEntry, true)
	require.NoError(t, err)
	require.True(t, settled)
	require.Equal(t, OrderFailed, state)
}

// NextTour moves the tour on once and clears the beam; DismissCrew and
// SetCrewStatusUnlessDismissed (#100). DismissCrew returns the tour_no it
// read, and NextTour on a dismissed crew member reports no change (#113).
func TestNextTourAndDismiss(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Status: CrewAway, CurrentBeam: "b1"}, testEntry))
	for i := range 2 {
		changed, err := s.NextTour(ctx, "sorell", 0, LogEntry{Kind: "crew", Text: "next"})
		require.NoError(t, err)
		require.Equal(t, i == 0, changed)
	}
	c, err := s.GetCrew(ctx, "sorell")
	require.NoError(t, err)
	require.Equal(t, 1, c.TourNo)
	require.Empty(t, c.CurrentBeam)
	require.Equal(t, CrewBeamingIn, c.Status, "the new tour provisions a beam (#106)")
	_, err = s.NextTour(ctx, "nobody", 0, testEntry)
	require.ErrorIs(t, err, sql.ErrNoRows)

	parked, err := s.InsertOrder(ctx, Order{Title: "p", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)
	_, err = s.InsertOrder(ctx, Order{Title: "q", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)
	_, err = s.ParkOrder(ctx, parked, testEntry)
	require.NoError(t, err)
	var told []string
	for i := range 2 {
		changed, tourNo, err := s.DismissCrew(ctx, "sorell", LogEntry{Kind: "crew", Text: "dismissed"}, func(tx *sql.Tx, order string) error {
			told = append(told, order)
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, i == 0, changed)
		if changed {
			require.Equal(t, 1, tourNo, "the tour_no NextTour set")
		}
	}
	require.Equal(t, []string{parked}, told, "only the parked order, once")
	_, _, err = s.DismissCrew(ctx, "nobody", testEntry, nil)
	require.ErrorIs(t, err, sql.ErrNoRows)
	changed, err := s.NextTour(ctx, "sorell", 1, testEntry)
	require.NoError(t, err)
	require.False(t, changed, "dismissed: no next tour")
	c, err = s.GetCrew(ctx, "sorell")
	require.NoError(t, err)
	require.Equal(t, 1, c.TourNo)

	dismissed, err := s.SetCrewStatusUnlessDismissed(ctx, "sorell", CrewOnDeck, "b2", LogEntry{Kind: "beam", Text: "up"})
	require.NoError(t, err)
	require.True(t, dismissed)
	c, err = s.GetCrew(ctx, "sorell")
	require.NoError(t, err)
	require.Equal(t, CrewDismissed, c.Status)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "tuvok", Name: "Tuvok"}, testEntry))
	dismissed, err = s.SetCrewStatusUnlessDismissed(ctx, "tuvok", CrewOnDeck, "b3", LogEntry{Kind: "beam", Text: "up"})
	require.NoError(t, err)
	require.False(t, dismissed)
	require.Equal(t, []string{"next", "dismissed", "up"}, texts(t, s))
}

// #106: a crew member beaming in claims nothing, directed, pool or its
// parked order already away, and writes nothing; once its hello moves it
// on deck (UpdateCrewStatusFrom), it claims. A dismissal while it beams in
// stands: the hello's update leaves it dismissed.
func TestBeamingInClaimsNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Division: "science", Status: CrewBeamingIn}, testEntry))
	directed, err := s.InsertOrder(ctx, Order{Title: "d", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	pool, err := s.InsertOrder(ctx, Order{Title: "p", Division: "science"}, testEntry)
	require.NoError(t, err)

	claim := func(want string) {
		t.Helper()
		id, found, err := s.ClaimOldestDirectedOrder(ctx, "sorell", LogEntry{Kind: "order", Text: "claimed"})
		require.NoError(t, err)
		if found {
			require.Equal(t, want, id)
			return
		}
		id, found, err = s.ClaimOldestPoolOrder(ctx, "sorell", "science", LogEntry{Kind: "order", Text: "claimed"})
		require.NoError(t, err)
		require.Equal(t, want != "", found)
		require.Equal(t, want, id)
	}
	claim("")
	for _, id := range []string{directed, pool} {
		o, err := s.GetOrder(ctx, id)
		require.NoError(t, err)
		require.NotEqual(t, OrderAway, o.State, id)
	}
	c, err := s.GetCrew(ctx, "sorell")
	require.NoError(t, err)
	require.Equal(t, CrewBeamingIn, c.Status)
	require.Empty(t, texts(t, s), "nothing logged")

	// Its order already away (parked by a watch change) isn't taken up
	// either.
	away, err := s.InsertOrder(ctx, Order{Title: "a", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)
	claim("")
	require.Equal(t, CrewBeamingIn, getCrewStatus(t, s, "sorell"))

	changed, err := s.UpdateCrewStatusFrom(ctx, "sorell", CrewBeamingIn, CrewOnDeck, LogEntry{Kind: "crew", Text: "on deck"})
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = s.UpdateCrewStatusFrom(ctx, "sorell", CrewBeamingIn, CrewOnDeck, LogEntry{Kind: "crew", Text: "on deck"})
	require.NoError(t, err)
	require.False(t, changed, "a re-run writes nothing")
	claim(away)
	require.Equal(t, CrewAway, getCrewStatus(t, s, "sorell"))
	require.Equal(t, []string{"on deck", "claimed"}, texts(t, s))

	// Dismissed while beaming in: the hello moves nothing.
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "tuvok", Name: "Tuvok", Status: CrewBeamingIn}, testEntry))
	_, _, err = s.DismissCrew(ctx, "tuvok", testEntry, nil)
	require.NoError(t, err)
	changed, err = s.UpdateCrewStatusFrom(ctx, "tuvok", CrewBeamingIn, CrewOnDeck, testEntry)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, CrewDismissed, getCrewStatus(t, s, "tuvok"))
	_, err = s.UpdateCrewStatusFrom(ctx, "nobody", CrewBeamingIn, CrewOnDeck, testEntry)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func getCrewStatus(t *testing.T, s *Store, id string) string {
	t.Helper()
	c, err := s.GetCrew(t.Context(), id)
	require.NoError(t, err)
	return c.Status
}
