package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// SetOrderProgress (#141) keeps an order's latest report: a later seq of
// the same tour or any seq of a later tour replaces it, an older one, a
// report from someone else's beam or one while parked doesn't. Leaving away
// and hailing (recall, settle, belay) clears it; parking keeps it.
func TestOrderProgress(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Division: "ops"}, testEntry))
	id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell", Division: "ops"}, testEntry)
	require.NoError(t, err)

	report := func(crew string, tour int, seq uint64, pct float64, note string) bool {
		t.Helper()
		var changed bool
		recorded, err := s.RecordInbox(ctx, crew, tour, seq, func(tx InboxTx) (err error) {
			changed, err = tx.SetOrderProgress(id, pct, note)
			return err
		})
		require.NoError(t, err)
		require.True(t, recorded)
		return changed
	}
	progress := func() (*float64, string) {
		t.Helper()
		o, err := s.GetOrder(ctx, id)
		require.NoError(t, err)
		return o.ProgressPct, o.ProgressNote
	}
	requireProgress := func(pct float64, note string) {
		t.Helper()
		got, gotNote := progress()
		require.NotNil(t, got)
		require.InDelta(t, pct, *got, 0)
		require.Equal(t, note, gotNote)
	}
	requireNone := func() {
		t.Helper()
		got, note := progress()
		require.Nil(t, got)
		require.Empty(t, note)
	}

	require.False(t, report("sorell", 1, 1, 5, "early"), "directed, not away yet")
	requireNone()
	_, _, err = s.ClaimOldestDirectedOrder(ctx, "sorell", testEntry)
	require.NoError(t, err)

	require.True(t, report("sorell", 1, 3, 30, "surveying"))
	requireProgress(30, "surveying")
	require.False(t, report("sorell", 1, 2, 20, "stale"), "an older seq of the same tour")
	requireProgress(30, "surveying")
	require.False(t, report("tuvok", 1, 4, 90, "not mine"), "another crew member's beam")
	require.True(t, report("sorell", 1, 5, 0, ""), "0% with no note is a report")
	requireProgress(0, "")

	hail, err := s.InsertHail(ctx, Hail{OrderID: id, CrewID: "sorell", Question: "q"}, testEntry)
	require.NoError(t, err)
	require.True(t, report("sorell", 1, 6, 40, "hailing"))
	requireProgress(40, "hailing")

	// Parked (its hail closed first, #134): the report stays, and a late
	// one from the parked beam is dropped; the resuming tour's reports
	// replace it from seq 1.
	_, err = s.TimeOutOpenHail(ctx, hail, testEntry)
	require.NoError(t, err)
	_, err = s.ParkOrder(ctx, id, testEntry)
	require.NoError(t, err)
	requireProgress(40, "hailing")
	require.False(t, report("sorell", 1, 7, 50, "while parked"))
	_, err = s.UnparkOrder(ctx, id, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, report("sorell", 2, 1, 45, "resumed"))
	requireProgress(45, "resumed")
	require.False(t, report("sorell", 1, 8, 60, "old tour"), "an earlier tour's report")
	requireProgress(45, "resumed")

	// Recalled: cleared, and the next claim starts with none.
	_, recalled, err := s.RecallOrder(ctx, id, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, recalled)
	requireNone()
	_, found, err := s.ClaimOldestPoolOrder(ctx, "sorell", "ops", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	requireNone()
	_, err = s.UnparkOrder(ctx, id, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, report("sorell", 2, 2, 10, "again"))
	requireProgress(10, "again")

	// Settled: cleared, and later reports are dropped.
	_, settled, err := s.SettleOrder(ctx, id, OrderDone, testEntry, nil)
	require.NoError(t, err)
	require.True(t, settled)
	requireNone()
	require.False(t, report("sorell", 2, 3, 100, "late"))
	requireNone()
	all, err := s.ListOrders(ctx)
	require.NoError(t, err)
	require.Nil(t, all[0].ProgressPct)
}

// Belaying (SettleOpenOrder) clears the progress too, and ListOrders
// carries it.
func TestOrderProgressBelayAndList(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	_, _, err = s.ClaimOldestDirectedOrder(ctx, "sorell", testEntry)
	require.NoError(t, err)
	_, err = s.RecordInbox(ctx, "sorell", 0, 1, func(tx InboxTx) error {
		_, err := tx.SetOrderProgress(id, 70, "nearly")
		return err
	})
	require.NoError(t, err)
	all, err := s.ListOrders(ctx)
	require.NoError(t, err)
	require.NotNil(t, all[0].ProgressPct)
	require.InDelta(t, 70, *all[0].ProgressPct, 0)
	require.Equal(t, "nearly", all[0].ProgressNote)

	_, _, belayed, err := s.BelayOrder(ctx, id, testEntry, nil)
	require.NoError(t, err)
	require.True(t, belayed)
	o, err := s.GetOrder(ctx, id)
	require.NoError(t, err)
	require.Nil(t, o.ProgressPct)
	require.Empty(t, o.ProgressNote)
}
