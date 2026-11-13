package bridge

import (
	"slices"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// hasStep reports whether workflow id has recorded a step named name.
func hasStep(t require.TestingT, d *Daemon, id, name string) bool {
	steps, err := dbos.GetWorkflowSteps(d.dbos, id)
	require.NoError(t, err)
	return slices.ContainsFunc(steps, func(s dbos.StepInfo) bool { return s.StepName == name })
}

// A dismissal that reaches a tour in its watch change's settle wait, which
// reads only its mission's signals, still ends the tour dismissed: next-tour
// finds the crew member dismissed in the store, starts no tour 1 and removes
// the beam (#100).
func TestDismissDuringWatchChangeSettleWait(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	// In the store before the daemon starts, so the tour's first claim
	// takes it: a claim after the watch-change time would leave it to tour
	// 1 (#138).
	id := insertOrder(t, home, store.Order{Title: "t", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	// The order's ship-repos never finishes copying, so its mission reads
	// neither the stand_down nor the beam_lost before the tour gives up.
	// The watch changes 10s after the beam is made, and the settle wait
	// lasts 10s: room for the claim, and for the dismissal to land in the
	// settle wait, on a loaded machine.
	p := &fakeProvider{hello: true, copyGate: make(chan struct{}), lifetime: time.Hour + 10*time.Second}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.watchChangeLead = time.Hour
	cfg.standDownTimeout = 200 * time.Millisecond
	cfg.settleTimeout = 10 * time.Second
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)

	tourID := tourWorkflowID("sorell", 0)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.True(c, hasStep(c, r.d, tourID, "settle-deadline"))
	}, 30*time.Second, 20*time.Millisecond, "the tour reaches its settle wait")
	require.True(t, hasStep(t, r.d, tourID, "stand-down-deadline"), "the settle wait is the watch change's")
	outcome, err := r.d.DismissCrew(t.Context(), "sorell")
	require.NoError(t, err)
	require.Equal(t, OutcomeRequested, outcome)

	require.Equal(t, CrewTourResult{Ended: store.CrewDismissed}, tourResult(t, r.d, tourID))
	cr := getCrew(t, r.d, "sorell")
	require.Equal(t, store.CrewDismissed, cr.Status)
	require.Equal(t, 0, cr.TourNo)
	require.Empty(t, workflows(t, r.d, tourWorkflowID("sorell", 1)), "no next tour")
	require.Equal(t, 1, p.createdCount())
	require.Equal(t, []string{"fake-1"}, p.removed())
	require.Equal(t, MissionFailed, missionResult(t, r.d, id).Status)
}

// A dismissal queued while the tour waits on its beam, read by no one when
// the tour then ends in sickbay (its pump never starts), still ends it
// dismissed and removes the beam (#100).
func TestDismissBeforeTourSickbay(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{hello: true}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.tunePump = func(c *comms.PumpConfig) { c.CrewID = "" } // StartPump refuses it
	cfg.pumpBackoff = 300 * time.Millisecond
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Equal(c, "fake-1", getCrew(c, r.d, "sorell").CurrentBeam)
	}, 10*time.Second, 20*time.Millisecond, "provisioned")
	tourID := tourWorkflowID("sorell", 0)
	require.False(t, hasStep(t, r.d, tourID, "start-pump"), "the pump is still failing")
	outcome, err := r.d.DismissCrew(t.Context(), "sorell")
	require.NoError(t, err)
	require.Equal(t, OutcomeRequested, outcome)

	require.Equal(t, CrewTourResult{Ended: store.CrewDismissed}, tourResult(t, r.d, tourID))
	require.True(t, hasStep(t, r.d, tourID, "tour-sickbay"), "ended through tourSickbay")
	require.Equal(t, store.CrewDismissed, getCrew(t, r.d, "sorell").Status)
	require.Equal(t, []string{"fake-1"}, p.removed())
}

// A beam made inside the watch-change lead is logged, and its tour claims
// nothing: it changes the watch at once rather than take an order only to
// stand it down (#100 review).
func TestWatchDueClaimsNothing(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{hello: true, lifetime: time.Minute}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.watchChangeLead = time.Hour
	// Tours rotate as fast as beams are made: lift the creation rate cap.
	cfg.TransporterLimits = TransporterLimits{RatePerMinute: 1000}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	id, err := r.d.store.InsertOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}}, testEntry)
	require.NoError(t, err)

	require.Equal(t, CrewTourResult{Ended: TourEndedWatchChange}, tourResult(t, r.d, tourWorkflowID("sorell", 0)))
	out, ok := stepOutput(t, r.d, tourWorkflowID("sorell", 0), "claim-order")
	require.True(t, ok)
	require.JSONEq(t, `{"OrderID":"","Found":false,"WatchDue":true}`, out)
	require.Equal(t, store.OrderDirected, getOrder(t, r.d, id).State)
	require.Empty(t, workflows(t, r.d, missionWorkflowID(id)))
	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(entries, func(e store.LogEntry) bool {
		return e.Text == "beam fake-1 expires at "+mustExpiry(t, r.d, "fake-1")+
			", within the watch-change lead 1h0m0s: its tour changes the watch at once"
	}), "the short-lived beam is logged")

	// Every tour rotates at once; dismissal stops it.
	_, err = r.d.DismissCrew(t.Context(), "sorell")
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		cr := getCrew(c, r.d, "sorell")
		require.Equal(c, store.CrewDismissed, cr.Status)
		live, _, err := workflowLive(r.d, tourWorkflowID("sorell", cr.TourNo))
		require.NoError(c, err)
		require.False(c, live)
	}, 20*time.Second, 50*time.Millisecond)
	require.Equal(t, store.OrderDirected, getOrder(t, r.d, id).State)
}

// mustExpiry is beam alias's recorded ExpiresAt, as provision logs it.
func mustExpiry(t *testing.T, d *Daemon, alias string) string {
	beams, err := d.store.ListBeams(t.Context(), "sorell")
	require.NoError(t, err)
	for _, b := range beams {
		if b.Alias == alias {
			return b.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	require.Fail(t, "no beam", alias)
	return ""
}

// DismissCrew signals the tour its own transaction read: a NextTour that
// commits between an earlier read and the dismissal must not make it
// signal the old tour (#113). Here tour 1 never started, so DismissCrew
// removes the beam itself, and tour 0 gets no dismissed signal.
func TestDismissSignalsTourReadInTransaction(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{hello: true}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.dismissHook = func(d *Daemon, crewID string) {
		changed, err := d.store.NextTour(t.Context(), crewID, 0, testEntry)
		assert.NoError(t, err)
		assert.True(t, changed)
	}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")

	requireOutcome(t, OutcomeDone)(r.d.DismissCrew(t.Context(), "sorell"))
	require.Equal(t, []string{"fake-1"}, p.removed())
	beams, err := r.d.store.ListBeams(t.Context(), "sorell")
	require.NoError(t, err)
	require.Len(t, beams, 1)
	require.NotNil(t, beams[0].RemovedAt, "DismissCrew sets the beam's removed_at (#111)")
	tourID := tourWorkflowID("sorell", 0)
	live, _, err := workflowLive(r.d, tourID)
	require.NoError(t, err)
	require.True(t, live, "tour 0 was not signalled")
	require.False(t, hasStep(t, r.d, tourID, "tour-end"))
}

// A dismissal that commits between next-tour's dismissed read and its
// NextTour ends the tour dismissed: NextTour reports no change, no tour 1
// starts, and the tour removes its beam (#113).
func TestDismissInsideNextTour(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{hello: true, lifetime: time.Minute}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.watchChangeLead = time.Hour // the watch changes at once
	cfg.nextTourHook = func(d *Daemon, crewID string) {
		_, _, err := d.store.DismissCrew(t.Context(), crewID, store.LogEntry{Kind: logCrew, CrewID: crewID, Text: "crew dismissed"}, nil)
		assert.NoError(t, err)
	}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)

	tourID := tourWorkflowID("sorell", 0)
	require.Equal(t, CrewTourResult{Ended: store.CrewDismissed}, tourResult(t, r.d, tourID))
	require.True(t, hasStep(t, r.d, tourID, "next-tour"))
	cr := getCrew(t, r.d, "sorell")
	require.Equal(t, store.CrewDismissed, cr.Status)
	require.Equal(t, 0, cr.TourNo)
	require.Empty(t, workflows(t, r.d, tourWorkflowID("sorell", 1)), "no next tour")
	require.Equal(t, 1, p.createdCount())
	require.Equal(t, []string{"fake-1"}, p.removed())
}
