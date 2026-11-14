package store

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// commitLog records each commit's entries through the store's log hook.
type commitLog struct {
	mu      sync.Mutex
	commits [][]LogEntry
}

func hookCommits(t *testing.T, s *Store) *commitLog {
	t.Helper()
	c := &commitLog{}
	require.NoError(t, s.SetLogHook(t.Context(), func(es []LogEntry) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.commits = append(c.commits, es)
	}, nil))
	return c
}

// last is the last commit's entries as "<order>: <text>".
func (c *commitLog) last() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.commits[len(c.commits)-1] {
		out = append(out, e.OrderID+": "+e.Text)
	}
	return out
}

func insertT(t *testing.T, s *Store, o Order) string {
	t.Helper()
	id, err := s.InsertOrder(t.Context(), o, testEntry)
	require.NoError(t, err)
	return id
}

func getT(t *testing.T, s *Store, id string) Order {
	t.Helper()
	o, err := s.GetOrder(t.Context(), id)
	require.NoError(t, err)
	return o
}

func claimPool(t *testing.T, s *Store, crew, division string) (string, bool) {
	t.Helper()
	id, found, err := s.ClaimOldestPoolOrder(t.Context(), crew, division, testEntry)
	require.NoError(t, err)
	return id, found
}

func claimDirected(t *testing.T, s *Store, crew string) (string, bool) {
	t.Helper()
	id, found, err := s.ClaimOldestDirectedOrder(t.Context(), crew, testEntry)
	require.NoError(t, err)
	return id, found
}

// #142: neither claim takes an order whose after isn't done, a red one
// included, nor while its after is recalled back to the pool; both take it
// once the after is done. The order reads its after and what it still
// waits on.
func TestAfterClaims(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	for _, c := range []Crew{{ID: "sorell", Name: "Sorell", Division: "ops"}, {ID: "kirk", Name: "Kirk", Division: "ops"},
		{ID: "uhura", Name: "Uhura"}} {
		require.NoError(t, s.InsertCrew(ctx, c, testEntry))
	}
	first := insertT(t, s, Order{Title: "first", Division: "ops"})
	pooled := insertT(t, s, Order{Title: "pooled", Division: "ops", Priority: PriorityRed, After: []string{first}})
	directed := insertT(t, s, Order{Title: "directed", Assignee: "uhura", After: []string{first, first}})
	o := getT(t, s, pooled)
	require.Equal(t, []string{first}, o.After)
	require.Equal(t, []string{first}, o.WaitingOn)
	require.Equal(t, []string{first}, getT(t, s, directed).After, "a repeated after counts once")
	require.Empty(t, getT(t, s, first).After)

	got, found := claimPool(t, s, "sorell", "ops")
	require.True(t, found)
	require.Equal(t, first, got, "the red order waits")
	_, found = claimPool(t, s, "kirk", "ops")
	require.False(t, found)
	_, found = claimDirected(t, s, "uhura")
	require.False(t, found)

	// Recalled to the pool, the after is still not done.
	_, recalled, err := s.RecallOrder(ctx, first, "sorell", testEntry)
	require.NoError(t, err)
	require.True(t, recalled)
	got, found = claimPool(t, s, "kirk", "ops")
	require.True(t, found)
	require.Equal(t, first, got)
	_, found = claimDirected(t, s, "uhura")
	require.False(t, found)

	_, _, err = s.SettleOrder(ctx, first, OrderDone, testEntry, nil)
	require.NoError(t, err)
	o = getT(t, s, pooled)
	require.Equal(t, []string{first}, o.After)
	require.Empty(t, o.WaitingOn)
	got, found = claimPool(t, s, "sorell", "ops")
	require.True(t, found)
	require.Equal(t, pooled, got)
	got, found = claimDirected(t, s, "uhura")
	require.True(t, found)
	require.Equal(t, directed, got)
}

// A split order counts as not done until it settles done (#142): an order
// after it waits while its children run.
func TestAfterSplitOrder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSplit(t, 1)
	require.NoError(t, f.s.InsertCrew(ctx, Crew{ID: "scotty", Name: "Scotty", Division: "eng"}, testEntry))
	next := insertT(t, f.s, Order{Title: "next", Division: "eng", After: []string{f.parent}})
	_, found := claimPool(t, f.s, "scotty", "eng")
	require.False(t, found, "the parent is split")

	f.claim(t, f.children[0])
	_, found = claimPool(t, f.s, "scotty", "eng")
	require.False(t, found)
	_, _, err := f.s.SettleOrder(ctx, f.children[0], OrderDone, testEntry, nil)
	require.NoError(t, err)
	require.Equal(t, OrderDone, f.state(t, f.parent).State)
	ready, err := f.s.HasReadyDependents(ctx, f.children[0])
	require.NoError(t, err)
	require.True(t, ready, "through the split parent the child's settle made done")
	got, found := claimPool(t, f.s, "scotty", "eng")
	require.True(t, found)
	require.Equal(t, next, got)
	ready, err = f.s.HasReadyDependents(ctx, f.children[0])
	require.NoError(t, err)
	require.False(t, ready, "claimed")
}

// InsertOrder refuses an after naming no order, the order itself, or one
// that ended failed, belayed or withdrawn, writing nothing (#142). One
// already done is accepted and satisfied.
func TestAfterRefusedAtInsert(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Division: "ops"}, testEntry))
	ended := map[string]string{}
	for _, state := range []string{OrderFailed, OrderBelayed, OrderWithdrawn, OrderDone} {
		ended[state] = insertT(t, s, Order{Title: state, Division: "ops", State: state})
	}
	before, err := s.ListOrders(ctx)
	require.NoError(t, err)
	logBefore, err := s.LastLogID(ctx)
	require.NoError(t, err)

	for _, tc := range []struct{ after, why string }{
		{"ORD-0404", "after ORD-0404: no such order"},
		{"ORD-0005", "after ORD-0005: an order can't wait on itself"},
		{ended[OrderFailed], "after ORD-0001: it ended failed and will never be done"},
		{ended[OrderBelayed], "after ORD-0002: it ended belayed and will never be done"},
		{ended[OrderWithdrawn], "after ORD-0003: it ended withdrawn and will never be done"},
	} {
		_, err := s.InsertOrder(ctx, Order{Title: "late", Division: "ops", After: []string{ended[OrderDone], tc.after}}, testEntry)
		require.ErrorIs(t, err, ErrAfter, tc.after)
		require.ErrorContains(t, err, tc.why)
	}
	after, err := s.ListOrders(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after, "nothing written")
	logAfter, err := s.LastLogID(ctx)
	require.NoError(t, err)
	require.Equal(t, logBefore, logAfter)
	var rows int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM order_after`).Scan(&rows))
	require.Zero(t, rows)

	id := insertT(t, s, Order{Title: "after done", Division: "ops", After: []string{ended[OrderDone]}})
	require.Empty(t, getT(t, s, id).WaitingOn)
	got, found := claimPool(t, s, "sorell", "ops")
	require.True(t, found)
	require.Equal(t, id, got)
}

// #142's cascade: an after that ends belayed withdraws the unclaimed
// order waiting on it and, down the chain, the one waiting on that, in the
// belay's transaction, each logged with its cause. A dependent already
// claimed is left to run. failed and withdrawn cascade the same way.
func TestAfterCascade(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	for _, c := range []Crew{{ID: "sorell", Name: "Sorell", Division: "ops"}, {ID: "kirk", Name: "Kirk", Division: "ops"}} {
		require.NoError(t, s.InsertCrew(ctx, c, testEntry))
	}
	first := insertT(t, s, Order{Title: "first", Division: "ops"})
	second := insertT(t, s, Order{Title: "second", Assignee: "kirk", After: []string{first}})
	third := insertT(t, s, Order{Title: "third", Division: "ops", After: []string{second}})
	// No claim takes an order whose after isn't done, so the claimed
	// dependent is inserted away, as if claimed.
	claimed := insertT(t, s, Order{Title: "claimed", Division: "ops", Assignee: "kirk", State: OrderAway, After: []string{first}})
	got, found := claimPool(t, s, "sorell", "ops")
	require.True(t, found)
	require.Equal(t, first, got)
	commits := hookCommits(t, s)

	_, _, belayed, err := s.BelayOrder(ctx, first, LogEntry{Kind: "order", OrderID: first, Text: "order belayed: scrapped"}, nil)
	require.NoError(t, err)
	require.True(t, belayed)
	require.Equal(t, []string{
		first + ": order belayed: scrapped",
		second + ": order withdrawn: it waits on " + first + ", which was belayed",
		third + ": order withdrawn: it waits on " + second + ", which was withdrawn",
	}, commits.last(), "one transaction")
	for id, state := range map[string]string{first: OrderBelayed, second: OrderWithdrawn, third: OrderWithdrawn, claimed: OrderAway} {
		require.Equal(t, state, getT(t, s, id).State, id)
	}
	e, err := s.LastOrderLog(ctx, second, "order")
	require.NoError(t, err)
	require.Equal(t, "kirk", e.CrewID)
	var payload map[string]string
	require.NoError(t, json.Unmarshal(e.Payload, &payload))
	require.Equal(t, map[string]string{"reason": "it waits on " + first + ", which was belayed", "after": first}, payload)

	// A re-run writes nothing.
	_, _, belayed, err = s.BelayOrder(ctx, first, testEntry, nil)
	require.NoError(t, err)
	require.False(t, belayed)
	require.Len(t, commits.commits, 1)

	for _, tc := range []struct {
		how string
		end func(id string) error
	}{
		{"failed", func(id string) error {
			_, found := claimPool(t, s, "sorell", "ops")
			require.True(t, found)
			_, _, err := s.SettleOrder(ctx, id, OrderFailed, testEntry, nil)
			return err
		}},
		{"was withdrawn", func(id string) error {
			_, _, err := s.WithdrawOrder(ctx, id, testEntry)
			return err
		}},
	} {
		head := insertT(t, s, Order{Title: "head " + tc.how, Division: "ops"})
		dep := insertT(t, s, Order{Title: "dep " + tc.how, Division: "ops", After: []string{head}})
		require.NoError(t, tc.end(head))
		require.Equal(t, OrderWithdrawn, getT(t, s, dep).State, tc.how)
		e, err := s.LastOrderLog(ctx, dep, "order")
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("order withdrawn: it waits on %s, which %s", head, tc.how), e.Text)
	}
}

// Split children name earlier siblings by index (AfterSiblings), stored as
// their ids; a later or own index is refused and nothing is created.
func TestInsertChildOrdersAfter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "vance", Name: "Vance", Division: "command"}, testEntry))
	parent := insertT(t, s, Order{Title: "rfc", Assignee: "vance", State: OrderAway})
	entry := func(ids []string) (LogEntry, error) { return LogEntry{Kind: "order", Text: "split"}, nil }
	_, err := s.InsertChildOrders(ctx, parent, "hail:H-0001", []Order{{Title: "a", Division: "ops"},
		{Title: "b", Division: "ops", AfterSiblings: []int{1}}}, entry)
	require.ErrorIs(t, err, ErrAfter)
	ids, err := s.ChildBatch(ctx, parent, "hail:H-0001")
	require.NoError(t, err)
	require.Empty(t, ids)

	ids, err = s.InsertChildOrders(ctx, parent, "hail:H-0002", []Order{{Title: "a", Division: "ops"},
		{Title: "b", Division: "ops", AfterSiblings: []int{0}}, {Title: "c", Division: "ops", AfterSiblings: []int{0, 1}}}, entry)
	require.NoError(t, err)
	require.Empty(t, getT(t, s, ids[0]).After)
	require.Equal(t, []string{ids[0]}, getT(t, s, ids[1]).After)
	require.Equal(t, []string{ids[0], ids[1]}, getT(t, s, ids[2]).WaitingOn)
	deps, err := s.OpenDependents(ctx, ids[0])
	require.NoError(t, err)
	require.Equal(t, []string{ids[1], ids[2]}, deps)
}

// HasReadyDependents looks only at the orders waiting on the order given,
// once it is done: not at one still waiting on another order, nor at
// orders waiting on an unrelated order that is ready.
func TestHasReadyDependents(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Division: "ops"}, testEntry))
	a := insertT(t, s, Order{Title: "a", Division: "ops", State: OrderAway, Assignee: "sorell"})
	b := insertT(t, s, Order{Title: "b", Division: "ops"})
	both := insertT(t, s, Order{Title: "both", Division: "ops", After: []string{a, b}})
	done := insertT(t, s, Order{Title: "done", Division: "ops", State: OrderDone})
	insertT(t, s, Order{Title: "ready elsewhere", Division: "ops", After: []string{done}})
	ready := func(id string) bool {
		t.Helper()
		r, err := s.HasReadyDependents(ctx, id)
		require.NoError(t, err)
		return r
	}
	require.False(t, ready(a), "a is not done")
	_, _, err := s.SettleOrder(ctx, a, OrderDone, testEntry, nil)
	require.NoError(t, err)
	require.False(t, ready(a), "both still waits on b")
	require.False(t, ready("ORD-0404"))
	_, err = s.db.ExecContext(ctx, `UPDATE orders SET state = ? WHERE id = ?`, OrderDone, b)
	require.NoError(t, err)
	require.True(t, ready(a))
	require.True(t, ready(b))
	require.Equal(t, OrderPool, getT(t, s, both).State)
}

// A belayed split parent's cascade withdraws a child waiting on a sibling
// with its own cause, the parent's belay, and an order outside the family
// waiting on a child with the after's. The withdraw's AfterCascade and
// WaitingChain name the orders waiting on an order, down the chain.
func TestAfterCascadeInBelayedFamily(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "vance", Name: "Vance", Division: "command"}, testEntry))
	parent := insertT(t, s, Order{Title: "rfc", Assignee: "vance", State: OrderAway})
	kids, err := s.InsertChildOrders(ctx, parent, "hail:H-0001", []Order{{Title: "a", Division: "ops"},
		{Title: "b", Division: "ops", AfterSiblings: []int{0}}}, func([]string) (LogEntry, error) { return testEntry, nil })
	require.NoError(t, err)
	outside := insertT(t, s, Order{Title: "outside", Division: "ops", After: []string{kids[1]}})
	_, _, err = s.SettleOrder(ctx, parent, OrderSplit, testEntry, nil)
	require.NoError(t, err)
	commits := hookCommits(t, s)

	cctx, col := WithAfterCascade(ctx)
	_, cascade, belayed, err := s.BelaySplitOrder(cctx, parent, LogEntry{Kind: "order", OrderID: parent, Text: "order belayed"}, nil)
	require.NoError(t, err)
	require.True(t, belayed)
	require.Equal(t, []ChildBelay{{ID: kids[0], State: OrderPool, Action: CascadeWithdrawn},
		{ID: kids[1], State: OrderPool, Action: CascadeWithdrawn}}, cascade)
	require.Equal(t, []string{
		parent + ": order belayed",
		kids[0] + ": order withdrawn: its parent " + parent + " was belayed",
		kids[1] + ": order withdrawn: its parent " + parent + " was belayed",
		outside + ": order withdrawn: it waits on " + kids[1] + ", which was withdrawn",
		parent + ": belay cascaded to 2 child orders: " + kids[0] + " withdrawn, " + kids[1] + " withdrawn",
	}, commits.last())
	require.Equal(t, []string{outside}, col.IDs())

	first := insertT(t, s, Order{Title: "first", Division: "ops"})
	second := insertT(t, s, Order{Title: "second", Division: "ops", After: []string{first}})
	third := insertT(t, s, Order{Title: "third", Division: "ops", After: []string{second, first}})
	tx, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	chain, err := WaitingChain(ctx, tx, first)
	require.NoError(t, tx.Rollback())
	require.NoError(t, err)
	require.Equal(t, []string{second, third}, chain)
	cctx, col = WithAfterCascade(ctx)
	_, withdrawn, err := s.WithdrawOrder(cctx, first, testEntry)
	require.NoError(t, err)
	require.True(t, withdrawn)
	require.Equal(t, []string{second, third}, col.IDs())
}
