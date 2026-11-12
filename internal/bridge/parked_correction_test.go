package bridge

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// parkedAfterCommand starts a holodeck bridge whose order has H-0001
// decided by Command and H-0002 held for the captain, then kills sorell's
// beam: the tour ends in sickbay and the mission parks, so nothing resumes
// the order until sorell is re-energized (#167). With division, sorell is
// in ops and the order goes to ops, so a recall can hand it to another
// crew member there. It returns the script too: a serve started later
// reads what the test writes to it.
func parkedAfterCommand(t *testing.T, division bool) (*holodeckBridge, *hdaemon, string, string) {
	t.Helper()
	script := commandScript(t,
		map[string]any{"decision": "answer", "option": 0, "rationale": "deploys are routine", "confidence": 0.9},
		toolHail("make deploy"), toolHail("git push origin main"))
	b := newHolodeckBridge(t, script)
	b.regs = defaultRegs(t)
	hd := b.start(t)
	hd.addCommand(t)
	var (
		c  store.Crew
		id string
	)
	if division {
		c = hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
		id = hd.createDivisionOrder(t, "sorell", "ops")
	} else {
		c = hd.addCrew(t)
		id = hd.createOrder(t)
	}
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCommand}, hailResult(t, hd.d, "H-0001"))
	hd.waitLog(t, "hail H-0002 for the captain")

	require.NoError(t, b.h.Kill(c.CurrentBeam))
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, hd.d, tourWorkflowID("sorell", 0)))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.NotNil(ct, hd.order(ct, id).ParkedAt)
	}, 30*time.Second, 20*time.Millisecond)
	return b, hd, id, script
}

// #167's acceptance: countermands of a parked order's Command-decided hail
// are held by its mission and reach the resumed run right after its
// order, with the hail's question (#190). Two while parked deliver only
// the last. With restart, the bridge stops once the mission has held both
// and a new one replays it: the held corrections come back from its
// recorded messages.
func TestCountermandParkedDeliveredOnResume(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			t.Parallel()
			testCountermandParkedDelivered(t, restart)
		})
	}
}

func testCountermandParkedDelivered(t *testing.T, restart bool) {
	b, hd, id, _ := parkedAfterCommand(t, false)
	mission := missionWorkflowID(id)

	deny, allow := 1, 0
	res, err := hd.d.countermandHail(t.Context(), HailsCountermandArgs{HailID: "H-0001", Option: &deny, Reason: "staging is frozen"})
	require.NoError(t, err)
	require.Equal(t, "order "+id+" is parked: the agent is told on resume", res.Note)
	require.NoError(t, countermand(t.Context(), hd, "H-0001", "", &allow, "thawed"))
	require.Empty(t, corrections(t, b.stepLog))
	if restart {
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(b.logFile)
			return err == nil && strings.Count(string(data), "mission: holding correction for the resume") == 2
		}, 30*time.Second, 20*time.Millisecond)
		hd.stop(t)
		hd = b.start(t)
	}

	// Both corrections are queued before the resume, so the mission reads
	// them parked.
	requireOutcome(t, OutcomeRequested)(hd.d.ReenergizeCrew(t.Context(), "sorell"))
	// The resumed run hails again: H-0003 (Command) then H-0004 (the
	// captain). The correction went out before either.
	hd.waitHail(t, "H-0004")
	// It names the hail's question, which the resumed run never asked
	// (#190).
	require.Equal(t, []comms.CorrectionBody{{HailID: "H-0001", Ref: id, Note: "allow (thawed)", Question: "Run make deploy?"}},
		corrections(t, b.stepLog))

	// In the step log it follows the first run's lines and is the new
	// serve's first delivery after the order.
	steps := readSteps(t, b.stepLog)
	firstHail := slices.IndexFunc(steps, func(e map[string]any) bool {
		return e["order"] == id && e["action"] == "hail" && e["index"] == float64(0)
	})
	corr := slices.IndexFunc(steps, func(e map[string]any) bool { return e["action"] == "correction" })
	require.GreaterOrEqual(t, firstHail, 0)
	require.Greater(t, corr, firstHail)
	require.Equal(t, id, steps[corr]["order"])
	require.Equal(t, float64(1), steps[corr]["delivery"])

	// The mission sent it right after the resume's send-order.
	names := stepNames(t, hd.d, mission)
	require.Equal(t, 1, countSteps(t, hd.d, mission, "send-correction"))
	at := slices.Index(names, "send-correction")
	require.Greater(t, at, slices.Index(names, "unpark"))
	require.Equal(t, "send-order", names[at-1])

	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0004", "", &allow))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	for _, e := range hd.logEntries(t) {
		require.NotContains(t, e.Text, "not delivered")
	}
}

// #167: a belay of a parked order ends its mission with a correction still
// held; it is logged as not delivered.
func TestCountermandParkedThenBelayed(t *testing.T) {
	t.Parallel()
	b, hd, id, _ := parkedAfterCommand(t, false)
	deny := 1
	require.NoError(t, countermand(t.Context(), hd, "H-0001", "", &deny, ""))
	require.NoError(t, hd.d.BelayOrder(t.Context(), id, "scrapped"))
	require.Equal(t, MissionBelayed, missionResult(t, hd.d, id).Status)

	e := hd.waitLog(t, "correction for hail H-0001 not delivered: ")
	require.Equal(t, "correction for hail H-0001 not delivered: the order was belayed before it resumed", e.Text)
	require.Equal(t, id, e.OrderID)
	require.Zero(t, countSteps(t, hd.d, missionWorkflowID(id), "send-correction"))
	require.Empty(t, corrections(t, b.stepLog))
}

// #167: a held correction for sorell's hail isn't delivered to tuvok, who
// resumes the order after a recall: the mission logs it as not delivered.
func TestCountermandParkedResumedElsewhere(t *testing.T) {
	t.Parallel()
	b, hd, id, script := parkedAfterCommand(t, true)
	deny := 1
	require.NoError(t, countermand(t.Context(), hd, "H-0001", "", &deny, ""))

	// tuvok's serve reads the script's second version.
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 100, "report": {"summary": "done on tuvok"}}]`), 0o600))
	hd.addCrewWith(t, store.Crew{ID: "tuvok", Name: "Tuvok", Division: "ops"})
	require.NoError(t, hd.d.RecallOrder(t.Context(), id, ""))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.OrderPool, hd.order(ct, id).State)
	}, 30*time.Second, 20*time.Millisecond)
	require.NoError(t, hd.d.SetStandingOrders(t.Context(), true))

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "tuvok", hd.order(t, id).Assignee)
	e := hd.waitLog(t, "correction for hail H-0001 not delivered: ")
	require.Equal(t, "correction for hail H-0001 not delivered: the order resumed on tuvok, not sorell", e.Text)
	require.Equal(t, "sorell", e.CrewID)
	require.Zero(t, countSteps(t, hd.d, missionWorkflowID(id), "send-correction"))
	require.Empty(t, corrections(t, b.stepLog))
}
