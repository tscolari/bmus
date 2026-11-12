package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/transporter"
)

// crewEventsTopic is the DBOS topic a crew tour receives CrewSignals on.
const crewEventsTopic = "crew-events"

// CrewSignal kinds.
const (
	SignalOrderAssigned   = "order_assigned"
	SignalHeartbeatMissed = "heartbeat_missed"
	SignalDismissed       = "dismissed"
	// SignalCrewError carries an error message the combadge sent without an
	// order: a crew-level failure, never routed to a mission. The comms pump
	// sends it with Reason and Code from the error body. The tour logs it;
	// one with code harness_check_failed then ends the tour (#172).
	SignalCrewError = "crew_error"
	// SignalCrewHello is sent by the comms pump, with Beam, for every hello
	// combadge sends: serve sends one on each attach. The tour claims its
	// first order only after its beam's (awaitHello, #173); later ones are
	// ignored.
	SignalCrewHello = "crew_hello"
	// SignalMissionDone is sent by a mission on every exit, with OrderID,
	// once the mission has settled its order (done or failed), to the
	// current tour of the crew member it last ran on.
	SignalMissionDone = "mission_done"
	// SignalMissionParked is sent by a mission, with OrderID, once it has
	// parked (its beam went away: watch change, beam loss; or the captain
	// recalled the order, #101), to the current tour of its crew member.
	// The mission lives on, waiting for a resume. A mission that drops a
	// resume because a recall moved the order first sends it to the
	// resuming tour too.
	SignalMissionParked = "mission_parked"
)

// Error codes the bridge itself puts on an order's failure (log payload and
// MissionResult.Code), next to the combadge's own (busy,
// combadge_restarted, ...).
const (
	// CodeBeamLost: the tour got heartbeat_missed while the mission ran,
	// or a watch change got no checkpoint in time. The tour sends the
	// mission a synthetic error with this code, and the mission parks.
	CodeBeamLost = "beam_lost"
	// CodeSendFailed: a command (the order, a hail's answer) couldn't be
	// written to the beam within the step's retries.
	CodeSendFailed = "send_failed"
	// CodeTimeout: no message from the crew (or no hail answer) for
	// missionWaitTimeout.
	CodeTimeout = "timeout"
	// CodeUnsettled: the tour's backstop failed an order its mission left
	// away.
	CodeUnsettled = "unsettled"
	// CodeBridgeError: a bridge-side step failed for good (e.g. the store).
	CodeBridgeError = "bridge_error"
	// CodeDismissed: the tour was dismissed while the mission ran.
	CodeDismissed = "dismissed"
	// CodeCheckpointFailed: a checkpoint couldn't be pulled into the cargo
	// bay (logged; the mission goes on).
	CodeCheckpointFailed = "checkpoint_failed"
	// CodeProvisionFailed and CodePumpFailed: why a tour put its crew
	// member in sickbay before any mission.
	CodeProvisionFailed = "provision_failed"
	CodePumpFailed      = "pump_failed"
	// CodeNoHello: the beam's combadge didn't say hello within
	// helloTimeout (#173).
	CodeNoHello = "no_hello"
)

// CodeHarnessCheckFailed is the code of the crew-level error combadge
// serve sends when its startup check fails (#172): a PreToolUse ask didn't
// reach canUseTool, so no hail would reach the captain. serve then exits.
// The tour ends in sickbay with the error's reason (harnessCheckFailed).
const CodeHarnessCheckFailed = "harness_check_failed"

// Captain's log kinds (spec §7: log.kind).
const (
	logBeam   = "beam"
	logCrew   = "crew"
	logOrder  = "order"
	logHail   = "hail"
	logDone   = "done"
	logFail   = "fail"
	logBridge = "bridge" // bridge-wide settings, e.g. standing orders
)

// CrewTourInput is the input of a crew tour workflow, id crew/<CrewID>/tour/<TourNo>.
type CrewTourInput struct {
	CrewID string
	TourNo int
}

// CrewTourResult is how a crew tour ended.
type CrewTourResult struct {
	// Ended is why the tour ended: "dismissed", "sickbay" or
	// "watch_change" (TourEndedWatchChange).
	Ended string
}

// TourEndedWatchChange is CrewTourResult.Ended for a tour that handed over
// to the crew member's next tour before its beam's purge.
const TourEndedWatchChange = "watch_change"

// CrewSignal is sent to a crew tour on crewEventsTopic: by CreateOrder,
// RouteOrder and wakeDivision (order_assigned), DismissCrew (dismissed), the comms pump
// (heartbeat_missed from its sickbay check, crew_error, crew_hello) and missions
// (mission_done, mission_parked).
type CrewSignal struct {
	Kind    string
	OrderID string `json:",omitempty"`
	// Reason and Code are a crew_error's ErrorBody.
	Reason string `json:",omitempty"`
	Code   string `json:",omitempty"`
	// Beam is the beam a heartbeat_missed, crew_hello or crew_error is
	// about: the beam of the pump that sent it. The tour ignores a
	// heartbeat_missed or harness_check_failed for a beam that isn't its own.
	Beam string `json:",omitempty"`
	// TourID, on mission_parked, is the tour the mission ran under when it
	// parked. A tour ignores one naming another tour: a late park from
	// before this tour resumed the order.
	TourID string `json:",omitempty"`
	// StaleBeam, on mission_done or mission_parked: a command to the beam
	// failed to send, so the beam may still be running the order. A tour
	// that isn't ending anyway ends in sickbay, as on beam loss.
	StaleBeam bool `json:",omitempty"`
}

const (
	// defaultMissionCheckEvery bounds each wait for a crew signal. On
	// timeout a busy tour checks the mission is still alive, so one that
	// ended without its mission_done (a panic, out of recovery attempts, a
	// failed final step) is settled within this, not after a day; an idle
	// tour claims again, so a pool order whose wake-up it missed waits no
	// longer than this (spec phase-2-plan-amendment section 2).
	// Config.missionCheckEvery overrides it in tests.
	defaultMissionCheckEvery = 10 * time.Minute

	// missionSettleTimeout bounds how long a tour ending (sickbay or
	// dismissal) waits for its running mission's mission_done or
	// mission_parked after sending it a synthetic error. It covers a
	// mission stuck in a send step's retries (sendRetries, or
	// sendOrderStep's own loop on the same schedule: under 3 minutes). After it the tour's backstop fails the order.
	// Config.settleTimeout overrides it in tests.
	missionSettleTimeout = 5 * time.Minute

	// defaultWatchChangeLead is how long before its beam's purge
	// (ExpiresAt) a tour hands over to the next (D5).
	// Config.watchChangeLead overrides it in tests.
	defaultWatchChangeLead = time.Hour

	// standDownTimeout bounds a watch change's wait for the running
	// mission to park (its stopping checkpoint) or finish. Past it the
	// tour sends the mission beam_lost, which parks it on the checkpoints
	// it has.
	// Config.standDownTimeout overrides it in tests.
	standDownTimeout = 30 * time.Minute

	// defaultHelloTimeout bounds a tour's wait for its beam's hello
	// (awaitHello). A serve that dies first is caught sooner by the pump's
	// sickbay check (heartbeat_missed); this catches one that runs but never
	// lets an attach through. Config.helloTimeout overrides it in tests.
	defaultHelloTimeout = 10 * time.Minute
)

// Step retry policies. A recorded step error is final, so steps whose
// failure is usually transient retry first. Workflow code reacts to a step
// error only by err != nil (never errors.Is on it) after checking ctx.Err():
// a cancelled workflow context means DBOS is shutting down, the step's
// outcome was not recorded, and the workflow must return at once and stay
// PENDING.
var (
	// sendRetries: steps sending a command through the comms pump, which
	// returns comms.ErrNotConnected while it (re)connects. About 2m45s in
	// all.
	sendRetries = []dbos.StepOption{dbos.WithStepMaxRetries(15),
		dbos.WithStepBaseInterval(500 * time.Millisecond), dbos.WithStepMaxInterval(15 * time.Second)}
	// provisionRetries: a provision attempt (checking the recorded beam,
	// starting combadge). About 10m. A failed beam creation is retried by
	// transporterWorkflow instead.
	provisionRetries = []dbos.StepOption{dbos.WithStepMaxRetries(20),
		dbos.WithStepBaseInterval(time.Second), dbos.WithStepMaxInterval(time.Minute)}
	// storeRetries: store-only steps (a busy database, mostly).
	storeRetries = []dbos.StepOption{dbos.WithStepMaxRetries(5),
		dbos.WithStepBaseInterval(200 * time.Millisecond), dbos.WithStepMaxInterval(5 * time.Second)}
	// copyRetries: pulling checkpoint files out of a beam.
	copyRetries = []dbos.StepOption{dbos.WithStepMaxRetries(5),
		dbos.WithStepBaseInterval(time.Second), dbos.WithStepMaxInterval(15 * time.Second)}
)

// storeStep runs fn as a durable step named name, with storeRetries.
func storeStep(ctx dbos.Context, name string, fn func(context.Context, *Daemon) error) error {
	_, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
		return struct{}{}, fn(c, daemonFrom(c))
	}, append([]dbos.StepOption{dbos.WithStepName(name)}, storeRetries...)...)
	return err
}

// crewTourWorkflow runs one tour of a crew member (spec phase-1-plan §7,
// corrected in phase-1-decisions: the tour never blocks on a mission's
// result, so heartbeat_missed is seen mid-mission; watch change from
// spec/phase-2-plan-2026-10-19.md section 8). A tour lives as long as its
// beam: it ends in sickbay, dismissed, or handing over to the crew member's
// next tour shortly before the beam's purge.
//
// Steps:
//  1. Run the transporter workflow beam/<crew>/<tour> on the transporter
//     queue and wait for it: it removes an earlier tour's beam, gives the
//     crew member a beam with combadge serve running and sets it
//     beaming_in with that beam (see transporterWorkflow). A dismissed crew member
//     ends the tour dismissed (remove-beam removes a beam made meanwhile);
//     a failure sends it to sickbay. Then start-pump records whether the
//     comms pump started (see startPump): a failure sends the crew member
//     to sickbay. EnsurePump is then called again outside any step, on
//     every run (replays included).
//  2. awaitHello: wait for the beam's combadge to say hello before the
//     first claim (#173). The crew member is beaming_in with its
//     current_beam since provisioning (since crew add, a re-energize or a
//     watch change without one), and its hello-logged step sets it on_deck
//     (#106). The tour is the only caller of claimOrder, and the store
//     claims nothing for a crew member beaming in.
//  3. Loop. While idle: claim-order (the order already away, else the
//     highest-priority, oldest directed order, else, with standing orders
//     on, the same from the pool of the crew member's division -> away,
//     crew away; nothing, and a watch change, once the watch is due); if
//     found, start the mission order/<id> as a child workflow, or, when
//     its mission is parked, send it resume with this crew member and
//     tour. Then Recv a CrewSignal (missionCheckEvery, capped at the watch
//     change, watchAt: the beam's ExpiresAt less watchChangeLead):
//     - order_assigned (CreateOrder, wakeDivision): loop (claims if idle;
//     a busy tour claims after the mission).
//     - mission_done for the running order: settle-mission (backstop: close
//     a hail left open, fail the order if still away; crew on_deck), then
//     loop. Others are stale and ignored.
//     - mission_parked for the running order (a recall took the order from
//     this crew member, or reached its parked mission before this tour's
//     resume, #101): mission-parked sets the crew member on deck, with no
//     backstop: the mission lives on. Loop.
//     Either of these with StaleBeam ends the tour in sickbay.
//     - heartbeat_missed for the tour's beam: end the tour in sickbay (see
//     endTour); the mission parks. One naming another beam is ignored.
//     - dismissed: end the tour (see endTour), setting the crew member
//     dismissed, then remove-beam.
//     - crew_error: log it; one with code harness_check_failed ends the
//     tour as beam loss does, in sickbay with its reason (#172).
//     - crew_hello: ignored (serve says hello on every attach).
//     - timeout: watch-due records whether watchAt has passed; if it has,
//     change the watch (see watchChange). Else, if a mission runs,
//     mission-alive checks it hasn't ended without telling the tour; if it
//     has, settle it. Loop (an idle tour claims again, catching a missed
//     wake-up).
func crewTourWorkflow(ctx dbos.Context, in CrewTourInput) (CrewTourResult, error) {
	tourID := tourWorkflowID(in.CrewID, in.TourNo)
	d := daemonFrom(ctx)
	// The child's start and its result are both recorded, so a replay
	// gets the same beam (or failure) without running it again.
	h, err := dbos.RunWorkflow(ctx, transporterWorkflow, TransporterInput{CrewID: in.CrewID, TourNo: in.TourNo},
		dbos.WithWorkflowID(beamWorkflowID(in.CrewID, in.TourNo)), dbos.WithQueue(d.transporterQueue))
	if err != nil {
		return CrewTourResult{}, err
	}
	provisioned, recorded, err := awaitBeam(ctx, d, tourID, h)
	if err != nil {
		if !recorded {
			if ctx.Err() == nil {
				// awaitBeam gave up on an unrecorded failure: the tour
				// ends in ERROR, and an ERROR tour is never replayed. Not
				// a step, then: sickbay lets re-energize start a new tour
				// rather than leave the crew member on deck with none
				// (#110).
				err = errors.Join(err, awaitFailed(ctx, d, in, err))
			}
			// Otherwise DBOS is shutting down: the tour stays PENDING.
			return CrewTourResult{}, err
		}
		// The transporter workflow's own error (or its cancellation),
		// recorded in the tour.
		return tourSickbay(ctx, in, transporter.Beam{}, "provisioning failed: "+err.Error(), CodeProvisionFailed)
	}
	if provisioned.Dismissed {
		// Dismissed before the tour got going: nothing to undo but a beam
		// made before the dismissal landed.
		d.markTourEnded(tourID)
		if provisioned.Beam.Alias != "" {
			if err := removeTourBeam(ctx, in, provisioned.Beam); err != nil {
				return CrewTourResult{}, err
			}
		}
		return CrewTourResult{Ended: store.CrewDismissed}, nil
	}
	if provisioned.Failed != "" {
		return tourSickbay(ctx, in, transporter.Beam{}, "provisioning failed: "+provisioned.Failed, CodeProvisionFailed)
	}
	beam := provisioned.Beam
	// From the recorded beam, so a replay computes the same time. A beam
	// with no known expiry never changes the watch.
	var watchAt time.Time
	if !beam.ExpiresAt.IsZero() {
		watchAt = beam.ExpiresAt.Add(-d.watchChangeLead)
	}

	// The first run's outcome is recorded, and the tour branches only on
	// that: a replay takes the same path whatever EnsurePump does now.
	started, err := dbos.RunAsStep(ctx, func(c context.Context) (pumpStart, error) {
		return startPump(c, daemonFrom(c), in, beam)
	}, dbos.WithStepName("start-pump"))
	if err != nil {
		// DBOS shutting down (nothing was recorded) or failing.
		return CrewTourResult{}, err
	}
	if started.Failed != "" {
		return tourSickbay(ctx, in, beam, "no comms pump: "+started.Failed, CodePumpFailed)
	}
	// Not a step: the pump is process-local, so every run of the workflow
	// (a replay after a bridge restart included) registers it again. On the
	// first run the pump is already there and this is a no-op.
	if err := rearmPump(ctx, d, in, beam); err != nil {
		return CrewTourResult{}, err
	}
	if res, ended, err := awaitHello(ctx, in, beam); err != nil || ended {
		return res, err
	}

	var running string // order id of the running mission, "" when idle
	for {
		if running == "" {
			claim, err := dbos.RunAsStep(ctx, func(c context.Context) (orderClaim, error) {
				return claimOrder(c, daemonFrom(c), in.CrewID, watchAt)
			}, append([]dbos.StepOption{dbos.WithStepName("claim-order")}, storeRetries...)...)
			if err != nil {
				return CrewTourResult{}, err
			}
			switch {
			case claim.WatchDue:
				// Don't take an order only to stand it down.
				return watchChange(ctx, in, beam, "")
			case claim.Found && claim.Parked:
				// The order's mission is parked (watch change, beam loss):
				// hand it this crew member and tour. The pump is up.
				body, err := json.Marshal(resumeBody{CrewID: in.CrewID, TourID: tourID})
				if err != nil {
					return CrewTourResult{}, err
				}
				msg := comms.Message{Type: typeResume, Crew: in.CrewID, Order: claim.OrderID, Body: body}
				if err := dbos.Send(ctx, missionWorkflowID(claim.OrderID), msg, fromCrewTopic); err != nil {
					return CrewTourResult{}, err
				}
				running = claim.OrderID
			case claim.Found:
				// The mission exists before anything can be sent to it: its
				// first step sends the order command.
				if _, err := dbos.RunWorkflow(ctx, missionWorkflow,
					MissionInput{OrderID: claim.OrderID, CrewID: in.CrewID, TourID: tourID},
					dbos.WithWorkflowID(missionWorkflowID(claim.OrderID))); err != nil {
					return CrewTourResult{}, err
				}
				running = claim.OrderID
			}
		}

		// Idle or busy, the tour re-checks every missionCheckEvery: an idle
		// one claims again, so a wake-up it missed (wakeDivision is best
		// effort) costs minutes, not a day.
		wait := d.missionCheckEvery
		if !watchAt.IsZero() {
			// Only the wait: whether the watch changes is decided by
			// watch-due's recorded result.
			wait = min(wait, max(time.Until(watchAt), time.Millisecond))
		}
		sig, err := dbos.Recv[CrewSignal](ctx, crewEventsTopic, wait)
		if errors.Is(err, dbos.ErrTimeout) {
			if !watchAt.IsZero() {
				due, err := dbos.RunAsStep(ctx, func(context.Context) (bool, error) {
					return !time.Now().Before(watchAt), nil
				}, dbos.WithStepName("watch-due"))
				if err != nil {
					return CrewTourResult{}, err
				}
				if due {
					return watchChange(ctx, in, beam, running)
				}
			}
			if running != "" {
				alive, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
					return missionAlive(daemonFrom(c), running)
				}, append([]dbos.StepOption{dbos.WithStepName("mission-alive")}, storeRetries...)...)
				if err != nil {
					return CrewTourResult{}, err
				}
				if !alive {
					d.logger.Warn("crew tour: mission ended without mission_done; settling it", "tour", tourID, "order", running)
					if err := settleMission(ctx, in.CrewID, running); err != nil {
						return CrewTourResult{}, err
					}
					running = ""
				}
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil || !isRecvDecodeError(err) {
				// DBOS shutting down (nothing was recorded; the tour stays
				// PENDING for the next Launch) or failing.
				return CrewTourResult{}, err
			}
			// A signal that doesn't decode. Its Recv is recorded, so a
			// replay decodes it and skips it the same way.
			d.logger.Warn("crew tour: ignoring undecodable crew signal", "tour", tourID, "err", err)
			continue
		}

		switch sig.Kind {
		case SignalOrderAssigned:
			// Idle: loop and claim. Busy: the claim after the mission
			// picks the order up.
		case SignalMissionDone, SignalMissionParked:
			if !missionSignalFor(sig, running, tourID) {
				d.logger.Info("crew tour: ignoring stale "+sig.Kind, "tour", tourID, "order", sig.OrderID, "running", running)
				continue
			}
			if sig.Kind == SignalMissionDone {
				err = settleMission(ctx, in.CrewID, running)
			} else {
				// The mission is alive and no longer this tour's: no
				// backstop.
				err = storeStep(ctx, "mission-parked", func(c context.Context, d *Daemon) error {
					return d.store.UpdateCrewStatus(c, in.CrewID, store.CrewOnDeck,
						store.LogEntry{Kind: logCrew, CrewID: in.CrewID, OrderID: running, Text: "crew back on deck"})
				})
			}
			if err != nil {
				return CrewTourResult{}, err
			}
			order := running
			running = ""
			if sig.StaleBeam {
				reason := "the beam may still be running order " + order
				return endTour(ctx, in, beam, "", nil, func(c context.Context, d *Daemon) error {
					return d.toSickbay(c, in.CrewID, beam.Alias, reason, CodeSendFailed)
				}, store.CrewSickbay)
			}
		case SignalHeartbeatMissed:
			if sig.Beam != beam.Alias {
				d.logger.Warn("crew tour: ignoring heartbeat_missed for another beam", "tour", tourID, "beam", sig.Beam, "tour_beam", beam.Alias)
				continue
			}
			return beamLost(ctx, in, beam, running, "beam lost")
		case SignalDismissed:
			return dismissTour(ctx, in, beam, running)
		case SignalCrewError:
			if err := logCrewError(ctx, in, sig); err != nil {
				return CrewTourResult{}, err
			}
			if checkFailedOn(d, tourID, sig, beam) {
				return harnessCheckFailed(ctx, in, beam, running, sig)
			}
		case SignalCrewHello:
			// A reconnect's hello: the wait for the first is over.
		default:
			d.logger.Warn("crew tour: ignoring unknown crew signal", "tour", tourID, "kind", sig.Kind)
		}
	}
}

// awaitHello is the tour's wait for its beam's hello, between start-pump and
// the first claim-order (#173): an order goes only to a combadge that is up.
// The hello-deadline step records when the wait gives up (helloTimeout), so
// a resumed tour waits only for what is left of it. While it waits:
//
//   - crew_hello for the tour's beam ends the wait, and hello-logged sets
//     the crew member on_deck from beaming_in, logging "crew on deck:
//     combadge said hello" (#106). One from another beam (a stale pump's)
//     is ignored.
//   - heartbeat_missed for the tour's beam ends the tour in sickbay as
//     beam lost: a serve that exits before its hello lands here, through
//     the pump's sickbay check. One for another beam is ignored.
//   - dismissed ends the tour dismissed.
//   - crew_error is logged. One with code harness_check_failed then ends
//     the tour in sickbay with its reason (harnessCheckFailed, #172): that
//     serve sends no hello and exits.
//   - order_assigned is dropped: the first claim picks the order up. So are
//     a mission's late mission_done or mission_parked: none runs yet.
//   - the deadline passing ends the tour in sickbay, code no_hello, unless
//     hello-extend finds the daemon started less than helloTimeout ago.
//     Then the wait gets a fresh helloTimeout, recorded by that step.
//
// A bridge restart mid-wait resumes the tour in its Recv. ensurePumps
// re-attaches the pump, and serve says hello again on that attach. A bridge
// that was down past the deadline (a laptop asleep) finds the Recv timed
// out at once: hello-extend gives that attach its time. Each daemon grants
// this at most once per tour, since a second timeout comes at least
// helloTimeout after the daemon started.
//
// As elsewhere, only recorded outcomes (a Recv's signal or timeout) decide
// the path. ended reports that the tour ended, with res.
func awaitHello(ctx dbos.Context, in CrewTourInput, beam transporter.Beam) (res CrewTourResult, ended bool, err error) {
	d := daemonFrom(ctx)
	tourID := tourWorkflowID(in.CrewID, in.TourNo)
	deadline, err := dbos.RunAsStep(ctx, func(c context.Context) (time.Time, error) {
		return time.Now().Add(daemonFrom(c).helloTimeout), nil
	}, dbos.WithStepName("hello-deadline"))
	if err != nil {
		return CrewTourResult{}, false, err
	}
	for {
		sig, err := dbos.Recv[CrewSignal](ctx, crewEventsTopic, max(time.Until(deadline), time.Millisecond))
		if errors.Is(err, dbos.ErrTimeout) {
			// The zero time: no extension.
			extended, err := dbos.RunAsStep(ctx, func(c context.Context) (time.Time, error) {
				d := daemonFrom(c)
				if time.Since(d.startedAt) < d.helloTimeout {
					return time.Now().Add(d.helloTimeout), nil
				}
				return time.Time{}, nil
			}, dbos.WithStepName("hello-extend"))
			if err != nil {
				return CrewTourResult{}, false, err
			}
			if !extended.IsZero() {
				d.logger.Info("crew tour: bridge restarted past the hello deadline; waiting again", "tour", tourID,
					"beam", beam.Alias, "until", extended)
				deadline = extended
				continue
			}
			d.logger.Warn("crew tour: no hello from combadge", "tour", tourID, "beam", beam.Alias)
			res, err := endTour(ctx, in, beam, "", nil, func(c context.Context, d *Daemon) error {
				return d.toSickbay(c, in.CrewID, beam.Alias,
					fmt.Sprintf("combadge on beam %s did not say hello within %s", beam.Alias, d.helloTimeout), CodeNoHello)
			}, store.CrewSickbay)
			return res, true, err
		}
		if err != nil {
			if ctx.Err() != nil || !isRecvDecodeError(err) {
				return CrewTourResult{}, false, err
			}
			d.logger.Warn("crew tour: ignoring undecodable crew signal", "tour", tourID, "err", err)
			continue
		}
		switch sig.Kind {
		case SignalCrewHello:
			if sig.Beam != beam.Alias {
				d.logger.Info("crew tour: ignoring hello from another beam", "tour", tourID, "beam", sig.Beam, "tour_beam", beam.Alias)
				continue
			}
			err := storeStep(ctx, "hello-logged", func(c context.Context, d *Daemon) error {
				return d.helloOnDeck(c, in.CrewID, beam.Alias)
			})
			return CrewTourResult{}, false, err
		case SignalHeartbeatMissed:
			if sig.Beam != beam.Alias {
				d.logger.Warn("crew tour: ignoring heartbeat_missed for another beam", "tour", tourID, "beam", sig.Beam, "tour_beam", beam.Alias)
				continue
			}
			res, err := beamLost(ctx, in, beam, "", "beam lost")
			return res, true, err
		case SignalDismissed:
			res, err := dismissTour(ctx, in, beam, "")
			return res, true, err
		case SignalCrewError:
			if err := logCrewError(ctx, in, sig); err != nil {
				return CrewTourResult{}, false, err
			}
			if checkFailedOn(d, tourID, sig, beam) {
				res, err := harnessCheckFailed(ctx, in, beam, "", sig)
				return res, true, err
			}
		case SignalOrderAssigned, SignalMissionDone, SignalMissionParked:
			// No mission runs yet: the first claim picks up any order.
		default:
			d.logger.Warn("crew tour: ignoring unknown crew signal", "tour", tourID, "kind", sig.Kind)
		}
	}
}

// helloOnDeck is the hello-logged step: the beam's combadge said hello, so
// a crew member beaming in goes on deck, logged "crew on deck: combadge
// said hello" (#106). One in any other status is left as it is and nothing
// is logged: a dismissal stands, and the tour then claims nothing. So is
// one already on deck: a re-run of the step, or a tour provisioned before
// #106, whose row never left on_deck.
func (d *Daemon) helloOnDeck(ctx context.Context, crewID, beam string) error {
	entry, err := logEntry(logCrew, crewID, "", "crew on deck: combadge said hello", map[string]string{"beam": beam})
	if err != nil {
		return err
	}
	_, err = d.store.UpdateCrewStatusFrom(ctx, crewID, store.CrewBeamingIn, store.CrewOnDeck, entry)
	return err
}

// beamLost ends the tour in sickbay: the beam stopped answering. A running
// mission is sent the beam_lost error with reason, and parks.
func beamLost(ctx dbos.Context, in CrewTourInput, beam transporter.Beam, running, reason string) (CrewTourResult, error) {
	return lostToSickbay(ctx, in, beam, running, reason, "beam lost: heartbeats stopped and the beam failed its probe", CodeBeamLost)
}

// checkFailedOn reports whether sig is a crew_error with code
// harness_check_failed from the tour's beam. One from another beam (a stale
// pump's) is ignored with a log line, as heartbeat_missed is.
func checkFailedOn(d *Daemon, tourID string, sig CrewSignal, beam transporter.Beam) bool {
	if sig.Code != CodeHarnessCheckFailed {
		return false
	}
	if sig.Beam != beam.Alias {
		d.logger.Warn("crew tour: ignoring harness_check_failed from another beam", "tour", tourID, "beam", sig.Beam, "tour_beam", beam.Alias)
		return false
	}
	return true
}

// harnessCheckFailed ends the tour in sickbay with the crew_error's reason,
// code harness_check_failed (#172). The beam's serve can't get a hail to
// the captain and exits, so it is handled as a lost beam: a running mission
// is sent beam_lost ("harness check failed: <reason>"), and parks for
// another crew member to resume.
func harnessCheckFailed(ctx dbos.Context, in CrewTourInput, beam transporter.Beam, running string, sig CrewSignal) (CrewTourResult, error) {
	return lostToSickbay(ctx, in, beam, running, "harness check failed: "+sig.Reason, sig.Reason, CodeHarnessCheckFailed)
}

// lostToSickbay ends the tour in sickbay with sickReason and code, sending
// a running mission the beam_lost error with missionReason: it parks.
func lostToSickbay(ctx dbos.Context, in CrewTourInput, beam transporter.Beam, running, missionReason, sickReason, code string) (CrewTourResult, error) {
	msg, err := syntheticMessage(comms.TypeError, comms.ErrorBody{Reason: missionReason, Code: CodeBeamLost}, in.CrewID, running)
	if err != nil {
		return CrewTourResult{}, err
	}
	return endTour(ctx, in, beam, running, &msg, func(c context.Context, d *Daemon) error {
		return d.toSickbay(c, in.CrewID, beam.Alias, sickReason, code)
	}, store.CrewSickbay)
}

// dismissTour ends the tour dismissed: a running mission is sent the
// dismissed error and fails its order, tour-end sets the crew row
// dismissed (DismissCrew has normally done so already), so a restarted
// daemon's ensurePumps and attachCrewTours leave it alone, and remove-beam
// frees the beam's slot (endTour).
func dismissTour(ctx dbos.Context, in CrewTourInput, beam transporter.Beam, running string) (CrewTourResult, error) {
	msg, err := syntheticMessage(comms.TypeError, comms.ErrorBody{Reason: "crew member dismissed", Code: CodeDismissed}, in.CrewID, running)
	if err != nil {
		return CrewTourResult{}, err
	}
	return endTour(ctx, in, beam, running, &msg, func(c context.Context, d *Daemon) error {
		return d.store.UpdateCrewStatus(c, in.CrewID, store.CrewDismissed,
			store.LogEntry{Kind: logCrew, CrewID: in.CrewID, Text: "crew dismissed"})
	}, SignalDismissed)
}

// endDismissed ends a tour whose terminal step found its crew member
// dismissed (#100): DismissCrew records the dismissal in the store before
// it signals the tour, so a signal the tour never read (it was ending
// already) still ends it dismissed. The running mission, if any, is sent
// the dismissed error: one that parked after DismissCrew looked for a
// parked order would otherwise keep its order. remove-beam frees the
// beam's slot.
func endDismissed(ctx dbos.Context, in CrewTourInput, beam transporter.Beam, running string) (CrewTourResult, error) {
	d := daemonFrom(ctx)
	if running != "" {
		msg, err := syntheticMessage(comms.TypeError, comms.ErrorBody{Reason: "crew member dismissed", Code: CodeDismissed}, in.CrewID, running)
		if err != nil {
			return CrewTourResult{}, err
		}
		if err := dbos.Send(ctx, missionWorkflowID(running), msg, fromCrewTopic); err != nil {
			return CrewTourResult{}, err
		}
	}
	d.markTourEnded(tourWorkflowID(in.CrewID, in.TourNo))
	d.StopPump(in.CrewID)
	if err := removeTourBeam(ctx, in, beam); err != nil {
		return CrewTourResult{}, err
	}
	return CrewTourResult{Ended: store.CrewDismissed}, nil
}

// removeTourBeam is the remove-beam step: it removes the tour's beam, best
// effort (removeBeam). With no beam given it removes the latest beam this
// tour created, if any (a provision that failed after Create).
func removeTourBeam(ctx dbos.Context, in CrewTourInput, beam transporter.Beam) error {
	return storeStep(ctx, "remove-beam", func(c context.Context, d *Daemon) error {
		if beam.Alias == "" {
			return d.removeLatestTourBeam(c, in.CrewID, in.TourNo)
		}
		return d.removeBeam(c, in.CrewID, store.BeamRecord{Alias: beam.Alias, ExpiresAt: beam.ExpiresAt, TourNo: in.TourNo})
	})
}

// removeLatestTourBeam removes crewID's latest beam, best effort
// (removeBeam), if tour tourNo created it.
func (d *Daemon) removeLatestTourBeam(ctx context.Context, crewID string, tourNo int) error {
	beams, err := d.store.ListBeams(ctx, crewID)
	if err != nil {
		return err
	}
	if n := len(beams); n == 0 || beams[n-1].TourNo != tourNo {
		return nil
	}
	return d.removeBeam(ctx, crewID, beams[len(beams)-1])
}

// crewDismissed reports whether crewID is dismissed.
func (d *Daemon) crewDismissed(ctx context.Context, crewID string) (bool, error) {
	c, err := d.store.GetCrew(ctx, crewID)
	if err != nil {
		return false, err
	}
	return c.Status == store.CrewDismissed, nil
}

// logCrewError is the log-crew-error step: a crew_error goes into the
// captain's log.
func logCrewError(ctx dbos.Context, in CrewTourInput, sig CrewSignal) error {
	daemonFrom(ctx).logger.Warn("crew tour: crew-level error", "tour", tourWorkflowID(in.CrewID, in.TourNo),
		"reason", sig.Reason, "code", sig.Code)
	return storeStep(ctx, "log-crew-error", func(c context.Context, d *Daemon) error {
		return d.appendLog(c, logFail, in.CrewID, "", "crew error: "+sig.Reason,
			map[string]string{"reason": sig.Reason, "code": sig.Code})
	})
}

// watchChange hands the crew member over to its next tour before the beam
// is purged (spec/phase-2-plan-2026-10-19.md section 8):
//
//  1. A running mission is sent stand_down, and the tour waits, with the
//     pump still up for the beam's checkpoint_ready, for its
//     mission_parked or mission_done, up to standDownTimeout (a recorded
//     deadline). Meanwhile heartbeat_missed for the beam ends the tour in
//     sickbay (beamLost) and dismissed ends it dismissed (dismissTour), as
//     in the main loop; crew_error is logged (and harness_check_failed ends
//     the tour as harnessCheckFailed), crew_hello dropped silently and
//     anything else dropped with a log line.
//     Past the deadline the mission is sent beam_lost ("watch change: no
//     checkpoint in 30m") and waited on for missionSettleTimeout more.
//  2. markTourEnded, StopPump.
//  3. next-tour: backstop a mission that neither parked nor finished, then
//     store.NextTour (tour_no+1, current_beam cleared, beaming_in).
//  4. Start tour tour_no+1 from the workflow body, under its deterministic
//     id: a replay gets the existing handle, and so does a tour
//     attachCrewTours already started after a crash between 3 and 4.
//  5. End as watch_change. The next tour's remove-previous removes the
//     beam, and its claim resumes a parked mission.
//
// A mission_parked or mission_done with StaleBeam needs nothing more here:
// the beam is going anyway.
func watchChange(ctx dbos.Context, in CrewTourInput, beam transporter.Beam, running string) (CrewTourResult, error) {
	d := daemonFrom(ctx)
	tourID := tourWorkflowID(in.CrewID, in.TourNo)
	d.logger.Info("crew tour: watch change", "tour", tourID, "beam", beam.Alias, "expires_at", beam.ExpiresAt, "running", running)
	parked, settled := false, running == ""
	if running != "" {
		msg, err := syntheticMessage(comms.TypeStandDown, comms.StandDownBody{Reason: "watch change"}, in.CrewID, running)
		if err != nil {
			return CrewTourResult{}, err
		}
		if err := dbos.Send(ctx, missionWorkflowID(running), msg, fromCrewTopic); err != nil {
			return CrewTourResult{}, err
		}
		deadline, err := dbos.RunAsStep(ctx, func(c context.Context) (time.Time, error) {
			return time.Now().Add(daemonFrom(c).standDownTimeout), nil
		}, dbos.WithStepName("stand-down-deadline"))
		if err != nil {
			return CrewTourResult{}, err
		}
	wait:
		for {
			// As in settleWait, only recorded outcomes end the wait.
			sig, err := dbos.Recv[CrewSignal](ctx, crewEventsTopic, max(time.Until(deadline), time.Millisecond))
			if errors.Is(err, dbos.ErrTimeout) {
				d.logger.Warn("crew tour: no checkpoint for the watch change", "tour", tourID, "order", running)
				break
			}
			if err != nil {
				if ctx.Err() != nil || !isRecvDecodeError(err) {
					return CrewTourResult{}, err
				}
				continue
			}
			switch sig.Kind {
			case SignalMissionParked, SignalMissionDone:
				if missionSignalFor(sig, running, tourID) {
					parked, settled = sig.Kind == SignalMissionParked, true
					break wait
				}
			case SignalHeartbeatMissed:
				if sig.Beam == beam.Alias {
					return beamLost(ctx, in, beam, running, "beam lost")
				}
			case SignalDismissed:
				return dismissTour(ctx, in, beam, running)
			case SignalCrewError:
				if err := logCrewError(ctx, in, sig); err != nil {
					return CrewTourResult{}, err
				}
				if checkFailedOn(d, tourID, sig, beam) {
					return harnessCheckFailed(ctx, in, beam, running, sig)
				}
				continue
			case SignalCrewHello:
				continue
			}
			d.logger.Info("crew tour: changing the watch; ignoring signal", "tour", tourID, "kind", sig.Kind, "order", sig.OrderID)
		}
		if !settled {
			msg, err := syntheticMessage(comms.TypeError, comms.ErrorBody{
				Reason: fmt.Sprintf("watch change: no checkpoint in %s", d.standDownTimeout), Code: CodeBeamLost,
			}, in.CrewID, running)
			if err != nil {
				return CrewTourResult{}, err
			}
			if err := dbos.Send(ctx, missionWorkflowID(running), msg, fromCrewTopic); err != nil {
				return CrewTourResult{}, err
			}
			if parked, err = settleWait(ctx, running, tourID); err != nil {
				return CrewTourResult{}, err
			}
		}
	}
	// Not steps: local effects only, idempotent.
	d.markTourEnded(tourID)
	d.StopPump(in.CrewID)
	dismissed, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
		d := daemonFrom(c)
		if running != "" && !parked {
			if err := d.backstop(c, in.CrewID, running, false); err != nil {
				return false, err
			}
		}
		// A dismissal the tour never read (it came during the settle wait)
		// stands: no next tour.
		if dismissed, err := d.crewDismissed(c, in.CrewID); err != nil || dismissed {
			return dismissed, err
		}
		if d.nextTourHook != nil {
			d.nextTourHook(d, in.CrewID)
		}
		entry, err := logEntry(logCrew, in.CrewID, "", fmt.Sprintf("watch change: tour %d hands over to tour %d", in.TourNo, in.TourNo+1),
			map[string]any{"beam": beam.Alias, "tour_no": in.TourNo + 1})
		if err != nil {
			return false, err
		}
		changed, err := d.store.NextTour(c, in.CrewID, in.TourNo, entry)
		if err != nil || changed {
			return false, err
		}
		// Unchanged: a re-run of this step, or a dismissal that committed
		// since the read above (#113), which stands too.
		return d.crewDismissed(c, in.CrewID)
	}, append([]dbos.StepOption{dbos.WithStepName("next-tour")}, storeRetries...)...)
	if err != nil {
		return CrewTourResult{}, err
	}
	if dismissed {
		return endDismissed(ctx, in, beam, running)
	}
	if _, err := dbos.RunWorkflow(ctx, crewTourWorkflow, CrewTourInput{CrewID: in.CrewID, TourNo: in.TourNo + 1},
		dbos.WithWorkflowID(tourWorkflowID(in.CrewID, in.TourNo+1))); err != nil {
		return CrewTourResult{}, err
	}
	return CrewTourResult{Ended: TourEndedWatchChange}, nil
}

// awaitFailed puts the crew member of a tour that awaitBeam gave up on in
// sickbay, code bridge_error, outside any step (#110). It also marks the
// tour ended. A crew member dismissed meanwhile stays dismissed
// (UpdateCrewStatus), and DismissCrew, finding the tour ended, removes its
// beam.
func awaitFailed(ctx context.Context, d *Daemon, in CrewTourInput, err error) error {
	d.logger.Error("crew tour: giving up on its transporter workflow", "tour", tourWorkflowID(in.CrewID, in.TourNo), "err", err)
	d.markTourEnded(tourWorkflowID(in.CrewID, in.TourNo))
	if err := d.toSickbay(ctx, in.CrewID, "", "awaiting its beam failed: "+err.Error(), CodeBridgeError); err != nil {
		return err
	}
	// A dismissal that landed during the retries may have found the tour
	// still live and left the beam to it: remove it here, best effort.
	if dismissed, err := d.crewDismissed(ctx, in.CrewID); err != nil || !dismissed {
		return err
	}
	return d.removeLatestTourBeam(ctx, in.CrewID, in.TourNo)
}

// awaitBeam waits for the tour's transporter workflow and returns its
// result, which GetResult records in the tour. recorded reports whether
// err is the child's recorded outcome (its own error, or its
// cancellation), the only error the tour may act on: any other (DBOS
// shutting down, the system database failing, a dead-lettered child)
// recorded nothing, and a step the tour ran on it would sit where a replay
// expects the GetResult.
//
// An unrecorded failure on a child that has ended, or a failure to read
// whether it has (childOutcome), is retried awaitEndedRetries times, a
// poll interval apart, before awaitBeam gives up and returns it (#110).
// Except a failure to record the result (stepIDSpent): GetResult took the
// step id before the write failed, so a retry would record the result one
// step later than a replay would. awaitBeam gives up on it at once.
//
// A handle to a child on a queue polls on a context DBOS shutdown doesn't
// cancel, so a single GetResult would hold up shutdown until the child
// ends. Each GetResult here gives up after a few polls instead: a timeout
// records nothing and takes no step id, so the loop's iterations are
// invisible to a replay, and between them a cancelled workflow context (DBOS
// shutting down) returns at once, leaving the tour PENDING. Whether a
// failed GetResult recorded anything is read from the tour's steps, and
// whether to wait on from the child's status, both outside any step.
func awaitBeam(ctx dbos.Context, d *Daemon, tourID string, h dbos.WorkflowHandle[TransporterResult]) (TransporterResult, bool, error) {
	failures := 0 // unrecorded failures on an ended child so far
	for {
		var res TransporterResult
		var err error
		if d.getResultFault != nil {
			err = d.getResultFault(d, tourID)
		}
		if err == nil {
			res, err = h.GetResult(dbos.WithHandlePollingInterval(d.queuePollEvery),
				dbos.WithHandleTimeout(2*d.queuePollEvery))
		}
		if err == nil {
			return res, true, nil
		}
		if ctx.Err() != nil {
			return res, false, err
		}
		timeout, spent := errors.Is(err, dbos.ErrTimeout), stepIDSpent(err)
		recorded, final, cerr := childOutcome(d, tourID, h.GetWorkflowID())
		if cerr != nil {
			// Not knowing counts as an ended child: retried, then given up.
			err, recorded, final, timeout = errors.Join(err, cerr), false, true, false
		}
		switch awaitNext(recorded, final, timeout, spent, failures) {
		case awaitDone:
			return res, recorded, err
		case awaitPause:
			if final {
				failures++
			}
			// A failure that didn't wait out the timeout: don't spin on it.
			d.logger.Warn("crew tour: awaiting its transporter workflow", "tour", tourID, "err", err)
			select {
			case <-ctx.Done():
				return res, false, err
			case <-time.After(d.queuePollEvery):
			}
		}
	}
}

// What awaitBeam does after a failed GetResult.
const (
	awaitDone  = iota // return the error
	awaitRetry        // call GetResult again at once
	awaitPause        // call GetResult again after a poll interval
)

// awaitEndedRetries is how many times awaitBeam retries an unrecorded
// failure on an ended child before it returns it (#110).
const awaitEndedRetries = 4

// awaitNext decides awaitBeam's next move after a failed GetResult:
// recorded is whether it recorded the child's outcome, final whether the
// child has ended, timeout whether the error was GetResult's own timeout,
// spent whether GetResult took a step id it recorded nothing under
// (stepIDSpent), failures how many unrecorded failures on the ended child
// came before. A recorded outcome is final, and so is a spent step id: the
// tour can't go on in step with its replay. Otherwise it waits on while the
// child runs, and
// once more after a timeout that raced the child's end; an unrecorded
// failure on an ended child (a dead letter, a result that can't be read or
// recorded, often transient) is retried after a pause awaitEndedRetries
// times, then returned.
func awaitNext(recorded, final, timeout, spent bool, failures int) int {
	switch {
	case recorded, spent:
		return awaitDone
	case timeout:
		return awaitRetry
	case !final:
		return awaitPause
	case failures < awaitEndedRetries:
		return awaitPause
	default:
		return awaitDone
	}
}

// stepIDSpent reports whether err, from a GetResult in a workflow, may have
// taken a step id it recorded nothing under. In DBOS v1.7.0 both handles
// record the child's result with RecordOperationResult under
// workflowState.nextStepID(), and a failed write returns a
// WorkflowExecutionError ("recording child workflow result"). The other
// unrecorded failures (a timeout, the child's dead letter, a result that
// doesn't decode, the await's own system-database errors) return before
// the step id is taken. Their other WorkflowExecutionErrors (an invalid
// context, a result that won't serialize) take none but are deterministic,
// so giving up on them at once loses nothing.
func stepIDSpent(err error) bool {
	var de *dbos.Error
	return errors.As(err, &de) && de.Code == dbos.ErrorCodeWorkflowExecution
}

// childOutcome reports whether tourID has recorded the result of its child
// childID and whether the child has ended. Both reads go through the
// daemon's own DBOS context, outside any workflow, and record nothing.
func childOutcome(d *Daemon, tourID, childID string) (recorded, final bool, err error) {
	steps, err := dbos.GetWorkflowSteps(d.dbos, tourID)
	if err != nil {
		return false, false, err
	}
	for _, s := range steps {
		if s.StepName == "DBOS.getResult" && s.ChildWorkflowID == childID {
			return true, true, nil
		}
	}
	wfs, err := dbos.ListWorkflows(d.dbos, dbos.WithFilterWorkflowIDs(childID),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return false, false, err
	}
	if len(wfs) == 0 {
		return false, false, nil
	}
	switch wfs[0].Status {
	case dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued:
		return false, false, nil
	}
	return false, true, nil
}

// isRecvDecodeError reports whether err, from a Recv that returned no DBOS
// error, is the message failing to decode into the wanted type. That is the
// only Recv failure a workflow skips; anything else is returned.
func isRecvDecodeError(err error) bool {
	var de *dbos.Error
	return !errors.As(err, &de) && strings.Contains(err.Error(), "decoding received message")
}

// pumpAttempts and defaultPumpBackoff bound startPump's retries: the wait
// doubles from the backoff (Config.pumpBackoff overrides it in tests).
const (
	pumpAttempts       = 5
	defaultPumpBackoff = 500 * time.Millisecond
)

// pumpStart is the start-pump step's recorded outcome.
type pumpStart struct {
	// Failed is why the pump didn't start; empty when it did.
	Failed string `json:",omitempty"`
}

// startPump is the start-pump step: it registers the crew member's pump
// (EnsurePump), retrying a failure with backoff inside the step, and
// returns the outcome as a value for the tour to branch on.
//
// ErrDaemonStopping is transient: the daemon closes its pumps just before
// DBOS shuts down and cancels ctx. Recording it would send a healthy crew
// member to sickbay, so the step waits for ctx and returns its error: DBOS
// records nothing for a step whose workflow was cancelled, and the tour
// stays PENDING. Any other failure (StartPump rejecting its config, in
// practice deterministic) is recorded as Failed after the retries.
func startPump(ctx context.Context, d *Daemon, in CrewTourInput, beam transporter.Beam) (pumpStart, error) {
	var err error
	for i := range pumpAttempts {
		if i > 0 {
			select {
			case <-ctx.Done():
				return pumpStart{}, ctx.Err()
			case <-time.After(d.pumpBackoff << (i - 1)):
			}
		}
		if _, err = d.EnsurePump(in.CrewID, beam, in.TourNo); err == nil {
			return pumpStart{}, nil
		}
		if errors.Is(err, ErrDaemonStopping) {
			<-ctx.Done()
			return pumpStart{}, ctx.Err()
		}
		d.logger.Error("crew tour: start comms pump", "crew", in.CrewID, "beam", beam.Alias, "attempt", i+1, "err", err)
	}
	return pumpStart{Failed: err.Error()}, nil
}

// rearmPump registers the pump again on every run of a tour whose
// start-pump step recorded success (EnsurePump is idempotent). Its outcome
// is not recorded and never changes the tour's path: ErrDaemonStopping
// waits for ctx and returns its error (the tour stays PENDING), and any
// other failure is only logged (Run's ensurePumps has usually started the
// pump already; without one, sends fail as send_failed).
func rearmPump(ctx dbos.Context, d *Daemon, in CrewTourInput, beam transporter.Beam) error {
	_, err := d.EnsurePump(in.CrewID, beam, in.TourNo)
	if errors.Is(err, ErrDaemonStopping) {
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil {
		d.logger.Error("crew tour: re-register comms pump", "crew", in.CrewID, "beam", beam.Alias, "err", err)
	}
	return nil
}

// tourSickbay ends a tour that never got as far as a mission in sickbay.
// A crew member dismissed meanwhile (a dismissal queued while the tour
// awaited its beam) stays dismissed, and the tour ends dismissed, removing
// beam (or a beam its provisioning made).
func tourSickbay(ctx dbos.Context, in CrewTourInput, beam transporter.Beam, reason, code string) (CrewTourResult, error) {
	dismissed, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
		d := daemonFrom(c)
		if err := d.toSickbay(c, in.CrewID, "", reason, code); err != nil {
			return false, err
		}
		return d.crewDismissed(c, in.CrewID)
	}, append([]dbos.StepOption{dbos.WithStepName("tour-sickbay")}, storeRetries...)...)
	if err != nil {
		return CrewTourResult{}, err
	}
	if dismissed {
		return endDismissed(ctx, in, beam, "")
	}
	daemonFrom(ctx).markTourEnded(tourWorkflowID(in.CrewID, in.TourNo))
	daemonFrom(ctx).StopPump(in.CrewID)
	return CrewTourResult{Ended: store.CrewSickbay}, nil
}

// endTour ends the tour (heartbeat_missed: sickbay; dismissed; a StaleBeam
// signal: sickbay). A running mission is sent msg on its from-crew topic
// (a synthetic error: beam_lost parks it, dismissed fails its order). The
// pump is stopped, then the tour waits up to missionSettleTimeout for the
// mission's mission_done or mission_parked (settleWait), so the order is
// settled or parked before the tour ends. Last, the tour-end step runs the
// backstop, unless the mission parked, and final (nil-able), and the tour
// ends as ended. A mission still running then has msg queued ahead of
// anything it hasn't read, so it fails or parks its order on its next Recv:
// the hail the backstop may close is one no answer can reach in time anyway
// (and the mission's own closeHail then finds it closed and does nothing).
//
// The beam isn't told. A crew member dismissed by then (dismissal, or a
// DismissCrew whose signal the tour never read) ends the tour dismissed,
// and remove-beam removes beam.
func endTour(ctx dbos.Context, in CrewTourInput, beam transporter.Beam, running string, msg *comms.Message,
	final func(context.Context, *Daemon) error, ended string) (CrewTourResult, error) {
	d := daemonFrom(ctx)
	if running != "" && msg != nil {
		// dbos.Send in a workflow is a durable step of its own.
		if err := dbos.Send(ctx, missionWorkflowID(running), *msg, fromCrewTopic); err != nil {
			return CrewTourResult{}, err
		}
	}
	// Not steps: local effects only, idempotent. The mark comes after the
	// synthetic message is queued, so a mission that sees it (shipRepos)
	// finds that message on its next Recv.
	d.markTourEnded(tourWorkflowID(in.CrewID, in.TourNo))
	d.StopPump(in.CrewID)

	parked := false
	if running != "" {
		var err error
		if parked, err = settleWait(ctx, running, tourWorkflowID(in.CrewID, in.TourNo)); err != nil {
			return CrewTourResult{}, err
		}
	}
	dismissed, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
		d := daemonFrom(c)
		if running != "" && !parked {
			if err := d.backstop(c, in.CrewID, running, false); err != nil {
				return false, err
			}
		}
		if final != nil {
			if err := final(c, d); err != nil {
				return false, err
			}
		}
		return d.crewDismissed(c, in.CrewID)
	}, append([]dbos.StepOption{dbos.WithStepName("tour-end")}, storeRetries...)...)
	if err != nil {
		return CrewTourResult{}, err
	}
	if dismissed {
		// dismissTour's own dismissed error is already queued: none again.
		if ended == SignalDismissed {
			running = ""
		}
		return endDismissed(ctx, in, beam, running)
	}
	return CrewTourResult{Ended: ended}, nil
}

// settleWait waits up to missionSettleTimeout (a recorded deadline, so a
// resumed tour waits only for what is left of it) for running's
// mission_done or mission_parked, dropping other signals, and reports
// whether the mission parked.
func settleWait(ctx dbos.Context, running, tourID string) (parked bool, err error) {
	deadline, err := dbos.RunAsStep(ctx, func(c context.Context) (time.Time, error) {
		return time.Now().Add(daemonFrom(c).settleTimeout), nil
	}, dbos.WithStepName("settle-deadline"))
	if err != nil {
		return false, err
	}
	for {
		// Never skip a Recv because the deadline passed: whether to
		// wait again is decided only by recorded outcomes (a timeout or
		// the mission's signal), so a replay takes the same path. Past
		// the deadline the Recv times out at once.
		sig, err := dbos.Recv[CrewSignal](ctx, crewEventsTopic, max(time.Until(deadline), time.Millisecond))
		if errors.Is(err, dbos.ErrTimeout) {
			daemonFrom(ctx).logger.Warn("crew tour: mission did not settle", "order", running)
			return false, nil
		}
		if err != nil {
			if ctx.Err() != nil || !isRecvDecodeError(err) {
				return false, err
			}
			continue
		}
		if (sig.Kind == SignalMissionDone || sig.Kind == SignalMissionParked) && missionSignalFor(sig, running, tourID) {
			return sig.Kind == SignalMissionParked, nil
		}
	}
}

// missionSignalFor reports whether sig, a mission_done or mission_parked,
// is about running in tour tourID: its order, and for mission_parked, a
// park under this tour. A mission_parked from the order's earlier tour can
// arrive after this tour resumed the order (the mission tells its crew
// member's current tour); the mission is this tour's again by then.
func missionSignalFor(sig CrewSignal, running, tourID string) bool {
	if sig.OrderID == "" || sig.OrderID != running {
		return false
	}
	return sig.Kind != SignalMissionParked || sig.TourID == "" || sig.TourID == tourID
}

// settleMission runs the backstop for orderID and puts the crew member back
// on_deck. The tour calls it only once the mission is over: after its
// mission_done (the mission's last step), or once mission-alive found it
// no longer PENDING or ENQUEUED. So the hail the backstop may close is never
// one a live mission still waits on.
func settleMission(ctx dbos.Context, crewID, orderID string) error {
	return storeStep(ctx, "settle-mission", func(c context.Context, d *Daemon) error {
		if err := d.backstop(c, crewID, orderID, true); err != nil {
			return err
		}
		return d.store.UpdateCrewStatus(c, crewID, store.CrewOnDeck,
			store.LogEntry{Kind: logCrew, CrewID: crewID, OrderID: orderID, Text: "crew back on deck"})
	})
}

// backstop closes a hail of crewID's that its mission left open and fails
// orderID if its mission left it away (or hailing) with crewID.
//
// Every mission exit settles its order, so the order check only catches a
// mission that ended some other way; otherwise ClaimOldestDirectedOrder
// would keep returning the order. The hail check catches more: a mission
// that panics settles nothing, and AnswerHail must not accept an answer for
// a workflow that is gone. (A mission that settles through bridge_error
// closes its own open hail since #47; this one then finds it closed.) The
// hail is closed as timed_out (closeHail) whatever the order's state.
//
// A recall and another crew member's claim can move the order between a
// settle wait's timeout and the backstop (#100), so both writes are limited
// to crewID: the hail to one crewID raised, the order to one still
// assigned to crewID (store.SettleAssignedOrder). An order whose mission
// has parked is left alone too: that mission is alive. Unless over: the
// mission is known to have ended (settle-mission: after its mission_done,
// or once mission-alive found it no longer running), so a parked order it
// left behind (a recalled order claimed and resumed into a dead mission)
// fails too, rather than be claimed again for good (#101 review).
//
// Both writes are conditional, so a re-run finds nothing left to do.
func (d *Daemon) backstop(ctx context.Context, crewID, orderID string, over bool) error {
	h, found, err := d.store.OpenHailForOrder(ctx, orderID)
	if err != nil {
		return err
	}
	if found && h.CrewID == crewID {
		if err := d.closeHail(ctx, MissionInput{OrderID: orderID, CrewID: crewID}, &openHail{ID: h.ID}); err != nil {
			return err
		}
	}
	entry, err := logEntry(logFail, crewID, orderID, "mission ended without settling",
		map[string]string{"reason": "mission ended without settling", "code": CodeUnsettled})
	if err != nil {
		return err
	}
	// Only from away or hailing, still crewID's and not parked, in one
	// transaction with its entry, so it can't overwrite a mission that
	// settled or parked meanwhile.
	_, _, err = d.store.SettleAssignedOrder(ctx, orderID, crewID, store.OrderFailed, entry, over)
	return err
}

// toSickbay sets the crew member's status to sickbay (keeping its current
// beam, or setting beam if not empty) and logs reason and code, in one
// transaction.
func (d *Daemon) toSickbay(ctx context.Context, crewID, beam, reason, code string) error {
	entry, err := logEntry(logBeam, crewID, "", "crew to sickbay: "+reason,
		map[string]string{"beam": beam, "reason": reason, "code": code})
	if err != nil {
		return err
	}
	if beam != "" {
		return d.store.SetCrewStatus(ctx, crewID, store.CrewSickbay, beam, entry)
	}
	return d.store.UpdateCrewStatus(ctx, crewID, store.CrewSickbay, entry)
}

// orderClaim is the claim-order step's result.
type orderClaim struct {
	OrderID string
	Found   bool
	// Parked: the order's mission is parked (its parked_at is set), so the
	// tour resumes it rather than starting one.
	Parked bool `json:",omitempty"`
	// WatchDue: the tour's watch change is due, and nothing was claimed.
	WatchDue bool `json:",omitempty"`
}

// claimOrder claims crewID's next order (or returns the one already away)
// and sets the crew member away, logging it in the same transaction, and
// reports whether the order's mission is parked: an order already away
// with this crew member after a watch change or re-energize, or (#101) a
// recalled pool order. It tries the directed claim
// (store.ClaimOldestDirectedOrder), then, when standing orders are on, the
// pool of the crew member's division (store.ClaimOldestPoolOrder): the
// standing-orders dispatcher (spec phase-2-plan section 4). Once watchAt
// (non-zero) has passed it claims nothing and reports WatchDue. Repeating
// it writes nothing: a claimed pool order is the crew member's order away.
// While the tsh login is expired (#102) it claims nothing: the order's beam
// steps couldn't run. The tour is woken when the login is valid again
// (checkLogin).
//
// It reads Daemon.StandingOrders, a mutable setting, so it must only run
// as a step.
func claimOrder(ctx context.Context, d *Daemon, crewID string, watchAt time.Time) (orderClaim, error) {
	if !watchAt.IsZero() && !time.Now().Before(watchAt) {
		return orderClaim{WatchDue: true}, nil
	}
	if d.loginExpired() {
		return orderClaim{}, nil
	}
	entry := store.LogEntry{Kind: logOrder, CrewID: crewID, Text: "order away: mission started"}
	id, found, err := d.store.ClaimOldestDirectedOrder(ctx, crewID, entry)
	if err != nil {
		return orderClaim{}, err
	}
	if !found {
		on, err := d.StandingOrders(ctx)
		if err != nil || !on {
			return orderClaim{}, err
		}
		c, err := d.store.GetCrew(ctx, crewID)
		if err != nil {
			return orderClaim{}, err
		}
		if id, found, err = d.store.ClaimOldestPoolOrder(ctx, crewID, c.Division, entry); err != nil || !found {
			return orderClaim{}, err
		}
	}
	o, err := d.store.GetOrder(ctx, id)
	if err != nil {
		return orderClaim{}, err
	}
	return orderClaim{OrderID: id, Found: true, Parked: o.ParkedAt != nil}, nil
}

// missionAlive reports whether orderID's mission is still running (or
// waiting to run). A mission that ended without its mission_done reaching
// the tour (it panicked, or ran out of recovery attempts) reads false.
func missionAlive(d *Daemon, orderID string) (bool, error) {
	live, _, err := workflowLive(d, missionWorkflowID(orderID))
	return live, err
}
