package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A rebase onto an order already in after (#148) checks that order as a
// new base, in the transaction: an open one is taken and after keeps it
// once; a split one is refused, and nothing is written.
func TestReorderOntoPlainAfter(t *testing.T) {
	t.Parallel()
	s := openTest(t)
	ctx := t.Context()
	app := []OrderRepo{{Name: "app", BaseRef: "main"}}
	ins := func(o Order) string {
		t.Helper()
		o.Title, o.Division, o.Repos = "o", "ops", app
		id, err := s.InsertOrder(ctx, o, testEntry)
		require.NoError(t, err)
		return id
	}
	open := ins(Order{})
	parent := ins(Order{State: OrderAway, Assignee: "kirk"})
	dep := ins(Order{After: []string{open, parent}})
	_, err := s.InsertChildOrders(ctx, parent, "split:test", []Order{{Title: "k", Division: "ops", Repos: app}},
		func([]string) (LogEntry, error) { return testEntry, nil })
	require.NoError(t, err)
	entry := func(_, _ Order) (LogEntry, error) { return testEntry, nil }

	base := OrderBasePrefix + parent
	_, _, err = s.ReorderOrder(ctx, dep, OrderChange{Base: &base}, entry)
	require.ErrorIs(t, err, ErrReorder)
	require.ErrorContains(t, err, "base order:"+parent+": "+parent+" was split into ORD-0004: base it on one of them")
	o, err := s.GetOrder(ctx, dep)
	require.NoError(t, err)
	require.Equal(t, app, o.Repos, "nothing written")

	base = OrderBasePrefix + open
	o, changed, err := s.ReorderOrder(ctx, dep, OrderChange{Base: &base}, entry)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, []OrderRepo{{Name: "app", BaseRef: base}}, o.Repos)
	require.Equal(t, []string{open, parent}, o.After, "the base is in after once")
}
