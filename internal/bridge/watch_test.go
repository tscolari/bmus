package bridge

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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

// watchProvider is a holodeck with a short Lifetime whose later beams live
// 24h, so a test sees exactly one watch change: the first beam's. (One
// Holodeck mints every alias: two would reuse a removed beam's.) With gate
// set, CopyFrom (a checkpoint's pull) waits for it to close, closing
// blocked the first time.
type watchProvider struct {
	*transporter.Holodeck

	mu      sync.Mutex
	created int
	gate    chan struct{}
	blocked chan struct{}
}

func (p *watchProvider) Create(ctx context.Context) (transporter.Beam, error) {
	p.mu.Lock()
	first := p.created == 0
	p.created++
	p.mu.Unlock()
	beam, err := p.Holodeck.Create(ctx)
	if err == nil && !first {
		beam.ExpiresAt = time.Now().Add(24 * time.Hour).UTC()
	}
	return beam, err
}

func (p *watchProvider) CopyFrom(ctx context.Context, beam transporter.Beam, remote, local string) error {
	p.mu.Lock()
	gate, blocked := p.gate, p.blocked
	if gate != nil && blocked != nil {
		close(blocked)
		p.blocked = nil
	}
	p.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Holodeck.CopyFrom(ctx, beam, remote, local)
}

// Verification 4 and 5 of spec/phase-2-plan-2026-10-19.md (#100): the
// crew member's first beam reaches its watch change with an order away.
// The tour stands the mission down; its stopping checkpoint lands, the
// mission parks, tour 1 starts on a new beam, removes the old one and
// resumes the same order/<id> mission, whose second order command carries
// the checkpoint's handover; the order finishes done on top of the first
// beam's commit, and is away throughout. With restart, the bridge stops
// while the tour waits for the mission to park (the stopping checkpoint's
// pull held up) and a new bridge reaches the same outcome, recording the
// new beam's seq 1 (#22).
func TestWatchChange(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		name := "plain"
		if restart {
			name = "restart mid-wait"
		}
		t.Run(name, func(t *testing.T) { testWatchChange(t, restart) })
	}
}

func testWatchChange(t *testing.T, restart bool) {
	// serve reads its script when it starts: the new beam's runs this
	// file's second version.
	script := filepath.Join(t.TempDir(), "script.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 10, "progress": "surveying"},
		{"at_pct": 40, "commit": "first change"},
		{"at_pct": 50, "sleep_ms": 120000},
		{"at_pct": 100, "report": {"summary": "never reached"}}]`), 0o600))
	b := newHolodeckBridge(t, script)
	const lead = time.Hour
	short, err := transporter.NewHolodeck(transporter.HolodeckConfig{
		Root: b.root, Combadge: combadgeBundle(t),
		Harness:  []string{"--harness", "holodeck", "--script", script},
		Lifetime: lead + 10*time.Second,
	})
	require.NoError(t, err)
	p := &watchProvider{Holodeck: short}
	b.provider, b.lead = p, lead

	var current atomic.Pointer[Daemon]
	hd := b.start(t)
	current.Store(hd.d)
	c := hd.addCrew(t)
	id := hd.createOrder(t)

	// The order's state, sampled from its claim until it is done.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.OrderAway, hd.order(ct, id).State)
	}, 10*time.Second, 10*time.Millisecond)
	var (
		statesMu sync.Mutex
		states   []string
	)
	stopWatch, watched := make(chan struct{}), make(chan struct{})
	var once sync.Once
	endWatch := func() { once.Do(func() { close(stopWatch); <-watched }) }
	defer endWatch()
	go func() {
		defer close(watched)
		for {
			select {
			case <-stopWatch:
				return
			case <-time.After(20 * time.Millisecond):
			}
			d := current.Load()
			if d == nil {
				continue
			}
			o, err := d.store.GetOrder(context.Background(), id)
			if err != nil {
				continue // a daemon stopping
			}
			statesMu.Lock()
			if len(states) == 0 || states[len(states)-1] != o.State {
				states = append(states, o.State)
			}
			statesMu.Unlock()
		}
	}()

	// The first run committed and sleeps: the beam's watch change comes
	// in its sleep.
	var first string
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		for _, e := range readSteps(ct, b.stepLog) {
			if e["order"] == id && e["action"] == "commit" {
				first = e["commits"].(map[string]any)["app"].(string)
			}
		}
		require.NotEmpty(ct, first)
	}, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 50, "progress": "resumed"},
		{"at_pct": 80, "commit": "second change"},
		{"at_pct": 100, "report": {"summary": "done after the watch change"}}]`), 0o600))

	if restart {
		blocked := make(chan struct{})
		p.mu.Lock()
		p.gate, p.blocked = make(chan struct{}), blocked
		p.mu.Unlock()
		select {
		case <-blocked:
		case <-time.After(30 * time.Second):
			t.Fatal("the stopping checkpoint was never pulled")
		}
		requirePending(t, hd.d, tourWorkflowID("sorell", 0))
		current.Store(nil)
		hd.stop(t)
		p.mu.Lock()
		p.gate = nil
		p.mu.Unlock()
		hd = b.start(t)
		current.Store(hd.d)
	}

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "done after the watch change", res.Report.Summary)
	o := hd.order(t, id)
	require.Equal(t, store.OrderDone, o.State)
	require.Nil(t, o.ParkedAt)
	require.Len(t, workflows(t, hd.d, missionWorkflowID(id)), 1, "one mission throughout")
	require.Equal(t, CrewTourResult{Ended: TourEndedWatchChange}, tourResult(t, hd.d, tourWorkflowID("sorell", 0)))
	requirePending(t, hd.d, tourWorkflowID("sorell", 1))

	// Tour 1 runs on a new beam; the old one is gone.
	var cr store.Crew
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cr = hd.crew(ct, "sorell")
		require.Equal(ct, store.CrewOnDeck, cr.Status)
	}, 10*time.Second, 50*time.Millisecond)
	require.Equal(t, 1, cr.TourNo)
	require.NotEqual(t, c.CurrentBeam, cr.CurrentBeam)
	exists, err := b.h.Exists(t.Context(), transporter.Beam{Alias: c.CurrentBeam})
	require.NoError(t, err)
	require.False(t, exists, "the old beam was removed")
	beams, err := hd.d.store.ListBeams(t.Context(), "sorell")
	require.NoError(t, err)
	require.Len(t, beams, 2)
	require.Equal(t, []int{0, 1}, []int{beams[0].TourNo, beams[1].TourNo})
	seen, err := hd.d.store.SeenInbox(t.Context(), "sorell", 1, 1)
	require.NoError(t, err)
	require.True(t, seen, "the new beam's seq 1 is recorded under tour 1")

	// The stopping checkpoint, from the old beam, carries the first
	// commit; the resumed run got its note and committed on top of it.
	cps, err := hd.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	require.Equal(t, c.CurrentBeam, cps[0].Beam)
	note := "Order " + id + ": stopped by stand_down: watch change. The holodeck stopped its script here."
	requireCheckpointFiles(t, b.home, cps[0], note, first)
	var resumed []map[string]any
	for _, e := range readSteps(t, b.stepLog) {
		if e["order"] == id && e["handover"] != nil {
			resumed = append(resumed, e)
		}
	}
	require.Len(t, resumed, 3, "the second run's three steps")
	require.Equal(t, note, resumed[0]["handover"])
	clone := filepath.Join(b.home, "repos", "app")
	require.Equal(t, res.Report.Repos[0].Commit, gitT(t, clone, "rev-parse", "bmus/"+id))
	require.Equal(t, first, gitT(t, clone, "rev-parse", "bmus/"+id+"^"))

	endWatch()
	statesMu.Lock()
	require.Equal(t, []string{store.OrderAway, store.OrderDone}, states, "away throughout")
	statesMu.Unlock()

	entries := hd.logEntries(t)
	require.Zero(t, kinds(entries)[logFail])
	require.Equal(t, 1, kinds(entries)[logDone])
	require.Equal(t, []string{"order parked: watch change"}, orderTexts(entries, id, "order parked"))
	require.Equal(t, []string{"order resumed on sorell"}, orderTexts(entries, id, "order resumed"))
	require.True(t, slices.ContainsFunc(entries, func(e store.LogEntry) bool {
		return e.Text == "beam "+c.CurrentBeam+" removed"
	}), "the old beam's removal is logged")
	require.True(t, slices.ContainsFunc(entries, func(e store.LogEntry) bool {
		return e.Text == "watch change: tour 0 hands over to tour 1"
	}))
	requireNoBackstop(t, entries)
	logs, err := os.ReadFile(b.logFile)
	require.NoError(t, err)
	require.NotContains(t, string(logs), "Check that your workflow is deterministic")
	if restart {
		require.Contains(t, string(logs), "bridge shutting down")
	}
}

// A stopping checkpoint whose pull outlasts the tour's stand-down and
// settle waits (#100 review): the mission parks the order before the pull,
// so next-tour's backstop leaves it alone, and tour 1 resumes it. The pull,
// from the old beam (not crew.current_beam, the new one by then), fails
// once tour 1 has removed that beam, and the order resumes from base_ref.
// The mission's late mission_parked, sent under tour 0 to tour 1 after the
// resume, is not taken for a recall.
func TestWatchChangeSlowStoppingPull(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "script.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 10, "progress": "surveying"},
		{"at_pct": 50, "sleep_ms": 120000},
		{"at_pct": 100, "report": {"summary": "never reached"}}]`), 0o600))
	b := newHolodeckBridge(t, script)
	h, err := transporter.NewHolodeck(transporter.HolodeckConfig{
		Root: b.root, Combadge: combadgeBundle(t),
		Harness:  []string{"--harness", "holodeck", "--script", script},
		Lifetime: time.Hour + 8*time.Second,
	})
	require.NoError(t, err)
	blocked := make(chan struct{})
	p := &watchProvider{Holodeck: h, gate: make(chan struct{}), blocked: blocked}
	b.provider, b.lead = p, time.Hour
	// Long enough for the stopping checkpoint to arrive, which the held
	// pull then outlasts, with the settle wait after it.
	b.standDown, b.settle = 4*time.Second, 500*time.Millisecond
	hd := b.start(t)
	c := hd.addCrew(t)
	id := hd.createOrder(t)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.OrderAway, hd.order(ct, id).State)
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 50, "progress": "resumed"},
		{"at_pct": 100, "report": {"summary": "done after a slow pull"}}]`), 0o600))

	select {
	case <-blocked:
	case <-time.After(30 * time.Second):
		t.Fatal("the stopping checkpoint was never pulled")
	}
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.True(ct, hasStep(ct, hd.d, missionWorkflowID(id), "park"))
	}, 10*time.Second, 20*time.Millisecond, "parked before the pull")
	require.False(t, hasStep(t, hd.d, missionWorkflowID(id), "pull-checkpoint"), "the held pull is the stopping checkpoint's")
	// Both waits pass with the pull held: tour 0 hands over and tour 1
	// claims the parked order, removing the old beam.
	tour1 := tourWorkflowID("sorell", 1)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.True(ct, hasStep(ct, hd.d, tour1, "claim-order"))
	}, 30*time.Second, 50*time.Millisecond)
	require.Equal(t, CrewTourResult{Ended: TourEndedWatchChange}, tourResult(t, hd.d, tourWorkflowID("sorell", 0)))
	require.Equal(t, store.OrderAway, hd.order(t, id).State, "parked, not backstopped")
	p.mu.Lock()
	close(p.gate)
	p.gate = nil
	p.mu.Unlock()

	// The pull retries against the gone beam (copyRetries, about 30s).
	mh, err := dbos.RetrieveWorkflow[MissionResult](hd.d.dbos, missionWorkflowID(id))
	require.NoError(t, err)
	res, err := mh.GetResult(dbos.WithHandleTimeout(90 * time.Second))
	require.NoError(t, err)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "done after a slow pull", res.Report.Summary)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.True(ct, hasStep(ct, hd.d, tour1, "settle-mission"))
	}, 10*time.Second, 50*time.Millisecond)
	require.False(t, hasStep(t, hd.d, tour1, "mission-parked"), "tour 0's late mission_parked taken for a recall")
	cps, err := hd.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Empty(t, cps, "the old beam was gone when the pull ran")
	entries := hd.logEntries(t)
	require.Zero(t, kinds(entries)[logFail])
	requireNoBackstop(t, entries)
	var failedPull bool
	for _, e := range entries {
		failedPull = failedPull || (e.OrderID == id && strings.Contains(e.Text, "could not be pulled") &&
			strings.Contains(e.Text, c.CurrentBeam))
	}
	require.True(t, failedPull, "the pull went to the old beam %s: %+v", c.CurrentBeam, orderTexts(entries, id, "checkpoint"))
}
