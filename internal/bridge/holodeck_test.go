package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/tscolari/bmus/internal/tracker"
	"github.com/tscolari/bmus/internal/transporter"
)

// These tests run the whole bridge (daemon, crew tour, mission, comms pump)
// against the real combadge bundle in holodeck beams. Like the comms pump's
// holodeck tests they take the bundle from $BMUS_COMBADGE (CI sets it, and
// then a missing bundle or node fails the test) or build it with
// `npm run build`, skipping if neither works.

var buildBundle = sync.OnceValues(func() (string, error) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "combadge"))
	if err != nil {
		return "", err
	}
	if _, err := exec.LookPath("npm"); err != nil {
		return "", fmt.Errorf("npm not found: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		return "", fmt.Errorf("combadge dependencies not installed (run npm ci in %s): %w", dir, err)
	}
	cmd := exec.Command("npm", "run", "build")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("npm run build: %w: %s", err, out)
	}
	return filepath.Join(dir, "dist", "combadge.mjs"), nil
})

func combadgeBundle(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("BMUS_COMBADGE"); p != "" {
		abs, err := filepath.Abs(p)
		require.NoError(t, err)
		_, err = os.Stat(abs)
		require.NoError(t, err, "BMUS_COMBADGE")
		_, err = exec.LookPath("node")
		require.NoError(t, err, "BMUS_COMBADGE is set but node isn't on PATH")
		return abs
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("holodeck tests need node: %v (or set BMUS_COMBADGE in an environment with node)", err)
	}
	p, err := buildBundle()
	if err != nil {
		t.Skipf("holodeck tests need the combadge bundle: set BMUS_COMBADGE or make `npm run build` work: %v", err)
	}
	return p
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "combadge", "test", "fixtures", name))
	require.NoError(t, err)
	return p
}

// holodeckBridge is a bridge home plus a holodeck whose beams run one
// script, with serve heartbeats every 500ms and pump timings scaled to them
// (stale after 5s, checked every 100ms).
//
// Not 100ms and 1s (#197): the pump records each line, heartbeats included,
// with a store commit before it reads the next, and a commit under disk load
// can take hundreds of ms (440ms on CI, #193). At ten heartbeats a second
// the pump fell behind its beam: heartbeats read seconds stale while lines
// kept arriving, a healthy connection was recycled, acks failed on the
// closed pipe, and the replay delayed hellos, claims and hails past tests'
// waits (TestWatchChangeSlowStoppingPull, TestCountermandParkedDeliveredOnResume).
// A recycle is never a beam loss (that takes a failed probe), but it costs a
// reconnect and a replay. Tests that wait for a killed beam with no mission
// after it set heartbeatStale lower.
type holodeckBridge struct {
	home    string
	root    string // holodeck beams
	stepLog string // holodeck step log, shared by every beam
	logFile string // the daemons' logs, printed if the test fails
	h       *transporter.Holodeck
	repo    upstream // registered as app
	// provider, if set, is the daemons' transporter instead of h; lead,
	// if set, their watch-change lead.
	provider transporter.Provider
	lead     time.Duration
	// standDown and settle, if set, are the daemons' watch-change and
	// settle waits.
	standDown, settle time.Duration
	// checkEvery, if set, is the daemons' missionCheckEvery; wakeSend, if
	// set, their wakeDivision send.
	checkEvery time.Duration
	wakeSend   func(tourID string, sig CrewSignal) error
	// divisions, if set, are the daemons' roster divisions.
	divisions map[string]DivisionConfig
	// heartbeatStale, if set, replaces the pumps' 5s staleness.
	heartbeatStale time.Duration
	// console runs the daemons with the console on.
	console bool
	// regs, if set, is the daemons' regs.yaml; nil runs them without one.
	regs *regs.Config
	// consultTimeout, if set, is the daemons' Config.ConsultTimeout.
	consultTimeout time.Duration
	// pullWorkHook and sendOrderHook, if set, are the daemons' Config's.
	pullWorkHook, sendOrderHook func(d *Daemon, orderID string)
	// tunePump, if set, adjusts the daemons' pump configs after the
	// timings above (an attachGate's wrap, say). A daemon keeps the one
	// set when it started.
	tunePump func(*comms.PumpConfig)
	// helloTimeout, if set, is the daemons' Config.helloTimeout.
	helloTimeout time.Duration
	// tracker, if set, is the daemons' Config.Tracker.
	tracker tracker.Tracker
}

func newHolodeckBridge(t *testing.T, script string) *holodeckBridge {
	t.Helper()
	bundle := combadgeBundle(t)
	// t.TempDir paths can push serve.sock past the Unix socket limit.
	base := os.TempDir()
	if len(base) > 40 {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "bridge")
	require.NoError(t, err)
	h, err := transporter.NewHolodeck(transporter.HolodeckConfig{
		Root:     root,
		Combadge: bundle,
		Harness:  []string{"--harness", "holodeck", "--script", script},
	})
	require.NoError(t, err)
	b := &holodeckBridge{
		home: privateHome(t), root: root, h: h,
		stepLog: filepath.Join(root, "steps.ndjson"),
		logFile: filepath.Join(t.TempDir(), "bridge.log"),
		repo:    testRepo(t),
	}
	// Registered before any daemon's cleanup, so it runs after the daemons
	// stop: Remove stops every serve, so none outlives the test.
	t.Cleanup(func() {
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.IsDir() {
				assert.NoError(t, h.Remove(context.Background(), transporter.Beam{Alias: e.Name()}))
			}
		}
		os.RemoveAll(root)
		if t.Failed() {
			out, _ := os.ReadFile(b.logFile)
			t.Logf("bridge log:\n%s", tail(string(out), 200))
		}
	})
	return b
}

func tail(s string, lines int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > lines {
		ls = ls[len(ls)-lines:]
	}
	return strings.Join(ls, "\n")
}

// hdaemon is a running daemon of a holodeckBridge.
type hdaemon struct {
	d      *Daemon
	cancel context.CancelFunc
	done   <-chan error
	once   sync.Once
}

// start runs a daemon on b's home, stopped at the end of the test unless
// stop is called first.
func (b *holodeckBridge) start(t *testing.T) *hdaemon {
	t.Helper()
	f, err := os.OpenFile(b.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan *Daemon, 1)
	tune := b.tunePump
	var provider transporter.Provider = b.h
	if b.provider != nil {
		provider = b.provider
	}
	cfg := Config{
		Home:         b.home,
		Transporter:  provider,
		Logger:       slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})),
		CombadgeArgs: []string{"--heartbeat-interval", "500", "--step-log", b.stepLog},
		Repos:        map[string]string{"app": b.repo.url},
		NoConsole:    !b.console,
		onReady:      func(d *Daemon) { ready <- d },
		tunePump: func(c *comms.PumpConfig) {
			c.HeartbeatStale = cmp.Or(b.heartbeatStale, 5*time.Second)
			c.SickbayCheckEvery = 100 * time.Millisecond
			c.BackoffMin, c.BackoffMax = 50*time.Millisecond, 500*time.Millisecond
			if tune != nil {
				tune(c)
			}
		},
		watchChangeLead:   b.lead,
		standDownTimeout:  b.standDown,
		settleTimeout:     b.settle,
		helloTimeout:      b.helloTimeout,
		missionCheckEvery: b.checkEvery,
		wakeSend:          b.wakeSend,
		Divisions:         b.divisions,
		ConsultTimeout:    b.consultTimeout,
		pullWorkHook:      b.pullWorkHook,
		sendOrderHook:     b.sendOrderHook,
		Tracker:           b.tracker,
	}
	if b.regs != nil {
		cfg.Regs, cfg.RegsConfigured = *b.regs, true
	}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	select {
	case d := <-ready:
		hd := &hdaemon{d: d, cancel: cancel, done: done}
		t.Cleanup(func() { hd.stop(t) })
		return hd
	case err := <-done:
		cancel()
		t.Fatalf("Run returned before ready: %v", err)
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("daemon not ready")
	}
	return nil
}

// stop shuts the daemon down gracefully and requires Run to return nil.
func (hd *hdaemon) stop(t *testing.T) {
	hd.once.Do(func() {
		hd.cancel()
		select {
		case err := <-hd.done:
			assert.NoError(t, err)
		case <-time.After(90 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
}

func (hd *hdaemon) crew(t require.TestingT, id string) store.Crew {
	c, err := hd.d.store.GetCrew(context.Background(), id)
	require.NoError(t, err)
	return c
}

func (hd *hdaemon) order(t require.TestingT, id string) store.Order {
	o, err := hd.d.store.GetOrder(context.Background(), id)
	require.NoError(t, err)
	return o
}

func (hd *hdaemon) logEntries(t require.TestingT) []store.LogEntry {
	e, err := hd.d.store.ListLog(context.Background(), 0)
	require.NoError(t, err)
	return e
}

// addCrew adds sorell and waits until its tour has provisioned a beam, the
// crew member is on_deck and the pump is connected.
func (hd *hdaemon) addCrew(t *testing.T) store.Crew {
	t.Helper()
	return hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Rank: "Lieutenant"})
}

// addCrewWith is addCrew with crew member add.
func (hd *hdaemon) addCrewWith(t *testing.T, add store.Crew) store.Crew {
	t.Helper()
	require.NoError(t, hd.d.AddCrew(t.Context(), add))
	var c store.Crew
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		c = hd.crew(ct, add.ID)
		require.Equal(ct, store.CrewOnDeck, c.Status)
		require.NotEmpty(ct, c.CurrentBeam)
		p, ok := hd.d.Pump(add.ID)
		require.True(ct, ok)
		require.True(ct, p.Connected())
	}, 30*time.Second, 50*time.Millisecond)
	return c
}

func (hd *hdaemon) createOrder(t *testing.T) string {
	t.Helper()
	id, err := hd.d.CreateOrder(t.Context(), store.Order{Title: "holodeck run", Body: "walk the script", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	return id
}

// waitHail waits for hail id to be open, its crew member and order hailing,
// and returns it.
func (hd *hdaemon) waitHail(t *testing.T, id string) store.Hail {
	t.Helper()
	var h store.Hail
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var err error
		h, err = hd.d.store.GetHail(context.Background(), id)
		require.NoError(ct, err)
		require.Equal(ct, store.HailOpen, h.Status)
		require.Equal(ct, store.CrewHailing, hd.crew(ct, "sorell").Status)
		require.Equal(ct, store.OrderHailing, hd.order(ct, h.OrderID).State)
	}, 30*time.Second, 50*time.Millisecond)
	return h
}

func missionResult(t *testing.T, d *Daemon, orderID string) MissionResult {
	t.Helper()
	h, err := dbos.RetrieveWorkflow[MissionResult](d.dbos, missionWorkflowID(orderID))
	require.NoError(t, err)
	res, err := h.GetResult(dbos.WithHandleTimeout(60 * time.Second))
	require.NoError(t, err)
	return res
}

func kinds(entries []store.LogEntry) map[string]int {
	m := map[string]int{}
	for _, e := range entries {
		m[e.Kind]++
	}
	return m
}

func requireNoBackstop(t *testing.T, entries []store.LogEntry) {
	t.Helper()
	for _, e := range entries {
		require.NotEqual(t, "mission ended without settling", e.Text, "the tour's backstop had to settle an order")
	}
}

// requireCheckpointFiles checks a checkpoint row's files are in the cargo
// bay under <home>/cargo/<order>/<beam>-<seq>/: the transcript, the note and
// app's bundle, which carries bmus/<order> at commit.
func requireCheckpointFiles(t *testing.T, home string, cp store.Checkpoint, handover, commit string) {
	t.Helper()
	require.NotEmpty(t, cp.Beam)
	dir := filepath.Join(home, "cargo", cp.OrderID, fmt.Sprintf("%s-%d", cp.Beam, cp.Seq))
	bundle := filepath.Join(dir, "app.bundle")
	require.Equal(t, []store.CheckpointRepo{{Name: "app", Commit: commit, BundlePath: bundle}}, cp.Repos)
	require.Equal(t, filepath.Join(dir, "transcript.jsonl"), cp.TranscriptPath)
	require.Equal(t, filepath.Join(dir, "handover.md"), cp.HandoverPath)
	require.Equal(t, commit+" refs/heads/bmus/"+cp.OrderID, bundleHeads(t, bundle))
	transcript, err := os.ReadFile(cp.TranscriptPath)
	require.NoError(t, err)
	require.Contains(t, string(transcript), cp.OrderID)
	note, err := os.ReadFile(cp.HandoverPath)
	require.NoError(t, err)
	require.Equal(t, handover+"\n", string(note))
}

// stepLogEntries reads the holodeck step log and requires that no (order,
// index) step ran twice.
func stepLogEntries(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []map[string]any
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &e))
		if e["action"] != "consult" && e["action"] != "correction" { // neither is a step of an order
			key := fmt.Sprintf("%v/%v", e["order"], e["index"])
			require.False(t, seen[key], "step %s ran twice", key)
			seen[key] = true
		}
		out = append(out, e)
	}
	return out
}

// #15's acceptance: a crew member with no orders sits on_deck; a directed
// order moves it away and starts a mission within about one Recv cycle.
func TestCrewTourOnDeckThenAway(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "default.json"))
	hd := b.start(t)
	c := hd.addCrew(t)

	beams, err := hd.d.store.ListBeams(t.Context(), "sorell")
	require.NoError(t, err)
	require.Len(t, beams, 1)
	require.Equal(t, c.CurrentBeam, beams[0].Alias)

	// Nothing to do: it stays on deck, its tour waiting.
	time.Sleep(2 * time.Second)
	require.Equal(t, store.CrewOnDeck, hd.crew(t, "sorell").Status)
	requirePending(t, hd.d, tourWorkflowID("sorell", 0))
	all, err := dbos.ListWorkflows(hd.d.dbos, dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	require.NoError(t, err)
	require.Len(t, all, 2, "only the tour runs, its transporter workflow done")
	for _, w := range all {
		if w.ID != tourWorkflowID("sorell", 0) {
			require.Equal(t, beamWorkflowID("sorell", 0), w.ID)
			require.Equal(t, dbos.WorkflowStatusSuccess, w.Status)
		}
	}

	start := time.Now()
	id := hd.createOrder(t)
	require.Equal(t, "ORD-0001", id)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.OrderAway, hd.order(ct, id).State)
		require.Contains(ct, []string{store.CrewAway, store.CrewHailing}, hd.crew(ct, "sorell").Status)
		require.Len(ct, workflows(ct, hd.d, missionWorkflowID(id)), 1)
	}, 5*time.Second, 20*time.Millisecond)
	t.Logf("order away and mission started %s after CreateOrder", time.Since(start).Round(time.Millisecond))

	// An unknown assignee is refused.
	_, err = hd.d.CreateOrder(t.Context(), store.Order{Title: "x", Assignee: "nobody"})
	require.ErrorIs(t, err, ErrNotFound)
}

// #58: the order message carries the order's model, else the crew
// member's, and the harness gets it as sent (the holodeck records it in its
// step log).
func TestOrderModelReachesHarness(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "short.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 50, "progress": "half"}, {"at_pct": 100, "report": {"summary": "done"}}]`), 0o600))
	b := newHolodeckBridge(t, script)
	hd := b.start(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Model: "model-crew"})

	var ids []string
	for _, model := range []string{"model-x", ""} {
		id, err := hd.d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: "sorell", Model: model,
			Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
		require.NoError(t, err)
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			require.Len(c, workflows(c, hd.d, missionWorkflowID(id)), 1)
		}, 30*time.Second, 20*time.Millisecond)
		require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
		ids = append(ids, id)
	}
	got := map[string][]any{}
	for _, e := range stepLogEntries(t, b.stepLog) {
		got[e["order"].(string)] = append(got[e["order"].(string)], e["model"])
	}
	require.Equal(t, map[string][]any{ids[0]: {"model-x", "model-x"}, ids[1]: {"model-crew", "model-crew"}}, got)
}

// #59's acceptance: with a division default model m1, a crew member
// overriding it with m2 and an order overriding both with m3, each order
// reaches its harness with its model, and crew.list shows each value's
// source.
func TestDivisionModelReachesHarness(t *testing.T) {
	t.Parallel()
	script := filepath.Join(t.TempDir(), "short.json")
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 100, "report": {"summary": "done"}}]`), 0o600))
	b := newHolodeckBridge(t, script)
	b.divisions = map[string]DivisionConfig{"ops": {Model: "m1"}}
	hd := b.start(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	hd.addCrewWith(t, store.Crew{ID: "tuvok", Name: "Tuvok", Division: "ops", Model: "m2"})

	want := map[string]any{}
	for _, o := range []struct{ assignee, model, want string }{
		{"sorell", "", "m1"},
		{"tuvok", "", "m2"},
		{"tuvok", "m3", "m3"},
	} {
		id, err := hd.d.CreateOrder(t.Context(), store.Order{Title: "t", Assignee: o.assignee, Model: o.model,
			Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
		require.NoError(t, err)
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			require.Len(c, workflows(c, hd.d, missionWorkflowID(id)), 1)
		}, 30*time.Second, 20*time.Millisecond)
		require.Equal(t, MissionDone, missionResult(t, hd.d, id).Status)
		want[id] = o.want
	}
	got := map[string]any{}
	for _, e := range stepLogEntries(t, b.stepLog) {
		got[e["order"].(string)] = e["model"]
	}
	require.Equal(t, want, got)

	var crew CrewListResult
	require.NoError(t, call(t, b.home, "crew.list", nil, &crew))
	require.Len(t, crew.Crew, 2)
	sources := map[string][4]string{}
	for _, c := range crew.Crew {
		sources[c.ID] = [4]string{c.Harness, c.HarnessSource, c.Model, c.ModelSource}
	}
	require.Equal(t, map[string][4]string{
		"sorell": {"", SourceDefault, "m1", SourceDivision},
		"tuvok":  {"", SourceDefault, "m2", SourceCrew},
	}, sources)
}

// #16's acceptance: the default script's mission blocks at its hail until
// the captain answers, pulls the request_handover checkpoint into the cargo
// bay and reaches done; the crew member is back on deck.
func TestMissionHailCheckpointDone(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "default.json"))
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)

	h := hd.waitHail(t, "H-0001")
	require.Equal(t, id, h.OrderID)
	require.Equal(t, comms.CategoryToolApproval, h.Category)
	require.Equal(t, []string{comms.OptionAllow, comms.OptionDeny}, h.Options)

	// Blocked until answered.
	time.Sleep(1500 * time.Millisecond)
	require.Equal(t, store.HailOpen, hd.waitHail(t, "H-0001").Status)
	require.Equal(t, store.OrderHailing, hd.order(t, id).State)
	cps, err := hd.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Empty(t, cps)

	// Bad answers are refused.
	bad := 2
	require.Error(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &bad))
	require.ErrorIs(t, hd.d.AnswerHail(t.Context(), "H-9999", "yes", nil), ErrNotFound)

	// Two captains answer at once: exactly one wins, the other is told the
	// hail is closed, and only the winner's answer reaches the beam.
	var (
		wg       sync.WaitGroup
		errs     [2]error
		decision = [2]string{comms.OptionAllow, comms.OptionDeny}
	)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opt := i
			errs[i] = hd.d.AnswerHail(context.Background(), "H-0001", "", &opt)
		}()
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		if err == nil {
			require.Equal(t, -1, winner, "both answers succeeded")
			winner = i
		} else {
			require.ErrorIs(t, err, store.ErrHailClosed)
		}
	}
	require.NotEqual(t, -1, winner, "neither answer succeeded: %v", errs)
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "the holodeck script reports whatever the decision")
	require.NotNil(t, res.Report)
	require.Equal(t, "holodeck mission complete", res.Report.Summary)
	require.Equal(t, store.OrderDone, hd.order(t, id).State)

	h, err = hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, store.HailDecided, h.Status)
	require.Equal(t, comms.DecidedByCaptain, h.DecidedBy)
	require.Equal(t, decision[winner], h.Decision)
	require.ErrorIs(t, hd.d.AnswerHail(t.Context(), "H-0001", "deny", nil), store.ErrHailClosed)

	cps, err = hd.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	requireCheckpointFiles(t, b.home, cps[0], "mid-mission checkpoint", b.repo.rev(t, "main"))

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
	}, 10*time.Second, 50*time.Millisecond)
	requirePending(t, hd.d, tourWorkflowID("sorell", 0))

	// The captain's log has the whole story, report included.
	var entries []store.LogEntry
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		entries = hd.logEntries(ct)
		k := kinds(entries)
		require.Equal(ct, 2, k[logBeam], "beam created and up")
		require.Equal(ct, 3, k[logHail], "hail opened, routed to the captain and decided")
		require.Equal(ct, 1, k[logDone])
		require.Zero(ct, k[logFail])
		// created, away, repo shipped, progress x2, checkpoint
		require.Equal(ct, 6, k[logOrder])
	}, 5*time.Second, 50*time.Millisecond, "%+v", entries)
	main := b.repo.rev(t, "main")
	for _, e := range entries {
		if e.Kind == logDone {
			require.Equal(t, "holodeck mission complete (app: bmus/"+id+" at "+main[:12]+")", e.Text)
			var p donePayload
			require.NoError(t, json.Unmarshal(e.Payload, &p))
			require.Equal(t, "holodeck mission complete", p.Summary)
			require.Equal(t, []landedRepo{{Name: "app", Branch: "bmus/" + id, Commit: main,
				Bundle: filepath.Join(b.home, "cargo", id, "final", "app.bundle")}}, p.Repos)
		}
	}
	// No commits: the work is the base, with its whole history.
	require.Equal(t, main, gitT(t, filepath.Join(b.home, "repos", "app"), "rev-parse", "bmus/"+id))
	requireNoBackstop(t, entries)

	steps := stepLogEntries(t, b.stepLog)
	require.Len(t, steps, 5)
	require.Equal(t, decision[winner], steps[1]["decision"])
}

// A busy tour leaves a new order directed and claims it after the running
// mission's mission_done; stale mission_done signals are ignored.
func TestBusyTourClaimsAfterMission(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "default.json"))
	hd := b.start(t)
	hd.addCrew(t)
	id1 := hd.createOrder(t)
	hd.waitHail(t, "H-0001")
	id2 := hd.createOrder(t)
	tourID := tourWorkflowID("sorell", 0)

	// A mission_done for an order that isn't running changes nothing.
	require.NoError(t, dbos.Send(hd.d.dbos, tourID, CrewSignal{Kind: SignalMissionDone, OrderID: id2}, crewEventsTopic))
	time.Sleep(2 * time.Second) // twice DBOS's notification poll
	require.Equal(t, store.OrderDirected, hd.order(t, id2).State, "busy: the new order waits")
	require.Equal(t, store.OrderHailing, hd.order(t, id1).State)
	require.Equal(t, store.CrewHailing, hd.crew(t, "sorell").Status)
	require.Empty(t, workflows(t, hd.d, missionWorkflowID(id2)))

	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	require.Equal(t, MissionDone, missionResult(t, hd.d, id1).Status)
	// The tour settles ORD-0001 and claims ORD-0002 straight away.
	h2 := hd.waitHail(t, "H-0002")
	require.Equal(t, id2, h2.OrderID)
	require.Equal(t, store.OrderHailing, hd.order(t, id2).State)

	// A repeated mission_done for the finished mission is stale too.
	require.NoError(t, dbos.Send(hd.d.dbos, tourID, CrewSignal{Kind: SignalMissionDone, OrderID: id1}, crewEventsTopic))
	time.Sleep(2 * time.Second)
	require.Equal(t, store.OrderHailing, hd.order(t, id2).State)
	require.Equal(t, store.CrewHailing, hd.crew(t, "sorell").Status)
	requirePending(t, hd.d, missionWorkflowID(id2))
	requireNoBackstop(t, hd.logEntries(t))
}

// Dismissal mid-mission (DismissCrew signals the live tour) fails the
// order through the mission (code dismissed), closes its open hail, stops
// the pump, ends the tour and removes the beam (#100).
func TestDismissMidMission(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "default.json"))
	hd := b.start(t)
	c := hd.addCrew(t)
	id := hd.createOrder(t)
	hd.waitHail(t, "H-0001")

	tourID := tourWorkflowID("sorell", 0)
	requireOutcome(t, OutcomeRequested)(hd.d.DismissCrew(t.Context(), "sorell"))
	h, err := dbos.RetrieveWorkflow[CrewTourResult](hd.d.dbos, tourID)
	require.NoError(t, err)
	tour, err := h.GetResult(dbos.WithHandleTimeout(30 * time.Second))
	require.NoError(t, err)
	require.Equal(t, CrewTourResult{Ended: SignalDismissed}, tour)

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionFailed, res.Status)
	require.Equal(t, CodeDismissed, res.Code)
	require.Equal(t, store.OrderFailed, hd.order(t, id).State, "settled before the tour ended")
	hail, err := hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, store.HailTimedOut, hail.Status)
	_, ok := hd.d.Pump("sorell")
	require.False(t, ok)
	require.Equal(t, store.CrewDismissed, hd.crew(t, "sorell").Status)
	exists, err := b.h.Exists(t.Context(), transporter.Beam{Alias: c.CurrentBeam})
	require.NoError(t, err)
	require.False(t, exists, "the beam was removed")
	requireOutcome(t, OutcomeDone)(hd.d.DismissCrew(t.Context(), "sorell")) // already dismissed
	_, err = hd.d.DismissCrew(t.Context(), "nobody")
	require.ErrorIs(t, err, ErrNotFound)
	requireNoBackstop(t, hd.logEntries(t))
}

// A crew member in sickbay has no live tour: DismissCrew dismisses it
// directly and tells its parked mission, which fails the order, and
// removes the dead beam (#100).
func TestDismissParkedInSickbay(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "crash.json"))
	hd := b.start(t)
	c := hd.addCrew(t)
	id := hd.createOrder(t)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cps, err := hd.d.store.ListCheckpoints(context.Background(), id)
		require.NoError(ct, err)
		require.Len(ct, cps, 1)
	}, 30*time.Second, 50*time.Millisecond)
	require.NoError(t, b.h.Kill(c.CurrentBeam))
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tourResult(t, hd.d, tourWorkflowID("sorell", 0)))
	require.NotNil(t, hd.order(t, id).ParkedAt)
	_, err := hd.d.ReenergizeCrew(t.Context(), "nobody")
	require.ErrorIs(t, err, ErrNotFound)

	requireOutcome(t, OutcomeDone)(hd.d.DismissCrew(t.Context(), "sorell"))
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionResult{Status: MissionFailed, Reason: "crew member dismissed", Code: CodeDismissed}, res)
	o := hd.order(t, id)
	require.Equal(t, store.OrderFailed, o.State)
	require.Nil(t, o.ParkedAt)
	require.Equal(t, store.CrewDismissed, hd.crew(t, "sorell").Status)
	exists, err := b.h.Exists(t.Context(), transporter.Beam{Alias: c.CurrentBeam})
	require.NoError(t, err)
	require.False(t, exists, "the beam was removed")
	var re *rpcError
	_, err = hd.d.ReenergizeCrew(t.Context(), "sorell")
	require.ErrorAs(t, err, &re, "dismissed: not in sickbay")
	require.Empty(t, workflows(t, hd.d, tourWorkflowID("sorell", 1)))
	requireNoBackstop(t, hd.logEntries(t))
}

// The Phase 1 exit criterion in holodeck, with Phase 2's parking: the beam
// dies mid-mission, after a checkpoint; the crew member lands in sickbay
// with the checkpoint in the cargo bay, and the mission parks, its order
// still away (#100). Re-energizing the crew member gives it a new tour and
// beam, which resume the mission from the checkpoint to done; a second
// re-energize starts nothing.
func TestSickbayMidMission(t *testing.T) {
	t.Parallel()
	// serve reads its script when it starts: the new beam's runs this
	// file's second version.
	script := filepath.Join(t.TempDir(), "script.json")
	data, err := os.ReadFile(fixture(t, "crash.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(script, data, 0o600))
	b := newHolodeckBridge(t, script)
	hd := b.start(t)
	c := hd.addCrew(t)
	id := hd.createOrder(t)

	var cps []store.Checkpoint
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		var err error
		cps, err = hd.d.store.ListCheckpoints(context.Background(), id)
		require.NoError(ct, err)
		require.Len(ct, cps, 1)
	}, 30*time.Second, 50*time.Millisecond)
	require.Equal(t, store.OrderAway, hd.order(t, id).State)

	require.NoError(t, b.h.Kill(c.CurrentBeam))
	killed := time.Now()

	tour := tourResult(t, hd.d, tourWorkflowID("sorell", 0))
	require.Equal(t, CrewTourResult{Ended: store.CrewSickbay}, tour)
	t.Logf("tour ended in sickbay %s after the kill", time.Since(killed).Round(time.Millisecond))

	cr := hd.crew(t, "sorell")
	require.Equal(t, store.CrewSickbay, cr.Status)
	require.Equal(t, c.CurrentBeam, cr.CurrentBeam)
	o := hd.order(t, id)
	require.Equal(t, store.OrderAway, o.State)
	require.NotNil(t, o.ParkedAt)
	_, ok := hd.d.Pump("sorell")
	require.False(t, ok, "pump stopped")
	requirePending(t, hd.d, missionWorkflowID(id))

	cps, err = hd.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	require.Equal(t, c.CurrentBeam, cps[0].Beam)
	requireCheckpointFiles(t, b.home, cps[0], "checkpoint before the beam is lost", b.repo.rev(t, "main"))

	entries := hd.logEntries(t)
	require.Zero(t, kinds(entries)[logFail])
	require.Equal(t, []string{"order parked: beam lost"}, orderTexts(entries, id, "order parked"))
	require.Contains(t, entries[len(entries)-1].Text, "sickbay")
	requireNoBackstop(t, entries)

	// Re-energize: a new tour (1) and beam resume the parked mission from
	// the checkpoint, on the script's second version.
	require.NoError(t, os.WriteFile(script, []byte(`[{"at_pct": 50, "progress": "resumed"},
		{"at_pct": 100, "report": {"summary": "done after sickbay"}}]`), 0o600))
	requireOutcome(t, OutcomeRequested)(hd.d.ReenergizeCrew(t.Context(), "sorell"))
	requireOutcome(t, OutcomeUnderWay)(hd.d.ReenergizeCrew(t.Context(), "sorell"))
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, "done after sickbay", res.Report.Summary)
	require.Equal(t, store.OrderDone, hd.order(t, id).State)
	require.Nil(t, hd.order(t, id).ParkedAt)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		cr := hd.crew(ct, "sorell")
		require.Equal(ct, store.CrewOnDeck, cr.Status)
		require.Equal(ct, 1, cr.TourNo)
		require.NotEqual(ct, c.CurrentBeam, cr.CurrentBeam)
	}, 10*time.Second, 50*time.Millisecond)
	requirePending(t, hd.d, tourWorkflowID("sorell", 1))
	require.Empty(t, workflows(t, hd.d, tourWorkflowID("sorell", 2)), "one re-energize, one tour")
	_, err = hd.d.ReenergizeCrew(t.Context(), "sorell")
	require.Error(t, err, "on deck: not in sickbay")

	// The new beam's run got the checkpoint's note, and the old beam is
	// gone.
	var resumed []map[string]any
	for _, e := range readSteps(t, b.stepLog) {
		if e["order"] == id && e["handover"] != nil {
			resumed = append(resumed, e)
		}
	}
	require.Len(t, resumed, 2)
	require.Equal(t, "checkpoint before the beam is lost", resumed[0]["handover"])
	exists, err := b.h.Exists(t.Context(), transporter.Beam{Alias: c.CurrentBeam})
	require.NoError(t, err)
	require.False(t, exists, "the old beam was removed")
	entries = hd.logEntries(t)
	require.Equal(t, 1, kinds(entries)[logDone])
	require.Zero(t, kinds(entries)[logFail])
	require.Equal(t, []string{"order resumed on sorell"}, orderTexts(entries, id, "order resumed"))
	requireNoBackstop(t, entries)
}

// requireOutcome returns a check that a DismissCrew or ReenergizeCrew call
// succeeded with outcome want.
func requireOutcome(t *testing.T, want string) func(string, error) {
	return func(got string, err error) {
		t.Helper()
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

// orderTexts returns the texts of orderID's entries starting with prefix.
func orderTexts(entries []store.LogEntry, orderID, prefix string) []string {
	var out []string
	for _, e := range entries {
		if e.OrderID == orderID && strings.HasPrefix(e.Text, prefix) {
			out = append(out, e.Text)
		}
	}
	return out
}

// readSteps reads the holodeck step log. A resumed order runs its script
// again, so unlike stepLogEntries it allows a step twice.
func readSteps(t require.TestingT, path string) []map[string]any {
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &e))
		out = append(out, e)
	}
	return out
}

// A crew-reported error fails the order through the mission's own path (a
// belay sent while the hail is open: the holodeck harness rejects the run,
// serve emits an error), closes the hail, and the tour goes back on deck
// and takes the next order.
func TestMissionCrewErrorFailsOrder(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "default.json"))
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	hd.waitHail(t, "H-0001")

	belay, err := comms.NewMessage(comms.TypeBelay, comms.BelayBody{Reason: "captain changed course"})
	require.NoError(t, err)
	belay.Order = id
	_, err = hd.d.SendCommand("sorell", belay)
	require.NoError(t, err)

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionFailed, res.Status)
	require.Equal(t, "belayed: captain changed course", res.Reason)
	require.Equal(t, store.OrderFailed, hd.order(t, id).State)
	h, err := hd.d.store.GetHail(t.Context(), "H-0001")
	require.NoError(t, err)
	require.Equal(t, store.HailTimedOut, h.Status, "the open hail is closed with the order")
	require.ErrorIs(t, hd.d.AnswerHail(t.Context(), "H-0001", "allow", nil), store.ErrHailClosed)

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
	}, 10*time.Second, 50*time.Millisecond)
	requirePending(t, hd.d, tourWorkflowID("sorell", 0))
	entries := hd.logEntries(t)
	requireNoBackstop(t, entries)
	require.Equal(t, 1, kinds(entries)[logFail])

	// The tour is free again: the next order goes away and hails.
	id2 := hd.createOrder(t)
	require.Equal(t, "ORD-0002", id2)
	h2 := hd.waitHail(t, "H-0002")
	require.Equal(t, id2, h2.OrderID)
	require.Equal(t, store.OrderHailing, hd.order(t, id2).State)
}

// The bridge restarts (gracefully, in process) while the mission waits on
// the captain; the next daemon resumes tour and mission from DBOS, the
// answer gets through and the mission completes, with no script step run
// twice. #18 covers the SIGKILL variant.
func TestMissionSurvivesGracefulRestart(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "default.json"))
	hd := b.start(t)
	c := hd.addCrew(t)
	id := hd.createOrder(t)
	hd.waitHail(t, "H-0001")
	hd.stop(t)

	hd = b.start(t)
	requirePending(t, hd.d, missionWorkflowID(id))
	requirePending(t, hd.d, tourWorkflowID("sorell", 0))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		p, ok := hd.d.Pump("sorell")
		require.True(ct, ok)
		require.True(ct, p.Connected())
	}, 20*time.Second, 50*time.Millisecond)
	require.Equal(t, c.CurrentBeam, hd.crew(t, "sorell").CurrentBeam, "the beam is reused, not reprovisioned")
	require.Equal(t, store.OrderHailing, hd.order(t, id).State, "still hailing after the restart")
	// The captain-routed hail's workflow recovered waiting, and the
	// answer wakes it (#149).
	requirePending(t, hd.d, hailWorkflowID("H-0001"))

	allow := 0
	require.NoError(t, hd.d.AnswerHail(t.Context(), "H-0001", "", &allow))
	require.Equal(t, HailResult{Decided: true, By: comms.DecidedByCaptain}, hailResult(t, hd.d, "H-0001"))
	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status)
	require.Equal(t, store.OrderDone, hd.order(t, id).State)
	cps, err := hd.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	requireCheckpointFiles(t, b.home, cps[0], "mid-mission checkpoint", b.repo.rev(t, "main"))
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
	}, 10*time.Second, 50*time.Millisecond)

	beams, err := hd.d.store.ListBeams(t.Context(), "sorell")
	require.NoError(t, err)
	require.Len(t, beams, 1)
	steps := stepLogEntries(t, b.stepLog)
	require.Len(t, steps, 5)
	requireNoBackstop(t, hd.logEntries(t))
}

// #53's acceptance in holodeck: the script commits on bmus/<order>, hands
// over, commits again and reports. The checkpoint carries the first commit;
// the report's bundle lands in the cargo bay as final/app.bundle and in the
// bridge's clone as bmus/<order> at the reported commit, before the order is
// done. The upstream's refs are untouched.
func TestMissionWorkComesBack(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, fixture(t, "work.json"))
	upRefs := refs(t, b.repo.url)
	hd := b.start(t)
	hd.addCrew(t)
	id := hd.createOrder(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.Len(c, workflows(c, hd.d, missionWorkflowID(id)), 1)
	}, 10*time.Second, 20*time.Millisecond)

	res := missionResult(t, hd.d, id)
	require.Equal(t, MissionDone, res.Status, "%+v", res)
	require.Equal(t, store.OrderDone, hd.order(t, id).State)
	require.NotNil(t, res.Report)
	require.Len(t, res.Report.Repos, 1)
	commit := res.Report.Repos[0].Commit
	require.Equal(t, "app", res.Report.Repos[0].Name)
	require.Equal(t, "bmus/"+id, res.Report.Repos[0].Branch)

	steps := stepLogEntries(t, b.stepLog)
	require.Len(t, steps, 5)
	first := steps[1]["commits"].(map[string]any)["app"].(string)
	require.Equal(t, commit, steps[3]["commits"].(map[string]any)["app"].(string))

	clone := filepath.Join(b.home, "repos", "app")
	require.Equal(t, commit, gitT(t, clone, "rev-parse", "bmus/"+id))
	require.Equal(t, first, gitT(t, clone, "rev-parse", "bmus/"+id+"^"))
	require.Equal(t, b.repo.rev(t, "main"), gitT(t, clone, "rev-parse", "bmus/"+id+"~2"))
	bundle := filepath.Join(b.home, "cargo", id, "final", "app.bundle")
	require.Equal(t, commit+" refs/heads/bmus/"+id, bundleHeads(t, bundle))
	require.Equal(t, upRefs, refs(t, b.repo.url), "nothing pushed")

	cps, err := hd.d.store.ListCheckpoints(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	requireCheckpointFiles(t, b.home, cps[0], "halfway checkpoint", first)

	var done []store.LogEntry
	for _, e := range hd.logEntries(t) {
		if e.Kind == logDone {
			done = append(done, e)
		}
	}
	require.Len(t, done, 1)
	require.Equal(t, "holodeck work committed (app: bmus/"+id+" at "+commit[:12]+")", done[0].Text)
	requireNoBackstop(t, hd.logEntries(t))
}
