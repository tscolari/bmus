package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// tourResult waits for tourID to end and returns its result. The timeout is
// a failure bound, as missionResult's is: on a loaded runner a tour's end
// (a mission parking, the settle wait for it) can take over 20s (#193).
func tourResult(t *testing.T, d *Daemon, tourID string) CrewTourResult {
	t.Helper()
	h, err := dbos.RetrieveWorkflow[CrewTourResult](d.dbos, tourID)
	require.NoError(t, err)
	res, err := h.GetResult(dbos.WithHandleTimeout(60 * time.Second))
	require.NoError(t, err)
	return res
}

// testEntry is the captain's log entry tests pass to a store method they
// call directly.
var testEntry = store.LogEntry{Kind: "test", Text: "test"}

func getCrew(t require.TestingT, d *Daemon, id string) store.Crew {
	c, err := d.store.GetCrew(context.Background(), id)
	require.NoError(t, err)
	return c
}

func waitOnDeck(t *testing.T, d *Daemon, id, beam string) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		cr := getCrew(c, d, id)
		require.Equal(c, store.CrewOnDeck, cr.Status)
		require.Equal(c, beam, cr.CurrentBeam)
		_, ok := d.Pump(id)
		require.True(c, ok)
	}, 10*time.Second, 20*time.Millisecond)
}

// A bridge that stops between Create and serve coming up leaves the beam in
// the beams table only: no current_beam, so no pump attaches to a beam with
// no serve. The next daemon's provision re-run reuses that beam, calling
// StartCombadge on it again, instead of creating another. A recorded beam
// that is gone is replaced.
func TestProvisionResumesRecordedBeam(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})

	// First daemon: serve never comes up.
	p1 := &fakeProvider{startGate: make(chan struct{})}
	r := startDaemonWith(t, home, p1)
	require.Eventually(t, func() bool { return len(p1.started()) > 0 }, 10*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"fake-1"}, p1.started())
	beams, err := r.d.store.ListBeams(t.Context(), "sorell")
	require.NoError(t, err)
	require.Len(t, beams, 1)
	require.Equal(t, "fake-1", beams[0].Alias)
	c := getCrew(t, r.d, "sorell")
	require.Empty(t, c.CurrentBeam, "current_beam is only set once serve is up")
	_, ok := r.d.Pump("sorell")
	require.False(t, ok)
	r.stop(t)
	require.Empty(t, p1.attached())

	// A second crew member whose recorded beam is gone.
	insertCrew(t, home, store.Crew{ID: "kirk", Name: "Kirk"})
	st, err := store.Open(t.Context(), home+"/"+store.DBFile)
	require.NoError(t, err)
	require.NoError(t, st.InsertBeam(t.Context(), store.BeamRecord{UUID: "fake-gone", Alias: "fake-gone", CrewID: "kirk"}, testEntry))
	require.NoError(t, st.Close())

	// Second daemon: the provision step re-runs.
	p2 := &fakeProvider{gone: map[string]bool{"fake-gone": true}, created: 1} // aliases from fake-2
	r = startDaemonWith(t, home, p2)
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	waitOnDeck(t, r.d, "kirk", "fake-2")
	require.Equal(t, 2, p2.createdCount(), "one Create: only the gone beam was replaced")
	require.ElementsMatch(t, []string{"fake-1", "fake-2"}, p2.started())
	beams, err = r.d.store.ListBeams(t.Context(), "sorell")
	require.NoError(t, err)
	require.Len(t, beams, 1, "sorell's beam was reused")
	beams, err = r.d.store.ListBeams(t.Context(), "kirk")
	require.NoError(t, err)
	require.Len(t, beams, 2)
}

// heartbeat_missed names its pump's beam; the tour only acts on its own
// beam's, and a stale pump's signal doesn't use up the real one's
// idempotency key.
func TestHeartbeatMissedForAnotherBeamIgnored(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	r := startDaemonWith(t, home, &fakeProvider{})
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	tourID := tourWorkflowID("sorell", 0)

	stale := pumpSink{d: r.d, crewID: "sorell", tourNo: 0, beam: "fake-old"}
	require.NoError(t, stale.HeartbeatMissed(t.Context()))
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		steps, err := dbos.GetWorkflowSteps(r.d.dbos, tourID)
		require.NoError(c, err)
		require.Equal(c, 1, countSignal(steps, SignalHeartbeatMissed))
	}, 10*time.Second, 50*time.Millisecond, "the stale signal never reached the tour")
	requirePending(t, r.d, tourID)
	require.Equal(t, store.CrewOnDeck, getCrew(t, r.d, "sorell").Status)

	real := pumpSink{d: r.d, crewID: "sorell", tourNo: 0, beam: "fake-1"}
	require.NoError(t, real.HeartbeatMissed(t.Context()))
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, r.d, tourID))
	c := getCrew(t, r.d, "sorell")
	require.Equal(t, store.CrewSickbay, c.Status)
	require.Equal(t, "fake-1", c.CurrentBeam)
	_, ok := r.d.Pump("sorell")
	require.False(t, ok, "pump stopped")
	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	require.Equal(t, logBeam, last.Kind)
	var p map[string]string
	require.NoError(t, json.Unmarshal(last.Payload, &p))
	require.Equal(t, CodeBeamLost, p["code"])
}

// Dismissal ends the tour, stops its pump and sets the crew row dismissed,
// so a restarted daemon attaches no pump to it (#35).
func TestDismissStopsPump(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	r := startDaemonWith(t, home, &fakeProvider{})
	waitOnDeck(t, r.d, "sorell", "fake-1")
	dismiss(t, r.d, tourWorkflowID("sorell", 0))
	_, ok := r.d.Pump("sorell")
	require.False(t, ok)
	c := getCrew(t, r.d, "sorell")
	require.Equal(t, store.CrewDismissed, c.Status)
	require.Equal(t, "fake-1", c.CurrentBeam, "the beam is kept")
	r.stop(t)

	// ensurePumps and attachCrewTours have run by the time Run is ready.
	p := &fakeProvider{}
	r = startDaemonWith(t, home, p)
	defer r.stop(t)
	require.Equal(t, store.CrewDismissed, getCrew(t, r.d, "sorell").Status)
	_, ok = r.d.Pump("sorell")
	require.False(t, ok, "no pump for a dismissed crew member")
	require.Empty(t, p.attached())
	wfs := workflows(t, r.d, tourWorkflowID("sorell", 0))
	require.Len(t, wfs, 1)
	require.Equal(t, dbos.WorkflowStatusSuccess, wfs[0].Status)
	_, err := r.d.CreateOrder(t.Context(), store.Order{Title: "x", Assignee: "sorell"})
	require.ErrorIs(t, err, ErrUnavailable)
}

// A mission that ends through bridge_error doesn't know its open hail; its
// order-failed looks it up and closes it (#47), and the tour's
// settle-mission finds it closed, so AnswerHail can't "succeed" into a
// workflow that is gone (#35) and the hail is logged closed once. The bridge
// error is a store step (decide-hail) failing for good while the hail is
// open: a trigger refuses its log entry, which rolls its decision back
// (#69).
func TestBridgeErrorClosesOpenHail(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	db, err := sql.Open("sqlite", filepath.Join(home, store.DBFile)+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER refuse_hail_log BEFORE INSERT ON log
		WHEN NEW.kind = 'hail' AND NEW.payload LIKE '%"decided_by"%'
		BEGIN SELECT RAISE(ABORT, 'injected: hail log refused'); END`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	r := startDaemonWith(t, home, &fakeProvider{hello: true})
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	id, err := r.d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell", Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, r.d, missionWorkflowID(id)), 1)
	}, 10*time.Second, 20*time.Millisecond)

	hail, err := comms.NewMessage(comms.TypeHail, comms.HailBody{Ref: "r1", Category: comms.CategoryToolApproval,
		Question: "Push?", Options: []string{comms.OptionAllow, comms.OptionDeny}})
	require.NoError(t, err)
	hail.Seq, hail.Crew, hail.Order = 2, "sorell", id
	require.NoError(t, dbos.Send(r.d.dbos, missionWorkflowID(id), hail, fromCrewTopic))
	// Straight to the mission, as AnswerHail's would be, but with the hail
	// still open: decide-hail decides it and fails on its log entry.
	// Production can't do this (AnswerHail decides first), but it is a
	// store step failing for good while a hail is open, which is what
	// order-failed's closeOpenHail must handle whatever the step.
	answer, err := comms.NewMessage(comms.TypeAnswer, comms.AnswerBody{HailID: "H-0001", Decision: comms.OptionAllow})
	require.NoError(t, err)
	answer.Crew, answer.Order = "sorell", id
	require.NoError(t, dbos.Send(r.d.dbos, missionWorkflowID(id), answer, fromCrewTopic))

	res := missionResult(t, r.d, id)
	require.Equal(t, MissionFailed, res.Status)
	require.Equal(t, CodeBridgeError, res.Code)
	require.Equal(t, store.OrderFailed, getOrder(t, r.d, id).State)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		h, err := r.d.store.GetHail(context.Background(), "H-0001")
		require.NoError(c, err)
		require.Equal(c, id, h.OrderID)
		require.Equal(c, store.HailTimedOut, h.Status)
		require.Equal(c, store.CrewOnDeck, getCrew(c, r.d, "sorell").Status)
	}, 10*time.Second, 20*time.Millisecond)
	require.ErrorIs(t, r.d.AnswerHail(t.Context(), "H-0001", comms.OptionAllow, nil), store.ErrHailClosed)

	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	closed := 0
	for _, e := range entries {
		if e.Text == "hail H-0001 closed unanswered" {
			closed++
		}
	}
	require.Equal(t, 1, closed)
	requirePending(t, r.d, tourWorkflowID("sorell", 0))
}

// A mission whose tour no longer backstops it closes its own open hail when
// it ends through bridge_error (#47). The mission runs outside the tour, on
// an away order with no assignee that the tour's claim never returns. The
// tour ignores its mission_done as stale, as an ended tour would never read
// it: only the mission's order-failed can close the hail. The bridge error is
// decide-hail failing for good while the hail is open, as in
// TestBridgeErrorClosesOpenHail.
func TestBridgeErrorClosesHailWithoutTour(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	db, err := sql.Open("sqlite", filepath.Join(home, store.DBFile)+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER refuse_hail_log BEFORE INSERT ON log
		WHEN NEW.kind = 'hail' AND NEW.payload LIKE '%"decided_by"%'
		BEGIN SELECT RAISE(ABORT, 'injected: hail log refused'); END`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	r := startDaemonWith(t, home, &fakeProvider{hello: true})
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	ctx := t.Context()
	id, err := r.d.store.InsertOrder(ctx, store.Order{Title: "t", State: store.OrderAway}, testEntry)
	require.NoError(t, err)
	_, err = dbos.RunWorkflow(r.d.dbos, missionWorkflow,
		MissionInput{OrderID: id, CrewID: "sorell", TourID: tourWorkflowID("sorell", 0)},
		dbos.WithWorkflowID(missionWorkflowID(id)))
	require.NoError(t, err)

	hail, err := comms.NewMessage(comms.TypeHail, comms.HailBody{Ref: "r1", Category: comms.CategoryToolApproval,
		Question: "Push?", Options: []string{comms.OptionAllow, comms.OptionDeny}})
	require.NoError(t, err)
	hail.Seq, hail.Crew, hail.Order = 2, "sorell", id
	require.NoError(t, dbos.Send(r.d.dbos, missionWorkflowID(id), hail, fromCrewTopic))
	// Straight to the mission, as AnswerHail's would be, but with the hail
	// still open: decide-hail decides it and fails on its log entry.
	// Production can't do this (AnswerHail decides first), but it is a
	// store step failing for good while a hail is open, which is what
	// order-failed's closeOpenHail must handle whatever the step.
	answer, err := comms.NewMessage(comms.TypeAnswer, comms.AnswerBody{HailID: "H-0001", Decision: comms.OptionAllow})
	require.NoError(t, err)
	answer.Crew, answer.Order = "sorell", id
	require.NoError(t, dbos.Send(r.d.dbos, missionWorkflowID(id), answer, fromCrewTopic))

	res := missionResult(t, r.d, id)
	require.Equal(t, MissionFailed, res.Status)
	require.Equal(t, CodeBridgeError, res.Code)
	require.Equal(t, store.OrderFailed, getOrder(t, r.d, id).State)

	// Closed by the time the mission returns, not later by a tour.
	h, err := r.d.store.GetHail(ctx, "H-0001")
	require.NoError(t, err)
	require.Equal(t, id, h.OrderID)
	require.Equal(t, store.HailTimedOut, h.Status)
	require.ErrorIs(t, r.d.AnswerHail(ctx, "H-0001", comms.OptionAllow, nil), store.ErrHailClosed)

	entries, err := r.d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	closed := 0
	for _, e := range entries {
		if e.Text == "hail H-0001 closed unanswered" {
			closed++
		}
	}
	require.Equal(t, 1, closed)
}

// isRecvDecodeError matches DBOS's error text, so check it against a real
// Recv that fails to decode: a DBOS upgrade that changes the text fails
// here, not by silently ending tours and missions on a bad message.
func TestIsRecvDecodeErrorMatchesDBOS(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	uri, err := store.FileURI(filepath.Join(dir, "dbos.db"))
	require.NoError(t, err)
	ctx, err := dbos.NewContext(context.Background(), dbos.Config{
		AppName:            "recv-decode-test",
		DatabaseURL:        "sqlite:" + uri,
		ApplicationVersion: "test",
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	defer dbos.Shutdown(ctx, 10*time.Second)

	type outcome struct {
		Decode, Timeout bool
		Text            string
	}
	recv := func(ctx dbos.Context, _ string) (outcome, error) {
		_, err := dbos.Recv[CrewSignal](ctx, crewEventsTopic, 20*time.Second)
		if err == nil {
			return outcome{}, errors.New("a string decoded as a CrewSignal")
		}
		var o outcome
		o.Decode, o.Text = isRecvDecodeError(err), err.Error()
		// A Recv timeout is a DBOS error, never taken for a decode error.
		_, err = dbos.Recv[CrewSignal](ctx, crewEventsTopic, 10*time.Millisecond)
		o.Timeout = errors.Is(err, dbos.ErrTimeout) && !isRecvDecodeError(err)
		return o, nil
	}
	dbos.RegisterWorkflow(ctx, recv, dbos.WithWorkflowName("recv-decode"))
	require.NoError(t, dbos.Launch(ctx))

	h, err := dbos.RunWorkflow(ctx, recv, "", dbos.WithWorkflowID("recv-decode-1"))
	require.NoError(t, err)
	require.NoError(t, dbos.Send(ctx, "recv-decode-1", "not a crew signal", crewEventsTopic))
	o, err := h.GetResult(dbos.WithHandleTimeout(20 * time.Second))
	require.NoError(t, err)
	require.True(t, o.Decode, "isRecvDecodeError no longer matches DBOS's decode error: %q", o.Text)
	require.True(t, o.Timeout)
}

func getOrder(t require.TestingT, d *Daemon, id string) store.Order {
	o, err := d.store.GetOrder(context.Background(), id)
	require.NoError(t, err)
	return o
}

// A pump that can't start is retried inside the start-pump step, whose
// recorded outcome (not a fresh EnsurePump) sends the crew member to
// sickbay with pump_failed (#35).
func TestPumpStartFailureIsRecorded(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan *Daemon, 1)
	cfg := testConfig(home)
	cfg.onReady = func(d *Daemon) { ready <- d }
	cfg.tunePump = func(c *comms.PumpConfig) { c.Link = nil } // StartPump rejects it
	cfg.pumpBackoff = 10 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	var r running
	select {
	case d := <-ready:
		r = running{d: d, cancel: cancel, done: done}
	case err := <-done:
		cancel()
		t.Fatalf("Run returned before ready: %v", err)
	}
	defer r.stop(t)

	tourID := tourWorkflowID("sorell", 0)
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, r.d, tourID))
	require.Equal(t, store.CrewSickbay, getCrew(t, r.d, "sorell").Status)
	_, ok := r.d.Pump("sorell")
	require.False(t, ok)

	steps, err := dbos.GetWorkflowSteps(r.d.dbos, tourID)
	require.NoError(t, err)
	var start *dbos.StepInfo
	for i := range steps {
		if steps[i].StepName == "start-pump" {
			start = &steps[i]
		}
	}
	require.NotNil(t, start, "%+v", steps)
	require.Nil(t, start.Error)
	out, ok := start.Output.(string)
	require.True(t, ok, "%T", start.Output)
	var ps pumpStart
	require.NoError(t, json.Unmarshal([]byte(out), &ps))
	require.Contains(t, ps.Failed, "Link is required")

	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	var p map[string]string
	require.NoError(t, json.Unmarshal(last.Payload, &p))
	require.Equal(t, CodePumpFailed, p["code"])
}

// The tour starts serve with the crew member's harness, none (serve's
// default, sdk) unless crew add named one, alongside the daemon's combadge
// args (#58), and with --role command in the command division (#155).
func TestProvisionPassesCrewHarness(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	p := &fakeProvider{hello: true}
	r := startDaemonWith(t, home, p)
	defer r.stop(t)
	require.NoError(t, r.d.AddCrew(t.Context(), store.Crew{ID: "sorell", Name: "Sorell"}))
	waitOnDeck(t, r.d, "sorell", "fake-1")
	require.NoError(t, r.d.AddCrew(t.Context(), store.Crew{ID: "tuvok", Name: "Tuvok", Harness: "echo", Model: "model-crew"}))
	waitOnDeck(t, r.d, "tuvok", "fake-2")
	require.NoError(t, r.d.AddCrew(t.Context(), store.Crew{ID: "vance", Name: "Vance", Division: DivisionCommand}))
	waitOnDeck(t, r.d, "vance", "fake-3")
	specs := p.startSpecs()
	require.Len(t, specs, 3)
	require.Equal(t, transporter.CombadgeSpec{CrewID: "sorell", Args: r.d.combadgeArgs}, specs[0])
	require.Equal(t, transporter.CombadgeSpec{CrewID: "tuvok", Harness: "echo", Args: r.d.combadgeArgs}, specs[1])
	require.Equal(t, transporter.CombadgeSpec{CrewID: "vance", Role: RoleCommand, Args: r.d.combadgeArgs}, specs[2],
		"Command's crew members run with --role command (#155)")
}

// Config.HailParkAfter reaches every crew member's CombadgeSpec, whatever
// its harness: the transporter passes --hail-park-after only when serve
// runs sdk (#180). Unset, it is zero, and no flag goes.
func TestProvisionPassesHailParkAfter(t *testing.T) {
	t.Parallel()
	for name, park := range map[string]time.Duration{"set": 2 * time.Minute, "unset": 0} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &fakeProvider{hello: true}
			cfg := testConfig(privateHome(t))
			cfg.Transporter = p
			cfg.HailParkAfter = park
			r := startDaemonConfig(t, cfg)
			defer r.stop(t)
			require.NoError(t, r.d.AddCrew(t.Context(), store.Crew{ID: "sorell", Name: "Sorell", Harness: "sdk"}))
			waitOnDeck(t, r.d, "sorell", "fake-1")
			require.NoError(t, r.d.AddCrew(t.Context(), store.Crew{ID: "tuvok", Name: "Tuvok", Harness: "echo"}))
			waitOnDeck(t, r.d, "tuvok", "fake-2")
			require.Equal(t, []transporter.CombadgeSpec{
				{CrewID: "sorell", Harness: "sdk", HailParkAfter: park, Args: r.d.combadgeArgs},
				{CrewID: "tuvok", Harness: "echo", HailParkAfter: park, Args: r.d.combadgeArgs},
			}, p.startSpecs())
		})
	}
}

// Run refuses a HailParkAfter combadge serve wouldn't take (#180).
func TestRunRefusesBadHailParkAfter(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		-time.Minute:                        "-1m0s is not positive",
		500 * time.Microsecond:              "500µs is shorter than 1ms",
		MaxHailParkAfter + time.Millisecond: "596h31m23.648s is over the cap of 596h31m23.647s",
	} {
		cfg := testConfig(privateHome(t))
		cfg.HailParkAfter = d
		require.ErrorContains(t, Run(t.Context(), cfg), "bridge: hail park after: "+want)
	}
	require.NoError(t, CheckHailParkAfter(MaxHailParkAfter))
	require.NoError(t, CheckHailParkAfter(time.Millisecond))
	require.Equal(t, int64(1<<31-1), MaxHailParkAfter.Milliseconds(), "combadge serve's cap, setTimeout's limit")
}

// #59: a crew member without a harness gets its division's default at
// provisioning, and its own overrides it. AddCrew refuses a division
// default this bridge can't run.
func TestProvisionPassesDivisionHarness(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	p := &fakeProvider{hello: true}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.Divisions = map[string]DivisionConfig{"ops": {Harness: "echo"}, "science": {}, "holo": {Harness: "holodeck"}}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	ctx := t.Context()

	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"}))
	waitOnDeck(t, r.d, "sorell", "fake-1")
	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "tuvok", Name: "Tuvok", Division: "ops", Harness: "sdk"}))
	waitOnDeck(t, r.d, "tuvok", "fake-2")
	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk", Division: "science"}))
	waitOnDeck(t, r.d, "kirk", "fake-3")
	require.Equal(t, []transporter.CombadgeSpec{
		{CrewID: "sorell", Harness: "echo", Args: r.d.combadgeArgs},
		{CrewID: "tuvok", Harness: "sdk", Args: r.d.combadgeArgs},
		{CrewID: "kirk", Args: r.d.combadgeArgs},
	}, p.startSpecs())
	c := getCrew(t, r.d, "sorell")
	require.Empty(t, c.Harness, "the division's default is resolved, not stored")

	err := r.d.AddCrew(ctx, store.Crew{ID: "data", Name: "Data", Division: "holo"})
	require.ErrorContains(t, err, `this bridge can't run harness "holodeck", division holo's default (it runs sdk, echo)`)
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "data", Name: "Data", Division: "holo", Harness: "echo"}),
		"the crew member's own harness overrides the division's")
}

// A roster changed at a restart can give a crew member added earlier a
// division default this bridge can't run. Its next provisioning sends it to
// sickbay at once, saying why, before making a beam (#59). Re-energizing
// it is refused the same way until the roster changes. Crashed: a crash
// cut off an earlier re-energize after its NextTour (#106).
func TestProvisionRefusesUnrunnableDivisionHarness(t *testing.T) {
	t.Parallel()
	for _, crashed := range []bool{false, true} {
		t.Run(fmt.Sprintf("crashed=%t", crashed), func(t *testing.T) {
			t.Parallel()
			testProvisionRefusesUnrunnableDivisionHarness(t, crashed)
		})
	}
}

func testProvisionRefusesUnrunnableDivisionHarness(t *testing.T, crashed bool) {
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	p := &fakeProvider{}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.Divisions = map[string]DivisionConfig{"ops": {Harness: "holodeck"}}
	r := startDaemonConfig(t, cfg)

	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, r.d, tourWorkflowID("sorell", 0)))
	require.Zero(t, p.createCallCount())
	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	require.Equal(t, `crew to sickbay: provisioning failed: this bridge can't run harness "holodeck", division ops's default `+
		`(it runs sdk, echo); holodeck needs a bridge started with --transporter holodeck --holodeck-script`, last.Text)
	var pl map[string]string
	require.NoError(t, json.Unmarshal(last.Payload, &pl))
	require.Equal(t, CodeProvisionFailed, pl["code"])

	ctx := t.Context()
	_, err = r.d.ReenergizeCrew(ctx, "sorell")
	require.ErrorContains(t, err, `this bridge can't run harness "holodeck", division ops's default (it runs sdk, echo)`)
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	c := getCrew(t, r.d, "sorell")
	require.Equal(t, [2]any{store.CrewSickbay, 0}, [2]any{c.Status, c.TourNo}, "no new tour")
	require.Empty(t, workflows(t, r.d, tourWorkflowID("sorell", 1)))
	r.stop(t)

	// With the roster fixed, re-energize starts the next tour. The crew
	// member is beaming in, out of sickbay, until its beam's hello (#106).
	cfg.Divisions = map[string]DivisionConfig{"ops": {Harness: "echo"}}
	gate := make(chan struct{})
	cfg.Transporter = &fakeProvider{hello: true, startGate: gate}
	r = startDaemonConfig(t, cfg)
	defer r.stop(t)
	if crashed {
		// An earlier call's NextTour, then a crash before its tour started:
		// beaming in with no tour behind it, which re-energize starts.
		_, err := r.d.store.NextTour(ctx, "sorell", 0, store.LogEntry{Kind: logCrew, CrewID: "sorell", Text: "crew re-energized: tour 1"})
		require.NoError(t, err)
		require.Empty(t, workflows(t, r.d, tourWorkflowID("sorell", 1)))
	}
	out, err := r.d.ReenergizeCrew(ctx, "sorell")
	require.NoError(t, err)
	require.Equal(t, OutcomeRequested, out)
	c = getCrew(t, r.d, "sorell")
	require.Equal(t, [3]any{store.CrewBeamingIn, 1, ""}, [3]any{c.Status, c.TourNo, c.CurrentBeam})
	requireOutcome(t, OutcomeUnderWay)(r.d.ReenergizeCrew(ctx, "sorell"))
	close(gate)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	require.Empty(t, workflows(t, r.d, tourWorkflowID("sorell", 2)))
	_, err = r.d.ReenergizeCrew(ctx, "sorell")
	require.Equal(t, CodeBadRequest, errorCode(ctx, err), "on deck")
}

func TestAPIValidation(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sick", Name: "Sick", Status: store.CrewSickbay})
	insertCrew(t, home, store.Crew{ID: "gone", Name: "Gone", Status: store.CrewDismissed})
	r := startDaemonWith(t, home, &fakeProvider{blockProvision: true})
	defer r.stop(t)
	ctx := t.Context()

	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell"}))
	requirePending(t, r.d, tourWorkflowID("sorell", 0))
	require.ErrorIs(t, r.d.AddCrew(ctx, store.Crew{ID: "sorell", Name: "Again"}), ErrExists)
	require.Error(t, r.d.AddCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk", TourNo: 3}))
	require.Error(t, r.d.AddCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk", Status: store.CrewAway}))
	require.Error(t, r.d.AddCrew(ctx, store.Crew{ID: "kirk"}))
	require.ErrorContains(t, r.d.AddCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk", Harness: "codex"}),
		`unknown harness "codex" (want one of sdk, holodeck, echo)`)
	// A bridge without a holodeck script can't run holodeck (#64).
	require.ErrorContains(t, r.d.AddCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk", Harness: "holodeck"}),
		`this bridge can't run harness "holodeck" (it runs sdk, echo)`)
	_, err := r.d.store.GetCrew(ctx, "kirk")
	require.ErrorIs(t, err, sql.ErrNoRows, "nothing inserted for a refused crew member")

	_, err = r.d.CreateOrder(ctx, store.Order{Title: "x", Assignee: "nobody"})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r.d.CreateOrder(ctx, store.Order{Title: "x", Assignee: "sick"})
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = r.d.CreateOrder(ctx, store.Order{Title: "x", Assignee: "gone"})
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = r.d.CreateOrder(ctx, store.Order{Title: "x"})
	require.EqualError(t, err, "order assignee or division is required")
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	for _, p := range []int{-1, store.PriorityRed + 1} {
		_, err = r.d.CreateOrder(ctx, store.Order{Title: "x", Division: "ops", Priority: p})
		require.ErrorContains(t, err, "out of range (0 routine, 1 yellow, 2 red)")
		require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	}
	orders, err := r.d.store.ListOrders(ctx)
	require.NoError(t, err)
	require.Empty(t, orders, "nothing inserted for a refused order")

	require.ErrorIs(t, r.d.AnswerHail(ctx, "H-0001", "yes", nil), ErrNotFound)
}

// The tour's backstop and the mission's own closeHail may both close the
// same hail (sequentially on a dismissal or sickbay, or racing): exactly one
// of them closes it and logs "closed unanswered" (#35).
func TestHailClosedOnce(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	r := startDaemonWith(t, home, &fakeProvider{blockProvision: true})
	defer r.stop(t)
	ctx := t.Context()

	newHail := func() (MissionInput, string) {
		t.Helper()
		orderID, err := r.d.store.InsertOrder(ctx, store.Order{Title: "t", Assignee: "sorell"}, testEntry)
		require.NoError(t, err)
		hailID, err := r.d.store.InsertHail(ctx, store.Hail{OrderID: orderID, CrewID: "sorell", Question: "?"}, testEntry)
		require.NoError(t, err)
		return MissionInput{OrderID: orderID, CrewID: "sorell"}, hailID
	}
	requireClosedOnce := func(hailID string) {
		t.Helper()
		h, err := r.d.store.GetHail(ctx, hailID)
		require.NoError(t, err)
		require.Equal(t, store.HailTimedOut, h.Status)
		entries, err := r.d.store.ListLog(ctx, 0)
		require.NoError(t, err)
		n := 0
		for _, e := range entries {
			if e.Text == "hail "+hailID+" closed unanswered" {
				n++
			}
		}
		require.Equal(t, 1, n, "hail %s", hailID)
	}

	// One after the other, and again (a re-run step).
	in, hailID := newHail()
	require.NoError(t, r.d.backstop(ctx, in.CrewID, in.OrderID, false))
	require.NoError(t, r.d.closeHail(ctx, in, &openHail{ID: hailID}))
	require.NoError(t, r.d.backstop(ctx, in.CrewID, in.OrderID, false))
	requireClosedOnce(hailID)

	// Racing.
	for range 5 {
		in, hailID := newHail()
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					errs[i] = r.d.backstop(context.Background(), in.CrewID, in.OrderID, false)
				} else {
					errs[i] = r.d.closeHail(context.Background(), in, &openHail{ID: hailID})
				}
			}()
		}
		wg.Wait()
		for _, err := range errs {
			require.NoError(t, err)
		}
		requireClosedOnce(hailID)
	}

	// A decided hail is left alone and not logged.
	in, hailID = newHail()
	require.NoError(t, r.d.store.MarkHailDecided(ctx, hailID, "captain", "yes", "", testEntry))
	require.NoError(t, r.d.closeHail(ctx, in, &openHail{ID: hailID}))
	h, err := r.d.store.GetHail(ctx, hailID)
	require.NoError(t, err)
	require.Equal(t, store.HailDecided, h.Status)
}

// A mission that outlives its tour's settle wait may still record or decide
// a hail; its crew-status writes leave a sickbay or dismissed crew member as
// it is (#43).
func TestLateMissionKeepsTerminalCrewStatus(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	r := startDaemonWith(t, home, &fakeProvider{blockProvision: true})
	defer r.stop(t)
	ctx := t.Context()

	for _, status := range []string{store.CrewSickbay, store.CrewDismissed} {
		orderID, err := r.d.store.InsertOrder(ctx, store.Order{Title: "t", Assignee: "sorell", State: store.OrderAway}, testEntry)
		require.NoError(t, err)
		in := MissionInput{OrderID: orderID, CrewID: "sorell"}
		require.NoError(t, r.d.store.UpdateCrewStatus(ctx, "sorell", status, testEntry))

		hailID, err := recordHail(ctx, r.d, in, comms.HailBody{Ref: "r", Category: comms.CategoryClarifyingQuestion, Question: "?"})
		require.NoError(t, err)
		require.Equal(t, status, getCrew(t, r.d, "sorell").Status, "record-hail")

		require.NoError(t, decideHail(ctx, r.d, in, comms.AnswerBody{HailID: hailID, DecidedBy: "captain", Decision: "yes"}))
		require.Equal(t, status, getCrew(t, r.d, "sorell").Status, "decide-hail")

		// The hail and its log entries are still written.
		h, err := r.d.store.GetHail(ctx, hailID)
		require.NoError(t, err)
		require.Equal(t, store.HailDecided, h.Status)
		entries, err := r.d.store.ListLog(ctx, 0)
		require.NoError(t, err)
		var texts []string
		for _, e := range entries {
			if e.Kind == logHail && e.OrderID == orderID {
				texts = append(texts, e.Text)
			}
		}
		require.Equal(t, []string{"?", "hail " + hailID + " decided: yes"}, texts)
	}

	// A crew member still away goes hailing and back. (Another one:
	// dismissed is terminal.)
	require.NoError(t, r.d.store.InsertCrew(ctx, store.Crew{ID: "tuvok", Name: "Tuvok", Status: store.CrewAway}, testEntry))
	orderID, err := r.d.store.InsertOrder(ctx, store.Order{Title: "t", Assignee: "tuvok", State: store.OrderAway}, testEntry)
	require.NoError(t, err)
	in := MissionInput{OrderID: orderID, CrewID: "tuvok"}
	hailID, err := recordHail(ctx, r.d, in, comms.HailBody{Ref: "r", Category: comms.CategoryClarifyingQuestion, Question: "?"})
	require.NoError(t, err)
	require.Equal(t, store.CrewHailing, getCrew(t, r.d, "tuvok").Status)
	require.NoError(t, decideHail(ctx, r.d, in, comms.AnswerBody{HailID: hailID, DecidedBy: "captain", Decision: "yes"}))
	require.Equal(t, store.CrewAway, getCrew(t, r.d, "tuvok").Status)
}

// A mission that outlives its tour's settle wait, its order failed by the
// tour's backstop, may still record or decide a hail after the next tour
// claimed another order; its crew-status writes leave the next tour's status
// alone (#48).
func TestLateMissionKeepsNextTourCrewStatus(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	r := startDaemonWith(t, home, &fakeProvider{blockProvision: true})
	defer r.stop(t)
	ctx := t.Context()

	// Tour 0's order A hails, then is failed by the backstop with its hail
	// still open; tour 1 has the crew member away on order B.
	orderA, err := r.d.store.InsertOrder(ctx, store.Order{Title: "a", Assignee: "sorell", State: store.OrderAway}, testEntry)
	require.NoError(t, err)
	inA := MissionInput{OrderID: orderA, CrewID: "sorell"}
	require.NoError(t, r.d.store.UpdateCrewStatus(ctx, "sorell", store.CrewAway, testEntry))
	hailA, err := recordHail(ctx, r.d, inA, comms.HailBody{Ref: "r", Category: comms.CategoryClarifyingQuestion, Question: "a?"})
	require.NoError(t, err)
	require.Equal(t, store.CrewHailing, getCrew(t, r.d, "sorell").Status)
	_, settled, err := r.d.store.SettleOrder(ctx, orderA, store.OrderFailed, testEntry, nil)
	require.NoError(t, err)
	require.True(t, settled)
	require.NoError(t, r.d.store.UpdateCrewStatus(ctx, "sorell", store.CrewAway, testEntry))
	orderB, err := r.d.store.InsertOrder(ctx, store.Order{Title: "b", Assignee: "sorell", State: store.OrderAway}, testEntry)
	require.NoError(t, err)
	inB := MissionInput{OrderID: orderB, CrewID: "sorell"}

	// A late record-hail for A (a replay finds hail A open, so decide it
	// first) leaves tour 1's away alone, and still writes the hail.
	require.NoError(t, decideHail(ctx, r.d, inA, comms.AnswerBody{HailID: hailA, DecidedBy: "captain", Decision: "yes"}))
	require.Equal(t, store.CrewAway, getCrew(t, r.d, "sorell").Status, "decide-hail")
	lateA, err := recordHail(ctx, r.d, inA, comms.HailBody{Ref: "r2", Category: comms.CategoryClarifyingQuestion, Question: "late?"})
	require.NoError(t, err)
	require.Equal(t, store.CrewAway, getCrew(t, r.d, "sorell").Status, "record-hail")
	h, err := r.d.store.GetHail(ctx, lateA)
	require.NoError(t, err)
	require.Equal(t, store.HailOpen, h.Status)

	// B hails; deciding A's late hail leaves the crew member hailing on B.
	hailB, err := recordHail(ctx, r.d, inB, comms.HailBody{Ref: "r3", Category: comms.CategoryClarifyingQuestion, Question: "b?"})
	require.NoError(t, err)
	require.Equal(t, store.CrewHailing, getCrew(t, r.d, "sorell").Status)
	require.NoError(t, decideHail(ctx, r.d, inA, comms.AnswerBody{HailID: lateA, DecidedBy: "captain", Decision: "no"}))
	require.Equal(t, store.CrewHailing, getCrew(t, r.d, "sorell").Status, "decide-hail")
	h, err = r.d.store.GetHail(ctx, lateA)
	require.NoError(t, err)
	require.Equal(t, store.HailDecided, h.Status)

	// A's hails and decisions are all logged.
	entries, err := r.d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	var texts []string
	for _, e := range entries {
		if e.Kind == logHail && e.OrderID == orderA {
			texts = append(texts, e.Text)
		}
	}
	require.Equal(t, []string{"a?", "hail " + hailA + " decided: yes", "late?", "hail " + lateA + " decided: no"}, texts)

	// B's own decision sets the crew member back away.
	require.NoError(t, decideHail(ctx, r.d, inB, comms.AnswerBody{HailID: hailB, DecidedBy: "captain", Decision: "yes"}))
	require.Equal(t, store.CrewAway, getCrew(t, r.d, "sorell").Status)
}

// #106 review: a crew member beaming in whose tour ended without a hello
// (here cancelled while it provisioned) is re-energized as one in sickbay
// would be: NextTour and a new tour, beaming in on tour 1.
func TestReenergizeBeamingInEndedTour(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemonWith(t, home, &fakeProvider{blockProvision: true})
	defer r.stop(t)
	ctx := t.Context()
	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell"}))
	requirePending(t, r.d, tourWorkflowID("sorell", 0))
	_, err := r.d.ReenergizeCrew(ctx, "sorell")
	require.NoError(t, err, "under way while its tour provisions")

	// Only the tour is cancelled. Its transporter workflow stays blocked in
	// provisioning: cancelling that too would let the cancelled tour's
	// goroutine, still in awaitBeam, see the child fail and send the crew
	// member to sickbay at any moment, even after the re-energize below.
	requirePending(t, r.d, beamWorkflowID("sorell", 0))
	require.NoError(t, dbos.CancelWorkflow(r.d.dbos, tourWorkflowID("sorell", 0)))
	require.Eventually(t, func() bool {
		live, found, err := workflowLive(r.d, tourWorkflowID("sorell", 0))
		return err == nil && found && !live
	}, 10*time.Second, 20*time.Millisecond)
	c := getCrew(t, r.d, "sorell")
	require.Equal(t, [2]any{store.CrewBeamingIn, 0}, [2]any{c.Status, c.TourNo})

	requireOutcome(t, OutcomeRequested)(r.d.ReenergizeCrew(ctx, "sorell"))
	c = getCrew(t, r.d, "sorell")
	require.Equal(t, [2]any{store.CrewBeamingIn, 1}, [2]any{c.Status, c.TourNo})
	requirePending(t, r.d, tourWorkflowID("sorell", 1))
	requireOutcome(t, OutcomeUnderWay)(r.d.ReenergizeCrew(ctx, "sorell"))
}

// Config.CheckpointEvery and CheckpointTurns reach every crew member's
// CombadgeSpec (#145); the transporter passes them only when serve runs
// sdk. Unset, they are nil, and no flag goes.
func TestProvisionPassesCheckpointCadence(t *testing.T) {
	t.Parallel()
	every, turns := 30*time.Minute, 0
	p := &fakeProvider{hello: true}
	cfg := testConfig(privateHome(t))
	cfg.Transporter = p
	cfg.CheckpointEvery, cfg.CheckpointTurns = &every, &turns
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	require.NoError(t, r.d.AddCrew(t.Context(), store.Crew{ID: "sorell", Name: "Sorell", Harness: "sdk"}))
	waitOnDeck(t, r.d, "sorell", "fake-1")
	specs := p.startSpecs()
	require.Len(t, specs, 1)
	require.Equal(t, every, *specs[0].CheckpointEvery)
	require.Equal(t, 0, *specs[0].CheckpointTurns)
}

// Run refuses a checkpoint cadence combadge serve wouldn't take (#145);
// 0 turns either trigger off.
func TestRunRefusesBadCheckpointCadence(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		-time.Minute:                        "-1m0s is negative",
		500 * time.Microsecond:              "500µs is shorter than 1ms",
		MaxHailParkAfter + time.Millisecond: "596h31m23.648s is over the cap",
	} {
		cfg := testConfig(privateHome(t))
		cfg.CheckpointEvery = &d
		require.ErrorContains(t, Run(t.Context(), cfg), "bridge: checkpoint every: "+want)
	}
	n := -1
	cfg := testConfig(privateHome(t))
	cfg.CheckpointTurns = &n
	require.ErrorContains(t, Run(t.Context(), cfg), "bridge: checkpoint turns: -1 is negative")
	require.NoError(t, CheckCheckpointEvery(0))
	require.NoError(t, CheckCheckpointEvery(time.Hour))
	require.NoError(t, CheckCheckpointTurns(0))
}
