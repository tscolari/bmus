package bridge

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// attachGate holds a pump's attaches back until release, as a serve that
// isn't letting attaches through yet would, so its hello is late (#173). It
// keeps the sessions it let through.
type attachGate struct {
	open chan struct{}
	once sync.Once

	mu       sync.Mutex
	refused  int
	sessions []comms.Session
}

func newAttachGate() *attachGate { return &attachGate{open: make(chan struct{})} }

func (g *attachGate) release() { g.once.Do(func() { close(g.open) }) }

// wrap is a holodeckBridge.tunePump: the pump attaches through g.
func (g *attachGate) wrap(c *comms.PumpConfig) { c.Link = gatedLink{Link: c.Link, g: g} }

func (g *attachGate) refusals() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refused
}

func (g *attachGate) last() comms.Session {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sessions) == 0 {
		return nil
	}
	return g.sessions[len(g.sessions)-1]
}

type gatedLink struct {
	comms.Link
	g *attachGate
}

func (l gatedLink) Attach(ctx context.Context, since uint64) (comms.Session, error) {
	select {
	case <-l.g.open:
	default:
		l.g.mu.Lock()
		l.g.refused++
		l.g.mu.Unlock()
		return nil, errors.New("attach gate: serve not letting attaches through yet")
	}
	s, err := l.Link.Attach(ctx, since)
	if err == nil {
		l.g.mu.Lock()
		l.g.sessions = append(l.g.sessions, s)
		l.g.mu.Unlock()
	}
	return s, err
}

// tourSteps returns the names of tourID's recorded steps, in order.
func tourSteps(t require.TestingT, d *Daemon, tourID string) ([]string, []dbos.StepInfo) {
	steps, err := dbos.GetWorkflowSteps(d.dbos, tourID)
	require.NoError(t, err)
	var names []string
	for _, s := range steps {
		names = append(names, s.StepName)
	}
	return names, steps
}

// requireAwaitingHello waits until tourID is in its hello wait with its
// pump's attaches refused, and has received an order_assigned there (a
// directed order created meanwhile), with no claim made.
func requireAwaitingHello(t *testing.T, d *Daemon, g *attachGate, tourID string) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		names, steps := tourSteps(ct, d, tourID)
		require.Contains(ct, names, "hello-deadline")
		require.NotContains(ct, names, "hello-logged")
		require.NotContains(ct, names, "claim-order")
		require.Positive(ct, countSignal(steps, SignalOrderAssigned), "the order's wake-up not received yet")
		require.Positive(ct, g.refusals())
	}, 30*time.Second, 50*time.Millisecond)
}

// helloBeforeClaim requires the tour's hello-logged step to come before its
// first claim-order.
func helloBeforeClaim(t require.TestingT, names []string) {
	hello, claim := slices.Index(names, "hello-logged"), slices.Index(names, "claim-order")
	require.GreaterOrEqual(t, hello, 0, "no hello-logged step: %v", names)
	require.Greater(t, claim, hello, "steps: %v", names)
}

// quickScript writes a holodeck script that reports at once.
func quickScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "quick.json")
	require.NoError(t, os.WriteFile(p, []byte(`[{"at_pct": 50, "progress": "half"}, {"at_pct": 100, "report": {"summary": "done"}}]`), 0o600))
	return p
}

// addSorell adds sorell and waits until its tour has provisioned a beam:
// sorell is beaming in with it, or on deck already if its hello came
// (#106).
func addSorell(t *testing.T, hd *hdaemon) store.Crew {
	t.Helper()
	require.NoError(t, hd.d.AddCrew(t.Context(), store.Crew{ID: "sorell", Name: "Sorell", Rank: "Lieutenant"}))
	var c store.Crew
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		c = hd.crew(ct, "sorell")
		require.Contains(ct, []string{store.CrewBeamingIn, store.CrewOnDeck}, c.Status)
		require.NotEmpty(ct, c.CurrentBeam)
	}, 30*time.Second, 50*time.Millisecond)
	return c
}

// #173: a combadge whose hello is late gets no order until it says hello,
// though a directed order waits. The crew member is beaming in with its
// beam the whole time, and on deck from the hello (#106).
func TestTourWaitsForHello(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, quickScript(t))
	g := newAttachGate()
	b.tunePump = g.wrap
	hd := b.start(t)
	c := addSorell(t, hd)
	id := hd.createOrder(t)
	tourID := tourWorkflowID("sorell", 0)

	requireAwaitingHello(t, hd.d, g, tourID)
	o := hd.order(t, id)
	require.Equal(t, store.OrderDirected, o.State)
	require.Empty(t, workflows(t, hd.d, missionWorkflowID(id)), "no mission before the hello")
	cr := hd.crew(t, "sorell")
	require.Equal(t, store.CrewBeamingIn, cr.Status)
	require.Equal(t, c.CurrentBeam, cr.CurrentBeam)
	s, err := hd.d.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, CrewCounts{BeamingIn: 1}, s.Crew)
	require.Equal(t, 1, s.BeamsLive)
	require.Equal(t, ConditionGreen, s.Condition, "no login wait: no reason")
	_, err = os.Stat(b.stepLog)
	require.ErrorIs(t, err, os.ErrNotExist, "the holodeck ran no step")

	g.release()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Contains(ct, []string{store.OrderAway, store.OrderDone}, hd.order(ct, id).State)
		require.Len(ct, workflows(ct, hd.d, missionWorkflowID(id)), 1)
	}, 30*time.Second, 50*time.Millisecond)
	names, _ := tourSteps(t, hd.d, tourID)
	helloBeforeClaim(t, names)
	entries := hd.logEntries(t)
	hello := slices.IndexFunc(entries, func(e store.LogEntry) bool {
		return e.Kind == logCrew && e.Text == "crew on deck: combadge said hello"
	})
	away := slices.IndexFunc(entries, func(e store.LogEntry) bool {
		return e.OrderID == id && e.Text == "order away: mission started"
	})
	require.GreaterOrEqual(t, hello, 0)
	require.Greater(t, away, hello, "the hello is logged before the order goes away")
	require.True(t, slices.ContainsFunc(entries, func(e store.LogEntry) bool {
		return e.Text == "beam "+c.CurrentBeam+" up, combadge started"
	}))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
}

// #173: a combadge that exits before its hello sends the crew member to
// sickbay, through the pump's sickbay check, and is never sent the order
// waiting for it.
func TestTourExitBeforeHello(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, quickScript(t))
	g := newAttachGate() // never released
	b.tunePump = g.wrap
	// The gate refuses every attach, so no line arrives and there is no
	// connection to recycle: a 1s staleness only shortens the wait for the
	// killed beam's probe (#197).
	b.heartbeatStale = time.Second
	hd := b.start(t)
	c := addSorell(t, hd)
	id := hd.createOrder(t)
	tourID := tourWorkflowID("sorell", 0)
	requireAwaitingHello(t, hd.d, g, tourID)

	require.NoError(t, b.h.Kill(c.CurrentBeam))
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, hd.d, tourID))
	require.Equal(t, store.CrewSickbay, hd.crew(t, "sorell").Status)
	require.Equal(t, store.OrderDirected, hd.order(t, id).State)
	require.Empty(t, workflows(t, hd.d, missionWorkflowID(id)))
	names, _ := tourSteps(t, hd.d, tourID)
	require.NotContains(t, names, "hello-logged")
	require.NotContains(t, names, "claim-order")
	require.True(t, slices.ContainsFunc(hd.logEntries(t), func(e store.LogEntry) bool {
		return strings.HasPrefix(e.Text, "crew to sickbay: beam lost")
	}))
}

// #173: a serve that runs but never lets an attach through ends its tour in
// sickbay, code no_hello, once helloTimeout has passed.
func TestTourNoHelloTimeout(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, quickScript(t))
	g := newAttachGate() // never released
	b.tunePump, b.helloTimeout = g.wrap, 2*time.Second
	hd := b.start(t)
	addSorell(t, hd)
	tourID := tourWorkflowID("sorell", 0)
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, hd.d, tourID))
	require.Equal(t, store.CrewSickbay, hd.crew(t, "sorell").Status)
	require.Positive(t, g.refusals())
	var sick store.LogEntry
	for _, e := range hd.logEntries(t) {
		if strings.HasPrefix(e.Text, "crew to sickbay:") {
			sick = e
		}
	}
	require.Contains(t, sick.Text, "did not say hello within 2s")
	require.Contains(t, string(sick.Payload), `"code":"`+CodeNoHello+`"`)
}

// #173: a reconnect mid-mission brings a second hello, which the tour takes
// without a warning; the mission keeps running.
func TestTourReconnectHello(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "default.json"))
	g := newAttachGate()
	g.release()
	b.tunePump = g.wrap
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	tourID := tourWorkflowID("sorell", 0)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.OrderHailing, hd.order(ct, id).State)
	}, 30*time.Second, 50*time.Millisecond)
	_, steps := tourSteps(t, hd.d, tourID)
	before := countSignal(steps, SignalCrewHello)
	require.Positive(t, before)

	// attach exits once its stdin closes, and the pump attaches again. (A
	// loaded machine may have recycled that session already: its error
	// doesn't matter, the next hello does.)
	_ = g.last().Close()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		_, steps := tourSteps(ct, hd.d, tourID)
		require.Greater(ct, countSignal(steps, SignalCrewHello), before)
	}, 30*time.Second, 50*time.Millisecond)

	requirePending(t, hd.d, missionWorkflowID(id))
	requirePending(t, hd.d, tourID)
	require.Equal(t, store.OrderHailing, hd.order(t, id).State)
	require.Equal(t, store.CrewHailing, hd.crew(t, "sorell").Status)
	logs, err := os.ReadFile(b.logFile)
	require.NoError(t, err)
	for _, line := range strings.Split(string(logs), "\n") {
		if strings.Contains(line, "crew tour:") {
			require.NotContains(t, line, "level=WARN", "the tour warned")
		}
	}
	require.Equal(t, 1, strings.Count(hd.logText(t), "crew on deck: combadge said hello"))
}

// logText joins the captain's log's texts, one per line.
func (hd *hdaemon) logText(t require.TestingT) string {
	var b strings.Builder
	for _, e := range hd.logEntries(t) {
		b.WriteString(e.Text + "\n")
	}
	return b.String()
}

// #173: a bridge restart while the tour waits for its hello ends the wait
// on the hello serve sends on the new bridge's attach, and the waiting
// order goes away then.
func TestTourRestartDuringHelloWait(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, quickScript(t))
	g := newAttachGate() // never released: the first bridge sees no hello
	b.tunePump = g.wrap
	hd := b.start(t)
	addSorell(t, hd)
	id := hd.createOrder(t)
	tourID := tourWorkflowID("sorell", 0)
	requireAwaitingHello(t, hd.d, g, tourID)
	require.Equal(t, store.CrewBeamingIn, hd.crew(t, "sorell").Status)
	hd.stop(t)

	// ensurePumps attaches the beaming-in crew member's beam (#106).
	b.tunePump = nil
	hd = b.start(t)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Len(ct, workflows(ct, hd.d, missionWorkflowID(id)), 1)
	}, 30*time.Second, 50*time.Millisecond)
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	names, _ := tourSteps(t, hd.d, tourID)
	helloBeforeClaim(t, names)
	count := func(name string) int {
		n := 0
		for _, s := range names {
			if s == name {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, count("hello-deadline"), "the resumed tour kept its wait")
	require.Equal(t, 1, count("hello-logged"))
	logs, err := os.ReadFile(b.logFile)
	require.NoError(t, err)
	require.NotContains(t, string(logs), "Check that your workflow is deterministic")
}

// #173 review: a bridge down past the tour's hello deadline (a laptop
// asleep) doesn't send a healthy crew member to sickbay. The resumed Recv
// times out at once, hello-extend grants the young daemon a fresh wait, and
// the re-attach's hello ends it.
func TestTourRestartPastHelloDeadline(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, quickScript(t))
	g := newAttachGate() // never released: the first bridge sees no hello
	b.tunePump, b.helloTimeout = g.wrap, 10*time.Second
	hd := b.start(t)
	addSorell(t, hd)
	tourID := tourWorkflowID("sorell", 0)
	var out string
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var ok bool
		out, ok = stepOutput(ct, hd.d, tourID, "hello-deadline")
		require.True(ct, ok)
	}, 30*time.Second, 20*time.Millisecond)
	id := hd.createOrder(t)
	hd.stop(t)
	var deadline time.Time
	require.NoError(t, json.Unmarshal([]byte(out), &deadline))
	// The bridge stays down past the deadline.
	require.Eventually(t, func() bool { return time.Now().After(deadline) }, 30*time.Second, 50*time.Millisecond)

	// The new bridge's attach waits for hello-extend: a hello that reached
	// the tour before its resumed Recv timed out would end the wait without
	// the path this test is about (#197 CI).
	g = newAttachGate()
	b.tunePump, b.helloTimeout = g.wrap, 30*time.Second
	hd = b.start(t)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		_, ok := stepOutput(ct, hd.d, tourID, "hello-extend")
		require.True(ct, ok)
	}, 30*time.Second, 20*time.Millisecond)
	g.release()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Len(ct, workflows(ct, hd.d, missionWorkflowID(id)), 1)
	}, 60*time.Second, 50*time.Millisecond)
	require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
	require.NotEqual(t, store.CrewSickbay, hd.crew(t, "sorell").Status)
	names, steps := tourSteps(t, hd.d, tourID)
	helloBeforeClaim(t, names)
	ext := slices.Index(names, "hello-extend")
	require.GreaterOrEqual(t, ext, 0, "steps: %v", names)
	require.Less(t, ext, slices.Index(names, "hello-logged"))
	var extended time.Time
	require.NoError(t, json.Unmarshal([]byte(steps[ext].Output.(string)), &extended))
	require.True(t, extended.After(deadline), "a fresh deadline was recorded")
	require.False(t, slices.ContainsFunc(hd.logEntries(t), func(e store.LogEntry) bool {
		return strings.HasPrefix(e.Text, "crew to sickbay")
	}))
}

// #173: after a watch change, the new beam's tour waits for its own hello
// before it resumes the order parked on the old beam.
func TestWatchChangeWaitsForNewHello(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "script.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 40, "commit": "first change"},
		{"at_pct": 50, "sleep_ms": 120000},
		{"at_pct": 100, "report": {"summary": "never reached"}}]`), 0o600))
	b := newHolodeckBridge(t, script)
	const lead = time.Hour
	short, err := transporter.NewHolodeck(transporter.HolodeckConfig{
		Root: b.root, Combadge: combadgeBundle(t),
		Harness:  []string{"--harness", "holodeck", "--script", script},
		Lifetime: lead + 15*time.Second, // room for the order to commit first, under load
	})
	require.NoError(t, err)
	b.provider, b.lead = &watchProvider{Holodeck: short}, lead
	g := newAttachGate()
	b.tunePump = func(c *comms.PumpConfig) {
		if c.TourNo >= 1 {
			g.wrap(c)
		}
	}
	hd := b.start(t)
	c := hd.addCrew(t)
	id := hd.createOrder(t)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		committed := slices.ContainsFunc(readSteps(ct, b.stepLog), func(e map[string]any) bool {
			return e["order"] == id && e["action"] == "commit"
		})
		require.True(ct, committed)
	}, 60*time.Second, 20*time.Millisecond)
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 100, "report": {"summary": "done after the watch change"}}]`), 0o600))

	// The watch changes; tour 1's beam doesn't let an attach through, and
	// the parked order waits.
	tour1 := tourWorkflowID("sorell", 1)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		names, _ := tourSteps(ct, hd.d, tour1)
		require.Contains(ct, names, "hello-deadline")
		require.Positive(ct, g.refusals())
		cr := hd.crew(ct, "sorell")
		require.Equal(ct, 1, cr.TourNo)
		require.NotEmpty(ct, cr.CurrentBeam)
		require.NotEqual(ct, c.CurrentBeam, cr.CurrentBeam)
		require.NotNil(ct, hd.order(ct, id).ParkedAt)
	}, 60*time.Second, 50*time.Millisecond)
	require.Equal(t, CrewTourResult{Ended: TourEndedWatchChange}, tourResult(t, hd.d, tourWorkflowID("sorell", 0)))
	names, _ := tourSteps(t, hd.d, tour1)
	require.NotContains(t, names, "hello-logged")
	require.NotContains(t, names, "claim-order")
	require.Empty(t, orderTexts(hd.logEntries(t), id, "order resumed"))

	g.release()
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "done after the watch change", res.Report.Summary)
	names, _ = tourSteps(t, hd.d, tour1)
	helloBeforeClaim(t, names)
	require.Equal(t, []string{"order resumed on sorell"}, orderTexts(hd.logEntries(t), id, "order resumed"))
}
