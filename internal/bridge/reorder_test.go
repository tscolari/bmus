package bridge

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// lastOrderEntry is order id's last log entry of kind order.
func lastOrderEntry(t *testing.T, st *store.Store, id string) store.LogEntry {
	t.Helper()
	e, err := st.LastOrderLog(t.Context(), id, logOrder)
	require.NoError(t, err)
	return e
}

// order.rebase and order.after (#148) on orders nobody has started: a
// base of order:<id> replaces the order: entry in after and a plain ref
// drops it, after entries are added and removed, each change is one log
// entry, and the after a base implies can't be removed.
func TestOrderRebaseAndAfter(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	up := testRepo(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": up.url, "lib": up.url}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	ctx := t.Context()
	st := r.d.store
	ins := func(o store.Order) string {
		t.Helper()
		o.Title, o.Division = "o", "ops"
		if o.Repos == nil {
			o.Repos = []store.OrderRepo{{Name: "app", BaseRef: "main"}}
		}
		id, err := st.InsertOrder(ctx, o, testEntry)
		require.NoError(t, err)
		return id
	}
	a := ins(store.Order{})
	b := ins(store.Order{Repos: []store.OrderRepo{{Name: "app", BaseRef: "order:" + a}}, After: []string{a}})
	c := ins(store.Order{})
	rebase := func(id, onto string) (OrderView, error) {
		var v OrderView
		err := call(t, home, "order.rebase", OrderRebaseArgs{ID: id, Onto: onto}, &v)
		return v, err
	}
	after := func(id string, add, remove []string) (OrderView, error) {
		var v OrderView
		err := call(t, home, "order.after", OrderAfterArgs{ID: id, Add: add, Remove: remove}, &v)
		return v, err
	}

	v, err := rebase(b, "order:"+c)
	require.NoError(t, err)
	require.Equal(t, []OrderRepo{{Name: "app", BaseRef: "order:" + c}}, v.Repos)
	require.Equal(t, []string{c}, v.After, "the new base replaces the old one in after")
	require.Equal(t, []string{c}, v.WaitingOn)
	e := lastOrderEntry(t, st, b)
	require.Equal(t, "order rebased onto order:ORD-0003 (was order:ORD-0001)", e.Text)
	require.JSONEq(t, `{"repo":"app","base_ref":"order:ORD-0003","was_base_ref":"order:ORD-0001","after":["ORD-0003"],"was_after":["ORD-0001"]}`,
		string(e.Payload))

	v, err = rebase(b, "main")
	require.NoError(t, err)
	require.Empty(t, v.After, "a plain ref drops the base's wait")
	require.Equal(t, "order rebased onto main (was order:ORD-0003)", lastOrderEntry(t, st, b).Text)

	v, err = after(b, []string{a, c, a}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{a, c}, v.After)
	require.Equal(t, "order after: ORD-0001, ORD-0003 (was none)", lastOrderEntry(t, st, b).Text)
	v, err = after(b, nil, []string{a})
	require.NoError(t, err)
	require.Equal(t, []string{c}, v.After)
	require.Equal(t, "order after: ORD-0003 (was ORD-0001, ORD-0003)", lastOrderEntry(t, st, b).Text)

	// Rebased onto an order it already waits on: in after once.
	v, err = rebase(b, "order:"+c)
	require.NoError(t, err)
	require.Equal(t, []string{c}, v.After)
	before := lastOrderEntry(t, st, b).ID
	v, err = rebase(b, "order:"+c)
	require.NoError(t, err, "the same base again changes nothing")
	require.Equal(t, before, lastOrderEntry(t, st, b).ID, "and logs nothing")
	v, err = after(b, []string{c}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{c}, v.After)
	require.Equal(t, before, lastOrderEntry(t, st, b).ID)

	_, err = after(b, nil, []string{c})
	re := requireRPCError(t, err, CodeBadRequest)
	require.Equal(t, "after ORD-0003: order ORD-0002 is based on order:ORD-0003, so it waits on it; rebase ORD-0002 to drop the wait", re.Message)
	_, err = after(b, nil, []string{a})
	re = requireRPCError(t, err, CodeBadRequest)
	require.Equal(t, "after ORD-0001: order ORD-0002 doesn't wait on it", re.Message)
	o, err := st.GetOrder(ctx, b)
	require.NoError(t, err)
	require.Equal(t, []string{c}, o.After)
}

// Rebase and after refuse, writing nothing: an order a crew member has
// started (claimed, recalled with its parked mission, with a checkpoint),
// one that ended or was split, a cycle through order_after or through a
// split order's children, the order itself, and a base or an after
// order.create would refuse.
func TestOrderReorderRefusals(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	up := testRepo(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": up.url, "lib": up.url}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	ctx := t.Context()
	st := r.d.store
	ins := func(o store.Order) string {
		t.Helper()
		o.Title, o.Division = "o", "ops"
		o.Repos = []store.OrderRepo{{Name: "app", BaseRef: "main"}}
		id, err := st.InsertOrder(ctx, o, testEntry)
		require.NoError(t, err)
		return id
	}
	open := ins(store.Order{})
	away := ins(store.Order{State: store.OrderAway, Assignee: "kirk"})
	parked := ins(store.Order{State: store.OrderAway, Assignee: "kirk"})
	_, recalled, err := st.RecallOrder(ctx, parked, "kirk", testEntry)
	require.NoError(t, err)
	require.True(t, recalled)
	checkpointed := ins(store.Order{})
	require.NoError(t, st.InsertCheckpoint(ctx, store.Checkpoint{OrderID: checkpointed, Beam: "b", Seq: 1,
		Repos:          []store.CheckpointRepo{{Name: "app", Commit: strings.Repeat("ab", 20), BundlePath: "/cargo/app.bundle"}},
		TranscriptPath: "/cargo/t.jsonl", HandoverPath: "/cargo/handover.md", CreatedAt: time.Now()}))
	done := ins(store.Order{State: store.OrderDone})
	withdrawn := ins(store.Order{State: store.OrderWithdrawn})
	split := ins(store.Order{State: store.OrderSplit})
	x := ins(store.Order{})
	y := ins(store.Order{After: []string{x}})
	z := ins(store.Order{After: []string{y}})
	parent := ins(store.Order{State: store.OrderAway, Assignee: "kirk"})
	kids, err := st.InsertChildOrders(ctx, parent, "split:test", []store.Order{{Title: "k1", Division: "ops",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}}}, func([]string) (store.LogEntry, error) { return testEntry, nil })
	require.NoError(t, err)

	snapshot := func() ([]store.Order, int64) {
		orders, err := st.ListOrders(ctx)
		require.NoError(t, err)
		last, err := st.LastLogID(ctx)
		require.NoError(t, err)
		return orders, last
	}
	orders, last := snapshot()

	started := ": a crew member has started it on its current base"
	for _, tc := range []struct {
		name, cmd string
		args      any
		msg       string
	}{
		{"claimed", "order.rebase", OrderRebaseArgs{ID: away, Onto: "order:" + open}, "order " + away + " is away" + started},
		{"claimed after", "order.after", OrderAfterArgs{ID: away, Add: []string{open}}, "order " + away + " is away" + started},
		{"parked", "order.rebase", OrderRebaseArgs{ID: parked, Onto: "main~1"},
			"order " + parked + " was recalled and keeps its parked mission, whose work is built on its current base"},
		{"checkpoint", "order.after", OrderAfterArgs{ID: checkpointed, Add: []string{open}},
			"order " + checkpointed + " has a checkpoint, whose work is built on its current base"},
		{"done", "order.rebase", OrderRebaseArgs{ID: done, Onto: "main~1"}, "order " + done + " ended done"},
		{"withdrawn", "order.after", OrderAfterArgs{ID: withdrawn, Add: []string{open}}, "order " + withdrawn + " ended withdrawn"},
		{"split", "order.rebase", OrderRebaseArgs{ID: split, Onto: "main~1"},
			"order " + split + " is split: its mission has ended, and its work is in its child orders"},
		{"cycle", "order.rebase", OrderRebaseArgs{ID: x, Onto: "order:" + y},
			"after " + y + ": " + y + " waits on " + x + ", directly or down its chain, so the wait would be a cycle"},
		{"cycle down the chain", "order.after", OrderAfterArgs{ID: x, Add: []string{z}},
			"after " + z + ": " + z + " waits on " + x + ", directly or down its chain, so the wait would be a cycle"},
		{"cycle through a split", "order.after", OrderAfterArgs{ID: kids[0], Add: []string{parent}},
			"after " + parent + ": " + parent + " waits on " + kids[0] + ", directly or down its chain, so the wait would be a cycle"},
		{"itself", "order.rebase", OrderRebaseArgs{ID: x, Onto: "order:" + x}, "after " + x + ": an order can't wait on itself"},
		{"unknown base", "order.rebase", OrderRebaseArgs{ID: open, Onto: "order:ORD-0404"}, "repo app: base_ref order:ORD-0404: no such order"},
		{"malformed base", "order.rebase", OrderRebaseArgs{ID: open, Onto: "order:first"},
			`repo app: base_ref "order:first": order: takes an order's id, such as order:ORD-0001`},
		{"option-like base", "order.rebase", OrderRebaseArgs{ID: open, Onto: "-x"}, `repo app: base_ref "-x" must not start with '-'`},
		{"withdrawn base", "order.rebase", OrderRebaseArgs{ID: open, Onto: "order:" + withdrawn},
			"repo app: base_ref order:" + withdrawn + ": it ended withdrawn and will never be done"},
		{"split base", "order.rebase", OrderRebaseArgs{ID: open, Onto: "order:" + parent},
			"repo app: base_ref order:" + parent + ": " + parent + " was split into " + kids[0] + ": base it on one of them"},
		{"other repo", "order.rebase", OrderRebaseArgs{ID: open, Onto: "main", Repo: "lib"}, "order " + open + " doesn't work on repo lib"},
		{"unknown after", "order.after", OrderAfterArgs{ID: open, Add: []string{"ORD-0404"}}, "after ORD-0404: no such order"},
		{"withdrawn after", "order.after", OrderAfterArgs{ID: open, Add: []string{withdrawn}},
			"after " + withdrawn + ": it ended withdrawn and will never be done"},
		{"both", "order.after", OrderAfterArgs{ID: y, Add: []string{x}, Remove: []string{x}}, "after " + x + ": it is both added and removed"},
		{"no onto", "order.rebase", OrderRebaseArgs{ID: open}, "onto is required: order:<id> or a ref"},
		{"nothing", "order.after", OrderAfterArgs{ID: open}, "nothing to change: give orders to add or remove"},
	} {
		err := call(t, home, tc.cmd, tc.args, nil)
		re := requireRPCError(t, err, CodeBadRequest)
		require.Equal(t, tc.msg, re.Message, tc.name)
	}
	err = call(t, home, "order.rebase", OrderRebaseArgs{ID: "ORD-0404", Onto: "main"}, nil)
	requireRPCError(t, err, CodeNotFound)

	gotOrders, gotLast := snapshot()
	require.Equal(t, orders, gotOrders, "a refusal writes nothing")
	require.Equal(t, last, gotLast, "and logs nothing")
}

// reorderDaemon is a store-only Daemon with vance, of Command, away on an
// order, and a decided order_reorder hail on it, allowed, asking for
// changes; plus three orders on repo app: two in the pool and one away.
func reorderDaemon(t *testing.T, changes ...comms.OrderReorderChange) (*Daemon, MissionInput, string) {
	t.Helper()
	d, in := reorderOrders(t)
	return d, in, reorderHailOn(t, d, in, changes...)
}

// reorderOrders sets up reorderDaemon's daemon and orders: ORD-0001,
// Command's, then ORD-0002 and ORD-0003 in the pool and ORD-0004 away.
func reorderOrders(t *testing.T) (*Daemon, MissionInput) {
	t.Helper()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.DBFile))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	e := store.LogEntry{Kind: "test"}
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "vance", Name: "Vance", Division: DivisionCommand}, e))
	app := []store.OrderRepo{{Name: "app", BaseRef: "main"}}
	_, err = st.InsertOrder(ctx, store.Order{Title: "plan", Assignee: "vance", Repos: app}, e)
	require.NoError(t, err)
	order, found, err := st.ClaimOldestDirectedOrder(ctx, "vance", e)
	require.NoError(t, err)
	require.True(t, found)
	for _, o := range []store.Order{{Title: "api"}, {Title: "ui"}, {Title: "docs", State: store.OrderAway, Assignee: "kirk"}} {
		o.Division, o.Repos = "ops", app
		_, err := st.InsertOrder(ctx, o, e)
		require.NoError(t, err)
	}
	d := &Daemon{store: st, logger: slog.New(slog.DiscardHandler), repos: map[string]string{"app": "x"},
		divisions: map[string]DivisionConfig{DivisionCommand: {}, "ops": {}},
		wakeSend:  func(string, CrewSignal) error { return nil }}
	return d, MissionInput{OrderID: order, CrewID: "vance"}
}

// reorderHailOn records an order_reorder hail on in's order asking for
// changes, decided allow by the captain, and returns its id.
func reorderHailOn(t *testing.T, d *Daemon, in MissionInput, changes ...comms.OrderReorderChange) string {
	t.Helper()
	ctx := t.Context()
	raw, err := json.Marshal(comms.OrderReorderContext{Changes: changes, Reason: "the API moved"})
	require.NoError(t, err)
	e := store.LogEntry{Kind: "test"}
	id, err := d.store.InsertHail(ctx, store.Hail{OrderID: in.OrderID, CrewID: in.CrewID, Category: comms.CategoryOrderReorder, Question: "q",
		Options: []string{comms.OptionAllow, comms.OptionDeny}, Context: raw}, e)
	require.NoError(t, err)
	require.NoError(t, d.store.DecideOpenHail(ctx, id, comms.DecidedByCaptain, comms.OptionAllow, "", e, nil))
	return id
}

// apply-reorder applies each change still possible and skips, naming it,
// one whose order a crew member claimed meanwhile; the skip is noted on
// the hail and logged once, and a re-run applies nothing twice. With
// nothing applied the outcome is refused.
func TestApplyReorder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	d, in, hail := reorderDaemon(t,
		comms.OrderReorderChange{Order: "ORD-0002", Onto: "order:ORD-0003"},
		comms.OrderReorderChange{Order: "ORD-0004", AfterAdd: []string{"ORD-0002"}})
	const skip = "ORD-0004: order ORD-0004 is away: a crew member has started it on its current base"
	want := reorderOutcome{Decision: "applied ORD-0002; skipped " + skip}
	for range 2 {
		out, err := applyReorder(ctx, d, in, hail)
		require.NoError(t, err)
		require.Equal(t, want, out)
	}
	o, err := d.store.GetOrder(ctx, "ORD-0002")
	require.NoError(t, err)
	require.Equal(t, []string{"ORD-0003"}, o.After)
	require.Equal(t, []store.OrderRepo{{Name: "app", BaseRef: "order:ORD-0003"}}, o.Repos)
	h, err := d.store.GetHail(ctx, hail)
	require.NoError(t, err)
	require.Equal(t, comms.OptionAllow, h.Decision)
	require.Equal(t, "reorder skipped: "+skip, h.Rationale)
	entries, err := d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	var texts []string
	for _, e := range entries {
		if e.Kind == logOrder {
			texts = append(texts, e.OrderID+" "+e.Text)
		}
	}
	require.Equal(t, []string{"ORD-0002 order rebased onto order:ORD-0003 (was main)", "ORD-0001 reorder skipped: " + skip}, texts,
		"each logged once")
	rebased := lastOrderEntry(t, d.store, "ORD-0002")
	require.Equal(t, "vance", rebased.CrewID)
	var p map[string]any
	require.NoError(t, json.Unmarshal(rebased.Payload, &p))
	require.Equal(t, hail, p["hail_id"])

	none := reorderHailOn(t, d, in, comms.OrderReorderChange{Order: "ORD-0004", Onto: "main~1"},
		comms.OrderReorderChange{Order: "ORD-0404", AfterAdd: []string{"ORD-0002"}})
	out, err := applyReorder(ctx, d, in, none)
	require.NoError(t, err)
	require.Equal(t, reorderOutcome{Decision: "applied nothing; skipped " + skip + "; skipped ORD-0404: order ORD-0404: not found",
		Refused: true}, out)
}

// A countermanded reorder, or one checkReorder now refuses, applies
// nothing and is noted once.
func TestApplyReorderRefused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	d, in := reorderOrders(t)
	require.NoError(t, d.store.InsertCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"}, testEntry))
	hail := reorderHailOn(t, d, MissionInput{OrderID: in.OrderID, CrewID: "sorell"},
		comms.OrderReorderChange{Order: "ORD-0002", Onto: "order:ORD-0003"})
	const why = "only Command reorders orders, and sorell is not in the command division"
	for range 2 {
		out, err := applyReorder(ctx, d, in, hail)
		require.NoError(t, err)
		require.Equal(t, reorderOutcome{Decision: "reorder refused: " + why, Refused: true}, out)
	}
	h, err := d.store.GetHail(ctx, hail)
	require.NoError(t, err)
	require.Equal(t, "reorder refused: "+why, h.Rationale)
	o, err := d.store.GetOrder(ctx, "ORD-0002")
	require.NoError(t, err)
	require.Empty(t, o.After)
}

// checkReorder's refusals of a malformed order_reorder hail.
func TestCheckReorder(t *testing.T) {
	t.Parallel()
	vance := store.Crew{ID: "vance", Division: DivisionCommand}
	ok := comms.OrderReorderContext{Reason: "r", Changes: []comms.OrderReorderChange{{Order: "ORD-0002", Onto: "order:ORD-0003"},
		{Order: "ORD-0004", AfterAdd: []string{"ORD-0002"}, AfterRemove: []string{"ORD-0001"}}}}
	hail := func(ctx any) store.Hail {
		raw, err := json.Marshal(ctx)
		require.NoError(t, err)
		return store.Hail{Category: comms.CategoryOrderReorder, Options: []string{comms.OptionAllow, comms.OptionDeny}, Context: raw}
	}
	rc, why := checkReorder(hail(ok), vance)
	require.Empty(t, why)
	require.Equal(t, ok, rc)
	with := func(fn func(*comms.OrderReorderContext)) comms.OrderReorderContext {
		c := ok
		c.Changes = []comms.OrderReorderChange{ok.Changes[0], ok.Changes[1]}
		fn(&c)
		return c
	}
	many := make([]comms.OrderReorderChange, 11)
	for i := range many {
		many[i] = comms.OrderReorderChange{Order: "ORD-" + strings.Repeat("1", 4) + string(rune('a'+i)), Onto: "main"}
	}
	ids := make([]string, 11)
	for i := range ids {
		ids[i] = "ORD-00" + string(rune('1'+i/10)) + string(rune('0'+i%10))
	}
	for _, tc := range []struct {
		name string
		h    store.Hail
		crew store.Crew
		why  string
	}{
		{"not Command", hail(ok), store.Crew{ID: "sorell", Division: "ops"}, "only Command reorders orders, and sorell is not in the command division"},
		{"options", func() store.Hail { h := hail(ok); h.Options = []string{"deny", "allow"}; return h }(), vance, "its options must be [allow deny]"},
		{"unknown field", hail(map[string]any{"changes": ok.Changes, "reason": "r", "note": 1}), vance, `its context is not a reorder: json: unknown field "note"`},
		{"no changes", hail(with(func(c *comms.OrderReorderContext) { c.Changes = nil })), vance, "it has no changes"},
		{"eleven", hail(with(func(c *comms.OrderReorderContext) { c.Changes = many })), vance, "it has 11 changes, more than 10"},
		{"no reason", hail(with(func(c *comms.OrderReorderContext) { c.Reason = " " })), vance, "it gives no reason"},
		{"long reason", hail(with(func(c *comms.OrderReorderContext) { c.Reason = strings.Repeat("r", 1001) })), vance,
			"its reason is longer than 1000 characters"},
		{"not an id", hail(with(func(c *comms.OrderReorderContext) { c.Changes[0].Order = "ord-2" })), vance, `change 1: "ord-2" is not an order id`},
		{"twice", hail(with(func(c *comms.OrderReorderContext) { c.Changes[1].Order = "ORD-0002" })), vance,
			"change 2 names ORD-0002 again: one change per order"},
		{"nothing", hail(with(func(c *comms.OrderReorderContext) { c.Changes[0].Onto = "" })), vance,
			"change 1 asks for nothing: give onto, after_add or after_remove"},
		{"spaced onto", hail(with(func(c *comms.OrderReorderContext) { c.Changes[0].Onto = " main" })), vance, `change 1's onto " main" has surrounding space`},
		{"dash onto", hail(with(func(c *comms.OrderReorderContext) { c.Changes[0].Onto = "-x" })), vance, `change 1's onto "-x" must not start with '-'`},
		{"long onto", hail(with(func(c *comms.OrderReorderContext) { c.Changes[0].Onto = strings.Repeat("m", 257) })), vance,
			"change 1's onto is longer than 256 bytes"},
		{"two-line onto", hail(with(func(c *comms.OrderReorderContext) { c.Changes[0].Onto = "a\nb" })), vance, "change 1's onto is not one line"},
		{"bad order onto", hail(with(func(c *comms.OrderReorderContext) { c.Changes[0].Onto = "order:x" })), vance,
			`change 1's onto "order:x": order: takes an order's id, such as order:ORD-0001`},
		{"many afters", hail(with(func(c *comms.OrderReorderContext) { c.Changes[1].AfterAdd = ids })), vance,
			"change 2's after_add has 11 ids, more than 10"},
		{"after not an id", hail(with(func(c *comms.OrderReorderContext) { c.Changes[1].AfterRemove = []string{"main"} })), vance,
			`change 2's after_remove: "main" is not an order id`},
		{"after twice", hail(with(func(c *comms.OrderReorderContext) { c.Changes[1].AfterAdd = []string{"ORD-0002", "ORD-0002"} })), vance,
			"change 2's after_add names ORD-0002 twice"},
		{"added and removed", hail(with(func(c *comms.OrderReorderContext) { c.Changes[1].AfterRemove = []string{"ORD-0002"} })), vance,
			"change 2 both adds and removes ORD-0002"},
	} {
		_, why := checkReorder(tc.h, tc.crew)
		require.Equal(t, tc.why, why, tc.name)
	}
	require.Equal(t, "its reason is not valid UTF-8", reorderReasonWhy("r\xff"))
	require.Empty(t, reorderReasonWhy(" r "))
}

// The bridge's reorder question shows each order's current base and
// after and what they would be, or why a change can't apply as read now.
func TestReorderQuestion(t *testing.T) {
	t.Parallel()
	d, _ := reorderOrders(t)
	q := func(changes ...comms.OrderReorderChange) string {
		raw, err := json.Marshal(comms.OrderReorderContext{Changes: changes, Reason: "the API\nmoved"})
		require.NoError(t, err)
		return d.reorderQuestion(t.Context(), raw)
	}
	require.Equal(t, `Reorder 3 orders: `+
		`1) ORD-0002 "api" (pool): base main -> order:ORD-0003; after none -> ORD-0003; `+
		`2) ORD-0004 "docs" (away): after none -> ORD-0002 (will be skipped: order ORD-0004 is away: a crew member has started it on its current base); `+
		`3) ORD-0404: no such order. Command's reason: "the API moved"`,
		q(comms.OrderReorderChange{Order: "ORD-0002", Onto: "order:ORD-0003"},
			comms.OrderReorderChange{Order: "ORD-0004", AfterAdd: []string{"ORD-0002"}},
			comms.OrderReorderChange{Order: "ORD-0404", Onto: "main"}))
	require.Equal(t, `Reorder 1 order: 1) ORD-0002 "api" (pool): after ORD-0001: order ORD-0002 doesn't wait on it. Command's reason: "the API moved"`,
		q(comms.OrderReorderChange{Order: "ORD-0002", AfterRemove: []string{"ORD-0001"}}))
	require.Equal(t, "Reorder orders: the request is malformed, and the bridge denies it", d.reorderQuestion(t.Context(), json.RawMessage(`[]`)))
}

// Views of a rebase entry carry the order's new base and wait, so the
// console's open-orders card shows them at once.
func TestReorderViews(t *testing.T) {
	t.Parallel()
	d, _ := reorderOrders(t)
	ctx := t.Context()
	onto := "order:ORD-0003"
	_, err := d.reorderOrder(ctx, "ORD-0002", store.OrderChange{Base: &onto}, reorderSource{})
	require.NoError(t, err)
	evs, err := consoleSource{d: d}.Views(ctx, lastOrderEntry(t, d.store, "ORD-0002"))
	require.NoError(t, err)
	require.NotEmpty(t, evs)
	v, ok := evs[0].Data.(OrderView)
	require.True(t, ok)
	require.Equal(t, []OrderRepo{{Name: "app", BaseRef: onto}}, v.Repos)
	require.Equal(t, []string{"ORD-0003"}, v.WaitingOn)
}

// #148's acceptance, rebase: an order nobody has claimed, rebased onto
// order:<newer>, waits for that newer order, is claimed after its done
// entry and ships its commit.
func TestOrderRebaseRunsOnWork(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "work.json"))
	hd := b.start(t)
	ctx := t.Context()
	first := hd.createPoolOrder(t, "ops", store.PriorityRoutine)
	dep := hd.createPoolOrder(t, "ops", store.PriorityRoutine)
	newer := hd.createPoolOrder(t, "ops", store.PriorityRoutine)
	v, err := hd.d.rebaseOrder(ctx, OrderRebaseArgs{ID: dep, Onto: "order:" + newer})
	require.NoError(t, err)
	require.Equal(t, []string{newer}, v.WaitingOn)
	hd.waitLog(t, "order rebased onto order:"+newer+" (was main)")

	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	require.NoError(t, hd.d.SetStandingOrders(ctx, true))
	for _, id := range []string{first, newer, dep} {
		missionDone(t, hd.d, id)
	}
	newerDone, err := hd.d.store.LastOrderLog(ctx, newer, logDone)
	require.NoError(t, err)
	var p donePayload
	require.NoError(t, json.Unmarshal(newerDone.Payload, &p))
	require.Len(t, p.Repos, 1)
	base := p.Repos[0].Commit

	entries := hd.logEntries(t)
	done, claimed := -1, -1
	var shipped []string
	for i, e := range entries {
		if e.OrderID == newer && e.Kind == logDone && done < 0 {
			done = i
		}
		if e.OrderID == dep && e.Text == "order away: mission started" && claimed < 0 {
			claimed = i
		}
		if e.OrderID == dep && strings.HasPrefix(e.Text, "repo app shipped") {
			shipped = append(shipped, e.Text)
		}
	}
	require.Positive(t, done)
	require.Greater(t, claimed, done, "the rebased order is claimed after the newer order is done")
	require.Equal(t, []string{"repo app shipped to the beam at " + base[:12] + " (order:" + newer + ")"}, shipped)
	clone := filepath.Join(b.home, "repos", "app")
	require.Equal(t, base, gitT(t, clone, "rev-parse", "bmus/"+dep+"~2"), "its work sits on the newer order's")
	requireNoBackstop(t, entries)
}

// reorderHailBody is a holodeck hail step body: an order_reorder hail, as
// the sdk harness's reorder_orders raises it.
func reorderHailBody(changes ...comms.OrderReorderChange) map[string]any {
	return map[string]any{"category": comms.CategoryOrderReorder, "question": "Reorder?",
		"options": []string{comms.OptionAllow, comms.OptionDeny},
		"context": comms.OrderReorderContext{Changes: changes, Reason: "the API moved"}}
}

// #148's acceptance, Command: reorder_orders under the default regs.yaml
// reaches the captain with the bridge's question. While it waits, a crew
// member claims one of its orders. Allowing it applies the other change,
// and the answer names the stale one as skipped. The same hail from a
// crew member outside Command is denied by policy.
func TestReorderOrdersHail(t *testing.T) {
	t.Parallel()
	question := map[string]any{"category": comms.CategoryClarifyingQuestion, "question": "Which way?"}
	b := newHolodeckBridge(t, hailsScript(t, reorderHailBody(
		comms.OrderReorderChange{Order: "ORD-0002", Onto: "order:ORD-0003"},
		comms.OrderReorderChange{Order: "ORD-0001", AfterAdd: []string{"ORD-0003"}}), question))
	b.regs = defaultRegs(t)
	hd := b.start(t)
	ctx := t.Context()
	hd.addCommand(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	stale := hd.createPoolOrder(t, "ops", store.PriorityRoutine)
	moved := hd.createPoolOrder(t, "eng", store.PriorityRoutine)
	onto := hd.createPoolOrder(t, "eng", store.PriorityRoutine)
	require.Equal(t, []string{"ORD-0001", "ORD-0002", "ORD-0003"}, []string{stale, moved, onto})
	parent := hd.createCommandOrder(t)

	e := hd.waitLog(t, "hail H-0001 for the captain")
	require.Equal(t, `hail H-0001 for the captain: captain_only[8]: category "order_reorder"`, e.Text)
	q := `Reorder 2 orders: 1) ORD-0002 "pool run" (pool): base main -> order:ORD-0003; after none -> ORD-0003; ` +
		`2) ORD-0001 "pool run" (pool): after none -> ORD-0003. Command's reason: "the API moved"`
	require.Equal(t, q, hd.hail(t, "H-0001").Question, "the captain reads the bridge's question")

	// sorell claims the order the second change names: its own reorder
	// hail is denied by policy, and its question holds it hailing.
	require.NoError(t, hd.d.SetStandingOrders(ctx, true))
	var denied store.Hail
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		hails, err := hd.d.store.ListHails(ctx, "")
		require.NoError(c, err)
		i := slices.IndexFunc(hails, func(h store.Hail) bool {
			return h.OrderID == stale && h.Category == comms.CategoryOrderReorder && h.Status == store.HailDecided
		})
		require.GreaterOrEqual(c, i, 0)
		denied = hails[i]
		require.True(c, slices.ContainsFunc(hails, func(h store.Hail) bool {
			return h.OrderID == stale && h.Category == comms.CategoryClarifyingQuestion && h.Status == store.HailOpen
		}))
		require.Equal(c, store.OrderHailing, hd.order(c, stale).State)
	}, 60*time.Second, 20*time.Millisecond)
	require.Equal(t, "invalid order reorder: only Command reorders orders, and sorell is not in the command division", denied.Decision)
	require.Equal(t, []string{"classify", "deny-reorder"}, hailStepNames(t, hd.d, denied.ID))

	allow := 0
	require.NoError(t, hd.d.AnswerHail(ctx, "H-0001", "", &allow))
	hd.waitLog(t, "order rebased onto order:ORD-0003 (was main)")
	o := hd.order(t, moved)
	require.Equal(t, []string{onto}, o.WaitingOn)
	require.Equal(t, []store.OrderRepo{{Name: "app", BaseRef: "order:" + onto}}, o.Repos)
	const skip = "ORD-0001: order ORD-0001 is hailing: a crew member has started it on its current base"
	hd.waitLog(t, "reorder skipped: "+skip)
	require.Empty(t, hd.order(t, stale).After, "the stale change wrote nothing")
	h := hd.hail(t, "H-0001")
	require.Equal(t, comms.OptionAllow, h.Decision)
	require.Equal(t, "reorder skipped: "+skip, h.Rationale)

	// Answer the questions left open (Command's own goes to the captain,
	// sorell's once Command's consult lane escalates it), and both finish.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		hails, err := hd.d.store.ListHails(ctx, store.HailOpen)
		require.NoError(c, err)
		for _, h := range hails {
			_ = hd.d.AnswerHail(ctx, h.ID, "left", nil)
		}
		for _, id := range []string{parent, stale} {
			wfs := workflows(c, hd.d, missionWorkflowID(id))
			require.Len(c, wfs, 1)
			require.Equal(c, dbos.WorkflowStatusSuccess, wfs[0].Status)
		}
	}, 90*time.Second, 50*time.Millisecond)
	steps := hailSteps(t, b.stepLog)
	i := slices.IndexFunc(steps, func(s map[string]any) bool { return s["order"] == parent && s["index"] == 0.0 })
	require.GreaterOrEqual(t, i, 0, "%v", steps)
	require.Equal(t, "applied ORD-0002; skipped "+skip, steps[i]["decision"], "the answer names what applied and what was skipped")
	require.Equal(t, 1, strings.Count(strings.Join(stepNames(t, hd.d, missionWorkflowID(parent)), ","), "apply-reorder"))
}

// A reorder allowed by a captain timeout's default_option (order_reorder
// dropped from captain_only) waits out its countermand window before
// apply-reorder. Countermanded to deny inside the window, it applies
// nothing.
func TestReorderCountermandWindow(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, reorderHailBody(comms.OrderReorderChange{Order: "ORD-0001", Onto: "order:ORD-0002"})))
	cfg := defaultRegs(t)
	n := len(cfg.CaptainOnly)
	cfg.CaptainOnly = slices.DeleteFunc(cfg.CaptainOnly, func(r regs.MatchRule) bool { return r.Category == comms.CategoryOrderReorder })
	require.Len(t, cfg.CaptainOnly, n-1)
	opt, window := 0, 8*time.Second
	cfg.Escalation.OnTimeout, cfg.Escalation.CaptainTimeout, cfg.Escalation.DefaultOption = regs.OnTimeoutDefaultOption, time.Second, &opt
	cfg.CommandMayAnswer.CountermandWindow = &window
	b.regs = cfg
	hd := b.start(t)
	hd.addCommand(t)
	moved := hd.createPoolOrder(t, "eng", store.PriorityRoutine)
	onto := hd.createPoolOrder(t, "eng", store.PriorityRoutine)
	parent := hd.createCommandOrder(t)

	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByPolicy}, hailResult(t, hd.d, "H-0001"))
	require.NotNil(t, hd.hail(t, "H-0001").CountermandUntil)
	_, err := hd.d.CountermandHail(t.Context(), "H-0001", "", ptr(1), "not now")
	require.NoError(t, err)

	require.Equal(t, MissionDone, missionResult(t, hd.d, parent).Status)
	names := strings.Join(stepNames(t, hd.d, missionWorkflowID(parent)), ",")
	require.Contains(t, names, "reorder-window")
	require.Equal(t, 1, strings.Count(names, "apply-reorder"))
	const why = "the captain countermanded it: deny"
	steps := hailSteps(t, b.stepLog)
	require.Len(t, steps, 1)
	require.Equal(t, "reorder refused: "+why, steps[0]["decision"])
	require.Equal(t, "not now; reorder refused: "+why, hd.hail(t, "H-0001").Rationale, "the countermand's reason, then the refusal")
	o := hd.order(t, moved)
	require.Equal(t, []store.OrderRepo{{Name: "app", BaseRef: "main"}}, o.Repos, "nothing applied")
	require.Empty(t, o.After)
	require.Empty(t, hd.order(t, onto).After)
}
