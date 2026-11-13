package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// ledgerProvider is a watchProvider whose later beams run script later:
// serve takes the last --script it is given.
type ledgerProvider struct {
	*watchProvider
	later string

	mu    sync.Mutex
	first string
}

func (p *ledgerProvider) Create(ctx context.Context) (transporter.Beam, error) {
	beam, err := p.watchProvider.Create(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil && p.first == "" {
		p.first = beam.Alias
	}
	return beam, err
}

func (p *ledgerProvider) StartCombadge(ctx context.Context, beam transporter.Beam, spec transporter.CombadgeSpec) error {
	p.mu.Lock()
	later := beam.Alias != p.first
	p.mu.Unlock()
	if later {
		spec.Args = append(slices.Clone(spec.Args), "--script", p.later)
	}
	return p.watchProvider.StartCombadge(ctx, beam, spec)
}

// The budget ledger (#118): an order with no budget of its own takes its
// division's, and its first send carries that limit with nothing used.
// The first beam's usage report becomes an order_usage row; the beam's
// watch change moves the order to a new beam, whose order send carries the
// same limit and the first beam's usage, and whose own report adds a
// second row.
func TestBudgetLedger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first, later := filepath.Join(dir, "first.json"), filepath.Join(dir, "later.json")
	require.NoError(t, os.WriteFile(first, []byte(`[
		{"at_pct": 10, "usage": {"input_tokens": 3000, "output_tokens": 500, "turns": 8}},
		{"at_pct": 50, "sleep_ms": 120000},
		{"at_pct": 100, "report": {"summary": "never reached"}}]`), 0o600))
	require.NoError(t, os.WriteFile(later, []byte(`[
		{"at_pct": 50, "usage": {"input_tokens": 1000, "output_tokens": 200, "turns": 3}},
		{"at_pct": 100, "report": {"summary": "done after the watch change"}}]`), 0o600))
	b := newHolodeckBridge(t, first)
	const lead = time.Hour
	short, err := transporter.NewHolodeck(transporter.HolodeckConfig{
		Root: b.root, Combadge: combadgeBundle(t),
		Harness:  []string{"--harness", "holodeck", "--script", first},
		Lifetime: lead + 15*time.Second,
	})
	require.NoError(t, err)
	b.provider, b.lead = &ledgerProvider{watchProvider: &watchProvider{Holodeck: short}, later: later}, lead
	b.divisions = map[string]DivisionConfig{"science": {Budget: Budget{Tokens: 10000, Turns: 50}}}

	hd := b.start(t)
	c := hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Rank: "Lieutenant", Division: "science"})
	id := hd.createOrder(t)

	// sends is each run's (budget_tokens, budget_turns, used_tokens,
	// used_turns) as its first step logged it.
	sends := func(t require.TestingT) [][4]float64 {
		var out [][4]float64
		for _, e := range readSteps(t, b.stepLog) {
			if e["order"] != id || e["index"] != float64(0) {
				continue
			}
			var v [4]float64
			for i, k := range []string{"budget_tokens", "budget_turns", "used_tokens", "used_turns"} {
				v[i], _ = e[k].(float64)
			}
			out = append(out, v)
		}
		return out
	}
	usage := func(t require.TestingT) []store.Usage {
		u, err := hd.d.store.OrderUsage(context.Background(), id)
		require.NoError(t, err)
		return u
	}

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, [][4]float64{{10000, 50, 0, 0}}, sends(ct), "the division's budget, nothing used")
		u := usage(ct)
		require.Len(ct, u, 1)
		require.Equal(ct, store.Usage{Beam: c.CurrentBeam, InputTokens: 3000, OutputTokens: 500, Turns: 8, Seq: u[0].Seq}, u[0])
	}, 15*time.Second, 20*time.Millisecond)

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "done after the watch change", res.Report.Summary)
	require.Equal(t, [][4]float64{{10000, 50, 0, 0}, {10000, 50, 3500, 8}}, sends(t),
		"the second send carries the limit and the first beam's usage")

	var cr store.Crew
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cr = hd.crew(ct, "sorell")
		require.Equal(ct, 1, cr.TourNo)
	}, 10*time.Second, 50*time.Millisecond)
	require.NotEqual(t, c.CurrentBeam, cr.CurrentBeam)
	got := map[string][3]int64{}
	for _, u := range usage(t) {
		got[u.Beam] = [3]int64{u.InputTokens, u.OutputTokens, u.Turns}
	}
	require.Equal(t, map[string][3]int64{c.CurrentBeam: {3000, 500, 8}, cr.CurrentBeam: {1000, 200, 3}}, got)
}

// writeScript writes a holodeck script into a temp dir and returns its path.
func writeScript(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.json")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o600))
	return path
}

// createBudgetOrder creates a holodeck order for sorell with budget o's
// BudgetTokens and BudgetTurns.
func (hd *hdaemon) createBudgetOrder(t *testing.T, o store.Order) string {
	t.Helper()
	o.Title, o.Body, o.Assignee = "holodeck run", "walk the script", "sorell"
	o.Repos = []store.OrderRepo{{Name: "app", BaseRef: "main"}}
	id, err := hd.d.CreateOrder(t.Context(), o)
	require.NoError(t, err)
	return id
}

// budgetScript is a holodeck script that reaches turn 11, then steps on and
// reports: with a 10-turn budget it hails at index 3.
const budgetScript = `[
	{"at_pct": 10, "usage": {"input_tokens": 1000, "output_tokens": 100, "turns": 10}},
	{"at_pct": 20, "progress": "ten turns in"},
	{"at_pct": 30, "usage": {"input_tokens": 1100, "output_tokens": 110, "turns": 11}},
	{"at_pct": 40, "progress": "turn eleven"},
	{"at_pct": 100, "report": {"summary": "finished on an extended budget"}}]`

// #119's acceptance: an order with a 10-turn budget raises a budget hail at
// turn 11, and extend lets it finish. The hail is the captain's under a
// regs.yaml that drops budget from captain_only and auto-allows any
// command, and its default_option timeout holds instead of deciding it.
// The bridge writes the question; the answer, by label, runs budget-extend,
// which doubles the limit, and the harness, given the new limit, walks on.
func TestBudgetHailExtend(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, writeScript(t, budgetScript))
	path := filepath.Join(t.TempDir(), regs.File)
	require.NoError(t, os.WriteFile(path, []byte(`captain_only:
  - category: order_split
auto_allow:
  - tool: Bash
    match: '.*'
escalation:
  on_timeout: default_option
  default_option: 0
  captain_timeout: 1s
`), 0o600))
	cfg, found, err := regs.LoadRegs(path)
	require.NoError(t, err)
	require.True(t, found)
	b.regs = &cfg
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createBudgetOrder(t, store.Order{BudgetTurns: 10})

	h := hd.waitHail(t, "H-0001")
	require.Equal(t, comms.CategoryBudget, h.Category)
	require.Equal(t, []string{comms.OptionExtend, comms.OptionStop}, h.Options)
	require.Equal(t, id+" has used 1.2k tokens and 11 of 10 turns. Extend by its budget again, or stop?", h.Question)
	require.JSONEq(t, `{"used": 1210, "limit": 0, "turns_used": 11, "turns_limit": 10}`, string(h.Context))

	// The captain timeout passes and holds: default_option never decides it.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		texts := orderTexts(hd.logEntries(ct), id, "hail H-0001 ")
		require.Contains(ct, texts, "hail H-0001 for the captain: category budget is fixed to the captain")
		require.True(ct, slices.ContainsFunc(texts, func(s string) bool {
			return strings.Contains(s, "holding for the captain (default_option does not apply to a captain-only hail)")
		}), "%q", texts)
	}, 30*time.Second, 50*time.Millisecond)
	h = hd.waitHail(t, "H-0001")
	require.Equal(t, regs.RouteCaptain, h.Route)

	require.ErrorContains(t, hd.d.AnswerHail(t.Context(), "H-0001", "keep going", nil), "answer with an option (extend or stop)")
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "Extend", nil))
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "finished on an extended budget", res.Report.Summary)

	o := hd.order(t, id)
	require.Equal(t, int64(1), o.BudgetExtended)
	h, err = hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, [2]string{store.HailDecided, comms.OptionExtend}, [2]string{h.Status, h.Decision})
	require.Contains(t, orderTexts(hd.logEntries(t), id, "budget extended"),
		"budget extended for hail H-0001: limit now unlimited tokens, 20 turns")
	require.Contains(t, stepNames(t, hd.d, missionWorkflowID(id)), "budget-extend")

	var walked [][2]any
	for _, e := range readSteps(t, b.stepLog) {
		if e["order"] == id {
			walked = append(walked, [2]any{e["index"], e["action"]})
		}
	}
	require.Equal(t, [][2]any{{0.0, "usage"}, {1.0, "progress"}, {2.0, "usage"}, {3.0, "budget"}, {3.0, "progress"}, {4.0, "report"}}, walked)
}

// #119's acceptance: stop leaves a checkpoint in the cargo bay and fails
// the order budget_exceeded.
func TestBudgetHailStop(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, writeScript(t, budgetScript))
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createBudgetOrder(t, store.Order{BudgetTurns: 10})

	h := hd.waitHail(t, "H-0001")
	require.Equal(t, comms.CategoryBudget, h.Category)
	stop := 1
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &stop))
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionFailed, res.Status, "%+v", res)
	require.Equal(t, CodeBudgetExceeded, res.Code)
	require.Contains(t, res.Reason, "budget exceeded: the captain stopped "+id)
	o := hd.order(t, id)
	require.Equal(t, store.OrderFailed, o.State)
	require.Zero(t, o.BudgetExtended)

	cps, err := hd.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	note, err := os.ReadFile(cps[0].HandoverPath)
	require.NoError(t, err)
	require.Contains(t, string(note), "budget exceeded")
	require.FileExists(t, filepath.Join(b.home, "cargo", id, fmt.Sprintf("%s-%d", cps[0].Beam, cps[0].Seq), "app.bundle"))
	require.NotContains(t, stepNames(t, hd.d, missionWorkflowID(id)), "budget-extend")
}

// #119's acceptance: a script reporting usage past 125% of its budget is
// belayed by the bridge, once, though another report past it follows. A
// report under the ceiling, and a late one for the ended order, belay
// nothing.
func TestBudgetCeilingBelays(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, writeScript(t, `[
		{"at_pct": 10, "usage": {"input_tokens": 1000, "output_tokens": 200, "turns": 2}},
		{"at_pct": 20, "usage": {"input_tokens": 1200, "output_tokens": 100, "turns": 3}},
		{"at_pct": 30, "usage": {"input_tokens": 1300, "output_tokens": 100, "turns": 4}},
		{"at_pct": 50, "sleep_ms": 120000},
		{"at_pct": 100, "report": {"summary": "never reached"}}]`))
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createBudgetOrder(t, store.Order{BudgetTokens: 1000})

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.OrderBelayed, hd.order(ct, id).State)
	}, 30*time.Second, 50*time.Millisecond)
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionBelayed, res.Status, "%+v", res)
	require.Equal(t, budgetCeilingReason, res.Reason)
	// Every report is recorded, the last after the belay perhaps; only one
	// belay is requested.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		u, err := hd.d.store.OrderUsage(context.Background(), id)
		require.NoError(ct, err)
		require.Len(ct, u, 1)
		require.Equal(ct, int64(1300), u[0].InputTokens)
	}, 15*time.Second, 20*time.Millisecond)
	require.Equal(t, []string{budgetBelayText}, orderTexts(hd.logEntries(t), id, "belay requested"))
	for _, e := range hd.logEntries(t) {
		if e.OrderID == id && e.Text == budgetBelayText {
			var p map[string]any
			require.NoError(t, json.Unmarshal(e.Payload, &p))
			require.Equal(t, backstopValue, p[backstopField], "the backstop's mark, which a captain's belay lacks")
		}
	}

	// A late report for the belayed order, far past the ceiling.
	_, err := hd.d.store.RecordInbox(t.Context(), "sorell", 99, 1, func(tx store.InboxTx) error {
		if _, err := tx.RecordUsage(id, store.Usage{Beam: "late", InputTokens: 50000}); err != nil {
			return err
		}
		return hd.d.budgetCeiling(t.Context(), tx, "sorell", id)
	})
	require.NoError(t, err)
	require.Equal(t, []string{budgetBelayText}, orderTexts(hd.logEntries(t), id, "belay requested"))
}

func TestBudgetQuestion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		c    comms.BudgetContext
		want string
	}{
		{comms.BudgetContext{Used: 1_200_000, Limit: 1_000_000, TurnsUsed: 140, TurnsLimit: 150},
			"ORD-0001 has used 1.2M of 1M tokens and 140 of 150 turns. Extend by its budget again, or stop?"},
		{comms.BudgetContext{Used: 420_000, TurnsUsed: 11, TurnsLimit: 10},
			"ORD-0001 has used 420k tokens and 11 of 10 turns. Extend by its budget again, or stop?"},
		{comms.BudgetContext{Used: 3500, Limit: 3000, TurnsUsed: 7},
			"ORD-0001 has used 3.5k of 3k tokens and 7 turns. Extend by its budget again, or stop?"},
		{comms.BudgetContext{Used: 950, Limit: 900},
			"ORD-0001 has used 950 of 900 tokens and 0 turns. Extend by its budget again, or stop?"},
	} {
		require.Equal(t, tc.want, budgetQuestion("ORD-0001", tc.c))
	}
	raw, c := canonBudgetContext([]byte(`{"used": 5, "limit": 4, "turns_used": 1, "turns_limit": 0, "note": "x"}`))
	require.Equal(t, comms.BudgetContext{Used: 5, Limit: 4, TurnsUsed: 1}, c)
	require.JSONEq(t, `{"used": 5, "limit": 4, "turns_used": 1, "turns_limit": 0}`, string(raw))
	_, c = canonBudgetContext([]byte(`"not an object"`))
	require.Zero(t, c)
}

func TestCeilingPassed(t *testing.T) {
	t.Parallel()
	limit := Budget{Tokens: 1000, Turns: 100}
	require.False(t, ceilingPassed(Budget{Tokens: 1250, Turns: 125}, limit), "at 125% exactly")
	require.True(t, ceilingPassed(Budget{Tokens: 1251}, limit))
	require.True(t, ceilingPassed(Budget{Turns: 126}, limit))
	require.False(t, ceilingPassed(Budget{Tokens: 1 << 40, Turns: 1 << 20}, Budget{}), "unlimited")
	require.True(t, ceilingPassed(Budget{Turns: 13}, Budget{Turns: 10}))
}

// injectCrew sends msg, of type typ with body, to order id's mission as
// crew member sorell's beam would, with seq.
func injectCrew(t *testing.T, d *Daemon, id, typ string, seq uint64, body any) {
	t.Helper()
	msg, err := comms.NewMessage(typ, body)
	require.NoError(t, err)
	msg.Seq, msg.Crew, msg.Order = seq, "sorell", id
	require.NoError(t, dbos.Send(d.dbos, missionWorkflowID(id), msg, fromCrewTopic))
}

// fakeMission starts a daemon on a fake provider with sorell, of division
// science, on deck, and an order for it whose order command has gone out.
func fakeMission(t *testing.T) (running, string) {
	t.Helper()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell", Division: "science"})
	r := startDaemonWith(t, home, &fakeProvider{hello: true})
	t.Cleanup(func() { r.stop(t) })
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id, err := r.d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell", Division: "science",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, ok := stepOutput(c, r.d, missionWorkflowID(id), "send-order")
		require.True(c, ok)
	}, 20*time.Second, 20*time.Millisecond)
	return r, id
}

// A hail that arrives while another is open on the order (#119) waits its
// turn: the open hail keeps its category, so answering it extends no
// budget, and the budget hail is recorded once the first answer has gone
// out, and extends the budget when it is answered.
func TestMissionQueuesHailBehindOpenOne(t *testing.T) {
	t.Parallel()
	r, id := fakeMission(t)
	injectCrew(t, r.d, id, comms.TypeHail, 2, comms.HailBody{Ref: "q1", Category: comms.CategoryClarifyingQuestion,
		Question: "Which way?", Options: []string{"left", "right"}})
	injectCrew(t, r.d, id, comms.TypeHail, 3, comms.HailBody{Ref: "b1", Category: comms.CategoryBudget, Question: "?",
		Options: []string{comms.OptionExtend, comms.OptionStop}, Context: json.RawMessage(`{"used": 9, "limit": 5, "turns_used": 1, "turns_limit": 0}`)})
	hail := func(t require.TestingT, hid string) store.Hail {
		h, err := r.d.store.GetHail(context.Background(), hid)
		require.NoError(t, err)
		return h
	}
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Equal(c, store.HailOpen, hail(c, "H-0001").Status)
	}, 20*time.Second, 20*time.Millisecond)
	require.Equal(t, comms.CategoryClarifyingQuestion, hail(t, "H-0001").Category)

	left := 0
	require.NoError(t, r.d.AnswerHail(t.Context(), "H-0001", "", &left))
	var b store.Hail
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		b = hail(c, "H-0002")
		require.Equal(c, store.HailOpen, b.Status)
	}, 20*time.Second, 20*time.Millisecond)
	require.Equal(t, comms.CategoryBudget, b.Category)
	require.Equal(t, id+" has used 9 of 5 tokens and 1 turns. Extend by its budget again, or stop?", b.Question)
	require.NotContains(t, stepNames(t, r.d, missionWorkflowID(id)), "budget-extend")
	require.Zero(t, getOrder(t, r.d, id).BudgetExtended)

	require.NoError(t, r.d.AnswerHail(t.Context(), "H-0002", "extend", nil))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Equal(c, int64(1), getOrder(c, r.d, id).BudgetExtended)
	}, 20*time.Second, 20*time.Millisecond)
}

// A budget stop that meets a pending recall (#119) fails the order
// budget_exceeded, and the log says the recall was superseded.
func TestBudgetStopSupersedesRecall(t *testing.T) {
	t.Parallel()
	r, id := fakeMission(t)
	require.NoError(t, r.d.RecallOrder(t.Context(), id, "reassigning"))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, ok := stepOutput(c, r.d, missionWorkflowID(id), "send-recall")
		require.True(c, ok)
	}, 20*time.Second, 20*time.Millisecond)
	injectCrew(t, r.d, id, comms.TypeError, 2, comms.ErrorBody{Reason: "budget exceeded: the captain stopped " + id, Code: CodeBudgetExceeded})

	res := missionResult(t, r.d, id)
	require.Equal(t, MissionFailed, res.Status, "%+v", res)
	require.Equal(t, CodeBudgetExceeded, res.Code)
	require.Equal(t, store.OrderFailed, getOrder(t, r.d, id).State)
	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Equal(t, []string{"recall superseded by the budget stop"}, orderTexts(entries, id, "recall superseded"))
}

// order.budget and the OrderView's budget fields (#120): the current
// limits and their sources, resolved for the assignee's division when the
// order has none; usage summed over the order's beams; a change keeps the
// extensions and logs once; an ended order is refused.
func TestRPCOrderBudget(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.Divisions = map[string]DivisionConfig{"science": {Budget: Budget{Tokens: 10000, Turns: 50}}, "ops": {}}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	ctx := t.Context()
	repos := []OrderRepo{{Name: "app", BaseRef: "main"}}

	require.NoError(t, call(t, home, "crew.add", CrewAddArgs{ID: "sorell", Name: "Sorell", Division: "science"}, nil))
	var created OrderCreateResult
	require.NoError(t, call(t, home, "order.create", OrderCreateArgs{Title: "survey", Assignee: "sorell", Repos: repos,
		BudgetTurns: 20}, &created))
	id := created.ID
	view := func() OrderView {
		t.Helper()
		var res OrderListResult
		require.NoError(t, call(t, home, "order.list", nil, &res))
		for _, o := range res.Orders {
			if o.ID == id {
				return o
			}
		}
		t.Fatalf("order %s not listed", id)
		return OrderView{}
	}
	v := view()
	require.Equal(t, [3]int64{10000, 20, 0}, [3]int64{v.BudgetTokens, v.BudgetTurns, v.BudgetExtended})
	require.Equal(t, BudgetSource{Tokens: SourceDivision, Turns: SourceOrder}, v.BudgetSource, "sorell's division's tokens")
	require.Equal(t, UsageView{}, v.Usage)

	for i, u := range []store.Usage{{Beam: "beam-a", InputTokens: 3000, OutputTokens: 500, Turns: 8},
		{Beam: "beam-b", InputTokens: 1000, OutputTokens: 200, Turns: 3}} {
		_, err := r.d.store.RecordInbox(ctx, "sorell", 0, uint64(101+i), func(tx store.InboxTx) error {
			_, err := tx.RecordUsage(id, u)
			return err
		})
		require.NoError(t, err)
	}
	// One extension, as a budget hail's extend grants it.
	_, err := r.d.store.RecordInbox(ctx, "sorell", 0, 103, func(tx store.InboxTx) error {
		_, err := tx.SQL().ExecContext(ctx, `UPDATE orders SET budget_extended = 1 WHERE id = ?`, id)
		return err
	})
	require.NoError(t, err)
	v = view()
	require.Equal(t, UsageView{InputTokens: 4000, OutputTokens: 700, Turns: 11}, v.Usage, "summed over both beams")
	require.Equal(t, [3]int64{20000, 40, 1}, [3]int64{v.BudgetTokens, v.BudgetTurns, v.BudgetExtended})

	// The order's own tokens; its turns and its extension kept.
	tokens := int64(5000)
	require.NoError(t, call(t, home, "order.budget", OrderBudgetArgs{ID: id, Tokens: &tokens}, &v))
	require.Equal(t, [3]int64{10000, 40, 1}, [3]int64{v.BudgetTokens, v.BudgetTurns, v.BudgetExtended})
	require.Equal(t, BudgetSource{Tokens: SourceOrder, Turns: SourceOrder}, v.BudgetSource)
	require.Equal(t, repos, v.Repos)
	stored, err := r.d.store.GetOrder(ctx, id)
	require.NoError(t, err)
	require.Equal(t, [3]int64{5000, 20, 1}, [3]int64{stored.BudgetTokens, stored.BudgetTurns, stored.BudgetExtended})
	set := func() []string {
		entries, err := r.d.store.ListLog(ctx, 0)
		require.NoError(t, err)
		var out []string
		for _, e := range entries {
			if strings.HasPrefix(e.Text, "budget set") {
				out = append(out, e.Text)
			}
		}
		return out
	}
	require.Equal(t, []string{"budget set: limit now 10k tokens, 40 turns, with 1 extension"}, set())
	require.NoError(t, call(t, home, "order.budget", OrderBudgetArgs{ID: id, Tokens: &tokens}, &v))
	require.Len(t, set(), 1, "no change, no entry")
	// 0 hands the turns back to the division.
	zero := int64(0)
	require.NoError(t, call(t, home, "order.budget", OrderBudgetArgs{ID: id, Turns: &zero}, &v))
	require.Equal(t, [2]int64{10000, 100}, [2]int64{v.BudgetTokens, v.BudgetTurns})
	require.Equal(t, BudgetSource{Tokens: SourceOrder, Turns: SourceDivision}, v.BudgetSource)

	neg := int64(-1)
	re := requireRPCError(t, call(t, home, "order.budget", OrderBudgetArgs{ID: id}, nil), CodeBadRequest)
	require.Equal(t, "nothing to set: give tokens or turns", re.Message)
	re = requireRPCError(t, call(t, home, "order.budget", OrderBudgetArgs{ID: id, Tokens: &neg}, nil), CodeBadRequest)
	require.Equal(t, "budget tokens must not be negative", re.Message)
	requireRPCError(t, call(t, home, "order.budget", OrderBudgetArgs{Tokens: &tokens}, nil), CodeBadRequest)
	requireRPCError(t, call(t, home, "order.budget", OrderBudgetArgs{ID: "ORD-0404", Tokens: &tokens}, nil), CodeNotFound)
	requireRPCError(t, call(t, home, "order.budget", map[string]any{"id": id, "tokens": 1, "extra": 1}, nil), CodeBadRequest)

	// An ended order is refused, and keeps its budget.
	require.NoError(t, call(t, home, "order.create", OrderCreateArgs{Title: "pool", Division: "ops", Repos: repos}, &created))
	v = OrderView{}
	require.NoError(t, call(t, home, "order.withdraw", OrderActionArgs{ID: created.ID}, &v))
	require.Equal(t, BudgetSource{Tokens: SourceDefault, Turns: SourceDefault}, v.BudgetSource)
	re = requireRPCError(t, call(t, home, "order.budget", OrderBudgetArgs{ID: created.ID, Tokens: &tokens}, nil), CodeBadRequest)
	require.Equal(t, "order "+created.ID+" is withdrawn, too late to change its budget", re.Message)
	stored, err = r.d.store.GetOrder(ctx, created.ID)
	require.NoError(t, err)
	require.Zero(t, stored.BudgetTokens)
}
