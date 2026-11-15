package comms

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

// dropConnection ends the pump's current session, if any, the way a graceful
// attach disconnect does: the beam stays up and the pump reconnects.
func (p *Pump) dropConnection() bool { return p.recycle() }

// fakeSession is a Session the test drives: lines written with emit come out
// of Stdout, and commands the pump writes arrive on cmds. Cancelling the
// Attach ctx (killing attach), or end, ends it. Close (stdin EOF) ends it too,
// as attach exits on it, after passing on every held command.
type fakeSession struct {
	blockWrites bool            // Write blocks until the session ends, like a stalled attach
	holdWrites  bool            // commands wait in held until relay or Close, like an attach that hasn't read its stdin yet
	ignoreClose bool            // Close doesn't end the session, like a wedged attach
	ctx         context.Context // the Attach ctx
	pr          *io.PipeReader
	pw          *io.PipeWriter
	cmds        chan Message
	done        chan struct{}
	once        sync.Once

	mu     sync.Mutex
	held   []Message // written, not yet passed on to cmds (holdWrites)
	closed bool      // stdin closed
}

func newFakeSession(ctx context.Context) *fakeSession {
	pr, pw := io.Pipe()
	s := &fakeSession{ctx: ctx, pr: pr, pw: pw, cmds: make(chan Message, 1000), done: make(chan struct{})}
	context.AfterFunc(ctx, s.end)
	return s
}

func (s *fakeSession) end() {
	s.once.Do(func() {
		s.pw.CloseWithError(io.EOF)
		s.pr.CloseWithError(io.ErrClosedPipe)
		close(s.done)
	})
}

// ended reports whether the session has ended or is ending: its Attach ctx
// is cancelled at once, while end runs on a goroutine of its own.
func (s *fakeSession) ended() bool {
	select {
	case <-s.done:
		return true
	default:
		return s.ctx.Err() != nil
	}
}

func (s *fakeSession) Write(p []byte) (int, error) {
	if s.blockWrites {
		<-s.done
		return 0, io.ErrClosedPipe
	}
	select {
	case <-s.done:
		return 0, io.ErrClosedPipe
	default:
	}
	var m Message
	if err := json.Unmarshal(p, &m); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, os.ErrClosed
	}
	if s.holdWrites {
		s.held = append(s.held, m)
	} else {
		s.cmds <- m
	}
	return len(p), nil
}

// hold makes later commands wait in held, as in an attach that stopped
// reading its stdin for a while (starved of CPU, say).
func (s *fakeSession) hold() {
	s.mu.Lock()
	s.holdWrites = true
	s.mu.Unlock()
}

// wedge makes Close leave the session running, as a wedged attach would.
func (s *fakeSession) wedge() {
	s.mu.Lock()
	s.ignoreClose = true
	s.mu.Unlock()
}

// relay passes the held commands on, unless the session was killed: a
// killed attach loses what it hadn't read from its stdin.
func (s *fakeSession) relay() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		s.held = nil
		return
	}
	for _, m := range s.held {
		s.cmds <- m
	}
	s.held = nil
}

// Close closes attach's stdin: attach passes on what it held and exits,
// unless it is wedged (ignoreClose).
func (s *fakeSession) Close() error {
	s.mu.Lock()
	s.closed = true
	wedged := s.ignoreClose
	s.mu.Unlock()
	if wedged {
		return nil
	}
	s.relay()
	s.end()
	return nil
}

func (s *fakeSession) Stdout() io.Reader { return s.pr }
func (s *fakeSession) Wait() error       { <-s.done; return nil }

// emit writes msg to the pump as a beam line. It fails if the session ended.
func (s *fakeSession) emit(t *testing.T, msg Message) {
	t.Helper()
	line, err := json.Marshal(msg)
	require.NoError(t, err)
	_, err = s.pw.Write(append(line, '\n'))
	require.NoError(t, err)
}

func (s *fakeSession) emitRaw(t *testing.T, line string) {
	t.Helper()
	_, err := s.pw.Write([]byte(line + "\n"))
	require.NoError(t, err)
}

// nextCmd returns the next command the pump wrote.
func (s *fakeSession) nextCmd(t *testing.T) Message {
	t.Helper()
	select {
	case m := <-s.cmds:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no command from the pump")
		return Message{}
	}
}

// noCmd requires the pump to write nothing for a short while.
func (s *fakeSession) noCmd(t *testing.T) {
	t.Helper()
	select {
	case m := <-s.cmds:
		t.Fatalf("unexpected command %s seq %d: %s", m.Type, m.Seq, m.Body)
	case <-time.After(100 * time.Millisecond):
	}
}

// nextAck returns the seq of the next command, which must be an ack.
func (s *fakeSession) nextAck(t *testing.T) uint64 {
	t.Helper()
	m := s.nextCmd(t)
	require.Equal(t, TypeAck, m.Type)
	var b AckBody
	require.NoError(t, json.Unmarshal(m.Body, &b))
	return b.Seq
}

// fakeLink hands out fakeSessions on sessions and records each attach's since.
type fakeLink struct {
	mu       sync.Mutex
	sinces   []uint64
	fail     int // attaches left to fail
	sessions chan *fakeSession

	probe  func() (bool, error)
	probes atomic.Int32

	blockWrites bool // sessions handed out from now on block writes

	last     *fakeSession // the previous session handed out
	overlaps int          // attaches made while the previous session was still open
}

func newFakeLink() *fakeLink {
	return &fakeLink{sessions: make(chan *fakeSession, 100), probe: func() (bool, error) { return true, nil }}
}

func (l *fakeLink) Attach(ctx context.Context, since uint64) (Session, error) {
	l.mu.Lock()
	l.sinces = append(l.sinces, since)
	fail := l.fail > 0
	if fail {
		l.fail--
	}
	l.mu.Unlock()
	if fail {
		return nil, errors.New("attach failed")
	}
	s := newFakeSession(ctx)
	l.mu.Lock()
	s.blockWrites = l.blockWrites
	if l.last != nil {
		select {
		case <-l.last.done:
		default:
			l.overlaps++
		}
	}
	l.last = s
	l.mu.Unlock()
	l.sessions <- s
	return s, nil
}

func (l *fakeLink) Probe(ctx context.Context) (bool, error) {
	l.probes.Add(1)
	l.mu.Lock()
	probe := l.probe
	l.mu.Unlock()
	return probe()
}

func (l *fakeLink) setProbe(fn func() (bool, error)) {
	l.mu.Lock()
	l.probe = fn
	l.mu.Unlock()
}

func (l *fakeLink) getSinces() []uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]uint64(nil), l.sinces...)
}

func (l *fakeLink) nextSession(t *testing.T) *fakeSession {
	t.Helper()
	select {
	case s := <-l.sessions:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("pump didn't attach")
		return nil
	}
}

// fakeSink records what the pump forwards.
type fakeSink struct {
	mu          sync.Mutex
	toMission   []Message
	crewErrors  []Message
	crewHellos  []Message
	missed      int
	failMission int // ToMission calls left to fail
	// missionErr, if set, is ToMission's error for msg (nil delivers it).
	missionErr func(Message) error
	toHail     []string // hail ids ToHail delivered to
	// hailErr, if set, is ToHail's error for a hail id (nil delivers it).
	hailErr func(string) error
	usages  []string // orders UsageRecorded was called for
}

func (s *fakeSink) ToMission(ctx context.Context, tx *sql.Tx, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx == nil {
		return errors.New("ToMission without a transaction")
	}
	if s.failMission > 0 {
		s.failMission--
		return errors.New("sink unavailable")
	}
	if s.missionErr != nil {
		if err := s.missionErr(msg); err != nil {
			return err
		}
	}
	s.toMission = append(s.toMission, msg)
	return nil
}

func (s *fakeSink) ToHail(ctx context.Context, tx *sql.Tx, msg Message, hailID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx == nil {
		return errors.New("ToHail without a transaction")
	}
	if s.hailErr != nil {
		if err := s.hailErr(hailID); err != nil {
			return err
		}
	}
	s.toHail = append(s.toHail, hailID)
	return nil
}

func (s *fakeSink) CrewError(ctx context.Context, tx *sql.Tx, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx == nil {
		return errors.New("CrewError without a transaction")
	}
	s.crewErrors = append(s.crewErrors, msg)
	return nil
}

func (s *fakeSink) CrewHello(ctx context.Context, tx *sql.Tx, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx == nil {
		return errors.New("CrewHello without a transaction")
	}
	s.crewHellos = append(s.crewHellos, msg)
	return nil
}

func (s *fakeSink) hellos() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.crewHellos...)
}

func (s *fakeSink) UsageRecorded(ctx context.Context, tx store.InboxTx, order string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tx.SQL() == nil {
		return errors.New("UsageRecorded called without the inbox transaction")
	}
	s.usages = append(s.usages, order)
	return nil
}

func (s *fakeSink) HeartbeatMissed(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missed++
	return nil
}

func (s *fakeSink) missions() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.toMission...)
}

func (s *fakeSink) hails() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.toHail...)
}

func (s *fakeSink) errors() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.crewErrors...)
}

func (s *fakeSink) missedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.missed
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), store.DBFile))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testPumpConfig has fast reconnects and a sickbay check that never fires
// unless a test shortens it.
func testPumpConfig(st *store.Store, link Link, sink Sink) PumpConfig {
	return PumpConfig{
		CrewID: "crew-1", Beam: "fake", Link: link, Store: st, Sink: sink, Logger: quietLogger(),
		BackoffMin: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
		HeartbeatStale: time.Hour, SickbayCheckEvery: time.Hour,
	}
}

func startTestPump(t *testing.T, cfg PumpConfig) *Pump {
	t.Helper()
	p, err := StartPump(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(p.Stop)
	return p
}

// beamMsg builds a beam->bridge message from crew-1.
func beamMsg(t *testing.T, seq uint64, typ, order string, body any) Message {
	t.Helper()
	m, err := NewMessage(typ, body)
	require.NoError(t, err)
	m.Seq, m.Crew, m.Order = seq, "crew-1", order
	return m
}

func hello(t *testing.T, seq uint64) Message {
	return beamMsg(t, seq, TypeHello, "", HelloBody{Protocol: Version, Harness: "fake", CombadgeVersion: "test"})
}

func heartbeat(t *testing.T, seq uint64, ts time.Time) Message {
	m := beamMsg(t, seq, TypeHeartbeat, "", HeartbeatBody{})
	m.TS = ts
	return m
}

func hail(t *testing.T, seq uint64, order string) Message {
	return beamMsg(t, seq, TypeHail, order, HailBody{Ref: "r1", Category: CategoryClarifyingQuestion, Question: "?"})
}

func contiguous(t *testing.T, st *store.Store) uint64 {
	t.Helper()
	n, err := st.ContiguousInboxSeq(t.Context(), "crew-1", 0)
	require.NoError(t, err)
	return n
}

func TestPumpRecordsDedupsAndAcksContiguously(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	before := uint64(time.Now().UnixMicro())
	p := startTestPump(t, testPumpConfig(st, link, sink))
	s := link.nextSession(t)
	require.Equal(t, []uint64{0}, link.getSinces(), "first attach is --since 0")

	// Not connected until the first line arrives.
	_, err := p.Send(Message{Type: TypeOrder})
	require.ErrorIs(t, err, ErrNotConnected)

	s.emit(t, hello(t, 1))
	require.Equal(t, uint64(1), s.nextAck(t))
	require.True(t, p.Connected())
	// The hello goes to the tour (#173), recorded once like any message.
	require.Len(t, sink.hellos(), 1)
	require.Equal(t, uint64(1), sink.hellos()[0].Seq)

	s.emit(t, beamMsg(t, 2, TypeProgress, "ORD-1", ProgressBody{Pct: 10, Note: "surveying"}))
	require.Equal(t, uint64(2), s.nextAck(t))
	entries, err := st.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "surveying", entries[0].Text)
	require.Equal(t, "ORD-1", entries[0].OrderID)
	require.Equal(t, "crew-1", entries[0].CrewID)
	require.JSONEq(t, `{"seq":2,"pct":10}`, string(entries[0].Payload))

	s.emit(t, hail(t, 3, "ORD-1"))
	require.Equal(t, uint64(3), s.nextAck(t))
	s.emit(t, hail(t, 3, "ORD-1")) // duplicate
	s.noCmd(t)
	require.Len(t, sink.missions(), 1)
	got := sink.missions()[0]
	require.Equal(t, TypeHail, got.Type, "the whole message goes to the mission")
	require.Equal(t, uint64(3), got.Seq)

	// A gap holds the ack back until it fills.
	s.emit(t, heartbeat(t, 5, time.Now()))
	s.noCmd(t)
	require.Equal(t, uint64(3), p.Acked())
	s.emit(t, heartbeat(t, 4, time.Now()))
	require.Equal(t, uint64(5), s.nextAck(t))
	require.Equal(t, uint64(5), contiguous(t, st))

	// Garbage and unknown types are skipped; unknown types still count.
	s.emitRaw(t, `{not json`)
	s.emit(t, Message{V: Version, Type: "mystery", Seq: 6, Crew: "crew-1", TS: time.Now().UTC(), Body: json.RawMessage(`{}`)})
	require.Equal(t, uint64(6), s.nextAck(t))

	// Commands get increasing ids at or above the clock in µs.
	id1, err := p.Send(Message{Type: TypeAnswer, Order: "ORD-1", Body: json.RawMessage(`{}`)})
	require.NoError(t, err)
	id2, err := p.Send(Message{Type: TypeAnswer, Order: "ORD-1", Body: json.RawMessage(`{}`)})
	require.NoError(t, err)
	require.GreaterOrEqual(t, id1, before)
	require.Greater(t, id2, id1)
	c1, c2 := s.nextCmd(t), s.nextCmd(t)
	require.Equal(t, id1, c1.Seq)
	require.Equal(t, id2, c2.Seq)
	require.Equal(t, "crew-1", c1.Crew)
	require.Equal(t, Version, c1.V)

	// A reconnect attaches from the contiguous seq, and a replay of
	// recorded messages is dropped.
	s.end()
	s = link.nextSession(t)
	require.Equal(t, []uint64{0, 6}, link.getSinces())
	s.emit(t, hail(t, 3, "ORD-1"))
	require.Equal(t, uint64(6), s.nextAck(t), "each connection acks once more, in case the last ack was lost")
	s.emit(t, hello(t, 7))
	require.Equal(t, uint64(7), s.nextAck(t))
	require.Len(t, sink.missions(), 1)
	require.Len(t, sink.hellos(), 2, "each attach's hello goes on")
}

// A progress report also becomes its order's progress (#141), in the same
// transaction as its entry; one recorded after a later seq is logged but
// doesn't replace it.
func TestPumpProgressOnTheOrder(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	require.NoError(t, st.InsertCrew(ctx, store.Crew{ID: "crew-1", Name: "One"}, store.LogEntry{Kind: "crew"}))
	id, err := st.InsertOrder(ctx, store.Order{Title: "t", Assignee: "crew-1"}, store.LogEntry{Kind: "order"})
	require.NoError(t, err)
	_, _, err = st.ClaimOldestDirectedOrder(ctx, "crew-1", store.LogEntry{Kind: "order"})
	require.NoError(t, err)
	link, sink := newFakeLink(), &fakeSink{}
	startTestPump(t, testPumpConfig(st, link, sink))
	s := link.nextSession(t)
	progress := func() (float64, string) {
		t.Helper()
		o, err := st.GetOrder(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, o.ProgressPct)
		return *o.ProgressPct, o.ProgressNote
	}

	s.emit(t, beamMsg(t, 1, TypeProgress, id, ProgressBody{Pct: 10, Note: "surveying"}))
	require.Equal(t, uint64(1), s.nextAck(t))
	pct, note := progress()
	require.InDelta(t, 10, pct, 0)
	require.Equal(t, "surveying", note)

	s.emit(t, beamMsg(t, 3, TypeProgress, id, ProgressBody{Pct: 60, Note: "testing"}))
	s.noCmd(t)
	s.emit(t, beamMsg(t, 2, TypeProgress, id, ProgressBody{Pct: 40, Note: "building"}))
	require.Equal(t, uint64(3), s.nextAck(t))
	pct, note = progress()
	require.InDelta(t, 60, pct, 0)
	require.Equal(t, "testing", note)
	entries, err := st.ListLog(ctx, 0)
	require.NoError(t, err)
	var notes []string
	for _, e := range entries {
		if e.CrewID == "crew-1" && e.Text != "" {
			notes = append(notes, e.Text)
		}
	}
	require.Equal(t, []string{"surveying", "testing", "building"}, notes, "every report is logged")
}

// A usage report is upserted into order_usage for (order, beam) (#118),
// keeping the row with the highest envelope seq: one recorded after a later
// seq, and a replay, change nothing. One for an unknown order, without an
// order or with a bad body is recorded in the inbox and dropped. The
// Sink's UsageRecorded runs only for a report that changed the row.
func TestPumpUsageRecorded(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := t.Context()
	id, err := st.InsertOrder(ctx, store.Order{Title: "t", Assignee: "crew-1"}, store.LogEntry{Kind: "order"})
	require.NoError(t, err)
	link, sink := newFakeLink(), &fakeSink{}
	startTestPump(t, testPumpConfig(st, link, sink))
	s := link.nextSession(t)
	usage := func() []store.Usage {
		t.Helper()
		u, err := st.OrderUsage(ctx, id)
		require.NoError(t, err)
		return u
	}

	s.emit(t, beamMsg(t, 1, TypeUsage, id, UsageBody{InputTokens: 100, OutputTokens: 20, Turns: 1}))
	require.Equal(t, uint64(1), s.nextAck(t))
	require.Equal(t, []store.Usage{{Beam: "fake", InputTokens: 100, OutputTokens: 20, Turns: 1, Seq: 1}}, usage())

	// Seq 3 before seq 2: 2 arrives late and is kept out.
	s.emit(t, beamMsg(t, 3, TypeUsage, id, UsageBody{InputTokens: 900, OutputTokens: 80, Turns: 10}))
	s.noCmd(t)
	s.emit(t, beamMsg(t, 2, TypeUsage, id, UsageBody{InputTokens: 500, OutputTokens: 40, Turns: 5}))
	require.Equal(t, uint64(3), s.nextAck(t))
	want := []store.Usage{{Beam: "fake", InputTokens: 900, OutputTokens: 80, Turns: 10, Seq: 3}}
	require.Equal(t, want, usage())

	// A replay after a reconnect is dropped by the inbox.
	s.end()
	s = link.nextSession(t)
	s.emit(t, beamMsg(t, 2, TypeUsage, id, UsageBody{InputTokens: 500, OutputTokens: 40, Turns: 5}))
	require.Equal(t, uint64(3), s.nextAck(t))
	require.Equal(t, want, usage())

	s.emit(t, beamMsg(t, 4, TypeUsage, "ORD-9999", UsageBody{InputTokens: 1}))
	s.emit(t, beamMsg(t, 5, TypeUsage, "", UsageBody{InputTokens: 1}))
	bad := beamMsg(t, 6, TypeUsage, id, UsageBody{})
	bad.Body = json.RawMessage(`{"turns":"many"}`)
	s.emit(t, bad)
	require.Equal(t, uint64(4), s.nextAck(t))
	require.Equal(t, uint64(5), s.nextAck(t))
	require.Equal(t, uint64(6), s.nextAck(t))
	require.Equal(t, want, usage())
	none, err := st.OrderUsage(ctx, "ORD-9999")
	require.NoError(t, err)
	require.Empty(t, none)
	require.Empty(t, sink.missions(), "usage never reaches a mission")
	sink.mu.Lock()
	defer sink.mu.Unlock()
	require.Equal(t, []string{id, id}, sink.usages, "the backstop runs for each report that changed the row (#119)")
}

// A finding goes straight to the store in the inbox transaction (#153):
// kept ones land in findings and log "finding <key>: <title>" (kind
// finding) on their order; a bad body, one without an order and one the
// store drops are recorded in the inbox, dropped with a warning and logged
// nowhere. None reaches a mission.
func TestPumpFindingRecorded(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	ctx := t.Context()
	id, err := st.InsertOrder(ctx, store.Order{Title: "t", Assignee: "crew-1"}, store.LogEntry{Kind: "order"})
	require.NoError(t, err)
	other, err := st.InsertOrder(ctx, store.Order{Title: "o", Assignee: "crew-2"}, store.LogEntry{Kind: "order"})
	require.NoError(t, err)
	logger, records := newRecordingLogger()
	link, sink := newFakeLink(), &fakeSink{}
	cfg := testPumpConfig(st, link, sink)
	cfg.Logger = logger
	startTestPump(t, cfg)
	s := link.nextSession(t)

	s.emit(t, beamMsg(t, 1, TypeFinding, id, FindingBody{Key: "leak", Title: "Snapshot leaks", Body: "b",
		Labels: []string{"bug"}, Severity: "high", BlockedBy: []string{"flaky", "#12"}}))
	s.emit(t, beamMsg(t, 2, TypeFinding, id, FindingBody{Key: "flaky", Title: "Flaky test"}))
	s.emit(t, beamMsg(t, 3, TypeFinding, id, FindingBody{Key: "leak", Title: "Snapshot leaks again"}))
	bad := []Message{
		beamMsg(t, 4, TypeFinding, id, FindingBody{Key: "Not A Key", Title: "t"}),
		beamMsg(t, 5, TypeFinding, id, FindingBody{Key: "k", Title: "two\nlines"}),
		beamMsg(t, 6, TypeFinding, id, FindingBody{Key: "k", Title: "t", Severity: "critical"}),
		beamMsg(t, 7, TypeFinding, id, FindingBody{Key: "k", Title: "t", BlockedBy: []string{"owner/repo"}}),
		beamMsg(t, 8, TypeFinding, id, FindingBody{}),
		beamMsg(t, 9, TypeFinding, "", FindingBody{Key: "k", Title: "t"}),
		beamMsg(t, 10, TypeFinding, other, FindingBody{Key: "k", Title: "t"}),
		beamMsg(t, 11, TypeFinding, "ORD-9999", FindingBody{Key: "k", Title: "t"}),
	}
	bad[4].Body = json.RawMessage(`{"key":"k","title":"t","labels":"bug"}`)
	for _, m := range bad {
		s.emit(t, m)
	}
	for seq := uint64(1); seq <= 11; seq++ {
		require.Equal(t, seq, s.nextAck(t))
	}
	// Seq 13 before seq 12, as after a reconnect: the older replacement is dropped.
	s.emit(t, beamMsg(t, 13, TypeFinding, id, FindingBody{Key: "leak", Title: "Snapshot leaks twice"}))
	s.noCmd(t)
	s.emit(t, beamMsg(t, 12, TypeFinding, id, FindingBody{Key: "leak", Title: "Stale"}))
	require.Equal(t, uint64(13), s.nextAck(t))

	got, err := st.OrderFindings(ctx, id)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, []string{"flaky", "leak"}, []string{got[0].Key, got[1].Key})
	require.Equal(t, "Snapshot leaks twice", got[1].Title, "the same key replaced the first")
	require.Empty(t, got[1].BlockedBy)
	require.Equal(t, "crew-1", got[1].CrewID)
	none, err := st.OrderFindings(ctx, other)
	require.NoError(t, err)
	require.Empty(t, none)

	entries, err := st.ListLog(ctx, 0)
	require.NoError(t, err)
	var logged []string
	for _, e := range entries {
		if e.Kind == FindingLogKind {
			require.Equal(t, id, e.OrderID)
			require.Equal(t, "crew-1", e.CrewID)
			logged = append(logged, e.Text)
		}
	}
	require.Equal(t, []string{"finding leak: Snapshot leaks", "finding flaky: Flaky test", "finding leak: Snapshot leaks again",
		"finding leak: Snapshot leaks twice"}, logged)

	var warned []uint64
	for _, r := range records() {
		if !strings.HasPrefix(r.Message, "comms: dropping finding") {
			continue
		}
		require.Equal(t, slog.LevelWarn, r.Level, r.Message)
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "seq" {
				warned = append(warned, a.Value.Uint64())
			}
			return true
		})
	}
	require.Equal(t, []uint64{4, 5, 6, 7, 8, 9, 10, 11, 12}, warned)
	require.Empty(t, sink.missions(), "a finding never reaches a mission")
}

func TestPumpErrorRouting(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	startTestPump(t, testPumpConfig(st, link, sink))
	s := link.nextSession(t)

	s.emit(t, beamMsg(t, 1, TypeError, "", ErrorBody{Reason: "disk full"}))
	require.Equal(t, uint64(1), s.nextAck(t))
	require.Empty(t, sink.missions(), "an error without an order never goes to a mission")
	require.Len(t, sink.errors(), 1)
	require.Equal(t, uint64(1), sink.errors()[0].Seq)

	s.emit(t, beamMsg(t, 2, TypeError, "ORD-1", ErrorBody{Reason: "busy", Code: "busy"}))
	require.Equal(t, uint64(2), s.nextAck(t))
	require.Len(t, sink.errors(), 1)
	require.Len(t, sink.missions(), 1)
	require.Equal(t, "ORD-1", sink.missions()[0].Order)

	for i, typ := range []string{TypeCheckpointReady, TypeReport} {
		var body any = CheckpointReadyBody{}
		if typ == TypeReport {
			body = ReportBody{Summary: "done"}
		}
		s.emit(t, beamMsg(t, uint64(3+i), typ, "ORD-1", body))
		require.Equal(t, uint64(3+i), s.nextAck(t))
	}
	require.Len(t, sink.missions(), 3)
}

func consultReply(t *testing.T, seq uint64, hailID string) Message {
	return beamMsg(t, seq, TypeConsultReply, "", ConsultReplyBody{HailID: hailID, Decision: "answer", Confidence: 0.9})
}

// A consult_reply goes to its hail's workflow (#151). One with no hail id, or
// a body that doesn't decode, is recorded and dropped without stalling the
// stream.
func TestPumpConsultReplyRouting(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	startTestPump(t, testPumpConfig(st, link, sink))
	s := link.nextSession(t)

	s.emit(t, consultReply(t, 1, "H-0091"))
	require.Equal(t, uint64(1), s.nextAck(t))
	require.Equal(t, []string{"H-0091"}, sink.hails())
	require.Equal(t, uint64(1), contiguous(t, st), "the inbox row is recorded")

	s.emit(t, consultReply(t, 2, ""))
	require.Equal(t, uint64(2), s.nextAck(t))
	bad := consultReply(t, 3, "H-0092")
	bad.Body = json.RawMessage(`{"hail_id":7}`)
	s.emit(t, bad)
	require.Equal(t, uint64(3), s.nextAck(t))
	s.emit(t, consultReply(t, 4, "H-0093"))
	require.Equal(t, uint64(4), s.nextAck(t))
	require.Equal(t, []string{"H-0091", "H-0093"}, sink.hails(), "bad replies are dropped")
	require.Empty(t, sink.missions())
}

// A reply to a hail workflow that doesn't exist is skipped as undeliverable
// after UndeliverableAfter, like a mission message.
func TestPumpSkipsUndeliverableConsultReply(t *testing.T) {
	st := openStore(t)
	link := newFakeLink()
	sink := &fakeSink{hailErr: func(id string) error {
		if id == "H-GONE" {
			return fmt.Errorf("%w: no workflow hail/H-GONE", ErrUndeliverable)
		}
		return nil
	}}
	cfg := testPumpConfig(st, link, sink)
	cfg.UndeliverableAfter = 300 * time.Millisecond
	p := startTestPump(t, cfg)
	start := time.Now()
	attempts := 0
	for p.Acked() < 2 {
		require.Less(t, time.Since(start), 10*time.Second, "never skipped")
		s := link.nextSession(t)
		attempts++
		if link.getSinces()[len(link.getSinces())-1] < 1 {
			if err := emitOrEnd(s, consultReply(t, 1, "H-GONE")); err != nil {
				continue
			}
		}
		if err := emitOrEnd(s, consultReply(t, 2, "H-0091")); err != nil {
			continue
		}
		require.Eventually(t, func() bool { return p.Acked() == 2 }, 5*time.Second, 5*time.Millisecond)
	}
	require.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "skipped before UndeliverableAfter")
	require.Greater(t, attempts, 1, "retried first")
	require.Equal(t, []string{"H-0091"}, sink.hails())
	entries, err := st.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, UndeliverableLogKind, entries[0].Kind)
	require.Contains(t, entries[0].Text, "hail/H-GONE")
	var logged Message
	require.NoError(t, json.Unmarshal(entries[0].Payload, &logged))
	require.Equal(t, uint64(1), logged.Seq)
	require.Equal(t, TypeConsultReply, logged.Type)
}

// A message the Sink can't take isn't recorded or acked; the pump drops the
// connection and gets it again from the replay.
func TestPumpRetriesWhenSinkFails(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{failMission: 1}
	startTestPump(t, testPumpConfig(st, link, sink))
	s := link.nextSession(t)
	s.emit(t, hello(t, 1))
	require.Equal(t, uint64(1), s.nextAck(t))
	s.emit(t, hail(t, 2, "ORD-1"))
	<-s.done // dropped

	require.Equal(t, uint64(1), contiguous(t, st))
	s = link.nextSession(t)
	require.Equal(t, []uint64{0, 1}, link.getSinces())
	s.emit(t, hail(t, 2, "ORD-1"))
	require.Equal(t, uint64(2), s.nextAck(t))
	require.Len(t, sink.missions(), 1)
}

func TestPumpSendWhileDisconnected(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	cfg := testPumpConfig(st, link, sink)
	cfg.BackoffMin, cfg.BackoffMax = time.Hour, time.Hour // stay disconnected
	p := startTestPump(t, cfg)
	s := link.nextSession(t)
	s.emit(t, hello(t, 1))
	s.nextAck(t)
	_, err := p.Send(Message{Type: TypeBelay, Order: "ORD-1", Body: json.RawMessage(`{}`)})
	require.NoError(t, err)
	s.end()
	require.Eventually(t, func() bool { return !p.Connected() }, 5*time.Second, 5*time.Millisecond)
	_, err = p.Send(Message{Type: TypeBelay, Order: "ORD-1", Body: json.RawMessage(`{}`)})
	require.ErrorIs(t, err, ErrNotConnected)

	p.Stop()
	_, err = p.Send(Message{Type: TypeBelay, Order: "ORD-1", Body: json.RawMessage(`{}`)})
	require.ErrorIs(t, err, ErrNotConnected)
}

func TestPumpBackoff(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	link.fail = 5
	cfg := testPumpConfig(st, link, sink)
	cfg.BackoffMin, cfg.BackoffMax = time.Second, 4*time.Second
	p, err := newPump(cfg)
	require.NoError(t, err)
	waits := make(chan time.Duration)
	p.sleep = func(ctx context.Context, d time.Duration) bool {
		select {
		case waits <- d:
			return true
		case <-ctx.Done():
			return false
		}
	}
	p.start(t.Context())
	t.Cleanup(p.Stop)
	next := func() time.Duration {
		t.Helper()
		select {
		case d := <-waits:
			return d
		case <-time.After(5 * time.Second):
			t.Fatal("pump didn't back off")
			return 0
		}
	}
	// Five failed attaches: 1s, 2s, 4s, then capped.
	for _, want := range []time.Duration{1, 2, 4, 4, 4} {
		require.Equal(t, want*time.Second, next())
	}
	// A connection that records a message resets the backoff...
	s := link.nextSession(t)
	s.emit(t, hello(t, 1))
	s.nextAck(t)
	s.end()
	require.Equal(t, time.Second, next())
	// ...and a short one that records nothing doesn't.
	s = link.nextSession(t)
	s.end()
	require.Equal(t, 2*time.Second, next())
}

func sickbayConfig(st *store.Store, link Link, sink Sink) PumpConfig {
	cfg := testPumpConfig(st, link, sink)
	cfg.HeartbeatStale = 150 * time.Millisecond
	cfg.SickbayCheckEvery = 20 * time.Millisecond
	return cfg
}

func TestPumpSickbayCheck(t *testing.T) {
	t.Run("gone beam signals once", func(t *testing.T) {
		st := openStore(t)
		link, sink := newFakeLink(), &fakeSink{}
		link.setProbe(func() (bool, error) { return false, nil })
		p := startTestPump(t, sickbayConfig(st, link, sink))
		link.nextSession(t) // connected but silent: the start time is the baseline
		require.Eventually(t, func() bool { return sink.missedCount() == 1 }, 5*time.Second, 10*time.Millisecond)
		probes := link.probes.Load()
		time.Sleep(300 * time.Millisecond)
		require.Equal(t, 1, sink.missedCount(), "signalled at most once")
		require.Equal(t, probes, link.probes.Load(), "no checks after signalling")
		select {
		case <-p.Done():
			t.Fatal("the pump stops only when told to")
		default:
		}
	})

	t.Run("healthy or unknown beam doesn't signal", func(t *testing.T) {
		for name, probe := range map[string]func() (bool, error){
			"healthy": func() (bool, error) { return true, nil },
			"unknown": func() (bool, error) { return false, errors.New("tsh login expired") },
		} {
			t.Run(name, func(t *testing.T) {
				st := openStore(t)
				link, sink := newFakeLink(), &fakeSink{}
				link.setProbe(probe)
				startTestPump(t, sickbayConfig(st, link, sink))
				require.Eventually(t, func() bool { return link.probes.Load() >= 3 }, 5*time.Second, 10*time.Millisecond)
				require.Zero(t, sink.missedCount())
			})
		}
	})

	t.Run("fresh heartbeats skip the probe", func(t *testing.T) {
		t.Parallel()
		st := openStore(t)
		link, sink := newFakeLink(), &fakeSink{}
		link.setProbe(func() (bool, error) { return false, nil })
		cfg := sickbayConfig(st, link, sink)
		// Heartbeats come every ~20ms, but the pump reads the next only once
		// it has recorded the last, and a store commit on a loaded runner can
		// take 440ms (#193): HeartbeatStale is well clear of that.
		cfg.HeartbeatStale = 2 * time.Second
		p := startTestPump(t, cfg)
		base := p.LastHeartbeat() // the pump's start, the heartbeat baseline
		s := link.nextSession(t)
		// Until the baseline alone would have gone stale, and some checks on.
		for seq := uint64(1); time.Since(base) < cfg.HeartbeatStale+10*cfg.SickbayCheckEvery; seq++ {
			s.emit(t, heartbeat(t, seq, time.Now()))
			time.Sleep(20 * time.Millisecond)
		}
		require.Zero(t, link.probes.Load())
		require.Zero(t, sink.missedCount())
	})

	t.Run("replayed heartbeats are judged by ts", func(t *testing.T) {
		st := openStore(t)
		link, sink := newFakeLink(), &fakeSink{}
		link.setProbe(func() (bool, error) { return false, nil })
		startTestPump(t, sickbayConfig(st, link, sink))
		s := link.nextSession(t)
		old := time.Now().Add(-time.Hour)
		for seq := uint64(1); seq <= 5; seq++ {
			s.emit(t, heartbeat(t, seq, old))
		}
		// The old heartbeats don't count as fresh: once the replay is over
		// and the beam goes quiet, it is probed and found gone.
		require.Eventually(t, func() bool { return sink.missedCount() == 1 }, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("no probe for a crew member in sickbay", func(t *testing.T) {
		st := openStore(t)
		require.NoError(t, st.InsertCrew(t.Context(), store.Crew{ID: "crew-1", Name: "One", Status: store.CrewSickbay}, store.LogEntry{Kind: "test"}))
		link, sink := newFakeLink(), &fakeSink{}
		startTestPump(t, sickbayConfig(st, link, sink))
		time.Sleep(300 * time.Millisecond)
		require.Zero(t, link.probes.Load())
		require.Zero(t, sink.missedCount())
	})
}

func TestNextIDIsMonotonic(t *testing.T) {
	p := &Pump{lastID: uint64(time.Now().Add(time.Hour).UnixMicro())} // clock behind the last id
	var last uint64 = p.lastID
	for range 1000 {
		id := p.nextIDLocked()
		assert.Greater(t, id, last)
		last = id
	}
	require.Less(t, last, uint64(1)<<53, "ids must be safe JavaScript integers")
}

// The pump has one reader: a new attach starts only after the previous
// session has been read to its end and has exited, so two connections can
// never record the same backlog concurrently or out of order.
func TestPumpSessionsNeverOverlap(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	p := startTestPump(t, testPumpConfig(st, link, sink))
	seq := uint64(0)
	for range 50 {
		s := link.nextSession(t)
		seq++
		s.emit(t, heartbeat(t, seq, time.Now()))
		s.nextAck(t)
		// Alternate a graceful drop (the pump cancels the connection) with
		// the session ending under it.
		if seq%2 == 0 {
			require.True(t, p.dropConnection())
		} else {
			s.end()
		}
	}
	link.nextSession(t)
	link.mu.Lock()
	defer link.mu.Unlock()
	require.Zero(t, link.overlaps)
	require.Equal(t, seq, contiguous(t, st))
}

// M1: stale heartbeats with a healthy beam mean a stuck connection, which is
// recycled rather than left alone.
//
// The pump reads no line while it records one, so a store commit as long as
// HeartbeatStale (one took 440ms on a loaded runner) looks like a silent
// connection, and the connection is recycled before its ack is written
// (#193). HeartbeatStale is well clear of that.
func TestPumpRecyclesSilentConnection(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	cfg := sickbayConfig(st, link, sink) // probe says healthy
	cfg.HeartbeatStale = 2 * time.Second
	startTestPump(t, cfg)
	s := link.nextSession(t)
	s.emit(t, hello(t, 1))
	s.nextAck(t)
	// The session now goes quiet but stays open, like a half-open attach.
	var s2 *fakeSession
	select {
	case s2 = <-link.sessions:
	case <-time.After(30 * time.Second):
		t.Fatal("the silent session wasn't recycled")
	}
	require.Equal(t, []uint64{0, 1}, link.getSinces()[:2], "reconnects from the contiguous seq")
	select {
	case <-s.done:
	default:
		t.Fatal("the silent session wasn't ended")
	}
	s2.emit(t, heartbeat(t, 2, time.Now()))
	require.Equal(t, uint64(2), s2.nextAck(t))
	require.Zero(t, sink.missedCount())
	require.Positive(t, link.probes.Load())
}

// #38: an attach that never sends its first line (tsh stalled, or a wedged
// serve whose socket backlog accepted the connect) never becomes the live
// connection, but with stale heartbeats and a healthy beam it is still
// recycled, after HeartbeatStale and not before, and the pump re-attaches.
// Every recycled session has ended before the next attach, and the pump's
// goroutine count doesn't grow across recycles.
func TestPumpRecyclesAttachWithoutFirstLine(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	cfg := sickbayConfig(st, link, sink) // probe says healthy
	// A wider window than sickbayConfig's leaves room for scheduling delays
	// under load in the grace lower bounds below.
	cfg.HeartbeatStale = 300 * time.Millisecond
	p := startTestPump(t, cfg)

	waitEnded := func(s *fakeSession) time.Duration {
		t.Helper()
		start := time.Now()
		select {
		case <-s.done:
		case <-time.After(cfg.HeartbeatStale + cfg.SickbayCheckEvery + 5*time.Second):
			t.Fatal("the silent attach wasn't recycled")
		}
		return time.Since(start)
	}

	s := link.nextSession(t)
	// The attempt started before the session was handed out, so it has to
	// last close to HeartbeatStale (minus the hand-off) before it is ended.
	require.GreaterOrEqual(t, waitEnded(s), cfg.HeartbeatStale/2, "recycled before its HeartbeatStale grace")
	require.False(t, p.Connected())

	var baseline int
	for i := range 6 {
		s = link.nextSession(t)
		if i == 1 {
			baseline = runtime.NumGoroutine()
		}
		// Heartbeats are long stale by now, so only the attempt's own grace
		// keeps the next check from ending it at once.
		require.GreaterOrEqual(t, waitEnded(s), cfg.HeartbeatStale/2, "re-attach recycled before its HeartbeatStale grace")
	}
	// Polled by hand: require.Eventually runs its condition on a goroutine of
	// its own, which would be counted.
	for deadline := time.Now().Add(5 * time.Second); runtime.NumGoroutine() > baseline; {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines grew across recycles: %d, was %d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A re-attach that does send lines becomes the live connection. The
	// sickbay check keeps recycling silent attempts meanwhile, as asserted
	// above, so a line the test emits HeartbeatStale or more into an attempt
	// (the poll above alone can take that long under load) races that
	// recycle: the session ends before or just after the pump reads the line,
	// and no ack comes (#66). With beam health unknown the check recycles
	// nothing. Once a probe has started after setProbe, any check that still
	// saw "healthy" has returned, so the first attempt open from then on
	// stays open until it sends.
	link.setProbe(func() (bool, error) { return false, errors.New("health unknown") })
	probes := link.probes.Load()
	require.Eventually(t, func() bool { return link.probes.Load() > probes }, 5*time.Second, 5*time.Millisecond)
	for s = link.nextSession(t); s.ended(); {
		s = link.nextSession(t)
	}
	s.emit(t, heartbeat(t, 1, time.Now()))
	require.Equal(t, uint64(1), s.nextAck(t))
	require.True(t, p.Connected())
	require.Zero(t, sink.missedCount())
	link.mu.Lock()
	defer link.mu.Unlock()
	require.Zero(t, link.overlaps)
}

// #81: recycling a live connection doesn't lose a command Send already wrote
// to it. attach hasn't read the order from its stdin yet when the recycle
// comes; killing it would drop the order although Send reported it sent, and
// the mission would wait for a hail that never comes. The recycle closes
// attach's stdin instead, so attach passes the order on and exits.
func TestPumpRecycleDeliversWrittenCommands(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	p := startTestPump(t, testPumpConfig(st, link, sink))
	s := link.nextSession(t)
	s.emit(t, hello(t, 1))
	require.Equal(t, uint64(1), s.nextAck(t))
	require.True(t, p.Connected())

	s.hold()
	order, err := NewMessage(TypeOrder, OrderBody{Title: "t", Body: "b"})
	require.NoError(t, err)
	order.Order = "ORD-0001"
	id, err := p.Send(order)
	require.NoError(t, err)
	require.True(t, p.dropConnection())

	got := s.nextCmd(t)
	require.Equal(t, TypeOrder, got.Type)
	require.Equal(t, id, got.Seq)
	require.Equal(t, "ORD-0001", got.Order)
	s.noCmd(t)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the recycled session didn't end")
	}
	s2 := link.nextSession(t)
	require.Equal(t, []uint64{0, 1}, link.getSinces())
	s2.emit(t, heartbeat(t, 2, time.Now()))
	require.Equal(t, uint64(2), s2.nextAck(t))
	require.True(t, p.Connected())
}

// #81: a recycled connection whose attach doesn't exit once its stdin is
// closed (half-open, or not relaying) is killed WriteTimeout later, and the
// pump re-attaches. Meanwhile it is no longer the live connection: Send
// fails at once with ErrNotConnected (its step retries it on the next
// connection) instead of writing to the connection being ended.
func TestPumpRecycleKillsAttachThatDoesNotExit(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	cfg := testPumpConfig(st, link, sink)
	cfg.WriteTimeout = 200 * time.Millisecond
	p := startTestPump(t, cfg)
	s := link.nextSession(t)
	s.emit(t, hello(t, 1))
	require.Equal(t, uint64(1), s.nextAck(t))

	s.wedge()
	start := time.Now()
	require.True(t, p.dropConnection())
	require.False(t, p.Connected())
	order, err := NewMessage(TypeOrder, OrderBody{Title: "t", Body: "b"})
	require.NoError(t, err)
	_, err = p.Send(order)
	require.ErrorIs(t, err, ErrNotConnected)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the wedged attach wasn't killed")
	}
	require.GreaterOrEqual(t, time.Since(start), cfg.WriteTimeout, "killed before its WriteTimeout grace")
	require.ErrorIs(t, s.ctx.Err(), context.Canceled, "ended by the kill")
	s2 := link.nextSession(t)
	s2.emit(t, heartbeat(t, 2, time.Now()))
	require.Equal(t, uint64(2), s2.nextAck(t))
	require.True(t, p.Connected())
}

// #81: once a connection has sent its first line it is no longer an attach
// attempt, so the #38 check doesn't kill it while it ends gracefully: a
// second recycle (the next sickbay check) finds nothing to recycle, even
// though the connection is older than HeartbeatStale, and attach still gets
// to pass on the order it hadn't read yet.
func TestPumpRecycleDoesNotKillEndingConnectionAsAttempt(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	cfg := testPumpConfig(st, link, sink)
	cfg.HeartbeatStale = 50 * time.Millisecond
	p := startTestPump(t, cfg)
	s := link.nextSession(t)
	s.emit(t, hello(t, 1))
	require.Equal(t, uint64(1), s.nextAck(t))
	time.Sleep(2 * cfg.HeartbeatStale) // the attach is now older than HeartbeatStale

	s.hold()
	s.wedge()
	order, err := NewMessage(TypeOrder, OrderBody{Title: "t", Body: "b"})
	require.NoError(t, err)
	order.Order = "ORD-0001"
	id, err := p.Send(order)
	require.NoError(t, err)
	require.True(t, p.dropConnection())
	require.False(t, p.dropConnection(), "the ending connection was recycled again, as an attempt")
	require.NoError(t, s.ctx.Err(), "attach was killed")

	s.relay()
	got := s.nextCmd(t)
	require.Equal(t, TypeOrder, got.Type)
	require.Equal(t, id, got.Seq)
}

// M1: a write attach doesn't take (it stopped reading stdin) times out,
// fails Send with ErrNotConnected and recycles the connection; the reader,
// blocked writing an ack, is freed by the same recycle.
func TestPumpWriteTimeoutRecyclesConnection(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	link.blockWrites = true
	cfg := testPumpConfig(st, link, sink)
	cfg.WriteTimeout = 100 * time.Millisecond
	p := startTestPump(t, cfg)
	s := link.nextSession(t)
	s.emit(t, hello(t, 1)) // its ack blocks
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stuck ack write didn't recycle the connection")
	}
	s = link.nextSession(t)
	require.Equal(t, uint64(1), link.getSinces()[1], "the message was recorded before the ack")
	h2 := hello(t, 2)
	go func() { _ = emitOrEnd(s, h2) }() // the pump is connected once it reads a line
	require.Eventually(t, p.Connected, 5*time.Second, 5*time.Millisecond)

	start := time.Now()
	_, err := p.Send(Message{Type: TypeBelay, Order: "ORD-1", Body: json.RawMessage(`{}`)})
	require.ErrorIs(t, err, ErrNotConnected)
	require.Less(t, time.Since(start), 5*time.Second)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stuck Send didn't recycle the connection")
	}
}

// M2: a message whose destination never appears is retried for
// UndeliverableAfter, then logged as undeliverable and skipped.
func TestPumpSkipsUndeliverableAfterTimeout(t *testing.T) {
	st := openStore(t)
	link := newFakeLink()
	sink := &fakeSink{missionErr: func(m Message) error {
		if m.Order == "ORD-GHOST" {
			return fmt.Errorf("%w: no mission order/ORD-GHOST", ErrUndeliverable)
		}
		return nil
	}}
	cfg := testPumpConfig(st, link, sink)
	cfg.UndeliverableAfter = 300 * time.Millisecond
	p := startTestPump(t, cfg)
	start := time.Now()
	attempts := 0
	for p.Acked() < 2 {
		require.Less(t, time.Since(start), 10*time.Second, "never skipped")
		s := link.nextSession(t)
		attempts++
		// serve replays from --since: the ghost hail, then a real one.
		if link.getSinces()[len(link.getSinces())-1] < 1 {
			if err := emitOrEnd(s, hail(t, 1, "ORD-GHOST")); err != nil {
				continue
			}
		}
		if err := emitOrEnd(s, hail(t, 2, "ORD-1")); err != nil {
			continue
		}
		require.Eventually(t, func() bool { return p.Acked() == 2 }, 5*time.Second, 5*time.Millisecond)
	}
	require.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "skipped before UndeliverableAfter")
	require.Greater(t, attempts, 1, "retried first")
	missions := sink.missions()
	require.Len(t, missions, 1)
	require.Equal(t, "ORD-1", missions[0].Order)
	entries, err := st.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, UndeliverableLogKind, entries[0].Kind)
	require.Equal(t, "ORD-GHOST", entries[0].OrderID)
	var logged Message
	require.NoError(t, json.Unmarshal(entries[0].Payload, &logged))
	require.Equal(t, uint64(1), logged.Seq)
	require.Equal(t, TypeHail, logged.Type)
}

// #160: a message whose destination has finished (ErrGone) is logged as
// undeliverable and skipped at once, on the same connection, with no wait
// for UndeliverableAfter; the next message goes through.
func TestPumpSkipsGoneAtOnce(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	link := newFakeLink()
	sink := &fakeSink{missionErr: func(m Message) error {
		if m.Order == "ORD-DONE" {
			return fmt.Errorf("%w: order/ORD-DONE", ErrGone)
		}
		return nil
	}}
	cfg := testPumpConfig(st, link, sink)
	cfg.UndeliverableAfter = time.Hour // a retry would never end in time
	startTestPump(t, cfg)
	s := link.nextSession(t)
	s.emit(t, hail(t, 1, "ORD-DONE"))
	s.emit(t, hail(t, 2, "ORD-1"))
	require.Equal(t, uint64(1), s.nextAck(t))
	require.Equal(t, uint64(2), s.nextAck(t))
	require.Equal(t, []uint64{0}, link.getSinces(), "no reconnect")
	missions := sink.missions()
	require.Len(t, missions, 1)
	require.Equal(t, "ORD-1", missions[0].Order)
	entries, err := st.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, UndeliverableLogKind, entries[0].Kind)
	require.Equal(t, "ORD-DONE", entries[0].OrderID)
	require.Contains(t, entries[0].Text, "has finished: order/ORD-DONE")
	var logged Message
	require.NoError(t, json.Unmarshal(entries[0].Payload, &logged))
	require.Equal(t, uint64(1), logged.Seq)
	require.Equal(t, TypeHail, logged.Type)
}

// emitOrEnd writes msg to the pump, or reports that the session ended
// (the pump dropped it after a rejected message).
func emitOrEnd(s *fakeSession, msg Message) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = s.pw.Write(append(line, '\n'))
	return err
}

// Other Sink errors are retried indefinitely: never skipped.
func TestPumpRetriesOtherSinkErrorsIndefinitely(t *testing.T) {
	st := openStore(t)
	link := newFakeLink()
	sink := &fakeSink{missionErr: func(Message) error { return errors.New("database is locked") }}
	cfg := testPumpConfig(st, link, sink)
	cfg.UndeliverableAfter = 50 * time.Millisecond
	startTestPump(t, cfg)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		s := link.nextSession(t)
		_ = emitOrEnd(s, hail(t, 1, "ORD-1"))
		<-s.done
	}
	require.Equal(t, uint64(0), contiguous(t, st))
	entries, err := st.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// L1: an undecodable line that still carries a seq is recorded with an
// undeliverable log entry, so it doesn't leave a permanent gap.
func TestPumpRecordsUndecodableLineWithSeq(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	startTestPump(t, testPumpConfig(st, link, sink))
	s := link.nextSession(t)
	s.emitRaw(t, `{"v":1,"seq":1,"crew":"crew-1","type":"heartbeat","ts":"yesterday","body":{}}`)
	require.Equal(t, uint64(1), s.nextAck(t))
	s.emitRaw(t, `{"seq":"two"}`) // no usable seq: skipped
	s.emit(t, hello(t, 2))
	require.Equal(t, uint64(2), s.nextAck(t))
	entries, err := st.ListLog(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, UndeliverableLogKind, entries[0].Kind)
	require.Contains(t, string(entries[0].Payload), "yesterday")
}

// #32: after a long outage the beam replays a backlog whose heartbeats carry
// their old ts, so they look stale for longer than HeartbeatStale. Lines are
// still arriving, so the healthy connection must not be recycled (that would
// restart the replay every check), and once it has been delivering for
// HeartbeatStale it isn't even probed. When the lines stop, the silent
// connection is recycled as before.
//
// The pump reads the next line only once it has recorded the last one, so
// a store write slowed by a loaded runner (a commit took 440ms) is a gap
// between lines, and one as long as HeartbeatStale is recycled as a silent
// connection would be (#193). HeartbeatStale is large enough that no store
// write gets near it (production's is 30s), and the test moves on when the
// pump counts the connection as delivering, not after multiples of it.
func TestPumpKeepsConnectionReplayingBacklog(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{} // probe says healthy
	cfg := testPumpConfig(st, link, sink)
	cfg.HeartbeatStale = 2 * time.Second
	cfg.SickbayCheckEvery = 20 * time.Millisecond
	p := startTestPump(t, cfg)
	s := link.nextSession(t)
	// The first line, which makes the connection live, comes a while after the
	// pump started (the heartbeat baseline), so the checks between heartbeats
	// going stale and the connection being live for HeartbeatStale are sure to
	// probe. It still comes well before HeartbeatStale, past which the first
	// probe would end the attach as one stalled before its first line.
	time.Sleep(cfg.HeartbeatStale / 4)

	old := time.Now().Add(-time.Hour)
	var seq uint64
	emit := func() {
		seq++
		s.emit(t, heartbeat(t, seq, old.Add(time.Duration(seq)*time.Second)))
		time.Sleep(10 * time.Millisecond)
	}
	// The replay's first HeartbeatStale: probed, and kept.
	deadline := time.Now().Add(30 * time.Second)
	for !p.delivering() {
		require.True(t, time.Now().Before(deadline), "the connection never counted as delivering")
		emit()
	}
	require.Positive(t, link.probes.Load(), "the replay's first HeartbeatStale is probed")
	// From then on: not probed. Lines keep coming for some checks.
	probesMid := link.probes.Load()
	for until := time.Now().Add(10 * cfg.SickbayCheckEvery); time.Now().Before(until); {
		emit()
	}
	require.True(t, p.delivering())
	require.Greater(t, time.Since(p.LastHeartbeat()), cfg.HeartbeatStale, "the backlog's heartbeats stayed stale")
	require.Equal(t, probesMid, link.probes.Load(), "a connection delivering for HeartbeatStale isn't probed")
	select {
	case <-s.done:
		t.Fatal("a connection still delivering lines was recycled")
	default:
	}
	require.Len(t, link.getSinces(), 1, "never reconnected")
	require.Equal(t, int64(1), p.connects.Load())
	require.Eventually(t, func() bool { return p.Acked() == seq }, 30*time.Second, 5*time.Millisecond)

	// The backlog ends and the connection goes silent: it is recycled.
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the silent connection wasn't recycled")
	}
	link.nextSession(t)
	require.Equal(t, seq, link.getSinces()[1], "reconnects from the contiguous seq")
	require.Zero(t, sink.missedCount())
}

// #32: lines arriving across short connections, as from a crash-looping
// serve that emits a hello each time it accepts an attach, don't skip the
// probe: no connection lives HeartbeatStale, so the beam is still probed and
// a probe that reports it unhealthy sends the crew member to sickbay.
func TestPumpProbesCrashLoopingServe(t *testing.T) {
	st := openStore(t)
	link, sink := newFakeLink(), &fakeSink{}
	link.setProbe(func() (bool, error) { return false, nil })
	startTestPump(t, sickbayConfig(st, link, sink))
	deadline := time.Now().Add(5 * time.Second)
	for seq := uint64(1); sink.missedCount() == 0; seq++ {
		require.True(t, time.Now().Before(deadline), "a crash-looping serve never reached sickbay")
		s := link.nextSession(t)
		if emitOrEnd(s, hello(t, seq)) == nil {
			time.Sleep(50 * time.Millisecond) // well under HeartbeatStale
		}
		s.end() // serve crashed: the socket closes and attach exits
	}
	require.Equal(t, 1, sink.missedCount())
}

// #32: once a write has timed out, the connection is abandoned and every
// later write to it (the reader's acks hold it directly) fails at once with
// ErrNotConnected instead of blocking for another WriteTimeout.
func TestPumpAbandonedConnectionFailsWritesAtOnce(t *testing.T) {
	st := openStore(t)
	link := newFakeLink()
	cfg := testPumpConfig(st, link, &fakeSink{})
	cfg.WriteTimeout = 100 * time.Millisecond
	p, err := newPump(cfg)
	require.NoError(t, err)

	sess := newFakeSession(t.Context())
	sess.blockWrites = true
	cancelled := make(chan struct{})
	var once sync.Once
	c := &conn{sess: sess, cancel: func() { once.Do(func() { close(cancelled) }) }}
	p.conn.Store(c)

	ack := func() error {
		m, err := NewMessage(TypeAck, AckBody{Seq: 1})
		require.NoError(t, err)
		p.wmu.Lock()
		defer p.wmu.Unlock()
		_, err = p.writeLocked(c, m)
		return err
	}
	require.ErrorIs(t, ack(), ErrNotConnected, "the first write times out")
	<-cancelled
	require.True(t, c.abandoned.Load())
	require.False(t, p.Connected())

	start := time.Now()
	require.ErrorIs(t, ack(), ErrNotConnected)
	require.Less(t, time.Since(start), cfg.WriteTimeout/2, "an abandoned connection isn't written to")
	sess.end()
}
