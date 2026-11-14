package bridge

import (
	"database/sql"
	"encoding/json"
	"fmt"
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

// splitHail is a holodeck hail step body: an order_split hail, as the sdk
// harness's split_order raises it, asking for children.
func splitHail(children ...comms.OrderSplitChild) map[string]any {
	return map[string]any{"category": comms.CategoryOrderSplit, "question": fmt.Sprintf("Split into %d child orders?", len(children)),
		"options": []string{comms.OptionAllow, comms.OptionDeny},
		"context": comms.OrderSplitContext{Children: children}}
}

// opsChildren are two children for division ops.
func opsChildren() []comms.OrderSplitChild {
	return []comms.OrderSplitChild{
		{Title: "child one", Body: "the first half", Division: "ops"},
		{Title: "child two", Body: "the second half", Division: "ops"},
	}
}

// createCommandOrder creates an order directed to vance, Command.
func (hd *hdaemon) createCommandOrder(t *testing.T) string {
	t.Helper()
	id, err := hd.d.CreateOrder(t.Context(), store.Order{Title: "split the rfc", Body: "into tickets", Assignee: "vance",
		Priority: store.PriorityYellow, Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	return id
}

// waitOrderState waits for order id to be in state and returns it.
func (hd *hdaemon) waitOrderState(t *testing.T, id, state string) store.Order {
	t.Helper()
	var o store.Order
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		o = hd.order(c, id)
		require.Equal(c, state, o.State)
	}, 60*time.Second, 20*time.Millisecond)
	return o
}

// #155's acceptance: Command's split_order under the default regs.yaml is
// held for the captain, approved, and creates the children with parent_id
// set. Command's mission ends, it goes back on deck, and the parent waits
// in split, counted neither open nor away, refusing recall. Once
// both children are done the parent settles done. The children, run by a
// crew member outside Command, split nothing: their own split hails are
// denied by policy.
func TestSplitOrder(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, splitHail(opsChildren()...)))
	b.regs = defaultRegs(t)
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	parent := hd.createCommandOrder(t)

	e := hd.waitLog(t, "hail H-0001 for the captain")
	require.Equal(t, `hail H-0001 for the captain: captain_only[2]: category "order_split"`, e.Text)
	// The captain reads the bridge's summary, not the agent's question.
	q := "Split " + parent + " into 2 orders: 1) child one [ops, yellow]; 2) child two [ops, yellow]"
	require.Equal(t, q, hd.hail(t, "H-0001").Question)
	hd.waitLog(t, q)
	require.Equal(t, store.OrderHailing, hd.waitOrderState(t, parent, store.OrderHailing).State)
	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, parent).Status)
	require.Equal(t, 1, strings.Count(strings.Join(stepNames(t, hd.d, missionWorkflowID(parent)), ","), "create-children"))

	p := hd.order(t, parent)
	require.Equal(t, store.OrderSplit, p.State)
	require.Equal(t, 2, p.OpenChildren)
	require.Equal(t, 2, hd.d.orderView(p).OpenChildren)
	kids := []string{"ORD-0002", "ORD-0003"}
	for i, id := range kids {
		o := hd.order(t, id)
		require.Equal(t, parent, o.ParentID)
		require.Equal(t, store.OrderPool, o.State)
		require.Equal(t, "ops", o.Division)
		require.Equal(t, store.PriorityYellow, o.Priority, "the parent's")
		require.Equal(t, opsChildren()[i].Title, o.Title)
		require.Equal(t, "hail:H-0001", o.CreatedBy)
		require.Equal(t, p.Repos, o.Repos)
	}
	hd.waitLog(t, "order split into 2 children: ORD-0002, ORD-0003")
	hd.waitLog(t, "split, waiting on its child orders: done after the hails")
	h := hd.hail(t, "H-0001")
	require.Equal(t, comms.OptionAllow, h.Decision, "the hail keeps the captain's decision")
	steps := hailSteps(t, b.stepLog)
	require.Len(t, steps, 1)
	require.Equal(t, "ORD-0002, ORD-0003", steps[0]["decision"], "the answer names the children")
	require.Equal(t, comms.DecidedByCaptain, steps[0]["decided_by"])

	// Command is back on deck, free for its next order.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Equal(c, store.CrewOnDeck, hd.crew(c, "vance").Status)
	}, 30*time.Second, 20*time.Millisecond)
	s, err := hd.d.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, OrderCounts{Open: 2, Away: 0}, s.Orders, "the split parent is neither open nor away")

	// Belaying it would cascade (#146, cascade_test.go); recall is refused.
	err = hd.d.RecallOrder(t.Context(), parent, "")
	require.ErrorContains(t, err, "order "+parent+" is split and its mission has ended; it waits on 2 open child orders: "+
		"recall those instead, or belay it to end them all")
	require.Equal(t, CodeBadRequest, errorCode(t.Context(), err))

	// sorell runs the children; the script's split hail is denied, as
	// sorell isn't Command.
	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))
	for _, id := range kids {
		missionDone(t, hd.d, id)
	}
	for _, id := range []string{"H-0002", "H-0003"} {
		require.Equal(t, HailResult{Decided: true, By: comms.DecidedByPolicy}, hailResult(t, hd.d, id))
		require.Equal(t, []string{"classify", "deny-split"}, hailStepNames(t, hd.d, id))
		h := hd.hail(t, id)
		require.Equal(t, "invalid order split: only Command splits orders, and sorell is not in the command division", h.Decision)
		require.Equal(t, regs.RouteAuto, h.Route)
	}
	p = hd.waitOrderState(t, parent, store.OrderDone)
	require.Zero(t, p.OpenChildren)
	done := hd.waitLog(t, "order done: its 2 child orders are done")
	require.Equal(t, parent, done.OrderID)
	orders, err := hd.d.store.ListOrders(t.Context())
	require.NoError(t, err)
	require.Len(t, orders, 3, "the children created none")
	requireNoBackstop(t, hd.logEntries(t))
}

// #142's acceptance: a split whose second child has after [0] runs its
// children in order. The second, red, would be claimed first; it waits in
// the pool, "waiting on" the first, while two idle ops crew members stand
// by, and is claimed only after the first is done.
func TestSplitAfterRunsInOrder(t *testing.T) {
	t.Parallel()
	red := store.PriorityRed
	kids := []comms.OrderSplitChild{
		{Title: "child one", Body: "the first half", Division: "ops"},
		{Title: "child two", Body: "the second half", Division: "ops", Priority: &red, After: []int{0}},
	}
	b := newHolodeckBridge(t, hailsScript(t, splitHail(kids...)))
	b.regs = defaultRegs(t)
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	hd.addCrewWith(t, store.Crew{ID: "chekov", Name: "Chekov", Division: "ops"})
	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))
	parent := hd.createCommandOrder(t)

	q := "Split " + parent + " into 2 orders: 1) child one [ops, yellow]; 2) child two [ops, red, after 1]"
	hd.waitLog(t, q)
	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	require.Equal(t, MissionDone, missionResult(t, hd.d, parent).Status)
	first, second := "ORD-0002", "ORD-0003"
	require.Equal(t, []string{first}, hd.order(t, second).After)

	o := hd.waitOrderState(t, first, store.OrderAway)
	require.Empty(t, o.After)
	w := hd.order(t, second)
	require.Equal(t, store.OrderPool, w.State, "the red child waits while an ops crew member is idle")
	v := hd.d.orderView(w)
	require.Equal(t, []string{first}, v.After)
	require.Equal(t, []string{first}, v.WaitingOn)

	missionDone(t, hd.d, first)
	missionDone(t, hd.d, second)
	hd.waitOrderState(t, parent, store.OrderDone)
	entries := hd.logEntries(t)
	done, claimed := -1, -1
	for i, e := range entries {
		if e.OrderID == first && e.Kind == logDone && done < 0 {
			done = i
		}
		if e.OrderID == second && e.Text == "order away: mission started" && claimed < 0 {
			claimed = i
		}
	}
	require.Positive(t, done)
	require.Greater(t, claimed, done, "the second child is claimed after the first is done")
	require.Empty(t, hd.order(t, second).WaitingOn)
	requireNoBackstop(t, entries)
}

// A malformed split is denied by policy before anyone sees it, with or
// without regs.yaml, and Command's mission goes on to its report: done,
// with no children.
func TestSplitInvalidDenied(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		children []comms.OrderSplitChild
		regs     bool
		why      string
	}{
		{"unknown division", []comms.OrderSplitChild{{Title: "t", Body: "b", Division: "nowhere"}}, true,
			`child 1: unknown division "nowhere" (the roster has command, ops)`},
		{"too many, no regs", slices.Repeat([]comms.OrderSplitChild{{Title: "t", Body: "b", Division: "ops"}}, 11), false,
			"it has 11 children, more than 10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := newHolodeckBridge(t, hailsScript(t, splitHail(tc.children...)))
			if tc.regs {
				b.regs = defaultRegs(t)
			}
			b.divisions = map[string]DivisionConfig{DivisionCommand: {}, "ops": {}}
			hd := b.start(t)
			hd.addCommand(t)
			parent := hd.createCommandOrder(t)

			require.Equal(t, HailResult{Decided: true, By: comms.DecidedByPolicy}, hailResult(t, hd.d, "H-0001"))
			require.Equal(t, []string{"classify", "deny-split"}, hailStepNames(t, hd.d, "H-0001"))
			require.Equal(t, MissionDone, missionResult(t, hd.d, parent).Status)
			require.NotContains(t, stepNames(t, hd.d, missionWorkflowID(parent)), "create-children")
			require.Equal(t, store.OrderDone, hd.order(t, parent).State)
			hd.waitLog(t, "hail H-0001 decided by policy: invalid order split: "+tc.why)
			steps := hailSteps(t, b.stepLog)
			require.Len(t, steps, 1)
			require.Equal(t, "invalid order split: "+tc.why, steps[0]["decision"])
			orders, err := hd.d.store.ListOrders(t.Context())
			require.NoError(t, err)
			require.Len(t, orders, 1)
		})
	}
}

// A split decided by policy (the captain timeout's default_option, with
// order_split no longer captain-only) creates its children once, in the
// mission. A countermand to deny afterwards reaches the agent, if its run
// is live, and leaves the children alone: the captain belays or withdraws
// them.
func TestSplitByTimeoutThenCountermand(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, splitHail(opsChildren()...)))
	cfg := defaultRegs(t)
	cfg.CaptainOnly = slices.DeleteFunc(cfg.CaptainOnly, func(r regs.MatchRule) bool { return r.Category == comms.CategoryOrderSplit })
	opt, window := 0, 10*time.Minute
	cfg.Escalation.OnTimeout, cfg.Escalation.CaptainTimeout, cfg.Escalation.DefaultOption = regs.OnTimeoutDefaultOption, time.Second, &opt
	cfg.CommandMayAnswer.CountermandWindow = &window
	b.regs = cfg
	hd := b.start(t)
	hd.addCommand(t)
	parent := hd.createCommandOrder(t)

	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByPolicy}, hailResult(t, hd.d, "H-0001"))
	require.Equal(t, MissionDone, missionResult(t, hd.d, parent).Status)
	require.Equal(t, 1, strings.Count(strings.Join(stepNames(t, hd.d, missionWorkflowID(parent)), ","), "create-children"))
	require.Equal(t, store.OrderSplit, hd.order(t, parent).State)
	steps := hailSteps(t, b.stepLog)
	require.Len(t, steps, 1)
	require.Equal(t, "ORD-0002, ORD-0003", steps[0]["decision"])

	_, err := hd.d.CountermandHail(t.Context(), "H-0001", "", ptr(1), "not now")
	require.NoError(t, err)
	require.Equal(t, comms.DecidedByCaptain, hd.hail(t, "H-0001").DecidedBy)
	for _, id := range []string{"ORD-0002", "ORD-0003"} {
		require.Equal(t, store.OrderPool, hd.order(t, id).State, "a countermand doesn't undo the split")
	}
	require.Equal(t, store.OrderSplit, hd.order(t, parent).State)

	// Withdrawing both children settles the parent failed, naming them.
	for _, id := range []string{"ORD-0002", "ORD-0003"} {
		require.NoError(t, hd.d.WithdrawOrder(t.Context(), id, "not now"))
	}
	require.Equal(t, store.OrderFailed, hd.order(t, parent).State)
	e := hd.waitLog(t, "order failed: child orders did not finish")
	require.Equal(t, "order failed: child orders did not finish: ORD-0002 withdrawn, ORD-0003 withdrawn", e.Text)
	var p map[string]any
	require.NoError(t, json.Unmarshal(e.Payload, &p))
	require.Equal(t, store.CodeChildrenFailed, p["code"])
}

func ptr[T any](v T) *T { return &v }

// checkSplit refuses every malformed split, and resolves a well-formed
// one's defaults from the parent.
func TestCheckSplit(t *testing.T) {
	t.Parallel()
	d := &Daemon{divisions: map[string]DivisionConfig{DivisionCommand: {}, "ops": {}}, repos: map[string]string{"app": "x"}}
	parent := store.Order{ID: "ORD-0001", Division: DivisionCommand, Priority: store.PriorityYellow,
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}}
	vance := store.Crew{ID: "vance", Division: DivisionCommand}
	hail := func(context string) store.Hail {
		return store.Hail{OrderID: "ORD-0001", Category: comms.CategoryOrderSplit,
			Options: []string{comms.OptionAllow, comms.OptionDeny}, Context: json.RawMessage(context)}
	}

	kids, why := d.checkSplit(hail(`{"parent":"ORD-0001","children":[{"title":" API ","body":"b","division":"ops","priority":2},`+
		`{"title":"UI","body":"b","after":[0,0]}]}`), parent, vance)
	require.Empty(t, why)
	require.Equal(t, []store.Order{
		{Title: "API", Body: "b", Division: "ops", Priority: store.PriorityRed, Repos: parent.Repos},
		{Title: "UI", Body: "b", Division: DivisionCommand, Priority: store.PriorityYellow, Repos: parent.Repos, AfterSiblings: []int{0}},
	}, kids)

	long := strings.Repeat("x", maxSplitTitle+1)
	for _, tc := range []struct {
		name string
		h    store.Hail
		crew store.Crew
		o    store.Order
		why  string
	}{
		{"not Command", hail(`{"children":[{"title":"t","body":"b"}]}`), store.Crew{ID: "sorell", Division: "ops"}, parent,
			"only Command splits orders, and sorell is not in the command division"},
		{"options", store.Hail{Options: []string{"yes"}, Context: json.RawMessage(`{}`)}, vance, parent, "its options must be [allow deny]"},
		{"not an object", hail(`[]`), vance, parent, "its context is not a split: json: cannot unmarshal array"},
		{"unknown field", hail(`{"children":[{"title":"t","body":"b","assignee":"sorell"}]}`), vance, parent,
			`its context is not a split: json: unknown field "assignee"`},
		{"two values", hail(`{"children":[]} {}`), vance, parent, "more than one JSON value"},
		{"other parent", hail(`{"parent":"ORD-0009","children":[{"title":"t","body":"b"}]}`), vance, parent,
			"it names parent ORD-0009, not its own order ORD-0001"},
		{"no children", hail(`{"children":[]}`), vance, parent, "it has no children"},
		{"no title", hail(`{"children":[{"title":"  ","body":"b"}]}`), vance, parent, "child 1 has no title"},
		{"long title", hail(`{"children":[{"title":"` + long + `","body":"b"}]}`), vance, parent, "child 1's title is longer than 200 characters"},
		{"two lines", hail(`{"children":[{"title":"t","body":"b"},{"title":"a\nb","body":"b"}]}`), vance, parent, "child 2's title is not one line"},
		{"no body", hail(`{"children":[{"title":"t","body":" "}]}`), vance, parent, "child 1 has no body"},
		{"no division", hail(`{"children":[{"title":"t","body":"b"}]}`), vance, store.Order{ID: "ORD-0001", Repos: parent.Repos},
			"child 1 has no division, and order ORD-0001 has none"},
		{"unknown division", hail(`{"children":[{"title":"t","body":"b","division":"x"}]}`), vance, parent, `child 1: unknown division "x"`},
		{"priority", hail(`{"children":[{"title":"t","body":"b","priority":3}]}`), vance, parent, "child 1's priority 3 is out of range"},
		{"after itself", hail(`{"children":[{"title":"t","body":"b","after":[0]}]}`), vance, parent,
			"child 1's after names child 1: a child waits on earlier children only"},
		{"after a later child", hail(`{"children":[{"title":"t","body":"b","after":[1]},{"title":"u","body":"b"}]}`), vance, parent,
			"child 1's after names child 2"},
		{"after negative", hail(`{"children":[{"title":"t","body":"b"},{"title":"u","body":"b","after":[-1]}]}`), vance, parent,
			"child 2's after names child 0"},
		{"repos gone", hail(`{"children":[{"title":"t","body":"b"}]}`), vance, store.Order{ID: "ORD-0001", Division: "ops",
			Repos: []store.OrderRepo{{Name: "gone", BaseRef: "main"}}}, `order ORD-0001's repos: unknown repo "gone"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kids, why := d.checkSplit(tc.h, tc.o, tc.crew)
			require.Nil(t, kids)
			require.Contains(t, why, tc.why)
		})
	}
}

// A split allowed by policy, as the captain timeout's default_option would
// under a regs.yaml that drops order_split from captain_only, and
// countermanded to deny before the mission's create-children runs, creates
// no children. The decision and the countermand commit together here, as
// in TestHailCountermandBeforeDecideHail: the mission reads the answer
// only after both. The refusal is on the hail's rationale and in the log,
// and the agent's answer says so.
func TestSplitCountermandedBeforeCreateChildren(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, splitHail(opsChildren()...)))
	cfg := defaultRegs(t)
	cfg.CaptainOnly = slices.DeleteFunc(cfg.CaptainOnly, func(r regs.MatchRule) bool { return r.Category == comms.CategoryOrderSplit })
	b.regs = cfg
	hd := b.start(t)
	hd.addCommand(t)
	parent := hd.createCommandOrder(t)
	hd.waitLog(t, "hail H-0001 for the captain: from Command")
	h := hd.hail(t, "H-0001")

	zero := 0
	answer := comms.AnswerBody{HailID: h.ID, DecidedBy: comms.DecidedByPolicy, Decision: comms.OptionAllow, Option: &zero,
		Rationale: "captain timeout: unanswered after 1s, default_option 0"}
	ans, err := syntheticMessage(comms.TypeAnswer, answer, h.CrewID, parent)
	require.NoError(t, err)
	corr, err := syntheticMessage(comms.TypeCorrection, comms.CorrectionBody{HailID: h.ID, Ref: parent, Note: "deny"}, h.CrewID, parent)
	require.NoError(t, err)
	entry, err := hailDecidedEntry(h.CrewID, parent, answer)
	require.NoError(t, err)
	until := time.Now().Add(time.Hour)
	require.NoError(t, hd.d.store.DecideOpenHailAs(t.Context(), h.ID, store.HailDecision{DecidedBy: answer.DecidedBy,
		Decision: answer.Decision, Rationale: answer.Rationale, CountermandUntil: &until}, entry, func(tx *sql.Tx) error {
		if err := dbos.Send(hd.d.dbos, missionWorkflowID(parent), ans, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE hails SET decided_by = 'captain', decision = 'deny', rationale = '',
			countermanded_at = ? WHERE id = ?`, time.Now().UTC(), h.ID); err != nil {
			return err
		}
		if err := dbos.Send(hd.d.dbos, missionWorkflowID(parent), corr, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
			return err
		}
		return hd.d.notifyHailClosed(tx, h.ID)
	}))

	require.Equal(t, MissionDone, missionResult(t, hd.d, parent).Status)
	require.Equal(t, 1, strings.Count(strings.Join(stepNames(t, hd.d, missionWorkflowID(parent)), ","), "create-children"))
	const why = "the captain countermanded it: deny"
	require.Equal(t, store.OrderDone, hd.order(t, parent).State, "no children: done, not split")
	orders, err := hd.d.store.ListOrders(t.Context())
	require.NoError(t, err)
	require.Len(t, orders, 1)
	got := hd.hail(t, h.ID)
	require.Equal(t, "deny", got.Decision)
	require.Equal(t, "split refused: "+why, got.Rationale)
	hd.waitLog(t, "order split refused: "+why)
	steps := hailSteps(t, b.stepLog)
	require.Len(t, steps, 1)
	require.Equal(t, "order split refused: "+why, steps[0]["decision"])
}

// splitDaemon is a store-only Daemon (hailStore's way) with vance, of
// Command, away on an order on repo app, and a decided order_split hail
// on it, allowed, asking for children: enough for createChildren.
func splitDaemon(t *testing.T, children ...comms.OrderSplitChild) (*Daemon, MissionInput, string) {
	t.Helper()
	return splitDaemonDecided(t, comms.OptionAllow, children...)
}

// splitDaemonDecided is splitDaemon with the hail decided as decision,
// with option 0, as AnswerHail stores a captain's free text.
func splitDaemonDecided(t *testing.T, decision string, children ...comms.OrderSplitChild) (*Daemon, MissionInput, string) {
	t.Helper()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.DBFile))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	e := store.LogEntry{Kind: "test"}
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "vance", Name: "Vance", Division: DivisionCommand}, e))
	_, err = st.InsertOrder(ctx, store.Order{Title: "rfc", Assignee: "vance", Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}}, e)
	require.NoError(t, err)
	order, found, err := st.ClaimOldestDirectedOrder(ctx, "vance", e)
	require.NoError(t, err)
	require.True(t, found)
	raw, err := json.Marshal(comms.OrderSplitContext{Children: children})
	require.NoError(t, err)
	id, err := st.InsertHail(ctx, store.Hail{OrderID: order, CrewID: "vance", Category: comms.CategoryOrderSplit, Question: "q",
		Options: []string{comms.OptionAllow, comms.OptionDeny}, Context: raw}, e)
	require.NoError(t, err)
	require.NoError(t, st.DecideOpenHail(ctx, id, comms.DecidedByCaptain, decision, "", e, nil))
	d := &Daemon{store: st, logger: slog.New(slog.DiscardHandler), repos: map[string]string{"app": "x"},
		divisions: map[string]DivisionConfig{DivisionCommand: {}, "ops": {}},
		wakeSend:  func(string, CrewSignal) error { return nil }}
	return d, MissionInput{OrderID: order, CrewID: "vance"}, id
}

// A re-run of create-children after its insert committed returns the same
// children, even when checkSplit would now refuse (the roster lost ops).
func TestCreateChildrenRerun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	d, in, hail := splitDaemon(t, opsChildren()...)
	out, err := createChildren(ctx, d, in, hail)
	require.NoError(t, err)
	require.Equal(t, splitOutcome{Decision: "ORD-0002, ORD-0003"}, out)
	d.divisions = map[string]DivisionConfig{DivisionCommand: {}}
	again, err := createChildren(ctx, d, in, hail)
	require.NoError(t, err)
	require.Equal(t, out, again)
	orders, err := d.store.ListOrders(ctx)
	require.NoError(t, err)
	require.Len(t, orders, 3)
}

// An option 0 answer with free text (`bmus hails answer --option 0
// --decision "ok, go ahead"`) allows the split: only a countermand
// refuses one.
func TestCreateChildrenFreeTextAllow(t *testing.T) {
	t.Parallel()
	d, in, hail := splitDaemonDecided(t, "ok, go ahead", opsChildren()...)
	out, err := createChildren(t.Context(), d, in, hail)
	require.NoError(t, err)
	require.Equal(t, splitOutcome{Decision: "ORD-0002, ORD-0003"}, out)
	ids, err := d.store.ChildBatch(t.Context(), in.OrderID, splitCreatedBy(hail))
	require.NoError(t, err)
	require.Equal(t, []string{"ORD-0002", "ORD-0003"}, ids)
}

// A refused split notes it on the hail and logs it once, however often
// create-children runs; the hail keeps its decision.
func TestCreateChildrenRefusedOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	d, in, hail := splitDaemon(t, comms.OrderSplitChild{Title: "t", Body: "b", Division: "nowhere"})
	const why = `child 1: unknown division "nowhere" (the roster has command, ops)`
	for range 2 {
		out, err := createChildren(ctx, d, in, hail)
		require.NoError(t, err)
		require.Equal(t, splitOutcome{Decision: "order split refused: " + why, Refused: true}, out)
	}
	h, err := d.store.GetHail(ctx, hail)
	require.NoError(t, err)
	require.Equal(t, comms.OptionAllow, h.Decision)
	require.Equal(t, "split refused: "+why, h.Rationale)
	entries, err := d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if e.Text == "order split refused: "+why {
			n++
		}
	}
	require.Equal(t, 1, n)
	orders, err := d.store.ListOrders(ctx)
	require.NoError(t, err)
	require.Len(t, orders, 1)
}

// The bridge's split question: defaults resolved, titles and divisions
// clipped to one line, at most maxSplitChildren listed, and a malformed
// context named as such.
func TestSplitQuestion(t *testing.T) {
	t.Parallel()
	o := store.Order{ID: "ORD-0001", Division: "ops", Priority: store.PriorityYellow}
	red := store.PriorityRed
	raw := func(children ...comms.OrderSplitChild) json.RawMessage {
		b, err := json.Marshal(comms.OrderSplitContext{Children: children})
		require.NoError(t, err)
		return b
	}
	require.Equal(t, "Split ORD-0001 into 2 orders: 1) API [science, red]; 2) UI and docs [ops, yellow]",
		splitQuestion(o, raw(comms.OrderSplitChild{Title: " API ", Division: "science", Priority: &red},
			comms.OrderSplitChild{Title: "UI\nand docs"})))
	require.Equal(t, "Split ORD-0001 into 3 orders: 1) API [ops, yellow]; 2) UI [ops, yellow, after 1]; 3) docs [ops, yellow, after 1 and 2]",
		splitQuestion(o, raw(comms.OrderSplitChild{Title: "API"}, comms.OrderSplitChild{Title: "UI", After: []int{0}},
			comms.OrderSplitChild{Title: "docs", After: []int{0, 1, 0}})))
	require.Contains(t, splitQuestion(o, raw(comms.OrderSplitChild{Title: strings.Repeat("é", maxSplitTitle)})),
		strings.Repeat("é", maxSplitTitle)+" [", "a title checkSplit takes is shown whole")
	long := strings.Repeat("x", maxSplitQuestionTitle+1)
	q := splitQuestion(store.Order{ID: "ORD-0001"}, raw(comms.OrderSplitChild{Title: long}))
	require.Equal(t, "Split ORD-0001 into 1 orders: 1) "+clip(long, maxSplitQuestionTitle)+" [no division, routine]", q)
	many := splitQuestion(o, raw(slices.Repeat([]comms.OrderSplitChild{{Title: "t"}}, 12)...))
	require.True(t, strings.HasPrefix(many, "Split ORD-0001 into 12 orders: 1) t [ops, yellow];"), many)
	require.True(t, strings.HasSuffix(many, "; 10) t [ops, yellow]; and 2 more"), many)
	for _, c := range []string{`[]`, `{}`, `{"children":[]}`, `nope`} {
		require.Equal(t, "Split ORD-0001 into child orders: the request is malformed, and the bridge denies it",
			splitQuestion(o, json.RawMessage(c)))
	}
}
