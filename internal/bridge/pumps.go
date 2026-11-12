package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// The pump registry: one comms.Pump per live crew member (plan §6, #14).
//
// CrewTour calls EnsurePump right after its provisioning step, on every run
// of the workflow (#15). Run also re-creates the pumps itself (ensurePumps)
// from the crew table, before resumed workflows need them. crew.current_beam
// is only set once serve is up in the beam, so neither starts a pump on a
// beam that is still being provisioned. Workflow steps reach a pump with
// SendCommand.
//
// Pumps run on the daemon's pump context, not the step that started them:
// they outlive steps and stop when the daemon shuts down (before DBOS and
// the store close) or when StopPump is called.

// ErrDaemonStopping is returned by EnsurePump once the daemon is shutting
// down.
var ErrDaemonStopping = errors.New("bridge: daemon is shutting down")

// pumpEntry is a registered pump and what it was started for.
type pumpEntry struct {
	pump   *comms.Pump
	beam   string
	tourNo int
}

// EnsurePump returns crewID's pump, starting one attached to beam if there is
// none. It is idempotent: a pump already running for the same beam and tour
// is returned as is. A pump for a different beam or tour is replaced (a new
// tour or beam means the old session is stale).
//
// tourNo names the tour that HeartbeatMissed and crew errors are sent to
// (crew/<crewID>/tour/<tourNo>), and the tour the pump's inbox rows are
// keyed under.
//
// The replaced pump is stopped after pumpsMu is released, since Stop waits
// for its goroutines (a Sink call in flight, a session ending). Until it has
// stopped, two pumps may briefly run for the crew member. That is harmless:
// RecordInbox records and forwards each seq once whichever pump gets it
// first (INSERT OR IGNORE under an immediate transaction), heartbeat_missed is
// sent with a per-tour idempotency key, only the registered pump takes Send
// calls, and the old pump's acks never collide with new command ids because
// serve doesn't dedup acks. StopPump has the same window. The inbox dedups
// within a tour only, which is enough: a new tour always gets a new beam
// (spec/phase-2-plan-2026-10-19.md section 8, StopPump before next-tour), so
// pumps of different tours never read the same outbox.
func (d *Daemon) EnsurePump(crewID string, beam transporter.Beam, tourNo int) (*comms.Pump, error) {
	var old *comms.Pump
	defer func() {
		if old != nil {
			old.Stop()
		}
	}()
	d.pumpsMu.Lock()
	defer d.pumpsMu.Unlock()
	if d.pumpsClosed {
		return nil, ErrDaemonStopping
	}
	if e, ok := d.pumps[crewID]; ok {
		if e.beam == beam.Alias && e.tourNo == tourNo {
			return e.pump, nil
		}
		d.logger.Info("replacing comms pump", "crew", crewID, "old_beam", e.beam, "old_tour", e.tourNo, "beam", beam.Alias, "tour", tourNo)
		old = e.pump
		delete(d.pumps, crewID)
	}
	cfg := comms.PumpConfig{
		CrewID: crewID,
		TourNo: tourNo,
		Beam:   beam.Alias,
		Link:   beamLink{p: d.transporter, beam: beam},
		Store:  d.store,
		Sink:   pumpSink{d: d, crewID: crewID, tourNo: tourNo, beam: beam.Alias},
		Logger: d.logger,
	}
	if d.tunePump != nil {
		d.tunePump(&cfg)
	}
	p, err := comms.StartPump(d.pumpCtx, cfg)
	if err != nil {
		return nil, err
	}
	d.pumps[crewID] = pumpEntry{pump: p, beam: beam.Alias, tourNo: tourNo}
	d.logger.Info("comms pump started", "crew", crewID, "beam", beam.Alias, "tour", tourNo)
	return p, nil
}

// StopPump stops and forgets crewID's pump, if any (dismissal, sickbay, beam
// removal). It waits for the pump to stop.
func (d *Daemon) StopPump(crewID string) {
	d.pumpsMu.Lock()
	e, ok := d.pumps[crewID]
	delete(d.pumps, crewID)
	d.pumpsMu.Unlock()
	if ok {
		e.pump.Stop()
		d.logger.Info("comms pump stopped", "crew", crewID, "beam", e.beam)
	}
}

// markTourEnded records that tour tourID has ended (endTour, tourSickbay),
// closing tourEnded(tourID). The mark is never removed, so a mission that
// looks later, after a step retry say, still sees it. It lives in memory: a
// restarted daemon's replayed endTour marks the tour again.
func (d *Daemon) markTourEnded(tourID string) {
	d.tourEndsMu.Lock()
	defer d.tourEndsMu.Unlock()
	ch, ok := d.tourEnds[tourID]
	if !ok {
		ch = make(chan struct{})
		d.tourEnds[tourID] = ch
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// tourEnded returns a channel closed once tourID is marked ended, already
// closed if it is. An empty tourID (a mission run outside a tour) never
// ends.
func (d *Daemon) tourEnded(tourID string) <-chan struct{} {
	if tourID == "" {
		return nil
	}
	d.tourEndsMu.Lock()
	defer d.tourEndsMu.Unlock()
	ch, ok := d.tourEnds[tourID]
	if !ok {
		ch = make(chan struct{})
		d.tourEnds[tourID] = ch
	}
	return ch
}

// Pump returns crewID's running pump.
func (d *Daemon) Pump(crewID string) (*comms.Pump, bool) {
	d.pumpsMu.Lock()
	defer d.pumpsMu.Unlock()
	e, ok := d.pumps[crewID]
	return e.pump, ok
}

// SendCommand sends a command to crewID's beam through its pump and returns
// the command id. With no pump registered, or no live session, it returns an
// error wrapping comms.ErrNotConnected. Call it only from a workflow step, and
// give that step retries (see comms.Pump.Send and the step-retry note in
// spec/phase-1-decisions).
func (d *Daemon) SendCommand(crewID string, msg comms.Message) (uint64, error) {
	p, ok := d.Pump(crewID)
	if !ok {
		return 0, fmt.Errorf("%w: no comms pump for %s", comms.ErrNotConnected, crewID)
	}
	return p.Send(msg)
}

// closePumps stops every pump and refuses new ones. Run calls it before
// shutting DBOS down, since pumps send to DBOS and write to the store.
func (d *Daemon) closePumps() {
	d.pumpsMu.Lock()
	d.pumpsClosed = true
	entries := d.pumps
	d.pumps = map[string]pumpEntry{}
	d.pumpsMu.Unlock()
	d.cancelPumps()
	for _, e := range entries {
		e.pump.Stop()
	}
}

// pumpCrewStatuses are the crew statuses whose beam should have a pump:
// the crew member has a live beam and tour. sickbay and dismissed don't.
var pumpCrewStatuses = map[string]bool{
	store.CrewOnDeck:  true,
	store.CrewAway:    true,
	store.CrewHailing: true,
}

// ensurePumps starts a pump for every crew member that is on_deck, away or
// hailing with a current beam, for tour crew.tour_no. It recreates the pumps
// a previous daemon had, before the resumed workflows need them. A crew
// member that is on_deck with no current beam is still being provisioned
// (the beam is only recorded in the beams table until serve is up); its tour
// starts the pump.
func (d *Daemon) ensurePumps(ctx context.Context) error {
	crew, err := d.store.ListCrew(ctx)
	if err != nil {
		return fmt.Errorf("list crew: %w", err)
	}
	for _, c := range crew {
		if !pumpCrewStatuses[c.Status] || c.CurrentBeam == "" {
			continue
		}
		if _, err := d.EnsurePump(c.ID, transporter.Beam{Alias: c.CurrentBeam}, c.TourNo); err != nil {
			return fmt.Errorf("start comms pump for %s: %w", c.ID, err)
		}
	}
	return nil
}

// beamLink is a comms.Link to one beam through the daemon's transporter.
type beamLink struct {
	p    transporter.Provider
	beam transporter.Beam
}

func (l beamLink) Attach(ctx context.Context, since uint64) (comms.Session, error) {
	s, err := l.p.AttachCombadge(ctx, l.beam, since)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (l beamLink) Probe(ctx context.Context) (bool, error) { return l.p.Probe(ctx, l.beam) }

// pumpSink is the comms.Sink of one crew member's pump: it hands messages to
// workflows with dbos.Send.
//
// Crash safety: messages tied to an inbound seq are sent with
// dbos.WithSendTransaction on the pump's inbox transaction. DBOS's
// notifications table lives in bridge.db with the domain tables, DBOS's SQLite
// system database uses the same modernc driver with foreign keys on, and a
// send outside a workflow accepts a caller's *sql.Tx, so the notification row
// and the inbox row commit or roll back together: a crash in between is
// impossible. On SQLite, Recv is woken by DBOS's notification poller, which
// picks up rows written by any connection. TestPumpSinkSendIsAtomicWithInbox
// checks both outcomes.
type pumpSink struct {
	d      *Daemon
	crewID string
	tourNo int
	beam   string // the beam the pump is attached to, named in heartbeat_missed
}

func (s pumpSink) ToMission(ctx context.Context, tx *sql.Tx, msg comms.Message) error {
	// A mission that hasn't started yet may be starting: #15/#16 start the
	// mission before the order command reaches the beam, so the pump retries
	// it for a while. One that has finished (failMission, a belay or
	// withdraw, settle) never reads the message again (#160).
	return sendLive(s.d, tx, missionWorkflowID(msg.Order), msg, fromCrewTopic)
}

func (s pumpSink) ToHail(ctx context.Context, tx *sql.Tx, msg comms.Message, hailID string) error {
	// Only Command answers consults: a reply from anyone else would use
	// up the workflow's one read of consultReplyTopic (#150). It is final,
	// so it is dropped at once, its inbox row kept.
	switch division, err := store.CrewDivision(ctx, tx, s.crewID); {
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("look up crew %s: %w", s.crewID, err)
	case division != DivisionCommand:
		s.d.logger.Warn("comms: dropping consult_reply from outside the command division", "crew", s.crewID,
			"division", division, "hail", hailID, "seq", msg.Seq)
		return nil
	}
	return sendLive(s.d, tx, hailWorkflowID(hailID), msg, consultReplyTopic)
}

// CrewError sends the tour a crew_error, in the inbox transaction. A tour
// that has ended can't act on it, so the error is logged undeliverable
// rather than lost.
func (s pumpSink) CrewError(ctx context.Context, tx *sql.Tx, msg comms.Message) error {
	var body comms.ErrorBody
	_ = json.Unmarshal(msg.Body, &body)
	return sendLive(s.d, tx, tourWorkflowID(s.crewID, s.tourNo),
		CrewSignal{Kind: SignalCrewError, Reason: body.Reason, Code: body.Code, Beam: s.beam}, crewEventsTopic)
}

// CrewHello sends the tour a crew_hello naming the pump's beam, in the inbox
// transaction like CrewError. Only the tour's hello wait acts on it (#173).
// A hello to a tour that has ended is dropped without a log entry: serve
// says hello on every attach, and it carries nothing to act on.
func (s pumpSink) CrewHello(ctx context.Context, tx *sql.Tx, msg comms.Message) error {
	err := sendLive(s.d, tx, tourWorkflowID(s.crewID, s.tourNo),
		CrewSignal{Kind: SignalCrewHello, Beam: s.beam}, crewEventsTopic)
	if errors.Is(err, comms.ErrGone) {
		return nil
	}
	return err
}

func (s pumpSink) HeartbeatMissed(ctx context.Context) error {
	// The idempotency key makes this at most once per tour and beam, even
	// from a new pump after a bridge restart. The beam is in the key and the
	// signal so a pump on a stale beam can neither put the crew member in
	// sickbay (the tour ignores signals for a beam that isn't its own) nor
	// use up the signal of the tour's real beam.
	//
	// Without a caller transaction DBOS retries a failing send (a busy
	// database, say) on its own context, so the send gets a DBOS context
	// cancelled with ctx, the pump's lifetime: Stop never waits on it.
	dctx, cancel := dbos.WithCancel(s.d.dbos)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	tour := tourWorkflowID(s.crewID, s.tourNo)
	return dbos.Send(dctx, tour, CrewSignal{Kind: SignalHeartbeatMissed, Beam: s.beam}, crewEventsTopic,
		dbos.WithIdempotencyKey(tour+"/"+s.beam+"/"+SignalHeartbeatMissed))
}

// sendLive sends v to workflow id on topic in tx, the pump's inbox
// transaction, if id is live (#160). A missing workflow is
// comms.ErrUndeliverable (it may be starting) and a finished one comms.ErrGone:
// DBOS accepts a send to a finished workflow, and nothing would read it. The
// check runs while tx holds SQLite's write lock (the store's
// _txlock=immediate), so the workflow can neither start nor finish between
// the check and the commit. PENDING, ENQUEUED and DELAYED are live
// (workflowLive).
func sendLive(d *Daemon, tx *sql.Tx, id string, v any, topic string) error {
	live, found, err := workflowLive(d, id)
	switch {
	case err != nil:
		return fmt.Errorf("look up %s: %w", id, err)
	case !found:
		return fmt.Errorf("%w: %s", comms.ErrUndeliverable, id)
	case !live:
		return fmt.Errorf("%w: %s", comms.ErrGone, id)
	}
	return undeliverable(dbos.Send(d.dbos, id, v, topic, dbos.WithSendTransaction(tx)))
}

// undeliverable marks a send to a workflow that doesn't exist with
// comms.ErrUndeliverable, keeping DBOS's error as the cause.
func undeliverable(err error) error {
	var de *dbos.Error
	if errors.As(err, &de) && de.Code == dbos.ErrorCodeNonExistentWorkflow {
		return fmt.Errorf("%w: %w", comms.ErrUndeliverable, err)
	}
	return err
}

// missionWorkflowID is the workflow id of an order's mission (spec §5).
func missionWorkflowID(orderID string) string { return "order/" + orderID }
