package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #156's acceptance: a week of hails decided by policy (auto_allow, a
// malformed split's deny, a captain timeout's default_option), Command and
// the captain, two of them countermanded, one closed unanswered and one
// still open. The share, the override rate and the percentiles match the
// hand-computed values. A hail with no created_at (from before #147) and
// one created a nanosecond before since are left out; one created at since
// counts.
func TestHailMetrics(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, testEntry))
	orderID, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	since := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	window := time.Now().Add(time.Hour) // countermand_until, still open
	hail := func(created time.Time) string {
		t.Helper()
		id, err := s.InsertHail(ctx, Hail{OrderID: orderID, CrewID: "sorell", Question: "?", Options: []string{"allow", "deny"}},
			LogEntry{TS: created, Kind: "test", Text: "hailed"})
		require.NoError(t, err)
		return id
	}
	decide := func(created time.Time, after time.Duration, dec HailDecision) string {
		t.Helper()
		id := hail(created)
		require.NoError(t, s.DecideOpenHailAs(ctx, id, dec, LogEntry{TS: created.Add(after), Kind: "test", Text: "decided"}, nil))
		return id
	}
	countermand := func(id string) {
		t.Helper()
		require.NoError(t, s.CountermandHail(ctx, id, "deny", "no", testEntry, nil))
	}
	day := func(n int) time.Time { return since.Add(time.Duration(n) * 24 * time.Hour) }

	// Left out: before since, and no created_at.
	decide(since.Add(-time.Nanosecond), time.Hour, HailDecision{DecidedBy: "captain", Decision: "allow"})
	old := decide(day(1), time.Hour, HailDecision{DecidedBy: "captain", Decision: "allow"})
	_, err = s.db.ExecContext(ctx, `UPDATE hails SET created_at = NULL WHERE id = ?`, old)
	require.NoError(t, err)

	// Counted: eight decided, by time-to-answer 1s, 2s, 30s, 60s, 90s, 5m,
	// 10m, 20m.
	decide(since, time.Second, HailDecision{DecidedBy: "policy", Decision: "allow", Route: "auto"})
	decide(day(1), 2*time.Second, HailDecision{DecidedBy: "policy", Decision: "invalid order split: no children", Route: "auto"})
	countermand(decide(day(2), 10*time.Minute, HailDecision{DecidedBy: "policy", Decision: "allow", CountermandUntil: &window}))
	countermand(decide(day(3), 30*time.Second, HailDecision{DecidedBy: "command", Decision: "allow", Route: "command",
		CountermandUntil: &window}))
	decide(day(4), time.Minute, HailDecision{DecidedBy: "command", Decision: "allow", Route: "command", CountermandUntil: &window})
	decide(day(5), 90*time.Second, HailDecision{DecidedBy: "command", Decision: "deny", Route: "command"})
	decide(day(5).Add(time.Hour), 5*time.Minute, HailDecision{DecidedBy: "captain", Decision: "deny"})
	decide(day(6), 20*time.Minute, HailDecision{DecidedBy: "captain", Decision: "allow"})
	// Closed unanswered, and open: in the total only.
	unanswered := hail(day(6).Add(time.Hour))
	closed, err := s.TimeOutOpenHail(ctx, unanswered, testEntry)
	require.NoError(t, err)
	require.True(t, closed)
	hail(day(6).Add(2 * time.Hour))

	m, err := s.HailMetrics(ctx, since)
	require.NoError(t, err)
	require.Equal(t, HailMetrics{
		Total: 10, Decided: 8, Countermanded: 2,
		ByOriginalDecider:  map[string]int{"policy": 3, "command": 3, "captain": 2},
		MedianTimeToAnswer: time.Minute,      // rank ceil(0.5*8) = 4
		P90TimeToAnswer:    20 * time.Minute, // rank ceil(0.9*8) = 8
	}, m)
	share, ok := m.AutoResolvedShare()
	require.True(t, ok)
	require.InDelta(t, 0.75, share, 1e-9) // (3 + 3) / 8
	rate, ok := m.CaptainOverrides()
	require.True(t, ok)
	require.InDelta(t, 0.25, rate, 1e-9) // 2 / 8

	// since in another zone is the same instant.
	again, err := s.HailMetrics(ctx, since.In(time.FixedZone("x", -7*3600)))
	require.NoError(t, err)
	require.Equal(t, m, again)

	// A period with no hails: zeros, and no rates.
	none, err := s.HailMetrics(ctx, day(30))
	require.NoError(t, err)
	require.Equal(t, HailMetrics{ByOriginalDecider: map[string]int{"policy": 0, "command": 0, "captain": 0}}, none)
	_, ok = none.AutoResolvedShare()
	require.False(t, ok)
	_, ok = none.CaptainOverrides()
	require.False(t, ok)
}

func TestNearestRank(t *testing.T) {
	ds := func(ns ...int) []time.Duration {
		out := make([]time.Duration, len(ns))
		for i, n := range ns {
			out[i] = time.Duration(n)
		}
		return out
	}
	for _, c := range []struct {
		sorted []time.Duration
		p      int
		want   time.Duration
	}{
		{nil, 50, 0},
		{ds(7), 50, 7},
		{ds(7), 90, 7},
		{ds(1, 2), 50, 1},
		{ds(1, 2), 90, 2},
		{ds(1, 2, 3), 50, 2},
		{ds(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 90, 9},
		{ds(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11), 90, 10},
		{ds(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 100, 10},
	} {
		require.Equal(t, c.want, nearestRank(c.sorted, c.p), "p%d of %v", c.p, c.sorted)
	}
}
