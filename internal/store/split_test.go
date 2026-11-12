package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// splitFixture is an away parent with n children created by hail H-0001,
// the parent then settled split by its report. Its log hook records each
// commit's entries.
type splitFixture struct {
	s        *Store
	parent   string
	children []string

	mu      sync.Mutex
	commits [][]LogEntry
}

func newSplit(t *testing.T, n int) *splitFixture {
	t.Helper()
	ctx := t.Context()
	f := &splitFixture{s: openTest(t)}
	require.NoError(t, f.s.InsertCrew(ctx, Crew{ID: "vance", Name: "Vance", Division: "command"}, testEntry))
	require.NoError(t, f.s.InsertCrew(ctx, Crew{ID: "sorell", Name: "Sorell", Division: "ops"}, testEntry))
	var err error
	f.parent, err = f.s.InsertOrder(ctx, Order{Title: "rfc", Assignee: "vance", Division: "command", State: OrderAway}, testEntry)
	require.NoError(t, err)
	var kids []Order
	for i := range n {
		kids = append(kids, Order{Title: fmt.Sprintf("child %d", i+1), Body: "b", Division: "ops"})
	}
	f.children, err = f.s.InsertChildOrders(ctx, f.parent, "hail:H-0001", kids, func(ids []string) (LogEntry, error) {
		return LogEntry{Kind: "order", Text: "order split into " + strings.Join(ids, ", ")}, nil
	})
	require.NoError(t, err)
	require.Len(t, f.children, n)
	current, settled, err := f.s.SettleOrder(ctx, f.parent, OrderSplit, LogEntry{Kind: "done", OrderID: f.parent, Text: "report"}, &testEntry)
	require.NoError(t, err)
	require.True(t, settled)
	require.Equal(t, OrderSplit, current)
	require.NoError(t, f.s.SetLogHook(ctx, func(es []LogEntry) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.commits = append(f.commits, es)
	}, nil))
	return f
}

// lastCommit is the texts of the last commit's entries.
func (f *splitFixture) lastCommit() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.commits[len(f.commits)-1] {
		out = append(out, e.OrderID+": "+e.Text)
	}
	return out
}

func (f *splitFixture) state(t *testing.T, id string) Order {
	t.Helper()
	o, err := f.s.GetOrder(t.Context(), id)
	require.NoError(t, err)
	return o
}

// claim claims child id for sorell, away.
func (f *splitFixture) claim(t *testing.T, id string) {
	t.Helper()
	got, found, err := f.s.ClaimOldestPoolOrder(t.Context(), "sorell", "ops", testEntry)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, id, got)
}

// InsertChildOrders creates the children once, pooled in their division
// with the parent and the batch marked, and a re-run returns the same ids
// writing nothing. A parent no longer away takes no children.
func TestInsertChildOrders(t *testing.T) {
	ctx := t.Context()
	f := newSplit(t, 2)
	require.Equal(t, []string{"ORD-0002", "ORD-0003"}, f.children)
	for i, id := range f.children {
		o := f.state(t, id)
		require.Equal(t, f.parent, o.ParentID)
		require.Equal(t, "hail:H-0001", o.CreatedBy)
		require.Equal(t, OrderPool, o.State)
		require.Equal(t, fmt.Sprintf("child %d", i+1), o.Title)
	}
	require.Equal(t, 2, f.state(t, f.parent).OpenChildren)
	require.Equal(t, []string{"order created: child 1", "order created: child 2", "order split into ORD-0002, ORD-0003", "report"}, texts(t, f.s))

	ids, err := f.s.InsertChildOrders(ctx, f.parent, "hail:H-0001", []Order{{Title: "x"}}, func([]string) (LogEntry, error) {
		return LogEntry{}, errors.New("not called on a re-run")
	})
	require.NoError(t, err)
	require.Equal(t, f.children, ids)
	orders, err := f.s.ListOrders(ctx)
	require.NoError(t, err)
	require.Len(t, orders, 3)

	_, err = f.s.InsertChildOrders(ctx, f.parent, "hail:H-0002", []Order{{Title: "x"}}, func([]string) (LogEntry, error) {
		return LogEntry{}, nil
	})
	require.ErrorIs(t, err, ErrParentEnded, "split, no longer away")
}

// #155's acceptance: the parent stays split until its last child settles,
// then settles done in that child's transaction. Status counts it neither
// open nor away.
func TestSplitParentSettlesDone(t *testing.T) {
	ctx := t.Context()
	f := newSplit(t, 2)
	snap, err := f.s.ReadStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, snap.OrderStates[OrderSplit])

	a, b := f.children[0], f.children[1]
	f.claim(t, a)
	_, _, err = f.s.SettleOrder(ctx, a, OrderDone, LogEntry{Kind: "done", OrderID: a, Text: "a done"}, nil)
	require.NoError(t, err)
	p := f.state(t, f.parent)
	require.Equal(t, OrderSplit, p.State)
	require.Equal(t, 1, p.OpenChildren)

	f.claim(t, b)
	_, _, err = f.s.SettleOrder(ctx, b, OrderDone, LogEntry{Kind: "done", OrderID: b, Text: "b done"}, nil)
	require.NoError(t, err)
	p = f.state(t, f.parent)
	require.Equal(t, OrderDone, p.State)
	require.Zero(t, p.OpenChildren)
	require.Equal(t, []string{b + ": b done", f.parent + ": order done: its 2 child orders are done"}, f.lastCommit(),
		"one transaction")
	e, err := f.s.LastOrderLog(ctx, f.parent, "order")
	require.NoError(t, err)
	require.Equal(t, "vance", e.CrewID)

	// A re-run of the parent's own report finds it settled: no late entry.
	current, settled, err := f.s.SettleOrder(ctx, f.parent, OrderSplit, testEntry, &LogEntry{Kind: "order", Text: "late"})
	require.NoError(t, err)
	require.False(t, settled)
	require.Equal(t, OrderDone, current)
	require.NotContains(t, texts(t, f.s), "late")
}

// #155's acceptance: one failed child settles the parent failed, naming
// it; belayed and withdrawn children count as not done, and each ending
// path settles the parent.
func TestSplitParentSettlesFailed(t *testing.T) {
	ctx := t.Context()
	f := newSplit(t, 3)
	a, b, c := f.children[0], f.children[1], f.children[2]

	f.claim(t, a)
	_, _, err := f.s.SettleOrder(ctx, a, OrderFailed, LogEntry{Kind: "fail", OrderID: a, Text: "a failed"}, nil)
	require.NoError(t, err)
	f.claim(t, b)
	_, _, _, err = f.s.BelayOrder(ctx, b, LogEntry{Kind: "order", OrderID: b, Text: "b belayed"}, nil)
	require.NoError(t, err)
	require.Equal(t, OrderSplit, f.state(t, f.parent).State)
	_, withdrawn, err := f.s.WithdrawOrder(ctx, c, LogEntry{Kind: "order", OrderID: c, Text: "c withdrawn"})
	require.NoError(t, err)
	require.True(t, withdrawn)

	want := fmt.Sprintf("order failed: child orders did not finish: %s failed, %s belayed, %s withdrawn", a, b, c)
	require.Equal(t, OrderFailed, f.state(t, f.parent).State)
	require.Equal(t, []string{c + ": c withdrawn", f.parent + ": " + want}, f.lastCommit())
	e, err := f.s.LastOrderLog(ctx, f.parent, "fail")
	require.NoError(t, err)
	require.Equal(t, want, e.Text)
	var p struct {
		Code    string   `json:"code"`
		NotDone []string `json:"not_done"`
	}
	require.NoError(t, json.Unmarshal(e.Payload, &p))
	require.Equal(t, CodeChildrenFailed, p.Code)
	require.Equal(t, []string{a + " failed", b + " belayed", c + " withdrawn"}, p.NotDone)
}

// #155's acceptance: a recalled child is back in the pool, still open, and
// keeps the parent split. Children that all ended before the parent's
// report settle it at the report, in the same transaction.
func TestSplitRecallAndEarlyChildren(t *testing.T) {
	ctx := t.Context()
	f := newSplit(t, 1)
	a := f.children[0]
	f.claim(t, a)
	_, recalled, err := f.s.RecallOrder(ctx, a, "sorell", LogEntry{Kind: "order", OrderID: a, Text: "recalled"})
	require.NoError(t, err)
	require.True(t, recalled)
	p := f.state(t, f.parent)
	require.Equal(t, OrderSplit, p.State)
	require.Equal(t, 1, p.OpenChildren)

	// Children done before the report: the report's settle ends the parent.
	s := openTest(t)
	parent, err := s.InsertOrder(ctx, Order{Title: "rfc", Assignee: "vance", State: OrderAway}, testEntry)
	require.NoError(t, err)
	ids, err := s.InsertChildOrders(ctx, parent, "hail:H-0001", []Order{{Title: "c", Division: "ops"}},
		func([]string) (LogEntry, error) { return LogEntry{Kind: "order", Text: "split"}, nil })
	require.NoError(t, err)
	_, withdrawn, err := s.WithdrawOrder(ctx, ids[0], testEntry)
	require.NoError(t, err)
	require.True(t, withdrawn)
	o, err := s.GetOrder(ctx, parent)
	require.NoError(t, err)
	require.Equal(t, OrderAway, o.State, "not split yet: nothing to settle")
	current, settled, err := s.SettleOrder(ctx, parent, OrderSplit, LogEntry{Kind: "done", Text: "report"}, nil)
	require.NoError(t, err)
	require.True(t, settled)
	require.Equal(t, OrderSplit, current, "what this call wrote")
	o, err = s.GetOrder(ctx, parent)
	require.NoError(t, err)
	require.Equal(t, OrderFailed, o.State)
	require.Equal(t, []string{"order created: c", "split", "report", "order failed: child orders did not finish: " + ids[0] + " withdrawn"}, texts(t, s))
}

// A parent that ends other than through settleSplit while children are
// open (its mission failed after create-children) logs them, once, in the
// transaction that ends it. Its children then run on, and their ends
// don't touch it.
func TestSplitParentEndsWithOpenChildren(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	parent, err := s.InsertOrder(ctx, Order{Title: "rfc", Assignee: "vance", State: OrderAway}, testEntry)
	require.NoError(t, err)
	ids, err := s.InsertChildOrders(ctx, parent, "hail:H-0001", []Order{{Title: "a", Division: "ops"}, {Title: "b", Division: "ops"}},
		func([]string) (LogEntry, error) { return LogEntry{Kind: "order", Text: "split"}, nil })
	require.NoError(t, err)
	_, withdrawn, err := s.WithdrawOrder(ctx, ids[0], testEntry)
	require.NoError(t, err)
	require.True(t, withdrawn)
	_, settled, err := s.SettleOrder(ctx, parent, OrderFailed, LogEntry{Kind: "fail", Text: "send failed"}, nil)
	require.NoError(t, err)
	require.True(t, settled)
	want := "order ended with 1 open child orders, which run on: " + ids[1]
	require.Equal(t, []string{"order created: a", "order created: b", "split", "send failed", want}, texts(t, s))
	e, err := s.LastOrderLog(ctx, parent, "order")
	require.NoError(t, err)
	require.Equal(t, want, e.Text)
	require.JSONEq(t, `{"open_children":["`+ids[1]+`"]}`, string(e.Payload))

	_, withdrawn, err = s.WithdrawOrder(ctx, ids[1], testEntry)
	require.NoError(t, err)
	require.True(t, withdrawn)
	o, err := s.GetOrder(ctx, parent)
	require.NoError(t, err)
	require.Equal(t, OrderFailed, o.State)
	require.Len(t, texts(t, s), 5, "nothing more for the parent")
}

// ChildBatch finds a batch by its marker; RefuseSplit notes a refusal on
// the hail once, after any rationale already there.
func TestChildBatchAndRefuseSplit(t *testing.T) {
	ctx := t.Context()
	f := newSplit(t, 2)
	ids, err := f.s.ChildBatch(ctx, f.parent, "hail:H-0001")
	require.NoError(t, err)
	require.Equal(t, f.children, ids)
	ids, err = f.s.ChildBatch(ctx, f.parent, "hail:H-0002")
	require.NoError(t, err)
	require.Empty(t, ids)

	require.NoError(t, f.s.InsertCrew(ctx, Crew{ID: "data", Name: "Data"}, testEntry))
	other, err := f.s.InsertOrder(ctx, Order{Title: "t", Assignee: "data", State: OrderAway}, testEntry)
	require.NoError(t, err)
	hail, err := f.s.InsertHail(ctx, Hail{OrderID: other, CrewID: "data", Category: "order_split", Question: "q"}, testEntry)
	require.NoError(t, err)
	require.NoError(t, f.s.DecideOpenHail(ctx, hail, "captain", "allow", "looks right", testEntry, nil))
	for i := range 2 {
		wrote, err := f.s.RefuseSplit(ctx, hail, "unknown division", LogEntry{Kind: "order", OrderID: other, Text: "refused"})
		require.NoError(t, err)
		require.Equal(t, i == 0, wrote)
	}
	h, err := f.s.GetHail(ctx, hail)
	require.NoError(t, err)
	require.Equal(t, "looks right; split refused: unknown division", h.Rationale)
	require.Equal(t, "allow", h.Decision)
	n := 0
	for _, e := range texts(t, f.s) {
		if e == "refused" {
			n++
		}
	}
	require.Equal(t, 1, n)
	_, err = f.s.RefuseSplit(ctx, "H-9999", "x", testEntry)
	require.ErrorIs(t, err, sql.ErrNoRows)
}
