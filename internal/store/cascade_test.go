package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// belayRecorder is a BelayFunc that records each child it sends a belay
// and reports it sent, unless reach names the child.
type belayRecorder struct {
	mu      sync.Mutex
	sent    []string
	reasons []string
	reach   map[string]BelayReach
}

func (b *belayRecorder) belay(_ *sql.Tx, o Order, reason string) (BelayReach, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.reach[o.ID]; ok {
		return r, nil
	}
	b.sent = append(b.sent, o.ID)
	b.reasons = append(b.reasons, reason)
	return BelaySent, nil
}

// #146's acceptance: belaying a split parent with three children, one in
// the pool, one away and one done, withdraws the pool child, sends the
// away child a belay and leaves the done child alone, in one transaction
// with the parent's belay. The parent ends belayed, and one entry on it
// names all three. The away child's mission then settles it belayed, and
// the parent stays belayed: settleSplit never touches it again.
func TestBelaySplitCascades(t *testing.T) {
	ctx := t.Context()
	f := newSplit(t, 3)
	a, b, c := f.children[0], f.children[1], f.children[2]
	f.claim(t, a)
	_, _, err := f.s.SettleOrder(ctx, a, OrderDone, LogEntry{Kind: "done", OrderID: a, Text: "a done"}, nil)
	require.NoError(t, err)
	f.claim(t, b)
	require.Equal(t, 2, f.state(t, f.parent).OpenChildren)

	rec := &belayRecorder{}
	current, cascade, belayed, err := f.s.BelaySplitOrder(ctx, f.parent,
		LogEntry{Kind: "order", OrderID: f.parent, Text: "order belayed: enough"}, rec.belay)
	require.NoError(t, err)
	require.True(t, belayed)
	require.Equal(t, OrderBelayed, current)
	require.Equal(t, []ChildBelay{
		{ID: a, State: OrderDone, Action: CascadeAlready},
		{ID: b, State: OrderAway, Action: CascadeBelayRequested},
		{ID: c, State: OrderPool, Action: CascadeWithdrawn},
	}, cascade)
	require.Equal(t, []string{b}, rec.sent)
	require.Equal(t, []string{"its parent " + f.parent + " was belayed"}, rec.reasons)

	want := "belay cascaded to 3 child orders: " + a + " already done, " + b + " belay requested, " + c + " withdrawn"
	require.Equal(t, []string{
		f.parent + ": order belayed: enough",
		b + ": belay requested: its parent " + f.parent + " was belayed",
		c + ": order withdrawn: its parent " + f.parent + " was belayed",
		f.parent + ": " + want,
	}, f.lastCommit(), "one transaction")
	p := f.state(t, f.parent)
	require.Equal(t, OrderBelayed, p.State)
	require.Equal(t, 1, p.OpenChildren, "the away child until its mission settles it")
	require.Equal(t, OrderDone, f.state(t, a).State)
	require.Equal(t, OrderAway, f.state(t, b).State)
	require.Equal(t, OrderWithdrawn, f.state(t, c).State)
	e, err := f.s.LastOrderLog(ctx, f.parent, "order")
	require.NoError(t, err)
	require.Equal(t, want, e.Text)
	require.Equal(t, "vance", e.CrewID)
	var payload struct {
		Children []ChildBelay `json:"children"`
	}
	require.NoError(t, json.Unmarshal(e.Payload, &payload))
	require.Equal(t, cascade, payload.Children)

	// The away child's mission settles it; the parent stays belayed.
	_, _, settled, err := f.s.BelayOrder(ctx, b, LogEntry{Kind: "order", OrderID: b, Text: "b belayed"}, rec.belay)
	require.NoError(t, err)
	require.True(t, settled)
	require.Equal(t, []string{b + ": b belayed"}, f.lastCommit())
	p = f.state(t, f.parent)
	require.Equal(t, OrderBelayed, p.State)
	require.Zero(t, p.OpenChildren)
	for _, text := range texts(t, f.s) {
		require.NotContains(t, text, "child orders did not finish")
		require.NotContains(t, text, "which run on")
	}

	// A second belay finds the parent belayed and writes nothing.
	n := len(texts(t, f.s))
	current, cascade, belayed, err = f.s.BelaySplitOrder(ctx, f.parent, testEntry, rec.belay)
	require.NoError(t, err)
	require.False(t, belayed)
	require.Nil(t, cascade)
	require.Equal(t, OrderBelayed, current)
	require.Len(t, texts(t, f.s), n)
	require.Equal(t, []string{b}, rec.sent)
}

// A child whose report beats its cascaded belay ends done; the parent
// stays belayed. A split parent its last child settled first is no longer
// split: the belay finds it done and writes nothing.
func TestBelaySplitRaces(t *testing.T) {
	ctx := t.Context()
	f := newSplit(t, 1)
	a := f.children[0]
	f.claim(t, a)
	rec := &belayRecorder{}
	_, _, belayed, err := f.s.BelaySplitOrder(ctx, f.parent, LogEntry{Kind: "order", OrderID: f.parent, Text: "order belayed"}, rec.belay)
	require.NoError(t, err)
	require.True(t, belayed)
	_, settled, err := f.s.SettleOrder(ctx, a, OrderDone, LogEntry{Kind: "done", OrderID: a, Text: "a done"}, nil)
	require.NoError(t, err)
	require.True(t, settled)
	require.Equal(t, []string{a + ": a done"}, f.lastCommit())
	current, _, belayed, err := f.s.BelayOrder(ctx, a, testEntry, rec.belay)
	require.NoError(t, err)
	require.False(t, belayed, "the child's mission finds it done")
	require.Equal(t, OrderDone, current)
	require.Equal(t, OrderBelayed, f.state(t, f.parent).State)

	g := newSplit(t, 1)
	g.claim(t, g.children[0])
	_, _, err = g.s.SettleOrder(ctx, g.children[0], OrderDone, testEntry, nil)
	require.NoError(t, err)
	require.Equal(t, OrderDone, g.state(t, g.parent).State)
	current, cascade, belayed, err := g.s.BelaySplitOrder(ctx, g.parent, LogEntry{Kind: "order", Text: "order belayed"}, rec.belay)
	require.NoError(t, err)
	require.False(t, belayed)
	require.Nil(t, cascade)
	require.Equal(t, OrderDone, current)
	require.NotContains(t, texts(t, g.s), "order belayed")
}

// An away parent whose mission settles it belayed cascades in that same
// transaction (the mission's order-belayed step): no orphan entry. A
// child whose mission hasn't started is reported not reached; one whose
// mission has ended is belayed by the cascade itself. A re-run of the
// step writes nothing and sends nothing.
func TestBelayAwayParentCascades(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Division: "ops"}, testEntry))
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "kira", Name: "Kira", Division: "ops"}, testEntry))
	require.NoError(t, s.InsertCrew(ctx, Crew{ID: "nog", Name: "Nog", Division: "ops"}, testEntry))
	parent, err := s.InsertOrder(ctx, Order{Title: "rfc", Assignee: "vance", State: OrderAway}, testEntry)
	require.NoError(t, err)
	ids, err := s.InsertChildOrders(ctx, parent, "hail:H-0001",
		[]Order{{Title: "a", Division: "ops"}, {Title: "b", Division: "ops"}, {Title: "c", Division: "ops"}, {Title: "d", Division: "ops"}},
		func([]string) (LogEntry, error) { return LogEntry{Kind: "order", Text: "split"}, nil })
	require.NoError(t, err)
	for _, crew := range []string{"sorell", "kira", "nog"} {
		_, found, err := s.ClaimOldestPoolOrder(ctx, crew, "ops", testEntry)
		require.NoError(t, err)
		require.True(t, found)
	}
	rec := &belayRecorder{reach: map[string]BelayReach{ids[1]: BelayNoMission, ids[2]: BelayMissionEnded}}
	current, assignee, belayed, err := s.BelayOrder(ctx, parent, LogEntry{Kind: "order", OrderID: parent, Text: "order belayed"}, rec.belay)
	require.NoError(t, err)
	require.True(t, belayed)
	require.Equal(t, OrderBelayed, current)
	require.Equal(t, "vance", assignee)
	require.Equal(t, []string{ids[0]}, rec.sent)
	want := "belay cascaded to 4 child orders: " + ids[0] + " belay requested, " + ids[1] +
		" not reached: its mission has not started, " + ids[2] + " belayed, " + ids[3] + " withdrawn"
	tx := texts(t, s)
	require.Equal(t, want, tx[len(tx)-1])
	for _, text := range tx {
		require.NotContains(t, text, "which run on")
	}
	require.Contains(t, tx, "order belayed: its parent "+parent+" was belayed")
	for id, state := range map[string]string{ids[0]: OrderAway, ids[1]: OrderAway, ids[2]: OrderBelayed, ids[3]: OrderWithdrawn} {
		o, err := s.GetOrder(ctx, id)
		require.NoError(t, err)
		require.Equal(t, state, o.State, id)
	}

	n := len(tx)
	_, _, belayed, err = s.BelayOrder(ctx, parent, LogEntry{Kind: "order", OrderID: parent, Text: "order belayed"}, rec.belay)
	require.NoError(t, err)
	require.False(t, belayed)
	require.Len(t, texts(t, s), n)
	require.Equal(t, []string{ids[0]}, rec.sent)

	// SettleOpenOrder never belays: only BelayOrder cascades.
	_, _, _, err = s.SettleOpenOrder(ctx, ids[0], OrderBelayed, testEntry)
	require.ErrorContains(t, err, "BelayOrder")
}

// Nested splits recurse: a split child, with no mission, is belayed in
// the same transaction and cascades to its own children. Every level
// logs its own entry and stays belayed as its children settle.
func TestBelayNestedSplit(t *testing.T) {
	ctx := t.Context()
	f := newSplit(t, 1)
	x := f.children[0]
	f.claim(t, x)
	grand, err := f.s.InsertChildOrders(ctx, x, "hail:H-0002", []Order{{Title: "g1", Division: "ops"}, {Title: "g2", Division: "ops"}},
		func([]string) (LogEntry, error) { return LogEntry{Kind: "order", Text: "x split"}, nil })
	require.NoError(t, err)
	_, settled, err := f.s.SettleOrder(ctx, x, OrderSplit, LogEntry{Kind: "done", OrderID: x, Text: "x report"}, nil)
	require.NoError(t, err)
	require.True(t, settled)
	require.NoError(t, f.s.InsertCrew(ctx, Crew{ID: "kira", Name: "Kira", Division: "ops"}, testEntry))
	g1, found, err := f.s.ClaimOldestPoolOrder(ctx, "kira", "ops", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, grand[0], g1)

	rec := &belayRecorder{}
	_, cascade, belayed, err := f.s.BelaySplitOrder(ctx, f.parent, LogEntry{Kind: "order", OrderID: f.parent, Text: "order belayed"}, rec.belay)
	require.NoError(t, err)
	require.True(t, belayed)
	require.Equal(t, []ChildBelay{{ID: x, State: OrderSplit, Action: CascadeBelayed}}, cascade)
	require.Equal(t, []string{g1}, rec.sent)
	why := "its parent " + x + " was belayed"
	require.Equal(t, []string{
		f.parent + ": order belayed",
		x + ": order belayed: its parent " + f.parent + " was belayed",
		g1 + ": belay requested: " + why,
		grand[1] + ": order withdrawn: " + why,
		x + ": belay cascaded to 2 child orders: " + g1 + " belay requested, " + grand[1] + " withdrawn",
		f.parent + ": belay cascaded to 1 child orders: " + x + " belayed",
	}, f.lastCommit())

	_, _, settled, err = f.s.BelayOrder(ctx, g1, LogEntry{Kind: "order", OrderID: g1, Text: "g1 belayed"}, rec.belay)
	require.NoError(t, err)
	require.True(t, settled)
	require.Equal(t, []string{g1 + ": g1 belayed"}, f.lastCommit())
	require.Equal(t, OrderBelayed, f.state(t, x).State)
	require.Equal(t, OrderBelayed, f.state(t, f.parent).State)
	require.False(t, strings.Contains(strings.Join(texts(t, f.s), "\n"), "child orders did not finish"))
}
