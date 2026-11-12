package store

import (
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ids returns the ids of entries.
func ids(entries []LogEntry) []int64 {
	out := []int64{}
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

// The log hook gets each commit's entries once, after the commit, and
// nothing for a rollback, a no-op or what was logged before it was set.
func TestLogHook(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	_, err := s.AppendLog(ctx, LogEntry{Kind: "bridge", Text: "before the hook"})
	require.NoError(t, err)

	var (
		mu    sync.Mutex
		calls [][]LogEntry
	)
	require.NoError(t, s.SetLogHook(ctx, func(entries []LogEntry) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, entries)
	}, nil))
	got := func() [][]LogEntry {
		mu.Lock()
		defer mu.Unlock()
		out := calls
		calls = nil
		return out
	}

	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell"}, LogEntry{Kind: "crew", CrewID: "sorell", Text: "added"}))
	c := got()
	require.Len(t, c, 1)
	require.Equal(t, []int64{2}, ids(c[0]))
	require.Equal(t, "sorell", c[0][0].CrewID)

	// A rolled back transaction hands nothing over, and its entry's id
	// isn't handed over later either.
	boom := errors.New("boom")
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.appendLog(ctx, tx, testEntry); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Empty(t, got())

	// A no-op logs nothing and hands nothing over.
	require.NoError(t, s.SetCrewStatus(ctx, "sorell", CrewOnDeck, "", testEntry))
	require.Empty(t, got())

	_, err = s.AppendLog(ctx, LogEntry{Kind: "bridge", Text: "plain"})
	require.NoError(t, err)
	c = got()
	require.Len(t, c, 1)
	require.Len(t, c[0], 1)
	require.Equal(t, "plain", c[0][0].Text)
	last := c[0][0].ID

	all, err := s.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, last, all[len(all)-1].ID)
}

// Concurrent writers' entries reach the hook once each, in id order.
func TestLogHookConcurrentWriters(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	var (
		mu  sync.Mutex
		got []int64
	)
	require.NoError(t, s.SetLogHook(ctx, func(entries []LogEntry) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, ids(entries)...)
	}, nil))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				_, err := s.InsertOrder(ctx, Order{Title: "t"}, testEntry)
				assert.NoError(t, err)
			}
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 80)
	for i, id := range got {
		require.Equal(t, int64(i+1), id)
	}
}

func TestListLogLast(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	last, err := s.LastLogID(ctx)
	require.NoError(t, err)
	require.Zero(t, last)
	for range 5 {
		_, err := s.AppendLog(ctx, testEntry)
		require.NoError(t, err)
	}
	last, err = s.LastLogID(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(5), last)
	entries, err := s.ListLogLast(ctx, 4, 2)
	require.NoError(t, err)
	require.Equal(t, []int64{3, 4}, ids(entries))
	entries, err = s.ListLogLast(ctx, 5, 10)
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2, 3, 4, 5}, ids(entries))
}

func TestListOrderLogLast(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	for i := range 6 {
		_, err := s.AppendLog(ctx, LogEntry{Kind: "test", OrderID: []string{"ORD-0001", "ORD-0002"}[i%2]})
		require.NoError(t, err)
	}
	entries, err := s.ListOrderLogLast(ctx, "ORD-0001", 2)
	require.NoError(t, err)
	require.Equal(t, []int64{3, 5}, ids(entries))
	entries, err = s.ListOrderLogLast(ctx, "ORD-0002", 10)
	require.NoError(t, err)
	require.Equal(t, []int64{2, 4, 6}, ids(entries))
	entries, err = s.ListOrderLogLast(ctx, "ORD-0003", 10)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestListOrderLogKind(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	for i := range 6 {
		_, err := s.AppendLog(ctx, LogEntry{Kind: []string{"order", "crew", "order"}[i%3], OrderID: []string{"ORD-0001", "ORD-0002"}[i/3]})
		require.NoError(t, err)
	}
	entries, err := s.ListOrderLogKind(ctx, "ORD-0001", "order")
	require.NoError(t, err)
	require.Equal(t, []int64{1, 3}, ids(entries))
	entries, err = s.ListOrderLogKind(ctx, "ORD-0002", "crew")
	require.NoError(t, err)
	require.Equal(t, []int64{5}, ids(entries))
	entries, err = s.ListOrderLogKind(ctx, "ORD-0003", "order")
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestFeed(t *testing.T) {
	f := NewFeed(3)
	a, b := f.Subscribe(), f.Subscribe()
	f.Publish([]LogEntry{{ID: 1}, {ID: 2}})
	require.Equal(t, int64(1), (<-a.C()).ID)
	require.Equal(t, int64(2), (<-a.C()).ID)

	// b hasn't read and has room for one more: a batch of two drops it,
	// whole. a gets the batch.
	f.Publish([]LogEntry{{ID: 3}, {ID: 4}})
	require.Equal(t, int64(3), (<-a.C()).ID)
	require.Equal(t, int64(4), (<-a.C()).ID)
	var rest []LogEntry
	for e := range b.C() {
		rest = append(rest, e)
	}
	require.Equal(t, []int64{1, 2}, ids(rest), "b keeps what it had, then its channel closes")
	b.Close() // after a drop: no panic

	a.Close()
	a.Close()
	_, ok := <-a.C()
	require.False(t, ok)
	f.Publish([]LogEntry{{ID: 5}}) // no subscribers: no panic, no block
}

// The store's hook into a Feed: a subscriber sees each commit's entries.
func TestFeedFromStore(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	f := NewFeed(0)
	require.NoError(t, s.SetLogHook(ctx, f.Publish, nil))
	sub := f.Subscribe()
	defer sub.Close()
	_, err := s.InsertOrder(ctx, Order{Title: "t"}, LogEntry{Kind: "order", Text: "created"})
	require.NoError(t, err)
	e := <-sub.C()
	require.Equal(t, "ORD-0001", e.OrderID)
	require.Equal(t, "created", e.Text)
}
