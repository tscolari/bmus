package comms

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// These tests run the pump against the real combadge bundle in a holodeck
// beam. Like the transporter's holodeck tests, they take the bundle from
// $BMUS_COMBADGE (CI sets it, and then a missing bundle or node fails the
// test) or build it with `npm run build`, skipping if neither works.

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

// holodeckLink is the bridge's Link over a transporter.Provider, plus test
// hooks: it records each attach's since, can force one, and counts probes.
type holodeckLink struct {
	h    *transporter.Holodeck
	beam transporter.Beam

	mu         sync.Mutex
	sinces     []uint64
	forceSince *uint64
	probes     int
}

func (l *holodeckLink) Attach(ctx context.Context, since uint64) (Session, error) {
	l.mu.Lock()
	l.sinces = append(l.sinces, since)
	if l.forceSince != nil {
		since = *l.forceSince
	}
	l.mu.Unlock()
	return l.h.AttachCombadge(ctx, l.beam, since)
}

func (l *holodeckLink) Probe(ctx context.Context) (bool, error) {
	l.mu.Lock()
	l.probes++
	l.mu.Unlock()
	return l.h.Probe(ctx, l.beam)
}

func (l *holodeckLink) getSinces() []uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]uint64(nil), l.sinces...)
}

func (l *holodeckLink) probeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.probes
}

// holodeckBeam is one holodeck beam running serve for crew-1 with the
// default.json script and 100ms heartbeats.
type holodeckBeam struct {
	h      *transporter.Holodeck
	beam   transporter.Beam
	dir    string
	dbPath string // the test's bridge store, opened with b.openStore
}

func startHolodeckBeam(t *testing.T) holodeckBeam {
	t.Helper()
	return startHolodeckBeamScript(t, "default.json")
}

// startHolodeckBeamScript is startHolodeckBeam with another script from
// combadge/test/fixtures.
func startHolodeckBeamScript(t *testing.T, fixture string) holodeckBeam {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "combadge", "test", "fixtures", fixture))
	require.NoError(t, err)
	// t.TempDir paths can push serve.sock past the Unix socket limit.
	base := os.TempDir()
	if len(base) > 40 {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "pump")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(root) })
	h, err := transporter.NewHolodeck(transporter.HolodeckConfig{
		Root:     root,
		Combadge: combadgeBundle(t),
		Harness:  []string{"--harness", "holodeck", "--script", script},
	})
	require.NoError(t, err)
	beam, err := h.Create(t.Context())
	require.NoError(t, err)
	// Remove stops serve (SIGTERM, then SIGKILL) so no serve outlives the
	// test. Registered before the pump's cleanup, so it runs after it.
	t.Cleanup(func() { require.NoError(t, h.Remove(context.Background(), beam)) })
	require.NoError(t, h.StartCombadge(t.Context(), beam, transporter.CombadgeSpec{
		CrewID: "crew-1",
		Args:   []string{"--heartbeat-interval", "100"},
	}))
	return holodeckBeam{h: h, beam: beam, dir: filepath.Join(root, beam.Alias), dbPath: filepath.Join(t.TempDir(), store.DBFile)}
}

// openStore opens the test's bridge store at b.dbPath.
func (b holodeckBeam) openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), b.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}

func (b holodeckBeam) link() *holodeckLink { return &holodeckLink{h: b.h, beam: b.beam} }

// servedAck reads the ack serve recorded in its state.json.
func (b holodeckBeam) servedAck(t require.TestingT) uint64 {
	data, err := os.ReadFile(filepath.Join(b.dir, "state.json"))
	require.NoError(t, err)
	var st struct{ Acked uint64 }
	require.NoError(t, json.Unmarshal(data, &st))
	return st.Acked
}

// holodeckPumpConfig: heartbeats every 100ms (serve flag) go stale after 5s,
// checked every 100ms: the production 30s/5s scaled down 6x/50x. Staleness
// is well clear of a store commit under disk load (440ms, #193) and of a
// node attach's first line on a loaded machine (over 1s, #138): a recycle
// of the healthy connection would fail a test's Send with ErrNotConnected
// (TestPumpHolodeckOrderFlowAndRestart, TestPumpHolodeckRecycleDeliversWrittenCommand)
// or leave a second attach running (#197). Tests that wait for a killed
// beam to go stale lower it.
func holodeckPumpConfig(st *store.Store, link Link, sink Sink) PumpConfig {
	return PumpConfig{
		CrewID: "crew-1", Beam: "holodeck", Link: link, Store: st, Sink: sink, Logger: quietLogger(),
		HeartbeatStale: 5 * time.Second, SickbayCheckEvery: 100 * time.Millisecond,
		BackoffMin: 50 * time.Millisecond, BackoffMax: 500 * time.Millisecond,
	}
}

func waitConnected(t *testing.T, p *Pump) {
	t.Helper()
	require.Eventually(t, p.Connected, 15*time.Second, 10*time.Millisecond, "pump never connected")
}

// waitMission waits for the sink to have received n mission messages and
// returns them.
func waitMission(t *testing.T, sink *fakeSink, n int) []Message {
	t.Helper()
	require.Eventually(t, func() bool { return len(sink.missions()) >= n }, 15*time.Second, 10*time.Millisecond,
		"want %d mission messages, have %d", n, len(sink.missions()))
	return sink.missions()
}

// requireInboxContiguous stops p (so nothing is recorded while it looks)
// and checks the inbox holds exactly seqs 1..N, N being the contiguous seq:
// COUNT(*) = MAX(seq) = N, read in one query. A gap or an out-of-order record
// left by the pump would show up here as a permanent hole. (Checking while
// the pump runs races with the next heartbeat being recorded between reads.)
func requireInboxContiguous(t *testing.T, st *store.Store, dbPath string, p *Pump) uint64 {
	t.Helper()
	p.Stop()
	n, err := st.ContiguousInboxSeq(t.Context(), "crew-1", p.cfg.TourNo)
	require.NoError(t, err)
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	defer db.Close()
	var count, maxSeq uint64
	require.NoError(t, db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(seq), 0) FROM inbox WHERE crew_id = ? AND tour_no = ?`,
		"crew-1", p.cfg.TourNo).Scan(&count, &maxSeq))
	require.Equal(t, n, maxSeq, "seqs recorded above the contiguous seq")
	require.Equal(t, n, count, "inbox isn't exactly 1..N")
	return n
}

// The issue's acceptance criterion, part one: killing the beam's serve makes
// heartbeat_missed arrive within the stale window plus a check (35s in
// production; 1s + 100ms here, asserted within twice that). The test sends
// nothing, so a recycle before the kill is harmless: 1s keeps it quick.
func TestPumpHolodeckKillSignalsHeartbeatMissed(t *testing.T) {
	b := startHolodeckBeam(t)
	st := b.openStore(t)
	link, sink := b.link(), &fakeSink{}
	cfg := holodeckPumpConfig(st, link, sink)
	cfg.HeartbeatStale = time.Second
	p := startTestPump(t, cfg)
	waitConnected(t, p)
	require.Eventually(t, func() bool { return time.Since(p.LastHeartbeat()) < 200*time.Millisecond }, 5*time.Second, 10*time.Millisecond)

	bound := 2 * (cfg.HeartbeatStale + cfg.SickbayCheckEvery)
	require.NoError(t, b.h.Kill(b.beam.Alias))
	killed := time.Now()
	require.Eventually(t, func() bool { return sink.missedCount() == 1 }, 10*time.Second, 5*time.Millisecond)
	took := time.Since(killed)
	t.Logf("heartbeat_missed %s after the kill", took.Round(time.Millisecond))
	// Ideally stale + check (35s in production); 2x of it allows for CI.
	require.LessOrEqual(t, took, bound)
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, 1, sink.missedCount())
}

// Part two: a graceful attach disconnect (the beam stays up) reconnects and
// never gets as far as a probe, let alone a signal.
func TestPumpHolodeckGracefulDisconnectDoesNotSignal(t *testing.T) {
	b := startHolodeckBeam(t)
	st := b.openStore(t)
	link, sink := b.link(), &fakeSink{}
	cfg := holodeckPumpConfig(st, link, sink)
	// Each reconnect starts a new node attach, which on a loaded machine
	// can take longer than 1s to send its first line; heartbeats would go
	// stale, and the probe recycle the attach (#138). As in production,
	// HeartbeatStale is well above a reconnect.
	cfg.HeartbeatStale = 10 * time.Second
	p := startTestPump(t, cfg)
	waitConnected(t, p)

	for i := range 5 {
		require.True(t, p.dropConnection(), "drop %d", i)
		// Half the stale window: a reconnect drifting toward it still fails.
		require.Eventually(t, func() bool { return p.connects.Load() == int64(i+2) && p.Connected() },
			cfg.HeartbeatStale/2, 5*time.Millisecond, "no reconnect after drop %d", i)
		time.Sleep(300 * time.Millisecond)
	}
	// Heartbeats still arrive on the last connection.
	last := time.Now()
	require.Eventually(t, func() bool { return p.LastHeartbeat().After(last) }, 10*time.Second, 5*time.Millisecond)
	require.Zero(t, link.probeCount(), "heartbeats went stale")
	require.Zero(t, sink.missedCount())

	// Every reconnect resumed from the contiguous seq.
	sinces := link.getSinces()
	require.Equal(t, uint64(0), sinces[0])
	for i := 1; i < len(sinces); i++ {
		require.GreaterOrEqual(t, sinces[i], sinces[i-1])
	}
	// serve is acked up to what the pump recorded...
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		acked := p.Acked()
		require.Positive(c, acked)
		require.GreaterOrEqual(c, b.servedAck(c), acked)
	}, 5*time.Second, 10*time.Millisecond)
	// ...and, once quiesced, the inbox is exactly 1..N.
	n := requireInboxContiguous(t, st, b.dbPath, p)
	require.LessOrEqual(t, b.servedAck(t), n)
}

// The order flow through the real holodeck harness, then a pump restart
// against the same store.
func TestPumpHolodeckOrderFlowAndRestart(t *testing.T) {
	b := startHolodeckBeam(t)
	st := b.openStore(t)
	link, sink := b.link(), &fakeSink{}
	p, err := StartPump(t.Context(), holodeckPumpConfig(st, link, sink))
	require.NoError(t, err)
	defer p.Stop()
	waitConnected(t, p)

	order, err := NewMessage(TypeOrder, OrderBody{Title: "holodeck", Body: "do it"})
	require.NoError(t, err)
	order.Order = "ORD-0001"
	_, err = p.Send(order)
	require.NoError(t, err)

	// progress goes to the log; the hail goes to the mission.
	msgs := waitMission(t, sink, 1)
	require.Equal(t, TypeHail, msgs[0].Type)
	require.Equal(t, "ORD-0001", msgs[0].Order)
	var h HailBody
	require.NoError(t, json.Unmarshal(msgs[0].Body, &h))
	require.Equal(t, CategoryToolApproval, h.Category)
	entries, err := st.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "surveying the codebase", entries[0].Text)
	require.Equal(t, "ORD-0001", entries[0].OrderID)

	allow := 0
	answer, err := NewMessage(TypeAnswer, AnswerBody{Ref: h.Ref, HailID: "H-0001", DecidedBy: DecidedByCaptain, Decision: "allow", Option: &allow})
	require.NoError(t, err)
	answer.Order = "ORD-0001"
	_, err = p.Send(answer)
	require.NoError(t, err)

	msgs = waitMission(t, sink, 3)
	require.Equal(t, []string{TypeHail, TypeCheckpointReady, TypeReport}, []string{msgs[0].Type, msgs[1].Type, msgs[2].Type})
	var report ReportBody
	require.NoError(t, json.Unmarshal(msgs[2].Body, &report))
	require.Equal(t, "holodeck mission complete", report.Summary)
	require.Eventually(t, func() bool {
		entries, err := st.ListLog(t.Context(), 0)
		return err == nil && len(entries) == 2 && entries[1].Text == "tests passing"
	}, 5*time.Second, 10*time.Millisecond)
	require.Empty(t, sink.errors())
	require.Zero(t, sink.missedCount())

	// Every inbound seq is recorded once and the ack is contiguous: serve
	// gets acked up to the pump's contiguous seq.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		n := contiguous(t, st)
		require.Equal(c, n, p.Acked())
		require.Equal(c, n, b.servedAck(c))
	}, 5*time.Second, 10*time.Millisecond)
	n := requireInboxContiguous(t, st, b.dbPath, p) // stops p
	require.GreaterOrEqual(t, n, msgs[2].Seq)

	// Restart: a new pump on the same store attaches from the contiguous
	// seq and delivers nothing twice.
	before := n
	link2 := b.link()
	p2, err := StartPump(t.Context(), holodeckPumpConfig(st, link2, sink))
	require.NoError(t, err)
	waitConnected(t, p2)
	require.Equal(t, before, link2.getSinces()[0])
	require.Eventually(t, func() bool { return p2.Acked() > before }, 5*time.Second, 10*time.Millisecond)
	p2.Stop()

	// Even a full replay (--since 0) is deduped by the inbox.
	zero := uint64(0)
	link3 := b.link()
	link3.forceSince = &zero
	before = contiguous(t, st)
	p3, err := StartPump(t.Context(), holodeckPumpConfig(st, link3, sink))
	require.NoError(t, err)
	defer p3.Stop()
	require.Eventually(t, func() bool { return p3.Acked() > before }, 5*time.Second, 10*time.Millisecond)

	require.Len(t, sink.missions(), 3, "no duplicates reached the sink")
	seen := map[uint64]bool{}
	for _, m := range sink.missions() {
		require.False(t, seen[m.Seq], "seq %d delivered twice", m.Seq)
		seen[m.Seq] = true
	}
	entries, err = st.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, entries, 2, "no progress logged twice")
	requireInboxContiguous(t, st, b.dbPath, p3)
}

// #22: a crew member's next tour runs a fresh beam, whose outbox numbers
// from seq 1 again. Its pump attaches with --since 0, not the previous
// tour's contiguous seq, and records and acks seq 1 instead of dropping it as
// a duplicate of the previous tour's.
func TestPumpHolodeckNextTourStartsFromSeqOne(t *testing.T) {
	b0 := startHolodeckBeam(t)
	st := b0.openStore(t)
	p0 := startTestPump(t, holodeckPumpConfig(st, b0.link(), &fakeSink{}))
	require.Eventually(t, func() bool { return p0.Acked() >= 3 }, 15*time.Second, 10*time.Millisecond)
	tour0 := requireInboxContiguous(t, st, b0.dbPath, p0) // stops p0

	// Tour 1, on a new beam, against the same store.
	b1 := startHolodeckBeam(t)
	link1 := b1.link()
	cfg := holodeckPumpConfig(st, link1, &fakeSink{})
	cfg.TourNo = 1
	p1 := startTestPump(t, cfg)
	waitConnected(t, p1)
	require.Equal(t, uint64(0), link1.getSinces()[0])
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.GreaterOrEqual(c, p1.Acked(), uint64(1))
		require.GreaterOrEqual(c, b1.servedAck(c), uint64(1))
	}, 5*time.Second, 10*time.Millisecond)
	seen, err := st.SeenInbox(t.Context(), "crew-1", 1, 1)
	require.NoError(t, err)
	require.True(t, seen, "tour 1's seq 1 not recorded")

	require.GreaterOrEqual(t, requireInboxContiguous(t, st, b0.dbPath, p1), uint64(1))
	require.Equal(t, tour0, contiguous(t, st), "tour 0's inbox untouched")
}

// attachPIDs returns the pids of the running `combadge attach` processes
// dialling b's socket, from /proc (Linux only).
func (b holodeckBeam) attachPIDs(t *testing.T) []int {
	t.Helper()
	sock := filepath.Join(b.dir, "serve.sock")
	entries, err := os.ReadDir("/proc")
	require.NoError(t, err)
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if slices.Contains(args, "attach") && slices.Contains(args, sock) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// #81: a command written just before the sickbay check recycles the
// connection must still reach serve. The live attach is stopped (SIGSTOP),
// as a relay starved of CPU is: the order sits unread in its stdin. The
// connection is then recycled, as the check does when heartbeats look stale
// but the beam answers, and the attach is let go (SIGCONT) the way a starved
// process gets the CPU back. serve must receive the order: its hail reaches
// the mission over the next connection.
func TestPumpHolodeckRecycleDeliversWrittenCommand(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("finds the attach process through /proc")
	}
	b := startHolodeckBeam(t)
	st := b.openStore(t)
	link, sink := b.link(), &fakeSink{}
	p := startTestPump(t, holodeckPumpConfig(st, link, sink))
	waitConnected(t, p)

	pids := b.attachPIDs(t)
	require.Len(t, pids, 1, "one live attach")
	attach := pids[0]
	require.NoError(t, syscall.Kill(attach, syscall.SIGSTOP))
	t.Cleanup(func() { _ = syscall.Kill(attach, syscall.SIGCONT) })

	order, err := NewMessage(TypeOrder, OrderBody{Title: "holodeck", Body: "do it"})
	require.NoError(t, err)
	order.Order = "ORD-0001"
	_, err = p.Send(order)
	require.NoError(t, err, "the write lands in the stopped attach's stdin")
	require.True(t, p.dropConnection())
	require.NoError(t, syscall.Kill(attach, syscall.SIGCONT))

	msgs := waitMission(t, sink, 1)
	require.Equal(t, TypeHail, msgs[0].Type)
	require.Equal(t, "ORD-0001", msgs[0].Order)
}

// recordingHandler keeps every log record at or above debug, for tests that
// check what the pump logged.
type recordingHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
}

func newRecordingLogger() (*slog.Logger, func() []slog.Record) {
	h := recordingHandler{mu: &sync.Mutex{}, records: &[]slog.Record{}}
	return slog.New(h), func() []slog.Record {
		h.mu.Lock()
		defer h.mu.Unlock()
		return slices.Clone(*h.records)
	}
}

func (recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, r.Clone())
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// #117: a holodeck script's usage steps reach the bridge as usage messages.
// The pump records and acks them, and for now only logs them at debug
// (#118 stores them): no mission, crew-error or log entry, and no
// unknown-type warning.
func TestPumpHolodeckUsage(t *testing.T) {
	t.Parallel()
	b := startHolodeckBeamScript(t, "usage.json")
	st := b.openStore(t)
	id, err := st.InsertOrder(t.Context(), store.Order{Title: "holodeck", Assignee: "crew-1"},
		store.LogEntry{Kind: "order", Text: "order created: holodeck"})
	require.NoError(t, err)
	require.Equal(t, "ORD-0001", id)
	logger, records := newRecordingLogger()
	link, sink := b.link(), &fakeSink{}
	cfg := holodeckPumpConfig(st, link, sink)
	cfg.Logger = logger
	p := startTestPump(t, cfg)
	waitConnected(t, p)

	order, err := NewMessage(TypeOrder, OrderBody{Title: "holodeck", Body: "count it"})
	require.NoError(t, err)
	order.Order = id
	_, err = p.Send(order)
	require.NoError(t, err)

	msgs := waitMission(t, sink, 1)
	require.Equal(t, TypeReport, msgs[0].Type)
	var report ReportBody
	require.NoError(t, json.Unmarshal(msgs[0].Body, &report))
	require.Equal(t, Usage{InputTokens: 5400, OutputTokens: 910}, report.Usage)

	type usageLog struct {
		Order                string
		Input, Output, Turns uint64
	}
	var got []usageLog
	var usageSeqs []uint64
	for _, r := range records() {
		require.NotEqual(t, "comms: skipping message of unknown type", r.Message)
		if r.Message != "comms: usage" {
			continue
		}
		require.Equal(t, slog.LevelDebug, r.Level)
		var u usageLog
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "order":
				u.Order = a.Value.String()
			case "input_tokens":
				u.Input = a.Value.Uint64()
			case "output_tokens":
				u.Output = a.Value.Uint64()
			case "turns":
				u.Turns = a.Value.Uint64()
			case "seq":
				usageSeqs = append(usageSeqs, a.Value.Uint64())
			}
			return true
		})
		got = append(got, u)
	}
	require.Equal(t, []usageLog{
		{Order: "ORD-0001", Input: 1200, Output: 300, Turns: 4},
		{Order: "ORD-0001", Input: 5400, Output: 910, Turns: 10},
	}, got)
	// Both came before the report, and were recorded in the inbox like any other message.
	require.Len(t, usageSeqs, 2)
	for _, seq := range usageSeqs {
		require.Less(t, seq, msgs[0].Seq)
		seen, err := st.SeenInbox(t.Context(), "crew-1", 0, seq)
		require.NoError(t, err)
		require.True(t, seen, "usage seq %d not recorded", seq)
	}

	// The latest report is the order's usage on this beam (#118).
	rows, err := st.OrderUsage(t.Context(), "ORD-0001")
	require.NoError(t, err)
	require.Equal(t, []store.Usage{{Beam: "holodeck", InputTokens: 5400, OutputTokens: 910, Turns: 10, Seq: usageSeqs[1]}}, rows)

	require.Len(t, sink.missions(), 1, "usage never reaches a mission")
	require.Empty(t, sink.errors())
	entries, err := st.ListLog(t.Context(), 0)
	require.NoError(t, err)
	var texts []string
	for _, e := range entries {
		texts = append(texts, e.Text)
	}
	require.Equal(t, []string{"order created: holodeck", "surveying the codebase"}, texts, "only the progress step is logged")
	requireInboxContiguous(t, st, b.dbPath, p)
}
