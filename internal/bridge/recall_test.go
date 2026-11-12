package bridge

import (
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

// hailScript writes a holodeck script that commits, then hails and waits:
// a recall or belay finds the run in its hail wait. Answered, it reports.
func hailScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "hail.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 10, "progress": "surveying"},
		{"at_pct": 40, "commit": "a change"},
		{"at_pct": 50, "hail": {"category": "clarifying_question", "question": "Which way?", "options": ["left", "right"]}},
		{"at_pct": 100, "report": {"summary": "done after the hail"}}]`), 0o600))
	return script
}

// createDivisionOrder creates an order directed to assignee in division.
func (hd *hdaemon) createDivisionOrder(t *testing.T, assignee, division string) string {
	t.Helper()
	id, err := hd.d.CreateOrder(t.Context(), store.Order{Title: "holodeck run", Body: "walk the script", Assignee: assignee,
		Division: division, Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	return id
}

// commitOf is the commit order id's run made at its commit step on its nth
// (from 0) run, from the holodeck step log.
func commitOf(t require.TestingT, stepLog, id string, nth int) string {
	var commits []string
	for _, e := range readSteps(t, stepLog) {
		if e["order"] == id && e["action"] == "commit" {
			commits = append(commits, e["commits"].(map[string]any)["app"].(string))
		}
	}
	require.Greater(t, len(commits), nth)
	return commits[nth]
}

// #101's acceptance: a recall of an order in its hail wait. The beam writes
// its stopping checkpoint; the order goes back to the pool, unassigned and
// parked, its hail closed and its crew member on deck. Once standing orders
// are on, the same crew member's next claim resumes order/<id> from that
// checkpoint on the same beam, and the order finishes done.
func TestRecallResumesFromCheckpoint(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailScript(t))
	hd := b.start(t)
	c := hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	id := hd.createDivisionOrder(t, "sorell", "ops")
	hail := hd.waitHail(t, "H-0001")
	require.Equal(t, id, hail.OrderID)
	first := commitOf(t, b.stepLog, id, 0)

	require.NoError(t, hd.d.RecallOrder(t.Context(), id, "reprioritise"))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		o := hd.order(ct, id)
		require.Equal(ct, store.OrderPool, o.State)
		require.Empty(ct, o.Assignee)
		require.NotNil(ct, o.ParkedAt)
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
	}, 30*time.Second, 50*time.Millisecond)
	requirePending(t, hd.d, missionWorkflowID(id))
	h, err := hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, store.HailTimedOut, h.Status)
	cps, err := hd.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	require.Equal(t, c.CurrentBeam, cps[0].Beam)
	note := "Order " + id + ": stopped by recall: reprioritise. The holodeck stopped its script here."
	requireCheckpointFiles(t, b.home, cps[0], note, first)
	// Standing orders are off: nobody claims the pool order yet.
	time.Sleep(time.Second)
	require.Equal(t, store.OrderPool, hd.order(t, id).State)

	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))
	answer := hd.waitHail(t, "H-0002")
	require.Equal(t, id, answer.OrderID)
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0002", "left", nil))
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "done after the hail", res.Report.Summary)
	o := hd.order(t, id)
	require.Equal(t, store.OrderDone, o.State)
	require.Equal(t, "sorell", o.Assignee)
	require.Nil(t, o.ParkedAt)
	require.Len(t, workflows(t, hd.d, missionWorkflowID(id)), 1, "one mission throughout")

	// The same beam and tour, and the resumed run got the checkpoint's note
	// and committed on top of its commit.
	cr := hd.crew(t, "sorell")
	require.Equal(t, c.CurrentBeam, cr.CurrentBeam)
	require.Equal(t, 0, cr.TourNo)
	var resumed []map[string]any
	for _, e := range readSteps(t, b.stepLog) {
		if e["order"] == id && e["handover"] != nil {
			resumed = append(resumed, e)
		}
	}
	require.Len(t, resumed, 4, "the second run's four steps")
	require.Equal(t, note, resumed[0]["handover"])
	clone := filepath.Join(b.home, "repos", "app")
	require.Equal(t, res.Report.Repos[0].Commit, gitT(t, clone, "rev-parse", "bmus/"+id))
	require.Equal(t, first, gitT(t, clone, "rev-parse", "bmus/"+id+"^"))

	entries := hd.logEntries(t)
	require.Zero(t, kinds(entries)[logFail])
	require.Equal(t, []string{"recall requested: reprioritise"}, orderTexts(entries, id, "recall requested"))
	require.Equal(t, []string{"order recalled: reprioritise"}, orderTexts(entries, id, "order recalled"))
	require.Equal(t, []string{"order resumed on sorell"}, orderTexts(entries, id, "order resumed"))
	require.Equal(t, []string{"hail H-0001 closed unanswered: order recalled"}, orderTexts(entries, id, "hail H-0001 closed"))
	requireNoBackstop(t, entries)
	requirePending(t, hd.d, tourWorkflowID("sorell", 0))
}

// #101's acceptance: a recall of an order parked by beam loss, its crew
// member in sickbay, moves it to the pool at once, and another crew member
// of the division claims and resumes it from the checkpoint.
func TestRecallParkedInSickbay(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "script.json")
	data, err := os.ReadFile(fixture(t, "crash.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(script, data, 0o600))
	b := newHolodeckBridge(t, script)
	hd := b.start(t)
	c := hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	id := hd.createDivisionOrder(t, "sorell", "ops")
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cps, err := hd.d.store.ListCheckpoints(t.Context(), id)
		require.NoError(ct, err)
		require.Len(ct, cps, 1)
	}, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, b.h.Kill(c.CurrentBeam))
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, hd.d, tourWorkflowID("sorell", 0)))
	require.NotNil(t, hd.order(t, id).ParkedAt)

	// tuvok's serve reads the script's second version.
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 50, "progress": "resumed"},
		{"at_pct": 100, "report": {"summary": "done on tuvok"}}]`), 0o600))
	t2 := hd.addCrewWith(t, store.Crew{ID: "tuvok", Name: "Tuvok", Division: "ops"})

	require.NoError(t, hd.d.RecallOrder(t.Context(), id, ""))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		o := hd.order(ct, id)
		require.Equal(ct, store.OrderPool, o.State)
		require.Empty(ct, o.Assignee)
		require.NotNil(ct, o.ParkedAt)
	}, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "done on tuvok", res.Report.Summary)
	o := hd.order(t, id)
	require.Equal(t, store.OrderDone, o.State)
	require.Equal(t, "tuvok", o.Assignee)
	require.Len(t, workflows(t, hd.d, missionWorkflowID(id)), 1, "one mission throughout")
	require.Equal(t, store.CrewSickbay, hd.crew(t, "sorell").Status)
	var resumed []map[string]any
	for _, e := range readSteps(t, b.stepLog) {
		if e["order"] == id && e["handover"] != nil {
			resumed = append(resumed, e)
		}
	}
	require.Len(t, resumed, 2)
	require.Equal(t, "checkpoint before the beam is lost", resumed[0]["handover"])

	entries := hd.logEntries(t)
	require.Zero(t, kinds(entries)[logFail])
	require.Equal(t, []string{"order recalled: recall"}, orderTexts(entries, id, "order recalled"))
	require.Equal(t, []string{"order resumed on tuvok"}, orderTexts(entries, id, "order resumed"))
	require.True(t, slices.ContainsFunc(entries, func(e store.LogEntry) bool {
		return e.OrderID == id && e.Text == "order away: mission started" && e.CrewID == t2.ID
	}))
	requireNoBackstop(t, entries)
}

// #101's acceptance: a belay sent right after a recall settles the order
// belayed, whether the mission reads it while recalling or parked; the
// crew member is back on deck and nothing fails.
func TestBelayAfterRecall(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailScript(t))
	hd := b.start(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	id := hd.createDivisionOrder(t, "sorell", "ops")
	hd.waitHail(t, "H-0001")

	require.NoError(t, hd.d.RecallOrder(t.Context(), id, "change of plan"))
	require.NoError(t, hd.d.BelayOrder(t.Context(), id, "scrapped"))
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionResult{Status: MissionBelayed, Reason: "scrapped"}, res)
	o := hd.order(t, id)
	require.Equal(t, store.OrderBelayed, o.State)
	require.Nil(t, o.ParkedAt)
	h, err := hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, store.HailTimedOut, h.Status)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
	}, 10*time.Second, 50*time.Millisecond)
	requirePending(t, hd.d, tourWorkflowID("sorell", 0))

	entries := hd.logEntries(t)
	require.Zero(t, kinds(entries)[logFail])
	require.Equal(t, []string{"order belayed: scrapped"}, orderTexts(entries, id, "order belayed"))
	requireNoBackstop(t, entries)
	err = hd.d.RecallOrder(t.Context(), id, "")
	require.ErrorContains(t, err, "is belayed, too late to recall")
}

// One test of belay's transitions (#101): an order away in its hail wait is
// belayed (its hail closed, its crew member on deck, its beam's run
// stopped), a recalled order parked in the pool is belayed too, and a
// pool order nobody claimed, or a settled one, is refused.
func TestBelayOrder(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailScript(t))
	hd := b.start(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	away := hd.createDivisionOrder(t, "sorell", "ops")
	hd.waitHail(t, "H-0001")

	require.NoError(t, hd.d.BelayOrder(t.Context(), away, ""))
	require.Equal(t, MissionResult{Status: MissionBelayed}, missionResult(t, hd.d, away))
	require.Equal(t, store.OrderBelayed, hd.order(t, away).State)
	h, err := hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, store.HailTimedOut, h.Status)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
	}, 10*time.Second, 50*time.Millisecond)
	require.ErrorContains(t, hd.d.BelayOrder(t.Context(), away, ""), "is belayed, too late to belay")

	// The beam's run ended belayed: the order runs nowhere and its next
	// order goes through.
	recalled := hd.createDivisionOrder(t, "sorell", "ops")
	hd.waitHail(t, "H-0002")
	require.NoError(t, hd.d.RecallOrder(t.Context(), recalled, ""))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		o := hd.order(ct, recalled)
		require.Equal(ct, store.OrderPool, o.State)
		require.NotNil(ct, o.ParkedAt)
	}, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, hd.d.BelayOrder(t.Context(), recalled, "not needed"))
	require.Equal(t, MissionResult{Status: MissionBelayed, Reason: "not needed"}, missionResult(t, hd.d, recalled))
	o := hd.order(t, recalled)
	require.Equal(t, store.OrderBelayed, o.State)
	require.Nil(t, o.ParkedAt)

	pool := hd.createPoolOrder(t, "ops", store.PriorityRoutine)
	require.ErrorContains(t, hd.d.BelayOrder(t.Context(), pool, ""), "has no mission to belay: withdraw it")
	require.Equal(t, store.OrderPool, hd.order(t, pool).State)
	require.ErrorIs(t, hd.d.BelayOrder(t.Context(), "ORD-9999", ""), ErrNotFound)

	entries := hd.logEntries(t)
	require.Zero(t, kinds(entries)[logFail])
	require.Equal(t, []string{"order belayed"}, orderTexts(entries, away, "order belayed"))
	require.Equal(t, []string{"belay requested"}, orderTexts(entries, away, "belay requested"))
	requireNoBackstop(t, entries)
	var belayed int
	for _, e := range readSteps(t, b.stepLog) {
		if e["order"] == away && e["action"] == "report" {
			belayed++
		}
	}
	require.Zero(t, belayed, "the belayed order never reported")
}

// orderDaemon is a daemon with no beams for withdraw and route: crew rows
// go straight into the store, so no tour claims anything, and wake-ups are
// recorded.
type orderDaemon struct {
	running
	mu    sync.Mutex
	woken []string
}

func startOrderDaemon(t *testing.T) *orderDaemon {
	t.Helper()
	od := &orderDaemon{}
	cfg := testConfig(privateHome(t))
	cfg.Divisions = map[string]DivisionConfig{"ops": {}, "eng": {}}
	cfg.wakeSend = func(tourID string, sig CrewSignal) error {
		od.mu.Lock()
		defer od.mu.Unlock()
		od.woken = append(od.woken, tourID)
		return nil
	}
	od.running = startDaemonConfig(t, cfg)
	t.Cleanup(func() { od.stop(t) })
	for _, c := range []store.Crew{
		{ID: "kirk", Name: "Kirk", Division: "ops", Status: store.CrewOnDeck},
		{ID: "spock", Name: "Spock", Division: "ops", Status: store.CrewSickbay},
	} {
		require.NoError(t, od.d.store.InsertCrew(t.Context(), c, testEntry))
	}
	return od
}

func (od *orderDaemon) insertOrder(t *testing.T, o store.Order) string {
	t.Helper()
	id, err := od.d.store.InsertOrder(t.Context(), o, testEntry)
	require.NoError(t, err)
	return id
}

// recalledOrder inserts an order of ops away with kirk, parks and recalls
// it: in the pool, unassigned and parked.
func (od *orderDaemon) recalledOrder(t *testing.T) string {
	t.Helper()
	id := od.insertOrder(t, store.Order{Title: "r", Assignee: "kirk", Division: "ops", State: store.OrderAway})
	_, err := od.d.store.ParkOrder(t.Context(), id, testEntry)
	require.NoError(t, err)
	_, recalled, err := od.d.store.RecallOrder(t.Context(), id, "kirk", testEntry)
	require.NoError(t, err)
	require.True(t, recalled)
	return id
}

// One test of withdraw's transitions (#101): a pool or directed order is
// withdrawn, once; a recalled (parked) one is refused with a pointer to
// belay, and a claimed or settled one as too late.
func TestWithdrawOrder(t *testing.T) {
	t.Parallel()
	od := startOrderDaemon(t)
	ctx := t.Context()
	pool := od.insertOrder(t, store.Order{Title: "p", Division: "ops"})
	directed := od.insertOrder(t, store.Order{Title: "d", Assignee: "kirk"})
	away := od.insertOrder(t, store.Order{Title: "a", Assignee: "kirk", State: store.OrderAway})
	parked := od.recalledOrder(t)

	require.NoError(t, od.d.WithdrawOrder(ctx, pool, "duplicate"))
	require.NoError(t, od.d.WithdrawOrder(ctx, directed, ""))
	require.Equal(t, store.OrderWithdrawn, getOrder(t, od.d, pool).State)
	require.Equal(t, store.OrderWithdrawn, getOrder(t, od.d, directed).State)
	require.ErrorContains(t, od.d.WithdrawOrder(ctx, pool, ""), "order "+pool+" is withdrawn, too late to withdraw")
	require.ErrorContains(t, od.d.WithdrawOrder(ctx, away, ""), "order "+away+" is away, too late to withdraw")
	err := od.d.WithdrawOrder(ctx, parked, "")
	require.ErrorContains(t, err, "order "+parked+" is parked; use belay")
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	require.Equal(t, store.OrderPool, getOrder(t, od.d, parked).State)
	require.ErrorIs(t, od.d.WithdrawOrder(ctx, "ORD-9999", ""), ErrNotFound)

	entries, err := od.d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"order withdrawn: duplicate"}, orderTexts(entries, pool, "order withdrawn"))
	require.Equal(t, []string{"order withdrawn"}, orderTexts(entries, directed, "order withdrawn"))
}

// One test of route's transitions (#101): a pool order is directed to a
// crew member and back to a pool, between divisions; a recalled order
// keeps its parked mission; a bad assignee or division, an order left with
// neither, and a claimed order are refused. A route to a pool wakes the
// division.
func TestRouteOrder(t *testing.T) {
	t.Parallel()
	od := startOrderDaemon(t)
	ctx := t.Context()
	str := func(s string) *string { return &s }
	id := od.insertOrder(t, store.Order{Title: "p", Division: "ops"})

	require.NoError(t, od.d.RouteOrder(ctx, id, str("kirk"), nil))
	o := getOrder(t, od.d, id)
	require.Equal(t, []string{store.OrderDirected, "kirk", "ops"}, []string{o.State, o.Assignee, o.Division})
	require.NoError(t, od.d.RouteOrder(ctx, id, str(""), str("eng")))
	o = getOrder(t, od.d, id)
	require.Equal(t, []string{store.OrderPool, "", "eng"}, []string{o.State, o.Assignee, o.Division})
	od.mu.Lock()
	require.Empty(t, od.woken, "eng has nobody on deck")
	od.mu.Unlock()
	require.NoError(t, od.d.RouteOrder(ctx, id, nil, str("ops")))
	od.mu.Lock()
	require.Equal(t, []string{tourWorkflowID("kirk", 0)}, od.woken, "kirk is ops's on-deck crew member")
	od.mu.Unlock()

	require.ErrorIs(t, od.d.RouteOrder(ctx, id, str("spock"), nil), ErrUnavailable)
	require.ErrorIs(t, od.d.RouteOrder(ctx, id, str("nobody"), nil), ErrNotFound)
	err := od.d.RouteOrder(ctx, id, nil, str("science"))
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	require.ErrorContains(t, od.d.RouteOrder(ctx, id, nil, str("")), "neither an assignee nor a division")
	require.ErrorContains(t, od.d.RouteOrder(ctx, id, nil, nil), "nothing to route")
	require.NoError(t, od.d.RouteOrder(ctx, id, str(""), str("ops")), "a route that changes nothing")
	o = getOrder(t, od.d, id)
	require.Equal(t, []string{store.OrderPool, "", "ops"}, []string{o.State, o.Assignee, o.Division}, "refusals change nothing")

	parked := od.recalledOrder(t)
	require.NoError(t, od.d.RouteOrder(ctx, parked, str("kirk"), nil))
	o = getOrder(t, od.d, parked)
	require.Equal(t, store.OrderDirected, o.State)
	require.NotNil(t, o.ParkedAt, "kirk's claim resumes the parked mission")

	away := od.insertOrder(t, store.Order{Title: "a", Assignee: "kirk", State: store.OrderAway})
	require.ErrorContains(t, od.d.RouteOrder(ctx, away, nil, str("eng")), "order "+away+" is away, too late to route")
	require.ErrorIs(t, od.d.RouteOrder(ctx, "ORD-9999", nil, str("ops")), ErrNotFound)

	entries, err := od.d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, []string{"order routed to kirk", "order routed to the eng pool", "order routed to the ops pool"},
		orderTexts(entries, id, "order routed"))
}

// Recall's refusals (#101): an order without a division points at route
// and belay; an unclaimed one at route; a parked one whose crew member is
// on deck (being handed to a new beam) is "being resumed"; a settled one is
// too late. One whose crew member is beaming in is accepted (#106). Nothing is logged or sent for any of them.
func TestRecallRefusals(t *testing.T) {
	t.Parallel()
	od := startOrderDaemon(t)
	ctx := t.Context()
	noDiv := od.insertOrder(t, store.Order{Title: "n", Assignee: "kirk", State: store.OrderAway})
	err := od.d.RecallOrder(ctx, noDiv, "")
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	require.ErrorContains(t, err, "order "+noDiv+" has no division")
	require.ErrorContains(t, err, "bmus order route")
	require.ErrorContains(t, err, "bmus order belay")

	pool := od.insertOrder(t, store.Order{Title: "p", Division: "ops"})
	require.ErrorContains(t, od.d.RecallOrder(ctx, pool, ""), "order "+pool+" is pool, not claimed yet: route it instead")

	resuming := od.insertOrder(t, store.Order{Title: "w", Assignee: "kirk", Division: "ops", State: store.OrderAway})
	_, err = od.d.store.ParkOrder(ctx, resuming, testEntry)
	require.NoError(t, err)
	require.ErrorContains(t, od.d.RecallOrder(ctx, resuming, ""), "order "+resuming+" is being resumed by kirk; try again")

	require.NoError(t, od.d.store.InsertCrew(ctx, store.Crew{ID: "scotty", Name: "Scotty", Division: "ops",
		Status: store.CrewDismissed}, testEntry))
	failing := od.insertOrder(t, store.Order{Title: "f", Assignee: "scotty", Division: "ops", State: store.OrderAway})
	_, err = od.d.store.ParkOrder(ctx, failing, testEntry)
	require.NoError(t, err)
	require.ErrorContains(t, od.d.RecallOrder(ctx, failing, ""), "order "+failing+" is being failed: its crew member scotty was dismissed")

	done := od.insertOrder(t, store.Order{Title: "d", Division: "ops", State: store.OrderDone})
	require.ErrorContains(t, od.d.RecallOrder(ctx, done, ""), "order "+done+" is done, too late to recall")
	require.ErrorIs(t, od.d.RecallOrder(ctx, "ORD-9999", ""), ErrNotFound)

	entries, err := od.d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Text, "recall requested")
	}

	// #106 review: a parked order whose crew member is beaming in (its new
	// tour claims nothing until its hello, perhaps a whole tsh login wait
	// away) can be recalled, as from sickbay.
	require.NoError(t, od.d.store.InsertCrew(ctx, store.Crew{ID: "worf", Name: "Worf", Division: "ops",
		Status: store.CrewBeamingIn}, testEntry))
	beaming := od.insertOrder(t, store.Order{Title: "b", Assignee: "worf", Division: "ops", State: store.OrderAway})
	_, err = od.d.store.ParkOrder(ctx, beaming, testEntry)
	require.NoError(t, err)
	// The check lets it through: only the delivery fails, as this order has
	// no mission workflow.
	err = od.d.RecallOrder(ctx, beaming, "reprioritise")
	require.ErrorContains(t, err, "deliver recall to "+missionWorkflowID(beaming))
	require.NotEqual(t, CodeBadRequest, errorCode(ctx, err))
}

// A recalled order whose parked mission is gone (#101 review): the claimer
// resumes into a dead workflow, mission-alive reads it ended, and
// settle-mission fails the order although it is parked, rather than leave it
// to be claimed again every re-check.
func TestClaimOfParkedOrderWithDeadMission(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailScript(t))
	b.checkEvery = 2 * time.Second
	hd := b.start(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	id := hd.createDivisionOrder(t, "sorell", "ops")
	hd.waitHail(t, "H-0001")
	require.NoError(t, hd.d.RecallOrder(t.Context(), id, ""))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		o := hd.order(ct, id)
		require.Equal(ct, store.OrderPool, o.State)
		require.NotNil(ct, o.ParkedAt)
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
	}, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, dbos.CancelWorkflow(hd.d.dbos, missionWorkflowID(id)))

	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.OrderFailed, hd.order(ct, id).State)
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
	}, 30*time.Second, 50*time.Millisecond)
	o := hd.order(t, id)
	require.Nil(t, o.ParkedAt)
	require.Equal(t, "sorell", o.Assignee)
	entries := hd.logEntries(t)
	require.Equal(t, []string{"mission ended without settling"}, orderTexts(entries, id, "mission ended"))
	require.Len(t, claims(entries), 2, "claimed once more, then failed")
	requirePending(t, hd.d, tourWorkflowID("sorell", 0))
}

// A recalled mission with no crew member that ends through bridge_error
// fails the order a tour has just claimed, its resume unread, and tells
// that tour mission_done, as a belay does (#113): the claimer is back on
// deck at once, not after mission-alive's next check. The bridge error is
// the mission's unpark step failing for good.
func TestRecalledMissionBridgeErrorReleasesClaimer(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailScript(t))
	hd := b.start(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	id := hd.createDivisionOrder(t, "sorell", "ops")
	hd.waitHail(t, "H-0001")
	require.NoError(t, hd.d.RecallOrder(t.Context(), id, ""))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		o := hd.order(ct, id)
		require.Equal(ct, store.OrderPool, o.State)
		require.NotNil(ct, o.ParkedAt)
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
	}, 30*time.Second, 50*time.Millisecond)
	db, err := sql.Open("sqlite", filepath.Join(b.home, store.DBFile)+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER refuse_resume BEFORE INSERT ON log
		WHEN NEW.text LIKE 'order resumed on %'
		BEGIN SELECT RAISE(ABORT, 'injected: resume refused'); END`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionFailed, res.Status)
	require.Equal(t, CodeBridgeError, res.Code)
	tourID := tourWorkflowID("sorell", 0)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.OrderFailed, hd.order(ct, id).State)
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
		// The step is recorded after it sets the crew member on deck.
		require.True(ct, hasStep(ct, hd.d, tourID, "settle-mission"))
	}, 20*time.Second, 50*time.Millisecond, "the claimer is released by mission_done")
	require.False(t, hasStep(t, hd.d, tourID, "mission-alive"), "not by mission-alive")
	entries := hd.logEntries(t)
	require.Len(t, claims(entries), 2, "claimed, then released")
	require.Empty(t, orderTexts(entries, id, "mission ended"), "the mission settled its order itself")
}

// A belay that reaches serve while it is still checking the order's repos
// out ends the run only after the checkout (#101 review). The mission waits
// for that end before it frees its tour, so the tour's next order isn't
// sent to a serve still running the belayed one, which would refuse it as
// busy. serve is held (SIGSTOP) while the order, the belay and, without the
// wait, the next order reach it.
func TestBelayDuringCheckoutDoesNotBusyNextOrder(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, shortScript(t))
	b.heartbeatStale = time.Minute // serve is held for seconds: no recycle
	hd := b.start(t)
	c := hd.addCrew(t)
	data, err := os.ReadFile(filepath.Join(b.root, c.CurrentBeam, "serve.pid"))
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	require.NoError(t, err)
	require.NoError(t, syscall.Kill(pid, syscall.SIGSTOP))
	resumed := false
	resume := func() {
		if !resumed {
			resumed = true
			assert.NoError(t, syscall.Kill(pid, syscall.SIGCONT))
		}
	}
	defer resume()

	first := hd.createOrder(t)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.True(ct, hasStep(ct, hd.d, missionWorkflowID(first), "send-order"))
	}, 30*time.Second, 20*time.Millisecond)
	require.NoError(t, hd.d.BelayOrder(t.Context(), first, "wrong repo"))
	next := hd.createOrder(t)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.True(ct, hasStep(ct, hd.d, missionWorkflowID(first), "send-belay"))
	}, 30*time.Second, 20*time.Millisecond)
	// Without the wait, the tour claims the next order and sends it now.
	time.Sleep(2 * time.Second)
	resume()

	require.Equal(t, MissionResult{Status: MissionBelayed, Reason: "wrong repo"}, missionResult(t, hd.d, first))
	missionDone(t, hd.d, next)
	require.Equal(t, store.OrderBelayed, hd.order(t, first).State)
	require.Equal(t, store.OrderDone, hd.order(t, next).State)
	requireNoBackstop(t, hd.logEntries(t))
}
