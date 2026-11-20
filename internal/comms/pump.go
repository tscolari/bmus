package comms

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tscolari/bmus/internal/store"
)

// The comms pump (spec phase-1-plan §6, issue #14).
//
// One Pump runs per live crew member. It holds a `combadge attach` session to
// the crew member's beam, reconnecting with backoff whenever it ends, and
//
//   - records every inbound message in the inbox, once, and acks it;
//   - writes progress straight to the ship's log, and usage straight
//     to order_usage;
//   - forwards hello, hail, checkpoint_ready, report, error and
//     consult_reply to the Sink, which hands them to workflows;
//   - writes commands from workflow steps (Send) to the beam;
//   - watches heartbeats and, when they go stale and the beam doesn't answer a
//     probe, tells the Sink the crew member's heartbeat is missed.
//
// Package placement: the pump lives here, next to the protocol, and must not
// import internal/bridge (bridge imports comms) or internal/transporter
// (transporter's tests import comms). Its outputs go through Sink and its
// beam through Link; the bridge implements both (Sink with dbos.Send, Link
// with transporter.Provider), and holds the registry of pumps. DBOS stays out
// of this package: the only DBOS-specific thing a Sink gets is the *sql.Tx to
// send on.

// ErrNotConnected is returned by Send while the pump has no live session to
// the beam: before the first attach, between reconnects, and after Stop.
// Callers are workflow steps; they fail and are retried (see Send).
var ErrNotConnected = errors.New("comms: combadge not connected")

// ErrUndeliverable is wrapped by a Sink error to say the message's
// destination doesn't exist yet (the bridge: a mission or hail workflow
// that hasn't started). The pump retries such a message for
// UndeliverableAfter, then records it as undeliverable in the ship's log
// and moves on, so one message can't stall the crew member's stream forever.
// Any other Sink error is retried indefinitely.
var ErrUndeliverable = errors.New("comms: message destination does not exist yet")

// ErrGone is wrapped by a Sink error to say the message's destination has
// finished for good (the bridge: a mission or hail workflow that has ended).
// Nothing will ever read the message, so the pump records it as
// undeliverable at once, without waiting UndeliverableAfter (#160).
var ErrGone = errors.New("comms: message destination has finished")

// Session is a running `combadge attach`, as transporter.Session describes
// it. transporter.Session satisfies it; it is restated here because this
// package can't import transporter. Close closes attach's stdin, and the
// pump may call it while a Write is in progress (an *os.File pipe allows
// that); attach then exits once it has passed on what it read.
type Session interface {
	io.WriteCloser
	Stdout() io.Reader
	Wait() error
}

// Link is how a pump reaches its crew member's beam. The bridge implements it
// over transporter.Provider (AttachCombadge and Probe for one beam).
type Link interface {
	// Attach starts `combadge attach --since <since>`. Cancelling ctx kills
	// it.
	Attach(ctx context.Context, since uint64) (Session, error)
	// Probe reports whether the beam is alive, as transporter.Provider.Probe:
	// an error means health is unknown, (false, nil) that the beam is gone.
	Probe(ctx context.Context) (healthy bool, err error)
}

// Sink receives what the pump forwards out of comms. The bridge implements it
// with dbos.Send.
//
// ToMission, ToHail, CrewError, CrewHello and UsageRecorded are called inside the store transaction that
// records the message in the inbox, and get that transaction: an
// implementation that writes on tx (dbos.WithSendTransaction) commits
// atomically with the inbox row, so a crash can neither lose the message (it
// isn't recorded or acked, so the beam replays it) nor deliver it twice (it is
// recorded, so the replay is dropped). Returning an error rolls the inbox row
// back; the pump then drops the connection and the message is redelivered
// after a reconnect, except as ErrUndeliverable and ErrGone say.
type Sink interface {
	// ToMission forwards msg (hail, checkpoint_ready, report, or an error
	// with an order) to the mission running msg.Order. The whole message
	// goes, Type and Seq included.
	ToMission(ctx context.Context, tx *sql.Tx, msg Message) error
	// ToHail forwards a consult_reply, which carries no order, to the
	// workflow of hail hailID (spec/phase-3-plan-2026-10-29.md §4).
	ToHail(ctx context.Context, tx *sql.Tx, msg Message, hailID string) error
	// CrewError handles an error message with no order: a crew-level
	// failure rather than a mission's.
	CrewError(ctx context.Context, tx *sql.Tx, msg Message) error
	// CrewHello tells the crew member's tour that combadge said hello.
	// serve sends one on every attach, so it comes again after each
	// reconnect (#173).
	CrewHello(ctx context.Context, tx *sql.Tx, msg Message) error
	// UsageRecorded runs in the inbox transaction of a usage report that
	// changed order's usage row (#119): the bridge's budget backstop. It
	// gets the transaction to read and write in.
	UsageRecorded(ctx context.Context, tx store.InboxTx, order string) error
	// HeartbeatMissed tells the crew member's tour that heartbeats stopped
	// and the beam failed its probe. The pump calls it at most once.
	HeartbeatMissed(ctx context.Context) error
}

// PumpConfig configures a Pump. Zero durations take the defaults.
type PumpConfig struct {
	CrewID string
	// TourNo is the crew member's tour the beam serves. The inbox is keyed
	// by tour too, since each tour's beam numbers its messages from seq 1.
	TourNo int
	// Beam names the beam in logs; the pump reaches it through Link.
	Beam   string
	Link   Link
	Store  *store.Store
	Sink   Sink
	Logger *slog.Logger // nil means slog.Default()

	// HeartbeatStale is how old the newest heartbeat may get before the
	// beam is probed. Default 30s (three missed 10s beats).
	HeartbeatStale time.Duration
	// SickbayCheckEvery is how often heartbeat freshness is checked.
	// Default 10s.
	SickbayCheckEvery time.Duration
	// BackoffMin and BackoffMax bound the wait before a reconnect, which
	// doubles after each failed connection. Defaults 1s and 30s.
	BackoffMin, BackoffMax time.Duration
	// BackoffResetAfter: a connection that lived at least this long, or
	// recorded a message, resets the backoff to BackoffMin. Default 10s.
	BackoffResetAfter time.Duration
	// ProbeTimeout bounds one Link.Probe. Default 60s.
	ProbeTimeout time.Duration
	// WriteTimeout bounds one write of a command line to the session (Send
	// and acks). A write that doesn't finish in time recycles the
	// connection. It is also how long a connection the sickbay check
	// recycles gets to pass on what was written to it and exit, after its
	// stdin is closed, before it is killed (see recycle). Default 10s.
	WriteTimeout time.Duration
	// UndeliverableAfter is how long a message the Sink rejects with
	// ErrUndeliverable keeps being retried before it is logged as
	// undeliverable and skipped. Default 5m. ErrGone isn't retried.
	UndeliverableAfter time.Duration
}

// Default pump timings.
const (
	DefaultHeartbeatStale = 30 * time.Second
	// DefaultSickbayCheckEvery: with a 30s stale window, a beam that dies
	// right after a heartbeat is detected within about 35s.
	DefaultSickbayCheckEvery  = 5 * time.Second
	DefaultBackoffMin         = time.Second
	DefaultBackoffMax         = 30 * time.Second
	DefaultBackoffResetAfter  = 10 * time.Second
	DefaultProbeTimeout       = 60 * time.Second
	DefaultWriteTimeout       = 10 * time.Second
	DefaultUndeliverableAfter = 5 * time.Minute
)

// Ship's log kinds the pump writes.
const (
	// progressLogKind is for progress entries: one of the log's kinds
	// (beam, order, hail, done, fail), as progress is an order's.
	progressLogKind = "order"
	// UndeliverableLogKind is for inbound messages the pump recorded
	// without handling: undecodable lines that still carry a seq, and
	// messages whose destination never appeared (ErrUndeliverable) or has
	// finished (ErrGone). The
	// payload holds the message (or the raw line) for the operator.
	UndeliverableLogKind = "undeliverable"
	// FindingLogKind is for a finding the pump kept on its order (#153);
	// store.RecordFinding writes the entry.
	FindingLogKind = store.FindingLogKind
)

// Pump is one crew member's comms pump. Start it with StartPump and end it
// with Stop.
type Pump struct {
	cfg    PumpConfig
	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}

	// wmu serialises writes to the session (Send and acks run on different
	// goroutines, and NDJSON lines mustn't interleave) and guards lastID.
	wmu    sync.Mutex
	lastID uint64 // last command id assigned
	// conn is the live connection, nil while not connected. It is atomic
	// rather than under wmu so the sickbay check can recycle a connection
	// whose write is stuck holding wmu.
	conn atomic.Pointer[conn]
	// attempt is the attach attempt in progress, from just before
	// Link.Attach until its first line arrives (it is then conn) or
	// connectOnce returns, nil otherwise. Unlike conn it is set before the
	// first line, so the sickbay check can recycle an attach that never sends
	// one (#38).
	attempt atomic.Pointer[attempt]

	// undeliverable tracks the message the Sink keeps rejecting with
	// ErrUndeliverable: its seq and when that started. Reader goroutine only.
	undeliverableSeq   uint64
	undeliverableSince time.Time

	// bmu guards lastBeat and lastLineAt.
	bmu      sync.Mutex
	lastBeat time.Time
	// lastLineAt is when the newest inbound line of any kind arrived, on
	// any connection; zero before the first. The sickbay check uses it to
	// tell a connection still delivering a replayed backlog (whose
	// heartbeats look stale by ts) from a silent one.
	lastLineAt time.Time

	contig   atomic.Uint64 // highest contiguous recorded seq, as last acked
	connects atomic.Int64  // attach sessions started, for tests and logs

	// sleep waits d or until ctx is done, reporting whether it waited the
	// full d. Tests replace it to observe backoff.
	sleep func(ctx context.Context, d time.Duration) bool
}

// conn is one attach session.
type conn struct {
	sess   Session
	cancel context.CancelFunc // ends the session (kills attach)
	// liveAt is when the connection's first line arrived and it became the
	// pump's live connection. Set before it is published in Pump.conn and
	// never changed after.
	liveAt time.Time
	// abandoned is set when a write to the connection timed out: it is
	// being torn down, and further writes fail at once with ErrNotConnected
	// instead of each blocking for WriteTimeout (the reader keeps acking
	// lines it reads until attach is killed).
	abandoned atomic.Bool
	// kill is the timer recycle starts to kill a connection that doesn't
	// end gracefully; connectOnce stops it once the connection has ended.
	kill atomic.Pointer[time.Timer]
}

// attempt is one run of connectOnce: an attach that may not have sent a line
// yet.
type attempt struct {
	cancel  context.CancelFunc // cancels the attempt's connCtx (kills attach)
	started time.Time
}

// StartPump starts a pump for cfg.CrewID; it runs until ctx is done or Stop
// is called. The pump's start time is the heartbeat baseline: if no fresh
// heartbeat arrives within HeartbeatStale of it, the beam is probed.
func StartPump(ctx context.Context, cfg PumpConfig) (*Pump, error) {
	p, err := newPump(cfg)
	if err != nil {
		return nil, err
	}
	p.start(ctx)
	return p, nil
}

func newPump(cfg PumpConfig) (*Pump, error) {
	switch {
	case cfg.CrewID == "":
		return nil, errors.New("comms: PumpConfig.CrewID is required")
	case cfg.Link == nil:
		return nil, errors.New("comms: PumpConfig.Link is required")
	case cfg.Store == nil:
		return nil, errors.New("comms: PumpConfig.Store is required")
	case cfg.Sink == nil:
		return nil, errors.New("comms: PumpConfig.Sink is required")
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.HeartbeatStale, DefaultHeartbeatStale)
	def(&cfg.SickbayCheckEvery, DefaultSickbayCheckEvery)
	def(&cfg.BackoffMin, DefaultBackoffMin)
	def(&cfg.BackoffMax, DefaultBackoffMax)
	def(&cfg.BackoffResetAfter, DefaultBackoffResetAfter)
	def(&cfg.ProbeTimeout, DefaultProbeTimeout)
	def(&cfg.WriteTimeout, DefaultWriteTimeout)
	def(&cfg.UndeliverableAfter, DefaultUndeliverableAfter)
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Pump{
		cfg:   cfg,
		log:   logger.With("crew", cfg.CrewID, "beam", cfg.Beam),
		done:  make(chan struct{}),
		sleep: sleepCtx,
	}, nil
}

func (p *Pump) start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	p.lastBeat = time.Now()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.run(ctx) }()
	go func() { defer wg.Done(); p.watch(ctx) }()
	go func() { wg.Wait(); close(p.done) }()
}

// CrewID returns the crew member the pump serves.
func (p *Pump) CrewID() string { return p.cfg.CrewID }

// Beam returns the beam the pump was started for (PumpConfig.Beam).
func (p *Pump) Beam() string { return p.cfg.Beam }

// Stop stops the pump, ending its session, and waits for its goroutines. It
// is idempotent. It must not be called from a Sink method.
func (p *Pump) Stop() {
	p.cancel()
	<-p.done
}

// Done is closed once the pump has stopped.
func (p *Pump) Done() <-chan struct{} { return p.done }

// Acked returns the highest contiguous inbound seq recorded, which is what
// the pump acks and reconnects with.
func (p *Pump) Acked() uint64 { return p.contig.Load() }

// Connected reports whether a session is live, so Send would write to it.
func (p *Pump) Connected() bool { return p.conn.Load() != nil }

// recycle ends the current connection, if any, so the pump reconnects from
// the contiguous seq. It doesn't take wmu, so it works while a write is stuck.
//
// The connection is ended gracefully (#81): it stops being the live one, so
// Send fails at once with ErrNotConnected and its caller retries on the next
// connection, and attach's stdin is closed. attach passes on whatever it
// hasn't read yet, then exits on the EOF, and the reader takes the lines
// still in flight up to there. Killing attach outright would drop commands
// Send had already written to its stdin and reported as sent (a mission's
// order, say), whenever the kill landed before attach read them: a relay
// starved of CPU, or simply a check that fires right after the write. Only an
// attach that hasn't exited WriteTimeout later (half-open, or not relaying)
// is killed.
//
// With no live connection it ends the attach attempt in progress instead, if
// that has been going for at least HeartbeatStale without sending a line: an
// attach stalled before its first line (tsh hanging, or a wedged serve whose
// socket backlog still accepts the connect) would otherwise never end while
// the probe reports healthy (#38). A younger attempt is left alone, so a slow
// attach isn't killed before it has had the same grace a live connection gets.
// Nothing can have been written to an attempt, so it is killed at once.
func (p *Pump) recycle() bool {
	if c := p.conn.Load(); c != nil && p.conn.CompareAndSwap(c, nil) {
		if err := c.sess.Close(); err != nil {
			p.log.Debug("comms: close attach stdin", "err", err)
		}
		c.kill.Store(time.AfterFunc(p.cfg.WriteTimeout, c.cancel))
		return true
	}
	if a := p.attempt.Load(); a != nil && time.Since(a.started) >= p.cfg.HeartbeatStale {
		a.cancel()
		return true
	}
	return false
}

// Send writes a command to the beam and returns the command id it assigned.
// msg needs Type, Body and (for order-bound commands) Order, e.g. from
// NewMessage; Send sets V, Seq (the command id) and Crew, and TS if zero.
//
// It returns an error wrapping ErrNotConnected if there is no live session or
// the write fails. Call it only from a workflow step (through the bridge's
// pump registry), never from workflow code: it is I/O.
//
// A nil error means the line was written to attach's stdin, not that serve
// received it. Phase 1 doesn't redeliver commands lost to a dropped
// connection (plan §6); the operator re-issues them. A step replayed by DBOS
// sends again under a new id, which is safe for Phase 1's commands: serve
// ignores an order it is already running or has finished, and an answer to a
// hail ref it no longer waits on.
func (p *Pump) Send(msg Message) (uint64, error) {
	if msg.Type == "" {
		return 0, errors.New("comms: Send: message has no type")
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	c := p.conn.Load()
	if c == nil {
		return 0, ErrNotConnected
	}
	return p.writeLocked(c, msg)
}

// nextIDLocked returns a new command id: max(last+1, now in µs).
//
// serve dedups command ids forever (state.json), so ids must never repeat for
// the life of the beam, across bridge restarts too, and nothing is persisted
// here. The wall clock in microseconds carries uniqueness across restarts: a
// restarted pump's first id is the current time, above every id the previous
// pump issued as long as the clock hasn't stepped back further than the gap
// between them (ids advance by one per command, far slower than one per µs).
// Within a pump, last+1 keeps ids strictly increasing whatever the clock does.
// Ids stay below 2^53 (JavaScript's safe integer limit, which serve checks)
// until the year 2255.
//
// A beam outlives a pump, and a crew member has one pump per process (the
// bridge's registry), so this per-pump counter is enough.
func (p *Pump) nextIDLocked() uint64 {
	id := max(p.lastID+1, uint64(time.Now().UnixMicro()))
	p.lastID = id
	return id
}

func (p *Pump) writeLocked(c *conn, msg Message) (uint64, error) {
	if c.abandoned.Load() {
		return 0, fmt.Errorf("%w: write %s command: connection abandoned after a write timeout", ErrNotConnected, msg.Type)
	}
	msg.V = Version
	msg.Seq = p.nextIDLocked()
	msg.Crew = p.cfg.CrewID
	if msg.TS.IsZero() {
		msg.TS = time.Now().UTC()
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return 0, fmt.Errorf("comms: marshal %s command: %w", msg.Type, err)
	}
	if err := p.writeTimeout(c, append(line, '\n')); err != nil {
		return 0, fmt.Errorf("%w: write %s command: %v", ErrNotConnected, msg.Type, err)
	}
	return msg.Seq, nil
}

// errWriteTimeout is a session write that didn't finish in WriteTimeout.
var errWriteTimeout = errors.New("write timed out")

// writeTimeout writes line to c, giving up after WriteTimeout. Session is a
// plain io.Writer with no deadline, so the write runs in a goroutine; on
// timeout the connection is cancelled, which kills attach and so fails the
// stuck write, and the pump reconnects. Until then c is no longer the live
// connection, so nothing else writes to it and no line can interleave with
// the abandoned one. c is also marked abandoned, so a writer still holding
// it (the reader's acks) fails at once rather than waiting out another
// WriteTimeout under wmu, which would stall Send on the next connection.
func (p *Pump) writeTimeout(c *conn, line []byte) error {
	done := make(chan error, 1)
	go func() {
		_, err := c.sess.Write(line)
		done <- err
	}()
	t := time.NewTimer(p.cfg.WriteTimeout)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		c.abandoned.Store(true)
		p.conn.CompareAndSwap(c, nil)
		c.cancel()
		p.log.Warn("comms: write to combadge timed out; recycling the connection", "timeout", p.cfg.WriteTimeout)
		return errWriteTimeout
	}
}

// run is the reconnect loop. Each iteration attaches with --since the
// highest contiguous seq recorded in the store (so a restarted bridge resumes
// where the last one stopped), serves the session until it ends, and backs off
// before the next one: BackoffMin, doubling up to BackoffMax, reset by a
// connection that lived BackoffResetAfter or recorded a message.
func (p *Pump) run(ctx context.Context) {
	backoff := p.cfg.BackoffMin
	for {
		healthy := p.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if healthy {
			backoff = p.cfg.BackoffMin
		}
		p.log.Debug("comms: reconnecting", "in", backoff)
		if !p.sleep(ctx, backoff) {
			return
		}
		backoff = min(2*backoff, p.cfg.BackoffMax)
	}
}

// connectOnce runs one attach session to its end. It reports whether the
// session was healthy enough to reset the backoff.
func (p *Pump) connectOnce(ctx context.Context) (healthy bool) {
	since, err := p.cfg.Store.ContiguousInboxSeq(ctx, p.cfg.CrewID, p.cfg.TourNo)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Error("comms: read inbox", "err", err)
		}
		return false
	}
	p.contig.Store(since)

	// Each connection gets its own child ctx, cancelled when the connection
	// ends, as transporter.Session requires to release the session.
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := time.Now()
	a := &attempt{cancel: cancel, started: started}
	p.attempt.Store(a)
	defer p.attempt.CompareAndSwap(a, nil)
	sess, err := p.cfg.Link.Attach(connCtx, since)
	if err != nil {
		if ctx.Err() == nil {
			p.log.Warn("comms: attach failed", "since", since, "err", err)
		}
		return false
	}
	p.connects.Add(1)
	p.log.Debug("comms: attached", "since", since)
	c := &conn{sess: sess, cancel: cancel}

	recorded, rerr := p.serveConn(connCtx, c, since)

	p.conn.CompareAndSwap(c, nil)
	cancel()
	if t := c.kill.Load(); t != nil {
		t.Stop()
	}
	werr := sess.Wait()
	if ctx.Err() == nil {
		p.log.Info("comms: session ended", "read_err", rerr, "exit", werr, "lived", time.Since(started).Round(time.Millisecond))
	}
	return recorded || time.Since(started) >= p.cfg.BackoffResetAfter
}

// serveConn reads c's NDJSON lines until it ends (EOF, a read error, ctx) or
// a message can't be recorded, and reports whether it recorded any message.
//
// The session counts as connected, and Send writes to it, from its first
// line: serve sends lines only after the attach handshake, so a line proves
// commands written now reach serve, unlike a just-started attach that may
// still fail to dial the socket.
func (p *Pump) serveConn(ctx context.Context, c *conn, since uint64) (recordedAny bool, err error) {
	in := &inbound{contig: since, ahead: map[uint64]bool{}}
	r := bufio.NewReader(c.sess.Stdout())
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// A partial last line was cut off by the session ending; serve
			// replays it on the next connection.
			return recordedAny, err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		p.noteLine()
		if in.lines == 0 && ctx.Err() == nil {
			// No longer an attempt: recycle ends it as a connection, and
			// gracefully, from now on. There is one attempt at a time.
			// Cleared before conn is published, so recycle never sees it as
			// both and kills it as an attempt.
			p.attempt.Store(nil)
			c.liveAt = time.Now()
			p.conn.Store(c)
		}
		in.lines++
		recorded, err := p.handleLine(ctx, in, line)
		if err != nil {
			if ctx.Err() == nil {
				p.log.Error("comms: handling inbound message failed; reconnecting to retry it", "err", err)
			}
			return recordedAny, err
		}
		recordedAny = recordedAny || recorded
		p.ackIfAdvanced(c, in)
	}
}

// inbound is one connection's receive state.
type inbound struct {
	lines  int
	contig uint64          // highest contiguous recorded seq
	ahead  map[uint64]bool // recorded seqs above contig+1
	acked  uint64          // highest ack sent on this connection
}

// advance notes seq as recorded and moves contig past every consecutive
// recorded seq.
func (in *inbound) advance(seq uint64) {
	if seq <= in.contig {
		return
	}
	in.ahead[seq] = true
	for in.ahead[in.contig+1] {
		delete(in.ahead, in.contig+1)
		in.contig++
	}
}

// handleLine decodes, dedups, records and dispatches one inbound line. It
// reports whether it recorded a new message. An error means the message
// wasn't recorded (store or Sink failure) and the connection should be
// dropped so it is redelivered.
func (p *Pump) handleLine(ctx context.Context, in *inbound, line []byte) (recorded bool, err error) {
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		return p.handleUndecodable(ctx, in, line, err)
	}
	if msg.Seq == 0 {
		p.log.Warn("comms: skipping message without seq", "type", msg.Type)
		return false, nil
	}
	if msg.Type == TypeHeartbeat {
		// Replayed heartbeats are old: freshness is judged by ts, whether or
		// not the message is new.
		p.noteHeartbeat(msg.TS)
	}
	if msg.Seq <= in.contig {
		return false, nil // already recorded; acked below if need be
	}
	recorded, err = p.cfg.Store.RecordInbox(ctx, p.cfg.CrewID, p.cfg.TourNo, msg.Seq, func(tx store.InboxTx) error {
		return p.dispatch(ctx, tx, msg)
	})
	if (errors.Is(err, ErrUndeliverable) || errors.Is(err, ErrGone)) && ctx.Err() == nil {
		recorded, err = p.handleUndeliverable(ctx, msg, line, err)
	}
	if err != nil {
		return false, fmt.Errorf("record %s seq %d: %w", msg.Type, msg.Seq, err)
	}
	// Recorded now or by an earlier connection: either way it is in the inbox.
	in.advance(msg.Seq)
	return recorded, nil
}

// handleUndeliverable deals with a message the Sink rejected with
// ErrUndeliverable or ErrGone (cause). For ErrUndeliverable, for
// UndeliverableAfter from the first rejection, it returns cause, so the
// connection drops and the message is retried after a reconnect: its
// destination may just not exist yet. After that, or at once for ErrGone, it
// records the seq with an undeliverable log entry holding the message, so the
// stream moves on and acks keep flowing.
func (p *Pump) handleUndeliverable(ctx context.Context, msg Message, line []byte, cause error) (bool, error) {
	if errors.Is(cause, ErrGone) {
		p.log.Warn("comms: message destination has finished; logged and skipped",
			"type", msg.Type, "seq", msg.Seq, "order", msg.Order, "err", cause)
	} else {
		now := time.Now()
		if p.undeliverableSeq != msg.Seq {
			p.undeliverableSeq, p.undeliverableSince = msg.Seq, now
		}
		if now.Sub(p.undeliverableSince) < p.cfg.UndeliverableAfter {
			return false, cause
		}
		p.log.Error("comms: giving up on an undeliverable message; logged and skipped",
			"type", msg.Type, "seq", msg.Seq, "order", msg.Order, "err", cause, "after", p.cfg.UndeliverableAfter)
	}
	recorded, err := p.cfg.Store.RecordInbox(ctx, p.cfg.CrewID, p.cfg.TourNo, msg.Seq, func(tx store.InboxTx) error {
		_, err := tx.AppendLog(store.LogEntry{
			Kind: UndeliverableLogKind, CrewID: p.cfg.CrewID, OrderID: msg.Order,
			Text:    fmt.Sprintf("%s seq %d undeliverable: %v", msg.Type, msg.Seq, cause),
			Payload: json.RawMessage(line),
		})
		return err
	})
	if err == nil {
		p.undeliverableSeq = 0
	}
	return recorded, err
}

// handleUndecodable deals with a line that isn't a valid Message. If a seq
// can still be read from it (say the ts is malformed), that seq is recorded
// with an undeliverable log entry holding the raw line: left unrecorded it
// would be a permanent gap, and the ack would never move past it again.
// Otherwise the line is logged and skipped.
func (p *Pump) handleUndecodable(ctx context.Context, in *inbound, line []byte, decodeErr error) (bool, error) {
	var head struct {
		Seq  uint64 `json:"seq"`
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &head) != nil || head.Seq == 0 {
		p.log.Warn("comms: skipping undecodable line", "err", decodeErr, "line", truncate(line))
		return false, nil
	}
	if head.Seq <= in.contig {
		return false, nil
	}
	p.log.Error("comms: recording undecodable message as undeliverable", "seq", head.Seq, "type", head.Type, "err", decodeErr)
	payload, err := json.Marshal(struct {
		Seq  uint64 `json:"seq"`
		Line string `json:"line"`
	}{head.Seq, string(line)})
	if err != nil {
		return false, err
	}
	recorded, err := p.cfg.Store.RecordInbox(ctx, p.cfg.CrewID, p.cfg.TourNo, head.Seq, func(tx store.InboxTx) error {
		_, err := tx.AppendLog(store.LogEntry{
			Kind: UndeliverableLogKind, CrewID: p.cfg.CrewID,
			Text:    fmt.Sprintf("undecodable %s seq %d: %v", head.Type, head.Seq, decodeErr),
			Payload: payload,
		})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("record undecodable seq %d: %w", head.Seq, err)
	}
	in.advance(head.Seq)
	return recorded, nil
}

// dispatch handles a newly recorded message inside its inbox transaction.
// Every message is recorded, heartbeats and hello included, so the inbox stays
// contiguous and acks keep moving; only some go anywhere.
func (p *Pump) dispatch(ctx context.Context, tx store.InboxTx, msg Message) error {
	if msg.Crew != p.cfg.CrewID {
		p.log.Error("comms: dropping message for another crew member", "msg_crew", msg.Crew, "type", msg.Type, "seq", msg.Seq)
		return nil
	}
	switch msg.Type {
	case TypeHello:
		var b HelloBody
		_ = json.Unmarshal(msg.Body, &b)
		p.log.Info("comms: combadge hello", "seq", msg.Seq, "protocol", b.Protocol, "harness", b.Harness, "combadge_version", b.CombadgeVersion)
		return p.cfg.Sink.CrewHello(ctx, tx.SQL(), msg)
	case TypeHeartbeat:
		return nil // noted in handleLine
	case TypeProgress:
		// Straight to the log and the order, never into a workflow (spec
		// D3, #141).
		var b ProgressBody
		if err := json.Unmarshal(msg.Body, &b); err != nil {
			p.log.Warn("comms: dropping progress with a bad body", "seq", msg.Seq, "err", err)
			return nil
		}
		payload, err := json.Marshal(struct {
			Seq uint64  `json:"seq"`
			Pct float64 `json:"pct"`
		}{msg.Seq, b.Pct})
		if err != nil {
			return err
		}
		if _, err := tx.SetOrderProgress(msg.Order, b.Pct, b.Note); err != nil {
			return err
		}
		_, err = tx.AppendLog(store.LogEntry{
			TS: msg.TS, Kind: progressLogKind, CrewID: p.cfg.CrewID, OrderID: msg.Order,
			Text: b.Note, Payload: payload,
		})
		return err
	case TypeUsage:
		// Straight to the store, never into a workflow, like progress
		// (#118): the beam's running totals for the order, the highest
		// seq's.
		var b UsageBody
		if err := json.Unmarshal(msg.Body, &b); err != nil {
			p.log.Warn("comms: dropping usage with a bad body", "seq", msg.Seq, "err", err)
			return nil
		}
		if msg.Order == "" {
			p.log.Error("comms: dropping usage without an order", "seq", msg.Seq)
			return nil
		}
		ok, err := tx.RecordUsage(msg.Order, store.Usage{Beam: p.cfg.Beam,
			InputTokens: toInt64(b.InputTokens), OutputTokens: toInt64(b.OutputTokens), Turns: toInt64(b.Turns)})
		if err != nil {
			return err
		}
		p.log.Debug("comms: usage", "seq", msg.Seq, "order", msg.Order, "input_tokens", b.InputTokens,
			"output_tokens", b.OutputTokens, "turns", b.Turns, "recorded", ok)
		if !ok {
			return nil
		}
		return p.cfg.Sink.UsageRecorded(ctx, tx, msg.Order)
	case TypeFinding:
		return p.recordFinding(tx, msg)
	case TypeHail, TypeCheckpointReady, TypeReport:
		if msg.Order == "" {
			p.log.Error("comms: dropping order message without an order", "type", msg.Type, "seq", msg.Seq)
			return nil
		}
		return p.cfg.Sink.ToMission(ctx, tx.SQL(), msg)
	case TypeError:
		var b ErrorBody
		_ = json.Unmarshal(msg.Body, &b)
		if msg.Order == "" {
			// A crew-level error is never sent to a mission.
			p.log.Error("comms: combadge reported a crew-level error", "seq", msg.Seq, "reason", b.Reason, "code", b.Code)
			return p.cfg.Sink.CrewError(ctx, tx.SQL(), msg)
		}
		p.log.Warn("comms: combadge reported an order error", "seq", msg.Seq, "order", msg.Order, "reason", b.Reason, "code", b.Code)
		return p.cfg.Sink.ToMission(ctx, tx.SQL(), msg)
	case TypeConsultReply:
		// No order to route by: the reply names its hail.
		var b ConsultReplyBody
		if err := json.Unmarshal(msg.Body, &b); err != nil || b.HailID == "" {
			p.log.Warn("comms: dropping consult_reply with a bad body", "seq", msg.Seq, "err", err)
			return nil
		}
		return p.cfg.Sink.ToHail(ctx, tx.SQL(), msg, b.HailID)
	default:
		p.log.Warn("comms: skipping message of unknown type", "type", msg.Type, "seq", msg.Seq)
		return nil
	}
}

// recordFinding keeps a finding on its order, straight to the store in the
// inbox transaction like usage (#153); the store logs it there. One the store
// won't keep (an ended order or another crew member's, a key past the
// order's cap, a key the findings workflow already took up), one without
// an order and one with a bad body are dropped with a warning, like a bad
// progress body.
func (p *Pump) recordFinding(tx store.InboxTx, msg Message) error {
	var b FindingBody
	if err := json.Unmarshal(msg.Body, &b); err != nil {
		p.log.Warn("comms: dropping finding with a bad body", "seq", msg.Seq, "order", msg.Order, "err", err)
		return nil
	}
	if err := b.Check(); err != nil {
		p.log.Warn("comms: dropping finding with a bad body", "seq", msg.Seq, "order", msg.Order, "err", err)
		return nil
	}
	if msg.Order == "" {
		p.log.Warn("comms: dropping finding without an order", "seq", msg.Seq, "key", b.Key)
		return nil
	}
	err := tx.RecordFinding(msg.Order, store.Finding{Key: b.Key, Title: b.Title, Body: b.Body,
		Labels: b.Labels, Severity: b.Severity, BlockedBy: b.BlockedBy}, msg.TS)
	if errors.Is(err, store.ErrFindingDropped) {
		p.log.Warn("comms: dropping finding", "seq", msg.Seq, "order", msg.Order, "key", b.Key, "err", err)
		return nil
	}
	return err
}

// toInt64 is n as a store count, capped at math.MaxInt64.
func toInt64(n uint64) int64 { return int64(min(n, math.MaxInt64)) }

// ackIfAdvanced acks the contiguous seq if it moved past the last ack sent on
// this connection. Acks are commands like any other and take a command id;
// serve doesn't dedup them.
func (p *Pump) ackIfAdvanced(c *conn, in *inbound) {
	p.contig.Store(in.contig)
	if in.contig <= in.acked {
		return
	}
	ack, err := NewMessage(TypeAck, AckBody{Seq: in.contig})
	if err != nil {
		panic(err) // AckBody always marshals
	}
	p.wmu.Lock()
	_, err = p.writeLocked(c, ack)
	p.wmu.Unlock()
	if err != nil {
		// The session is going down; the next one acks again.
		p.log.Debug("comms: ack failed", "seq", in.contig, "err", err)
		return
	}
	in.acked = in.contig
}

// noteHeartbeat records a heartbeat's freshness. It takes the earlier of the
// beam's ts and the arrival time: ts so a replayed backlog doesn't look
// fresh, arrival so a beam clock running ahead doesn't hide a stall.
func (p *Pump) noteHeartbeat(ts time.Time) {
	at := time.Now()
	if !ts.IsZero() && ts.Before(at) {
		at = ts
	}
	p.bmu.Lock()
	if at.After(p.lastBeat) {
		p.lastBeat = at
	}
	p.bmu.Unlock()
}

// noteLine records that an inbound line arrived.
func (p *Pump) noteLine() {
	now := time.Now()
	p.bmu.Lock()
	p.lastLineAt = now
	p.bmu.Unlock()
}

// lastLine returns when the newest inbound line arrived, zero if none has.
func (p *Pump) lastLine() time.Time {
	p.bmu.Lock()
	defer p.bmu.Unlock()
	return p.lastLineAt
}

// LastHeartbeat returns the newest heartbeat time seen (see noteHeartbeat),
// or the pump's start time if none has arrived.
func (p *Pump) LastHeartbeat() time.Time {
	p.bmu.Lock()
	defer p.bmu.Unlock()
	return p.lastBeat
}

// watch is the sickbay check: every SickbayCheckEvery, if the newest
// heartbeat is older than HeartbeatStale and the crew member isn't in
// sickbay already, probe the beam. A beam that is gone or unhealthy means
// HeartbeatMissed, sent once, after which watch ends; the pump keeps running
// until the tour decides what to do and stops it. A probe error (health
// unknown) only logs. A healthy beam isn't sickbay but a comms problem: the
// connection may be half-open or attach may have stopped relaying, so the
// pump recycles it and reconnects from the contiguous seq, unless lines are
// still arriving (see checkHeartbeat).
func (p *Pump) watch(ctx context.Context) {
	t := time.NewTicker(p.cfg.SickbayCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if p.checkHeartbeat(ctx) {
				return
			}
		}
	}
}

// checkHeartbeat runs one sickbay check and reports whether it signalled.
//
// Stale heartbeats don't always mean silence. After a long bridge outage the
// beam replays its backlog, and the replayed heartbeats carry their old ts
// (freshness is judged by ts, see noteHeartbeat), so they stay stale until
// the replay catches up, which can take longer than HeartbeatStale. Lines
// arriving prove the connection is neither stuck nor attached to a dead beam
// (serve dying closes the socket, attach exits and the session ends), so:
//
//   - The probe is skipped while the live connection is delivering: it has
//     been live for at least HeartbeatStale and its newest line is younger
//     than HeartbeatStale. The connection age matters for a crash-looping
//     serve: each restart accepts a new attach and emits a hello, so lines
//     keep arriving across connections, but no connection lives HeartbeatStale
//     (serve's 10s heartbeat interval is well under HeartbeatStale), so the probe still
//     runs and tsh's Probe still reports the crash loop (#30). A dead beam
//     delivers no lines at all and is probed as before.
//   - A healthy probe recycles the connection only if no line at all has
//     arrived for HeartbeatStale (on any connection, checked after the
//     probe, which can take a while). This covers the first HeartbeatStale of
//     a replaying connection, where the probe isn't skipped yet. Recycling
//     there would restart the replay from the contiguous seq every check and
//     the backlog might never drain (#32).
func (p *Pump) checkHeartbeat(ctx context.Context) bool {
	age := time.Since(p.LastHeartbeat())
	if age <= p.cfg.HeartbeatStale {
		return false
	}
	if p.delivering() {
		p.log.Debug("comms: heartbeats stale but lines still arriving; not probing", "age", age.Round(time.Second))
		return false
	}
	c, err := p.cfg.Store.GetCrew(ctx, p.cfg.CrewID)
	if err == nil && c.Status == store.CrewSickbay {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, p.cfg.ProbeTimeout)
	healthy, err := p.cfg.Link.Probe(pctx)
	cancel()
	switch {
	case ctx.Err() != nil:
		return false
	case err != nil:
		p.log.Warn("comms: heartbeats stale and beam health unknown; not declaring sickbay", "age", age.Round(time.Second), "err", err)
		return false
	case healthy:
		if quiet := time.Since(p.lastLine()); quiet < p.cfg.HeartbeatStale {
			p.log.Debug("comms: heartbeats stale but beam answers and lines still arriving; keeping the connection",
				"age", age.Round(time.Second), "quiet", quiet.Round(time.Millisecond))
			return false
		}
		recycled := p.recycle()
		p.log.Warn("comms: heartbeats stale but beam answers; recycling the connection", "age", age.Round(time.Second), "recycled", recycled)
		return false
	}
	if err := p.cfg.Sink.HeartbeatMissed(ctx); err != nil {
		if ctx.Err() == nil {
			p.log.Error("comms: signal heartbeat_missed failed; retrying at the next check", "err", err)
		}
		return false
	}
	p.log.Warn("comms: heartbeat missed and beam is gone; signalled the tour", "age", age.Round(time.Second))
	return true
}

// delivering reports whether the live connection has been up for at least
// HeartbeatStale and its newest line is younger than HeartbeatStale (see
// checkHeartbeat).
func (p *Pump) delivering() bool {
	c := p.conn.Load()
	if c == nil {
		return false
	}
	now := time.Now()
	return now.Sub(c.liveAt) >= p.cfg.HeartbeatStale && now.Sub(p.lastLine()) < p.cfg.HeartbeatStale
}

// sleepCtx waits d or until ctx is done, reporting whether it waited d.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func truncate(b []byte) string {
	const n = 200
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
