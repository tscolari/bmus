package store

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// RecallOrder (#101) moves an away order of its assignee to the pool,
// parked and unassigned, once; the pool claim then reports it parked.
func TestRecallOrder(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Division: "ops"}, testEntry))
	id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell", Division: "ops"}, testEntry)
	require.NoError(t, err)

	state, recalled, err := s.RecallOrder(ctx, id, "sorell", LogEntry{Kind: "order", Text: "recalled"})
	require.NoError(t, err)
	require.False(t, recalled, "directed, not away")
	require.Equal(t, OrderDirected, state)
	_, _, err = s.ClaimOldestDirectedOrder(ctx, "sorell", testEntry)
	require.NoError(t, err)
	state, recalled, err = s.RecallOrder(ctx, id, "tuvok", LogEntry{Kind: "order", Text: "recalled"})
	require.NoError(t, err)
	require.False(t, recalled, "not tuvok's")
	require.Equal(t, OrderAway, state)
	for i := range 2 {
		state, recalled, err = s.RecallOrder(ctx, id, "sorell", LogEntry{Kind: "order", Text: "recalled"})
		require.NoError(t, err)
		require.Equal(t, i == 0, recalled)
		require.Equal(t, OrderPool, state)
	}
	o, err := s.GetOrder(ctx, id)
	require.NoError(t, err)
	require.Empty(t, o.Assignee)
	require.NotNil(t, o.ParkedAt)
	_, _, err = s.RecallOrder(ctx, "ORD-9999", "sorell", testEntry)
	require.ErrorIs(t, err, sql.ErrNoRows)

	got, found, err := s.ClaimOldestPoolOrder(ctx, "sorell", "ops", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, id, got)
	o, err = s.GetOrder(ctx, id)
	require.NoError(t, err)
	require.Equal(t, OrderAway, o.State)
	require.NotNil(t, o.ParkedAt, "the claimer resumes the parked mission")
	require.Equal(t, []string{"recalled"}, texts(t, s))
}

// BelayOrder and SettleOpenOrder (#101) settle an away order, or a parked
// unclaimed one, reporting its assignee; withdraw takes the other unclaimed
// orders.
func TestBelayOrder(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	away, err := s.InsertOrder(ctx, Order{Title: "a", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)
	pool, err := s.InsertOrder(ctx, Order{Title: "p", Division: "ops"}, testEntry)
	require.NoError(t, err)

	for i := range 2 {
		state, assignee, belayed, err := s.BelayOrder(ctx, away, LogEntry{Kind: "order", Text: "belayed"}, nil)
		require.NoError(t, err)
		require.Equal(t, i == 0, belayed)
		require.Equal(t, OrderBelayed, state)
		require.Equal(t, "sorell", assignee)
	}
	state, _, belayed, err := s.BelayOrder(ctx, pool, LogEntry{Kind: "order", Text: "belayed pool"}, nil)
	require.NoError(t, err)
	require.False(t, belayed, "no parked mission to tell")
	require.Equal(t, OrderPool, state)
	_, err = s.db.ExecContext(ctx, `UPDATE orders SET parked_at = CURRENT_TIMESTAMP WHERE id = ?`, pool)
	require.NoError(t, err)
	state, assignee, failed, err := s.SettleOpenOrder(ctx, pool, OrderFailed, LogEntry{Kind: "fail", Text: "failed pool"})
	require.NoError(t, err)
	require.True(t, failed)
	require.Equal(t, OrderFailed, state)
	require.Empty(t, assignee)
	o, err := s.GetOrder(ctx, pool)
	require.NoError(t, err)
	require.Nil(t, o.ParkedAt)
	_, _, _, err = s.BelayOrder(ctx, "ORD-9999", testEntry, nil)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Equal(t, []string{"belayed", "failed pool"}, texts(t, s))
}

// WithdrawOrder (#101) takes an unclaimed order without a parked mission,
// once.
func TestWithdrawOrder(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	directed, err := s.InsertOrder(ctx, Order{Title: "d", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	parked, err := s.InsertOrder(ctx, Order{Title: "p", Division: "ops"}, testEntry)
	require.NoError(t, err)
	away, err := s.InsertOrder(ctx, Order{Title: "a", Assignee: "sorell", State: OrderAway}, testEntry)
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE orders SET parked_at = CURRENT_TIMESTAMP WHERE id = ?`, parked)
	require.NoError(t, err)

	for i := range 2 {
		o, withdrawn, err := s.WithdrawOrder(ctx, directed, LogEntry{Kind: "order", Text: "withdrawn"})
		require.NoError(t, err)
		require.Equal(t, i == 0, withdrawn)
		require.Equal(t, OrderWithdrawn, o.State)
	}
	o, withdrawn, err := s.WithdrawOrder(ctx, parked, testEntry)
	require.NoError(t, err)
	require.False(t, withdrawn)
	require.Equal(t, OrderPool, o.State)
	require.NotNil(t, o.ParkedAt)
	o, withdrawn, err = s.WithdrawOrder(ctx, away, testEntry)
	require.NoError(t, err)
	require.False(t, withdrawn)
	require.Equal(t, OrderAway, o.State)
	_, _, err = s.WithdrawOrder(ctx, "ORD-9999", testEntry)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Equal(t, []string{"withdrawn"}, texts(t, s))
}

// RouteOrder (#101) moves an unclaimed order between crew members and
// pools, keeping parked_at; a route that changes nothing, a refused one and
// a claimed order write nothing.
func TestRouteOrder(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "tuvok", Name: "Tuvok", Status: CrewSickbay}, testEntry))
	id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)
	str := func(v string) *string { return &v }
	var seen []string
	entry := func(text string) RouteFunc {
		return func(o Order, to, div string, c *Crew) (LogEntry, error) {
			status := "-"
			if c != nil {
				status = c.Status
			}
			seen = append(seen, o.State+" "+to+" "+div+" "+status)
			return LogEntry{Kind: "order", Text: text}, nil
		}
	}

	o, routed, err := s.RouteOrder(ctx, id, str(""), str("ops"), entry("to the pool"))
	require.NoError(t, err)
	require.True(t, routed)
	require.Equal(t, []string{OrderPool, "", "ops"}, []string{o.State, o.Assignee, o.Division})
	o, routed, err = s.RouteOrder(ctx, id, nil, str("ops"), entry("again"))
	require.NoError(t, err)
	require.False(t, routed, "nothing changes")
	require.Equal(t, OrderPool, o.State)
	_, err = s.db.ExecContext(ctx, `UPDATE orders SET parked_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
	require.NoError(t, err)
	o, routed, err = s.RouteOrder(ctx, id, str("tuvok"), nil, entry("to tuvok"))
	require.NoError(t, err)
	require.True(t, routed)
	require.Equal(t, []string{OrderDirected, "tuvok", "ops"}, []string{o.State, o.Assignee, o.Division})
	require.NotNil(t, o.ParkedAt, "a routed recalled order keeps its parked mission")
	refused := errors.New("refused")
	_, _, err = s.RouteOrder(ctx, id, str(""), nil, func(Order, string, string, *Crew) (LogEntry, error) {
		return LogEntry{}, refused
	})
	require.ErrorIs(t, err, refused)

	_, err = s.db.ExecContext(ctx, `UPDATE orders SET state = ? WHERE id = ?`, OrderAway, id)
	require.NoError(t, err)
	o, routed, err = s.RouteOrder(ctx, id, str("sorell"), nil, entry("never"))
	require.NoError(t, err)
	require.False(t, routed)
	require.Equal(t, OrderAway, o.State)
	_, _, err = s.RouteOrder(ctx, "ORD-9999", nil, nil, entry("never"))
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Equal(t, []string{"directed  ops -", "pool  ops -", "pool tuvok ops sickbay"}, seen)
	require.Equal(t, []string{"to the pool", "to tuvok"}, texts(t, s))
}

// RequestOrder (#101) logs only a request its check accepts, and hands the
// check the assignee's status.
func TestRequestOrder(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Status: CrewSickbay}, testEntry))
	id, err := s.InsertOrder(ctx, Order{Title: "t", Assignee: "sorell"}, testEntry)
	require.NoError(t, err)

	refused := errors.New("refused")
	err = s.RequestOrder(ctx, id, func(tx *sql.Tx, o Order, status string) (LogEntry, error) {
		return LogEntry{Kind: "order", Text: "never"}, refused
	})
	require.ErrorIs(t, err, refused)
	var seen string
	require.NoError(t, s.RequestOrder(ctx, id, func(tx *sql.Tx, o Order, status string) (LogEntry, error) {
		seen = o.ID + " " + o.Assignee + " " + status
		return LogEntry{Kind: "order", OrderID: o.ID, Text: "requested"}, nil
	}))
	require.Equal(t, id+" sorell sickbay", seen)
	err = s.RequestOrder(ctx, "ORD-9999", func(*sql.Tx, Order, string) (LogEntry, error) { return testEntry, nil })
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Equal(t, []string{"requested"}, texts(t, s))
}
