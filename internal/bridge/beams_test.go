package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// #97's acceptance: with MaxBeams 2, a third crew member is refused until
// one is dismissed. (The dismissal is written straight to the store:
// `bmus crew dismiss` is #100.)
func TestAddCrewBeamCap(t *testing.T) {
	t.Parallel()
	cfg := testConfig(privateHome(t))
	cfg.TransporterLimits = TransporterLimits{MaxBeams: 2}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	ctx := t.Context()

	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell"}))
	waitOnDeck(t, r.d, "sorell", "fake-1")
	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "tuvok", Name: "Tuvok"}))
	waitOnDeck(t, r.d, "tuvok", "fake-2")
	err := r.d.AddCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk"})
	require.ErrorContains(t, err, "beam cap reached (transporter.max_beams 2)")
	require.Equal(t, CodeBadRequest, errorCode(ctx, err))
	_, err = r.d.store.GetCrew(ctx, "kirk")
	require.Error(t, err, "nothing inserted for a refused crew member")
	require.Empty(t, workflows(t, r.d, tourWorkflowID("kirk", 0)))

	require.NoError(t, r.d.store.UpdateCrewStatus(ctx, "tuvok", store.CrewDismissed, testEntry))
	require.NoError(t, r.d.AddCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk"}))
	waitOnDeck(t, r.d, "kirk", "fake-3")
}

// overlapProvider is a fakeProvider that counts provisionings in flight,
// from Create to StartCombadge's return, each taking at least delay.
type overlapProvider struct {
	*fakeProvider
	delay time.Duration

	mu                sync.Mutex
	inflight, highest int
}

func (p *overlapProvider) Create(ctx context.Context) (transporter.Beam, error) {
	p.mu.Lock()
	p.inflight++
	p.highest = max(p.highest, p.inflight)
	p.mu.Unlock()
	time.Sleep(p.delay)
	return p.fakeProvider.Create(ctx)
}

func (p *overlapProvider) StartCombadge(ctx context.Context, beam transporter.Beam, spec transporter.CombadgeSpec) error {
	err := p.fakeProvider.StartCombadge(ctx, beam, spec)
	time.Sleep(p.delay)
	p.mu.Lock()
	p.inflight--
	p.mu.Unlock()
	return err
}

func (p *overlapProvider) maxInflight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.highest
}

// #97's acceptance: with MaxProvisioning 1, three crew added at once are
// provisioned one at a time, through the transporter queue.
func TestTransporterQueueOneAtATime(t *testing.T) {
	t.Parallel()
	p := &overlapProvider{fakeProvider: &fakeProvider{}, delay: 100 * time.Millisecond}
	cfg := testConfig(privateHome(t))
	cfg.Transporter = p
	cfg.TransporterLimits = TransporterLimits{MaxProvisioning: 1}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)

	ids := []string{"sorell", "tuvok", "kirk"}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			assert.NoError(t, r.d.AddCrew(context.Background(), store.Crew{ID: id, Name: id}))
		})
	}
	wg.Wait()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		for _, id := range ids {
			cr := getCrew(c, r.d, id)
			require.Equal(c, store.CrewOnDeck, cr.Status)
			require.NotEmpty(c, cr.CurrentBeam)
		}
	}, 20*time.Second, 20*time.Millisecond)
	require.Equal(t, 3, p.createdCount())
	require.Equal(t, 1, p.maxInflight(), "provisionings overlapped")
	for _, id := range ids {
		wfs := workflows(t, r.d, beamWorkflowID(id, 0))
		require.Len(t, wfs, 1)
		require.Equal(t, transporterWorkflowName, wfs[0].Name)
		require.Equal(t, transporterQueueName, wfs[0].QueueName)
	}
}

// stepNames returns the names of workflow id's recorded steps.
func stepNames(t *testing.T, d *Daemon, id string) []string {
	t.Helper()
	steps, err := dbos.GetWorkflowSteps(d.dbos, id)
	require.NoError(t, err)
	var names []string
	for _, s := range steps {
		names = append(names, s.StepName)
	}
	return names
}

// logTexts returns the texts of crewID's captain's log entries.
func logTexts(t *testing.T, d *Daemon, crewID string) []string {
	t.Helper()
	entries, err := d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	var texts []string
	for _, e := range entries {
		if e.CrewID == crewID {
			texts = append(texts, e.Text)
		}
	}
	return texts
}

// #97's acceptance: a beams add that fails twice, then succeeds, brings the
// crew member on deck with no sickbay. The retries are the transporter
// workflow's own, each attempt and wait recorded.
func TestProvisionRetriesCreate(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{createFails: 2}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.provisionBackoff = 10 * time.Millisecond
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)

	waitOnDeck(t, r.d, "sorell", "fake-1")
	require.Equal(t, 3, p.createCallCount())
	requirePending(t, r.d, tourWorkflowID("sorell", 0))
	require.Equal(t, []string{"remove-previous", "provision", "DBOS.sleep", "provision", "DBOS.sleep", "provision"},
		stepNames(t, r.d, beamWorkflowID("sorell", 0)))
	texts := logTexts(t, r.d, "sorell")
	require.Equal(t, 2, countPrefix(texts, "beam creation failed: fake: beams add failed"), "%q", texts)
	require.Zero(t, countPrefix(texts, "crew to sickbay"), "%q", texts)
}

// A beams add that keeps failing is retried with backoff (1, 2, 4, then 5
// at most, in units of provisionBackoff) until the waits would pass 30
// units, and the crew member then goes to sickbay as provision_failed.
func TestProvisionGivesUpAfterBudget(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{createFails: 1000}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.provisionBackoff = 5 * time.Millisecond
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)

	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, r.d, tourWorkflowID("sorell", 0)))
	// Waits 1+2+4+5+5+5+5 = 27 units; another 5 would pass 30.
	require.Equal(t, 8, p.createCallCount())
	require.Equal(t, 7, countPrefix(stepNames(t, r.d, beamWorkflowID("sorell", 0)), "DBOS.sleep"))
	c := getCrew(t, r.d, "sorell")
	require.Equal(t, store.CrewSickbay, c.Status)
	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	require.Equal(t, "crew to sickbay: provisioning failed: create beam: fake: beams add failed", last.Text)
	var pl map[string]string
	require.NoError(t, json.Unmarshal(last.Payload, &pl))
	require.Equal(t, CodeProvisionFailed, pl["code"])
}

// A tour whose crew member was dismissed before it got a beam ends
// dismissed, creating nothing.
func TestProvisionRefusesDismissed(t *testing.T) {
	t.Parallel()
	p := &fakeProvider{}
	r := startDaemonWith(t, privateHome(t), p)
	defer r.stop(t)
	ctx := t.Context()
	require.NoError(t, r.d.store.InsertCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell", Status: store.CrewDismissed}, testEntry))

	tourID := tourWorkflowID("sorell", 0)
	_, err := dbos.RunWorkflow(r.d.dbos, crewTourWorkflow, CrewTourInput{CrewID: "sorell"}, dbos.WithWorkflowID(tourID))
	require.NoError(t, err)
	require.Equal(t, CrewTourResult{Ended: store.CrewDismissed}, tourResult(t, r.d, tourID))
	require.Zero(t, p.createCallCount())
	require.Empty(t, p.started())
	c := getCrew(t, r.d, "sorell")
	require.Equal(t, store.CrewDismissed, c.Status)
	require.Empty(t, c.CurrentBeam)
	_, ok := r.d.Pump("sorell")
	require.False(t, ok)
}

// A later tour removes the beam an earlier tour created and never reuses
// it; a beam that can't be removed is logged and left to its TTL.
func TestTransporterRemovesPreviousBeam(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		removeErr error
		logged    string
	}{
		{name: "removed", logged: "beam fake-old removed"},
		{name: "remove fails", removeErr: assert.AnError, logged: "beam fake-old not removed, left to its TTL: " + assert.AnError.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := privateHome(t)
			insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell", TourNo: 1})
			st, err := store.Open(t.Context(), home+"/"+store.DBFile)
			require.NoError(t, err)
			require.NoError(t, st.InsertBeam(t.Context(), store.BeamRecord{UUID: "fake-old", Alias: "fake-old", CrewID: "sorell", TourNo: 0}, testEntry))
			require.NoError(t, st.Close())

			p := &fakeProvider{removeErr: tc.removeErr}
			r := startDaemonWith(t, home, p)
			waitOnDeck(t, r.d, "sorell", "fake-1")
			require.Equal(t, []string{"fake-old"}, p.removed())
			require.Equal(t, []string{"fake-1"}, p.started(), "tour 0's beam is never reused")
			beams, err := r.d.store.ListBeams(t.Context(), "sorell")
			require.NoError(t, err)
			require.Len(t, beams, 2)
			require.Equal(t, 1, beams[1].TourNo)
			require.Contains(t, logTexts(t, r.d, "sorell"), tc.logged)

			// A restart re-runs nothing: the tour's beam is its own.
			r.stop(t)
			r = startDaemonWith(t, home, p)
			defer r.stop(t)
			waitOnDeck(t, r.d, "sorell", "fake-1")
			require.Equal(t, []string{"fake-old"}, p.removed())
			require.Equal(t, 1, p.createdCount())
		})
	}
}

func countPrefix(texts []string, prefix string) int {
	n := 0
	for _, s := range texts {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// A transporter workflow cancelled mid-retries is an outcome the tour
// records (GetResult's checkpoint), so the tour acts on it: sickbay.
func TestTransporterCancelledGoesToSickbay(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{createFails: 1000}
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.provisionBackoff = time.Hour // sleeps until cancelled
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)

	require.Eventually(t, func() bool { return p.createCallCount() == 1 }, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, dbos.CancelWorkflow(r.d.dbos, beamWorkflowID("sorell", 0)))
	tourID := tourWorkflowID("sorell", 0)
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, r.d, tourID))
	require.Contains(t, stepNames(t, r.d, tourID), "DBOS.getResult")
	require.Equal(t, store.CrewSickbay, getCrew(t, r.d, "sorell").Status)
}

// awaitBeam acts only on a recorded outcome, and waits on only while the
// child runs (or once more after a timeout that raced its end). An
// unrecorded failure on an ended child (a dead letter, a result that can't
// be read) is retried awaitEndedRetries times before it is returned from
// the tour, not turned into sickbay by a step (#110).
func TestAwaitNext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		recorded, final, timeout, spent bool
		failures                        int
		want                            int
	}{
		{recorded: true, final: true, timeout: true, want: awaitDone}, // the child's own error was a timeout
		{recorded: true, final: true, want: awaitDone},
		{recorded: true, final: true, failures: awaitEndedRetries, want: awaitDone},
		{final: false, timeout: true, want: awaitRetry},
		{final: true, timeout: true, want: awaitRetry},
		{final: false, want: awaitPause},                              // e.g. the system database failing
		{final: false, failures: awaitEndedRetries, want: awaitPause}, // the child still runs
		{final: true, want: awaitPause},                               // one transient failure: retried
		{final: true, failures: awaitEndedRetries - 1, want: awaitPause},
		{final: true, failures: awaitEndedRetries, want: awaitDone}, // e.g. a dead letter
		{final: true, spent: true, want: awaitDone},                 // the result's record failed: no retry
	} {
		require.Equal(t, tc.want, awaitNext(tc.recorded, tc.final, tc.timeout, tc.spent, tc.failures), "%+v", tc)
	}
	require.True(t, stepIDSpent(&dbos.Error{Code: dbos.ErrorCodeWorkflowExecution, Message: "recording child workflow result"}))
	require.False(t, stepIDSpent(&dbos.Error{Code: dbos.ErrorCodeDeadLetterQueue}))
	require.False(t, stepIDSpent(errors.New("failed to deserialize workflow result")))
}

// errUnreadable is faultOnEndedChild's usual failure: one that took no step
// id, so awaitBeam retries it.
var errUnreadable = errors.New("injected: result can't be read")

// faultOnEndedChild returns a getResultFault that fails awaitBeam's
// GetResult calls with fail, once sorell's tour 0's transporter workflow
// has ended (the first call waits for that), until calls reaches limit.
func faultOnEndedChild(limit, calls *atomic.Int32, fail error) func(*Daemon, string) error {
	return func(d *Daemon, _ string) error {
		if calls.Load() >= limit.Load() {
			return nil
		}
		for range 1000 {
			if live, found, err := workflowLive(d, beamWorkflowID("sorell", 0)); err == nil && found && !live {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		calls.Add(1)
		return fail
	}
}

// One unrecorded failure on an ended transporter workflow is retried: the
// tour goes on and the crew member is on deck (#110).
func TestAwaitBeamRetriesTransientFailure(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	var limit, calls atomic.Int32
	limit.Store(1)
	cfg := testConfig(home)
	cfg.Transporter = &fakeProvider{hello: true}
	cfg.getResultFault = faultOnEndedChild(&limit, &calls, errUnreadable)
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	require.EqualValues(t, 1, calls.Load())
	// The pump exists before start-pump's outcome is recorded.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.True(c, hasStep(c, r.d, tourWorkflowID("sorell", 0), "start-pump"), "the tour went on")
	}, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, store.CrewOnDeck, getCrew(t, r.d, "sorell").Status)
}

// An unrecorded failure on an ended transporter workflow that persists
// ends the tour in ERROR with its crew member in sickbay, code
// bridge_error, so re-energize can bring it back (#110).
func TestAwaitBeamFailureGoesToSickbay(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	var limit, calls atomic.Int32
	limit.Store(1000)
	cfg := testConfig(home)
	cfg.Transporter = &fakeProvider{hello: true}
	cfg.getResultFault = faultOnEndedChild(&limit, &calls, errUnreadable)
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	tourID := tourWorkflowID("sorell", 0)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		wfs := workflows(c, r.d, tourID)
		require.Len(c, wfs, 1)
		require.Equal(c, dbos.WorkflowStatusError, wfs[0].Status)
	}, 20*time.Second, 20*time.Millisecond, "the tour ends in ERROR")
	require.EqualValues(t, awaitEndedRetries+1, calls.Load())
	c := getCrew(t, r.d, "sorell")
	require.Equal(t, store.CrewSickbay, c.Status)
	require.Equal(t, "fake-1", c.CurrentBeam)
	entries, err := r.d.store.ListLog(t.Context(), 0)
	require.NoError(t, err)
	last := entries[len(entries)-1]
	require.Equal(t, "crew to sickbay: awaiting its beam failed: injected: result can't be read", last.Text)
	var pl map[string]string
	require.NoError(t, json.Unmarshal(last.Payload, &pl))
	require.Equal(t, CodeBridgeError, pl["code"])

	// Re-energize brings the crew member back on a new tour.
	limit.Store(0)
	requireOutcome(t, OutcomeRequested)(r.d.ReenergizeCrew(t.Context(), "sorell"))
	waitOnDeck(t, r.d, "sorell", "fake-2")
	require.Equal(t, 1, getCrew(t, r.d, "sorell").TourNo)
}

// A failure to record the child's result took a step id, so awaitBeam
// gives up at once rather than record the result a step later than a
// replay would: sickbay, bridge_error (#110 review).
func TestAwaitBeamGivesUpOnSpentStepID(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	var limit, calls atomic.Int32
	limit.Store(1000)
	cfg := testConfig(home)
	cfg.Transporter = &fakeProvider{hello: true}
	cfg.getResultFault = faultOnEndedChild(&limit, &calls,
		&dbos.Error{Code: dbos.ErrorCodeWorkflowExecution, Message: "injected: recording child workflow result"})
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	tourID := tourWorkflowID("sorell", 0)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		wfs := workflows(c, r.d, tourID)
		require.Len(c, wfs, 1)
		require.Equal(c, dbos.WorkflowStatusError, wfs[0].Status)
	}, 20*time.Second, 20*time.Millisecond, "the tour ends in ERROR")
	require.EqualValues(t, 1, calls.Load(), "no retry")
	require.Equal(t, store.CrewSickbay, getCrew(t, r.d, "sorell").Status)
}

// A dismissal that lands while awaitBeam retries, before the tour reads
// live to DismissCrew's wait, leaves the beam to the tour: awaitFailed
// finds the crew member dismissed and removes it (#110 review).
func TestAwaitBeamFailureAfterDismissal(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	var limit, calls atomic.Int32
	limit.Store(1000)
	p := &fakeProvider{hello: true}
	cfg := testConfig(home)
	cfg.Transporter = p
	fault := faultOnEndedChild(&limit, &calls, errUnreadable)
	cfg.getResultFault = func(d *Daemon, tourID string) error {
		err := fault(d, tourID)
		if calls.Load() == 1 && err != nil {
			// Straight to the store: no signal, no beam removal.
			_, _, derr := d.store.DismissCrew(t.Context(), "sorell", store.LogEntry{Kind: logCrew, CrewID: "sorell", Text: "crew dismissed"}, nil)
			assert.NoError(t, derr)
		}
		return err
	}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	tourID := tourWorkflowID("sorell", 0)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		wfs := workflows(c, r.d, tourID)
		require.Len(c, wfs, 1)
		require.Equal(c, dbos.WorkflowStatusError, wfs[0].Status)
	}, 20*time.Second, 20*time.Millisecond, "the tour ends in ERROR")
	require.Equal(t, store.CrewDismissed, getCrew(t, r.d, "sorell").Status)
	require.Equal(t, []string{"fake-1"}, p.removed())
}

// A provision waiting for a tsh login (#102) survives a restart: the
// recorded attempts and waits replay, the wait goes on without logging
// again, and a new login brings the crew member on deck with the retry
// budget untouched.
func TestProvisionLoginWaitSurvivesRestart(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	p := &fakeProvider{}
	p.loginExpired.Store(true)
	cfg := testConfig(home)
	cfg.Transporter = p
	cfg.provisionBackoff = time.Millisecond
	cfg.loginWait = 20 * time.Millisecond
	r := startDaemonConfig(t, cfg)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Greater(c, countPrefix(stepNames(t, r.d, beamWorkflowID("sorell", 0)), "provision"), 5)
	}, 10*time.Second, 20*time.Millisecond)
	r.stop(t)

	r = startDaemonConfig(t, cfg)
	defer r.stop(t)
	n := countPrefix(stepNames(t, r.d, beamWorkflowID("sorell", 0)), "provision")
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Greater(c, countPrefix(stepNames(t, r.d, beamWorkflowID("sorell", 0)), "provision"), n+3)
	}, 10*time.Second, 20*time.Millisecond)
	p.loginExpired.Store(false)
	waitOnDeck(t, r.d, "sorell", "fake-1")
	require.Equal(t, 1, p.createCallCount())
	texts := logTexts(t, r.d, "sorell")
	require.Equal(t, 1, countPrefix(texts, "provisioning waits for a tsh login"), "%q", texts)
	require.Zero(t, countPrefix(texts, "crew to sickbay"), "%q", texts)
}
