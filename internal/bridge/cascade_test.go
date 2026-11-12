package bridge

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// cascadeScript is the holodeck script of the cascade tests: a split into
// three ops children, then a `git push` hail for the captain, then the
// report. Command's split goes to the captain; the children's are denied
// by policy (sorell isn't Command), and their push hail holds them for
// the captain.
func cascadeScript(t *testing.T) string {
	t.Helper()
	kids := append(opsChildren(), comms.OrderSplitChild{Title: "child three", Body: "the rest", Division: "ops"})
	return hailsScript(t, splitHail(kids...), toolHail("git push origin main"))
}

// cascadeBridge starts a bridge with the default regs.yaml, Command and
// sorell (ops), and Command's order: Command's split, H-0001, is allowed,
// creating ORD-0002 to ORD-0004 in the ops pool, and its push hail,
// H-0002, is open for the captain. Standing orders are off.
func cascadeBridge(t *testing.T) (*hdaemon, string) {
	t.Helper()
	b := newHolodeckBridge(t, cascadeScript(t))
	b.regs = defaultRegs(t)
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	parent := hd.createCommandOrder(t)
	hd.waitLog(t, "hail H-0001 for the captain")
	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	hd.waitLog(t, "hail H-0002 for the captain")
	hd.waitOrderState(t, parent, store.OrderHailing)
	require.Equal(t, 3, hd.order(t, parent).OpenChildren)
	return hd, parent
}

// cascadeEntry waits for order id's cascade entry and returns its text.
func (hd *hdaemon) cascadeEntry(t *testing.T, id string) string {
	t.Helper()
	e := hd.waitLog(t, "belay cascaded to ")
	require.Equal(t, id, e.OrderID)
	return e.Text
}

// requireNoOrphans requires no entry saying children run on, or a parent
// failed by its children.
func requireNoOrphans(t *testing.T, entries []store.LogEntry) {
	t.Helper()
	for _, e := range entries {
		require.NotContains(t, e.Text, "which run on")
		require.NotContains(t, e.Text, "child orders did not finish")
	}
}

// #146's acceptance, through the bridge: belaying a split parent with one
// child done, one in flight (hailing) and one in the pool withdraws the
// pool child, belays the one in flight through its mission and leaves the
// done one alone. The parent ends belayed at once, says so in one entry
// naming all three, and stays belayed once its last child settles. The
// belay reports the two open children it reached. A second belay is
// too late.
func TestBelaySplitParentCascades(t *testing.T) {
	t.Parallel()
	hd, parent := cascadeBridge(t)
	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0002", "", &allow))
	require.Equal(t, MissionDone, missionResult(t, hd.d, parent).Status)
	require.Equal(t, store.OrderSplit, hd.order(t, parent).State)

	// sorell runs ORD-0002 to done, then claims ORD-0003 and hails.
	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))
	hd.waitHail(t, "H-0004")
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0004", "", &allow))
	missionDone(t, hd.d, "ORD-0002")
	h := hd.waitHail(t, "H-0006")
	require.Equal(t, "ORD-0003", h.OrderID)
	require.Equal(t, store.OrderPool, hd.order(t, "ORD-0004").State)
	require.Equal(t, 2, hd.order(t, parent).OpenChildren)

	res, err := hd.d.belayAction(t.Context(), OrderActionArgs{ID: parent, Reason: "wrong plan"})
	require.NoError(t, err)
	require.Equal(t, 2, res.Children)
	require.Equal(t, store.OrderBelayed, res.State, "a split order has no mission: belayed at once")
	require.True(t, res.Belayed)
	require.Equal(t, store.OrderWithdrawn, hd.order(t, "ORD-0004").State)
	require.Equal(t, store.OrderDone, hd.order(t, "ORD-0002").State)
	require.Equal(t, "belay cascaded to 3 child orders: ORD-0002 already done, ORD-0003 belay requested, ORD-0004 withdrawn",
		hd.cascadeEntry(t, parent))
	hd.waitLog(t, "order belayed: wrong plan")

	require.Equal(t, MissionBelayed, missionResult(t, hd.d, "ORD-0003").Status)
	require.Equal(t, store.OrderBelayed, hd.order(t, "ORD-0003").State)
	hd.waitLog(t, "hail H-0006 closed unanswered: order belayed")
	hd.waitLog(t, "order belayed: its parent "+parent+" was belayed")
	p := hd.order(t, parent)
	require.Equal(t, store.OrderBelayed, p.State)
	require.Zero(t, p.OpenChildren)
	requireNoOrphans(t, hd.logEntries(t))

	err = hd.d.BelayOrder(t.Context(), parent, "")
	require.ErrorContains(t, err, "order "+parent+" is belayed, too late to belay")
	require.Equal(t, CodeBadRequest, errorCode(t.Context(), err))
}

// A parent still away (hailing) when the captain belays it: its mission
// settles it belayed and, in that same step, cascades: the child in flight
// gets a belay of its own, the pool children are withdrawn. The belay
// reports the three open children it reaches. No orphan entry.
func TestBelayAwayParentCascades(t *testing.T) {
	t.Parallel()
	hd, parent := cascadeBridge(t)
	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))
	h := hd.waitHail(t, "H-0004")
	require.Equal(t, "ORD-0002", h.OrderID)

	res, err := hd.d.belayAction(t.Context(), OrderActionArgs{ID: parent})
	require.NoError(t, err)
	require.Equal(t, 3, res.Children)
	require.Equal(t, MissionBelayed, missionResult(t, hd.d, parent).Status)
	require.Equal(t, "belay cascaded to 3 child orders: ORD-0002 belay requested, ORD-0003 withdrawn, ORD-0004 withdrawn",
		hd.cascadeEntry(t, parent))
	require.Contains(t, stepNames(t, hd.d, missionWorkflowID(parent)), "order-belayed")
	require.Equal(t, MissionBelayed, missionResult(t, hd.d, "ORD-0002").Status)
	for id, state := range map[string]string{parent: store.OrderBelayed, "ORD-0002": store.OrderBelayed,
		"ORD-0003": store.OrderWithdrawn, "ORD-0004": store.OrderWithdrawn} {
		require.Equal(t, state, hd.order(t, id).State, id)
	}
	hd.waitLog(t, "hail H-0002 closed unanswered: order belayed")
	hd.waitLog(t, "hail H-0004 closed unanswered: order belayed")
	requireNoOrphans(t, hd.logEntries(t))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Equal(c, store.CrewOnDeck, hd.crew(c, "sorell").Status)
	}, 30*time.Second, 20*time.Millisecond)
	n := 0
	for _, e := range hd.logEntries(t) {
		if strings.HasPrefix(e.Text, "belay cascaded to ") {
			n++
		}
	}
	require.Equal(t, 1, n, "one cascade entry")
}

// A belay the captain sends while the parent's mission is pulling its
// report is read after order-done settles the order split: belay-split
// belays it and cascades, and the pool children are withdrawn. The
// mission still ends done.
func TestBelayDuringReportCascades(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailsScript(t, splitHail(opsChildren()...)))
	b.regs = defaultRegs(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	b.pullWorkHook = func(_ *Daemon, orderID string) {
		if orderID != "ORD-0001" {
			return
		}
		enterOnce.Do(func() { close(entered) })
		<-release
	}
	hd := b.start(t)
	hd.addCommand(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	parent := hd.createCommandOrder(t)
	require.Equal(t, "ORD-0001", parent)
	hd.waitLog(t, "hail H-0001 for the captain")
	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	select {
	case <-entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the parent's mission never reached pull-work")
	}

	res, err := hd.d.belayAction(t.Context(), OrderActionArgs{ID: parent, Reason: "wrong plan"})
	require.NoError(t, err)
	require.Equal(t, 2, res.Children)
	require.Equal(t, store.OrderAway, res.State, "held in pull-work")
	require.False(t, res.Belayed, "queued for the mission")
	releaseOnce.Do(func() { close(release) })

	require.Equal(t, MissionDone, missionResult(t, hd.d, parent).Status)
	require.Contains(t, stepNames(t, hd.d, missionWorkflowID(parent)), "belay-split")
	hd.waitLog(t, "split, waiting on its child orders")
	require.Equal(t, "belay cascaded to 2 child orders: ORD-0002 withdrawn, ORD-0003 withdrawn", hd.cascadeEntry(t, parent))
	hd.waitLog(t, "order belayed: wrong plan")
	for id, state := range map[string]string{parent: store.OrderBelayed, "ORD-0002": store.OrderWithdrawn, "ORD-0003": store.OrderWithdrawn} {
		require.Equal(t, state, hd.order(t, id).State, id)
	}
	requireNoOrphans(t, hd.logEntries(t))
}
