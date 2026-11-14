package bridge

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// fromCrewTopic is the DBOS topic a mission receives on. Its senders use
// it, all with a whole comms.Message:
//   - the comms pump: hail, checkpoint_ready, report, or an error with an
//     order, Type and Seq included;
//   - AnswerHail: an answer (comms.TypeAnswer) with the captain's AnswerBody
//     (HailID, DecidedBy, Decision, Option; Ref is filled in by the
//     mission);
//   - the hail workflow (hail.go): an answer decided by policy, an
//     auto_allow rule or the captain timeout's default_option;
//   - the crew tour: a synthetic stand_down at watch change, a synthetic
//     error with code beam_lost on sickbay (or a watch change that got no
//     checkpoint) or dismissed on dismissal, and resume (typeResume) to the
//     parked mission of the order it claims;
//   - DismissCrew: the dismissed error, to a parked mission whose crew
//     member has no live tour;
//   - BelayOrder and RecallOrder: a synthetic belay or recall, naming no
//     crew member (#101).
//
// Answers share the topic with crew messages (rather than a separate
// "from-hail" topic, as the plan had it) so a mission waiting on a hail
// still sees beam_lost or a crew error, instead of waiting out the hail's
// 24h timeout with its order stuck away.
const fromCrewTopic = "from-crew"

// typeResume is the synthetic message a tour sends the parked mission of
// the order it claims, with a resumeBody. It never goes on the wire, so it
// isn't a comms type (comms.NewMessage doesn't know it).
const typeResume = "resume"

// resumeBody is a resume's body: the crew member and tour the mission now
// runs on.
type resumeBody struct {
	CrewID string `json:"crew_id"`
	TourID string `json:"tour_id"`
}

// belayTimeout bounds a belayed mission's wait for its run to end on the
// beam (awaitBelayed). The harness ends at once once the belay reaches it;
// serve holds the belay only while it checks the order's repos out. Past
// it the beam may still be running the order: StaleBeam.
// Config.belayTimeout overrides it in tests.
const belayTimeout = 10 * time.Minute

// missionWaitTimeout bounds each wait for the crew (or, with a hail open,
// for the captain's answer). On timeout the order fails. A parked mission
// waits on with no deadline.
const missionWaitTimeout = 24 * time.Hour

// Mission outcomes (MissionResult.Status).
const (
	MissionDone    = "done"
	MissionFailed  = "failed"
	MissionBelayed = "belayed" // the captain belayed the order (#101)
)

// MissionInput is the input of a mission workflow, id order/<OrderID>. A
// resumed mission runs on the crew member and tour of its resume instead
// (spec/phase-2-plan-2026-10-19.md section 7); the mission passes its
// current ones around in a MissionInput of its own.
type MissionInput struct {
	OrderID string
	CrewID  string
	// TourID is the workflow id of the crew tour that started (or
	// resumed) the mission. ship-repos watches it end (markTourEnded).
	TourID string
}

// MissionResult is how a mission ended.
type MissionResult struct {
	Status string            // MissionDone, MissionFailed or MissionBelayed
	Report *comms.ReportBody `json:",omitempty"`
	// Reason and Code say why a failed mission failed (also in the fail
	// log entry's payload); Reason is a belay's reason too.
	Reason string `json:",omitempty"`
	Code   string `json:",omitempty"`
	// StaleBeam: the belay couldn't be sent, so the beam may still be
	// running the order. mission_done carries it to the tour.
	StaleBeam bool `json:",omitempty"`
}

// openHail is the hail a mission is waiting on the captain for.
type openHail struct {
	ID  string // bridge hail id, H-0001
	Ref string // combadge's ref, echoed in the answer
	// Category is the hail's: an order_split answered with option 0
	// runs create-children (#155), a tracker request tracker-request
	// (#54), a budget hail budget-extend (#119).
	Category string
}

// heldCorrection is a correction a mission holds while its order is
// parked or stopping, for the resumed run (#167): Crew is the crew member
// that hailed (the message's).
type heldCorrection struct {
	Crew string
	Body comms.CorrectionBody
}

// heldCorrections are a mission's held corrections, the latest per hail.
// Only recorded messages fill it, so a replay rebuilds it.
type heldCorrections []heldCorrection

// hold keeps body for the resume, replacing an earlier one for its hail.
func (h *heldCorrections) hold(crew string, body comms.CorrectionBody) {
	c := heldCorrection{Crew: crew, Body: body}
	if i := slices.IndexFunc(*h, func(c heldCorrection) bool { return c.Body.HailID == body.HailID }); i >= 0 {
		(*h)[i] = c
		return
	}
	*h = append(*h, c)
}

// hailIDCompare orders hail ids by number: H-0002 before H-0010, and
// H-9999 before H-10000.
func hailIDCompare(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b))
}

// missionWorkflow carries one order from its claim to its terminal state
// (spec phase-1-plan section 8, with the corrections in phase-1-decisions;
// park and resume from spec/phase-2-plan-2026-10-19.md section 7). It can
// outlive its beam, its tour and its crew member: it parks when the beam
// goes away and resumes on whichever crew member's tour claims the order
// next.
//
// Every exit settles the order (done, failed or belayed) and then sends
// mission_done to the current tour of the crew member the mission last ran
// on: runMission returns a result and that crew member on every path, and
// this wrapper is the single place that notifies. A mission that ends
// recalled and not resumed (a belay of a recalled order, or bridge_error)
// tells the tour of the order's assignee, a claimer whose resume it never
// read, if any. The only exception is DBOS shutting down mid-mission
// (ctx cancelled): the mission returns at once, stays PENDING and is resumed
// by the next Launch.
//
// A mission never returns an error for a failed order: it ends SUCCESS with
// Status failed, so the tour reads the outcome from the store, not from
// the workflow's status.
func missionWorkflow(ctx dbos.Context, in MissionInput) (MissionResult, error) {
	var held heldCorrections
	res, crewID, err := runMission(ctx, in, &held)
	if err != nil {
		if ctx.Err() != nil {
			return res, err
		}
		// A bridge-side step failed for good (e.g. the store after its
		// retries). Still settle the order and tell the tour. The body's
		// open hail isn't known here: order-failed looks it up by order
		// (settleFailed with a nil hail) and closes it, even when the
		// tour has ended and runs no backstop (#47).
		res = MissionResult{Status: MissionFailed, Reason: err.Error(), Code: CodeBridgeError}
		assignee, ferr := settleFailed(ctx, MissionInput{OrderID: in.OrderID, CrewID: crewID}, nil, res.Reason, res.Code)
		if ferr != nil {
			return res, errors.Join(err, ferr)
		}
		// A recalled mission with no crew member: a claimer whose resume
		// it never read holds the order failed under it. Tell its tour,
		// as belayMission does (#113).
		crewID = cmp.Or(crewID, assignee)
	}
	// Corrections held for a resume that never came (#167).
	for _, h := range held {
		if err := logCorrectionDropped(ctx, h.Crew, in.OrderID, h.Body.HailID, "the order was "+res.Status+" before it resumed"); err != nil {
			return res, err
		}
	}
	if crewID == "" {
		return res, nil
	}
	if err := notifyTour(ctx, crewID, CrewSignal{Kind: SignalMissionDone, OrderID: in.OrderID, StaleBeam: res.StaleBeam}); err != nil {
		return res, err
	}
	return res, nil
}

// notifyTour sends sig to the current tour of crewID, read by a
// current-tour step: the tour the mission started on may have ended in a
// watch change meanwhile. A tour that ended (sickbay) never reads it. A tour
// that doesn't exist yet (the process died between next-tour and the tour's
// start) counts as delivered: that tour finds the order's state when it
// claims. The send is a durable step of its own, so its error, a missing
// tour's included, is recorded and a replay takes the same path.
func notifyTour(ctx dbos.Context, crewID string, sig CrewSignal) error {
	tour, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		cr, err := daemonFrom(c).store.GetCrew(c, crewID)
		if err != nil {
			return "", fmt.Errorf("crew %s: %w", crewID, err)
		}
		return tourWorkflowID(crewID, cr.TourNo), nil
	}, append([]dbos.StepOption{dbos.WithStepName("current-tour")}, storeRetries...)...)
	if err != nil {
		return err
	}
	err = dbos.Send(ctx, tour, sig, crewEventsTopic)
	if err != nil && ctx.Err() == nil && errors.Is(err, dbos.ErrNonExistentWorkflow) {
		daemonFrom(ctx).logger.Info("mission: tour not started yet; not telling it", "tour", tour, "signal", sig.Kind, "order", sig.OrderID)
		return nil
	}
	return err
}

// syntheticMessage is a bridge-made comms message of type typ and body to
// orderID's mission, naming crewID. Its TS is zero, keeping the recorded
// send free of wall-clock noise.
func syntheticMessage(typ string, body any, crewID, orderID string) (comms.Message, error) {
	msg, err := comms.NewMessage(typ, body)
	if err != nil {
		return comms.Message{}, err
	}
	msg.TS = time.Time{}
	msg.Crew, msg.Order = crewID, orderID
	return msg, nil
}

// runMission is the mission's body. It returns, with its result, the crew
// member it last ran on ("" when it ends recalled and not resumed). It
// returns an error only when a step failed in a way the mission doesn't
// handle itself: DBOS shutting down, or a store step out of retries.
//
// Its state lives in local variables set only from recorded results (Recv
// and step outputs): the crew member and tour it runs on (in's, then a
// resume's; none after a recall), whether the order went out to the current
// beam, whether a stand_down (parking) or a recall (recalling) went to it
// and whether it is parked. Held corrections live in *held, which
// missionWorkflow logs as not delivered when the mission ends with any.
//
// Steps: ship-repos (repos.go: from the order's latest checkpoint, else
// base_ref), send-order with the repos it recorded and the checkpoint's
// handover (skipped when ship-repos found the tour ended; sending nothing
// when the tour ends while it retries, sendOrderStep), then per message
// from fromCrewTopic.
//
// A message naming another crew member than the current one (a late
// message from a beam the order has left, a synthetic error for a crew
// member it no longer has) is dropped. resume, belay and recall name none:
// they target the order itself (spec/phase-2-plan-2026-10-19.md section 7).
//
// Whether running or parked:
//   - belay (BelayOrder): order-belayed closes an open hail and settles the
//     order belayed (from away or hailing, or unclaimed and parked: a
//     recalled order), cascading to its child orders (#146); a running mission whose order is on the beam then
//     sends the beam the belay (send-belay). Returns belayed; a failed send
//     tells the tour StaleBeam. The budget backstop's belay first drains
//     what is queued behind it (drainReport, #119): a report from the beam
//     completes the order instead. An order the tour's backstop failed first
//     returns failed.
//
// While running:
//   - hail: record-hail (insert, crew hailing, log), then start the hail's
//     workflow (hail.go) without waiting on it. A hail with another ref
//     while one is open is queued, and recorded once the open one's answer
//     is sent (#119); a park or recall drops the queue.
//   - answer for the open hail: decide-hail (confirm the decision its
//     sender recorded, crew away, log), then send-answer, relaying its
//     decided_by. An order_split hail answered with option 0 runs
//     create-children in between (createChildren, #155), whose result
//     becomes the answer's decision: the children's ids, or why none were
//     created (option deny). A tracker request answered with option 0
//     runs tracker-window, tracker-request and tracker-record there
//     instead (runTrackerRequest, #54): the answer's decision says what
//     the bridge did, or why it didn't (option deny). A budget hail
//     answered extend (option 0) runs budget-extend there (extendBudget,
//     #119), and the answer carries the order's new limit.
//   - correction (CountermandHail; also while parked): send-correction
//     passes it to the beam. Parked, stopping (a stand_down or recall went
//     out) or off the beam, it is held, the latest per hail, and sent
//     right after the resume's send-order, in hail order (deliverHeld,
//     #167). For another crew member than the current one (or, held, than
//     the one it resumes on) or out of send retries, it is dropped with a
//     warning and a log line (log-correction-dropped); so is one still
//     held when the mission ends. The mission goes on.
//   - stand_down (the tour, at watch change): send-stand-down passes it to
//     the beam and the mission waits for its stopping checkpoint (parking).
//     With no order on the beam it parks at once; a failed send parks on
//     the checkpoints already pulled, telling the tour StaleBeam. Dropped
//     while a stop is pending.
//   - recall (RecallOrder): send-recall passes it to the beam and the
//     mission waits for its stopping checkpoint (recalling). With no order
//     on the beam it recalls at once; a failed send recalls on the
//     checkpoints already pulled, telling the tour StaleBeam. With a
//     stand_down pending, the beam is stopping already: the stop becomes a
//     recall. A second recall is dropped.
//   - checkpoint_ready: pull-checkpoint (each repo's bundle, the transcript
//     and the note into <home>/cargo/<order>/<beam>-<seq>/, checkpoints
//     row, log). One marked stopping recalls the order while recalling and
//     parks it while parking, before its pull; any other is just a
//     checkpoint.
//   - report: pull-work (each repo's bundle into
//     <home>/cargo/<order>/final/; failing for good fails the order as
//     cargo_lost), fetch-work (each into the bridge's clone as
//     bmus/<order>; see work.go), then order-done (close an open hail,
//     mark the order done, or split when create-children made it
//     children, and log the report with each repo's branch and commit,
//     or, when the tour's backstop ended the order meanwhile, log that
//     the work arrived late). An order settled split then takes a belay
//     queued meanwhile (belayAfterSplit, #146). Returns done. A report
//     that beats a stand_down or a recall completes the order all the
//     same.
//   - error: code dismissed or budget_exceeded fails the order
//     (order-failed: close an open hail, mark failed, log reason and code);
//     a budget stop with a stand_down or recall pending logs "<stop>
//     superseded by the budget stop" first. Code beam_lost (the tour's)
//     parks the mission on the checkpoints already pulled, or recalls it
//     while recalling; so does any error while parking or recalling (the
//     harness couldn't checkpoint). Any other fails the order. A
//     combadge_restarted error that follows a report never gets here: the
//     mission returned on the report, and a message sent to a finished
//     workflow is never read.
//   - timeout: order-failed, as above.
//
// Parking (park): close an open hail, set the order's parked_at (or find
// the backstop failed it: the mission then returns failed), and send
// mission_parked to the crew member's current tour. Recalling
// (recall-order) is parking that also returns the order to its division's
// pool (store.RecallOrder: unassigned, parked) and wakes the division;
// mission_parked goes to the recalled crew member's current tour, and the
// mission then runs on no crew member until a resume.
//
// A parked mission waits with no deadline. It takes resume, belay (above),
// recall while it still has a crew member (one parked by beam loss, in
// sickbay: recall-order at once) and the dismissed error, which fails the
// order; it drops everything else. resume: unpark (only while the order is
// away with the resume's crew member as assignee), then ship-repos and
// send-order again on the new crew member's beam. A resume unpark refuses
// (a recall moved the order first) tells the resuming tour mission_parked,
// so it doesn't wait on an order that is no longer its.
func runMission(ctx dbos.Context, in MissionInput, held *heldCorrections) (MissionResult, string, error) {
	d := daemonFrom(ctx)
	cur := in
	var (
		hail *openHail
		// queued are hails that arrived while another was open, recorded
		// in turn once it is answered (#119): the store keeps one open
		// hail per order.
		queued    []comms.HailBody
		start     = true // ship the repos and send the order next
		onBeam    bool   // the order went out to the current beam
		beam      string // that beam, where its checkpoints and work come from
		parking   bool   // a stand_down went to the beam
		recalling bool   // a recall went to the beam (or followed a stand_down)
		stopFor   string // the stand_down's or recall's reason
		parked    bool
	)
	// park parks the mission. A non-nil result is the mission's end: the
	// backstop had failed the order.
	park := func(reason string, stale bool) (*MissionResult, error) {
		res, err := parkMission(ctx, cur, reason, stale)
		if err != nil || res != nil {
			return res, err
		}
		hail, queued, parking, recalling, parked = nil, nil, false, false, true
		return nil, nil
	}
	// recall recalls the order and parks the mission with no crew member,
	// as park does.
	recall := func(reason string, stale bool) (*MissionResult, error) {
		if res, err := recallOrder(ctx, cur, reason); err != nil || res != nil {
			return res, err
		}
		if err := notifyTour(ctx, cur.CrewID, parkedSignal(cur, stale)); err != nil {
			return nil, err
		}
		hail, queued, parking, recalling, parked = nil, nil, false, false, true
		cur.CrewID, cur.TourID = "", ""
		return nil, nil
	}
	// raise records hail body (record-hail) and starts its workflow
	// without waiting on it, then waits on it.
	raise := func(body comms.HailBody) error {
		id, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
			return recordHail(c, daemonFrom(c), cur, body)
		}, append([]dbos.StepOption{dbos.WithStepName("record-hail")}, storeRetries...)...)
		if err != nil {
			return err
		}
		// Started, not awaited, under the hail's id: a replay, or a
		// hail InsertHail found open already, starts nothing new.
		if _, err := dbos.RunWorkflow(ctx, hailWorkflow, HailInput{HailID: id},
			dbos.WithWorkflowID(hailWorkflowID(id))); err != nil {
			return err
		}
		hail = &openHail{ID: id, Ref: body.Ref, Category: body.Category}
		return nil
	}
	for {
		if start {
			start, onBeam, beam = false, false, ""
			var (
				ship shipped
				err  error
				lw   loginWaiter
			)
			for {
				lw.mark()
				ship, err = dbos.RunAsStep(ctx, func(c context.Context) (shipped, error) {
					return shipRepos(c, daemonFrom(c), cur)
				}, append([]dbos.StepOption{dbos.WithStepName("ship-repos"),
					dbos.WithStepRetryPredicate(func(err error) bool {
						return !errors.Is(err, errRepoPermanent) && !errors.Is(err, errWorkPermanent)
					}),
				}, daemonFrom(ctx).copyRetries...)...)
				if err == nil || ctx.Err() != nil {
					break
				}
				again, werr := lw.retry(ctx, "ship-repos", cur.CrewID, in.OrderID)
				if werr != nil {
					return MissionResult{}, cur.CrewID, werr
				}
				if !again {
					break
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return MissionResult{}, cur.CrewID, err
				}
				return failMission(ctx, cur, nil, "could not ship the order's repos to the beam: "+err.Error(), CodeRepoFailed)
			}
			// With the tour ended no order goes out: the loop below reads the
			// tour's synthetic message, already queued, and parks or fails
			// the order with it. send-order records 0 for a tour that ended
			// while it retried (#195).
			if !ship.TourEnded {
				id, err := dbos.RunAsStep(ctx, func(c context.Context) (uint64, error) {
					return sendOrderStep(c, daemonFrom(c), cur, ship)
				}, dbos.WithStepName("send-order"))
				if err != nil {
					if ctx.Err() != nil {
						return MissionResult{}, cur.CrewID, err
					}
					return failMission(ctx, cur, nil, "could not send the order to the beam: "+err.Error(), CodeSendFailed)
				}
				if id != orderNotSent {
					onBeam, beam = true, ship.Beam
					if err := deliverHeld(ctx, cur, held); err != nil {
						return MissionResult{}, cur.CrewID, err
					}
				}
			}
		}

		msg, err := dbos.Recv[comms.Message](ctx, fromCrewTopic, missionWaitTimeout)
		if errors.Is(err, dbos.ErrTimeout) {
			if parked {
				continue
			}
			reason := fmt.Sprintf("no word from the crew for %s", missionWaitTimeout)
			if hail != nil {
				reason = fmt.Sprintf("hail %s unanswered for %s", hail.ID, missionWaitTimeout)
			}
			return failMission(ctx, cur, hail, reason, CodeTimeout)
		}
		if err != nil {
			if ctx.Err() != nil || !isRecvDecodeError(err) {
				return MissionResult{}, cur.CrewID, err
			}
			// Recorded, so a replay skips it the same way.
			d.logger.Warn("mission: ignoring undecodable message", "order", in.OrderID, "err", err)
			continue
		}

		switch msg.Type {
		case typeResume:
			var body resumeBody
			if err := json.Unmarshal(msg.Body, &body); err != nil || !parked || body.CrewID == "" {
				d.logger.Info("mission: dropping resume", "order", in.OrderID, "parked", parked, "err", err)
				// A tour whose resume lost a race (the mission runs on
				// another tour) is released.
				if err == nil && !parked && body.TourID != cur.TourID {
					if err := notifyResumer(ctx, in.OrderID, body.TourID); err != nil {
						return MissionResult{}, cur.CrewID, err
					}
				}
				continue
			}
			resumed, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
				return unpark(c, daemonFrom(c), in.OrderID, body.CrewID)
			}, append([]dbos.StepOption{dbos.WithStepName("unpark")}, storeRetries...)...)
			if err != nil {
				return MissionResult{}, cur.CrewID, err
			}
			if !resumed {
				// A recall reached the mission first: the resuming tour,
				// whose claim found the order still away with its crew
				// member, is told it isn't running it.
				d.logger.Info("mission: dropping resume for an order no longer away with its crew member", "order", in.OrderID, "crew", body.CrewID)
				if err := notifyResumer(ctx, in.OrderID, body.TourID); err != nil {
					return MissionResult{}, cur.CrewID, err
				}
				continue
			}
			cur.CrewID, cur.TourID = body.CrewID, body.TourID
			parked, start = false, true
			continue

		case comms.TypeBelay:
			var body comms.BelayBody
			_ = json.Unmarshal(msg.Body, &body)
			if body.Reason == budgetCeilingReason && onBeam && !parked {
				// The backstop's belay follows a usage report, and the
				// run's report may be right behind it: finished work is
				// kept (#119).
				report, err := drainReport(ctx, cur)
				if err != nil {
					return MissionResult{}, cur.CrewID, err
				}
				if report != nil {
					res, err := completeMission(ctx, cur, beam, hail, *report)
					return res, cur.CrewID, err
				}
			}
			return belayMission(ctx, cur, onBeam && !parked, body.Reason)

		case comms.TypeCorrection:
			// The captain countermanded a decision (CountermandHail).
			// Queued behind the first decision's answer, so send-answer
			// has gone first. Delivery is best effort: whatever stops it
			// is logged and the mission goes on.
			var body comms.CorrectionBody
			if err := json.Unmarshal(msg.Body, &body); err != nil {
				d.logger.Warn("mission: ignoring correction with a bad body", "order", in.OrderID, "err", err)
				continue
			}
			// Parked or stopping, it waits for the resume (#167): the
			// resumed run gets it right after its order.
			var why string
			switch {
			case parked:
			case msg.Crew != cur.CrewID:
				why = fmt.Sprintf("the order is no longer on %s", msg.Crew)
			case !onBeam || parking || recalling:
				// Off the beam only when ship-repos or send-order found the
				// tour ended: the tour's message, already queued, parks the
				// order.
			default:
				err := sendCorrection(ctx, cur, body)
				if err == nil {
					continue
				}
				if ctx.Err() != nil {
					return MissionResult{}, cur.CrewID, err
				}
				why = fmt.Sprintf("could not send it to %s: %v", cur.CrewID, err)
			}
			if why == "" {
				d.logger.Info("mission: holding correction for the resume", "order", in.OrderID, "hail", body.HailID)
				held.hold(msg.Crew, body)
				continue
			}
			if err := logCorrectionDropped(ctx, msg.Crew, in.OrderID, body.HailID, why); err != nil {
				return MissionResult{}, cur.CrewID, err
			}
			continue

		case comms.TypeRecall:
			var body comms.RecallBody
			_ = json.Unmarshal(msg.Body, &body)
			reason := cmp.Or(body.Reason, "recall")
			switch {
			case parked && cur.CrewID == "":
				d.logger.Info("mission: already recalled; dropping recall", "order", in.OrderID)
				continue
			case parked:
				// Parked on beam loss, its crew member in sickbay (or the
				// order is being resumed, which RecallOrder refuses).
				if res, err := recall(reason, false); err != nil || res != nil {
					return deref(res), cur.CrewID, err
				}
				continue
			case recalling:
				d.logger.Info("mission: already recalling; dropping recall", "order", in.OrderID)
				continue
			case !onBeam:
				if res, err := recall(reason+": no order on the beam", false); err != nil || res != nil {
					return deref(res), cur.CrewID, err
				}
				continue
			case parking:
				// The beam is stopping for the stand_down already, and
				// ignores a second stop: its checkpoint recalls instead.
				d.logger.Info("mission: standing down; its checkpoint recalls the order", "order", in.OrderID)
				recalling, stopFor = true, reason
				continue
			}
			_, err := dbos.RunAsStep(ctx, func(c context.Context) (uint64, error) {
				m, err := comms.NewMessage(comms.TypeRecall, comms.RecallBody{Reason: reason})
				if err != nil {
					return 0, err
				}
				m.Order = in.OrderID
				return daemonFrom(c).SendCommand(cur.CrewID, m)
			}, append([]dbos.StepOption{dbos.WithStepName("send-recall")}, sendRetries...)...)
			if err != nil {
				if ctx.Err() != nil {
					return MissionResult{}, cur.CrewID, err
				}
				// The beam may still be running the order: StaleBeam.
				if res, err := recall(fmt.Sprintf("%s: could not send the recall: %v", reason, err), true); err != nil || res != nil {
					return deref(res), cur.CrewID, err
				}
				continue
			}
			recalling, stopFor = true, reason
			continue
		}

		if msg.Crew != cur.CrewID {
			d.logger.Info("mission: dropping a message for another crew member", "order", in.OrderID, "type", msg.Type,
				"msg_crew", msg.Crew, "crew", cur.CrewID)
			continue
		}
		if parked {
			if msg.Type == comms.TypeError {
				if body := errorBody(msg); body.Code == CodeDismissed {
					return failMission(ctx, cur, nil, body.Reason, body.Code)
				}
			}
			d.logger.Info("mission: parked; dropping message", "order", in.OrderID, "type", msg.Type, "seq", msg.Seq)
			continue
		}

		switch msg.Type {
		case comms.TypeStandDown:
			if parking || recalling {
				d.logger.Info("mission: already stopping; dropping stand_down", "order", in.OrderID)
				continue
			}
			var body comms.StandDownBody
			_ = json.Unmarshal(msg.Body, &body)
			reason := cmp.Or(body.Reason, "stand down")
			if !onBeam {
				if res, err := park(reason+": no order on the beam", false); err != nil || res != nil {
					return deref(res), cur.CrewID, err
				}
				continue
			}
			_, err := dbos.RunAsStep(ctx, func(c context.Context) (uint64, error) {
				m, err := comms.NewMessage(comms.TypeStandDown, comms.StandDownBody{Reason: reason})
				if err != nil {
					return 0, err
				}
				m.Order = in.OrderID
				return daemonFrom(c).SendCommand(cur.CrewID, m)
			}, append([]dbos.StepOption{dbos.WithStepName("send-stand-down")}, sendRetries...)...)
			if err != nil {
				if ctx.Err() != nil {
					return MissionResult{}, cur.CrewID, err
				}
				// The beam may still be running the order: StaleBeam.
				if res, err := park(fmt.Sprintf("%s: could not send the stand_down: %v", reason, err), true); err != nil || res != nil {
					return deref(res), cur.CrewID, err
				}
				continue
			}
			parking, stopFor = true, reason

		case comms.TypeHail:
			var body comms.HailBody
			if err := json.Unmarshal(msg.Body, &body); err != nil {
				d.logger.Warn("mission: ignoring hail with a bad body", "order", in.OrderID, "seq", msg.Seq, "err", err)
				continue
			}
			// Another hail is open: this one waits its turn, so the open
			// hail keeps its own category and ref (#119).
			if hail != nil && body.Ref != hail.Ref {
				if !slices.ContainsFunc(queued, func(q comms.HailBody) bool { return q.Ref == body.Ref }) {
					d.logger.Info("mission: hail queued behind the open one", "order", in.OrderID, "ref", body.Ref,
						"category", body.Category, "open", hail.ID)
					queued = append(queued, body)
				}
				continue
			}
			if err := raise(body); err != nil {
				return MissionResult{}, cur.CrewID, err
			}

		case comms.TypeAnswer:
			var body comms.AnswerBody
			if err := json.Unmarshal(msg.Body, &body); err != nil || hail == nil || body.HailID != hail.ID {
				d.logger.Warn("mission: ignoring answer for a hail it isn't waiting on", "order", in.OrderID, "hail", body.HailID, "err", err)
				continue
			}
			body.Ref = hail.Ref
			// Its sender (AnswerHail, or the hail workflow's
			// decide-policy) already decided the hail with body's
			// decided_by, set the crew member away and logged it, in the
			// transaction that queued this message; decide-hail confirms
			// it (a no-op with the same decision).
			if err := storeStep(ctx, "decide-hail", func(c context.Context, d *Daemon) error {
				return decideHail(c, d, cur, body)
			}); err != nil {
				return MissionResult{}, cur.CrewID, err
			}
			// An allowed split creates the children before the answer
			// goes out, so the answer can name them (#155). Whoever
			// decided, the answer reaches the mission once, and the
			// mission reads one answer per hail: this runs once.
			if hail.Category == comms.CategoryOrderSplit && body.Option != nil && *body.Option == 0 {
				split, err := dbos.RunAsStep(ctx, func(c context.Context) (splitOutcome, error) {
					return createChildren(c, daemonFrom(c), cur, body.HailID)
				}, append([]dbos.StepOption{dbos.WithStepName("create-children")}, storeRetries...)...)
				if err != nil {
					return MissionResult{}, cur.CrewID, err
				}
				body.Decision = split.Decision
				if split.Refused {
					deny := 1
					body.Option = &deny
				}
			}
			// An extension raises the order's limit before the answer goes
			// out, so the answer can carry it (#119). Keyed by the hail: a
			// re-run extends nothing more.
			if hail.Category == comms.CategoryBudget && body.Option != nil && *body.Option == 0 {
				limit, err := dbos.RunAsStep(ctx, func(c context.Context) (comms.BudgetLimit, error) {
					return daemonFrom(c).extendBudget(c, cur, body.HailID)
				}, append([]dbos.StepOption{dbos.WithStepName("budget-extend")}, storeRetries...)...)
				if err != nil {
					return MissionResult{}, cur.CrewID, err
				}
				body.Budget = &limit
			}
			// An allowed tracker request runs before the answer goes out,
			// so the answer can say what the bridge did (#54).
			if isTrackerRequest(hail.Category) && body.Option != nil && *body.Option == 0 {
				out, err := runTrackerRequest(ctx, cur, body.HailID)
				if err != nil {
					return MissionResult{}, cur.CrewID, err
				}
				body.Decision = out.Decision
				if out.Refused {
					deny := 1
					body.Option = &deny
				}
			}
			hail = nil
			_, err := dbos.RunAsStep(ctx, func(c context.Context) (uint64, error) {
				m, err := comms.NewMessage(comms.TypeAnswer, body)
				if err != nil {
					return 0, err
				}
				m.Order = in.OrderID
				return daemonFrom(c).SendCommand(cur.CrewID, m)
			}, append([]dbos.StepOption{dbos.WithStepName("send-answer")}, sendRetries...)...)
			if err != nil {
				if ctx.Err() != nil {
					return MissionResult{}, cur.CrewID, err
				}
				return failMission(ctx, cur, nil, fmt.Sprintf("could not send the answer to hail %s: %v", body.HailID, err), CodeSendFailed)
			}
			if len(queued) > 0 {
				next := queued[0]
				queued = queued[1:]
				if err := raise(next); err != nil {
					return MissionResult{}, cur.CrewID, err
				}
			}

		case comms.TypeCheckpointReady:
			var body comms.CheckpointReadyBody
			if err := json.Unmarshal(msg.Body, &body); err != nil {
				d.logger.Warn("mission: ignoring checkpoint_ready with a bad body", "order", in.OrderID, "seq", msg.Seq, "err", err)
				continue
			}
			seq := msg.Seq
			// The checkpoint that answers the stand_down or recall parks
			// (or recalls) the order before its pull: a pull slower than
			// the tour's waits must not find the order failed by its
			// backstop, which leaves a parked order, and one no longer its
			// crew member's, alone. The tour learns of the park after the
			// pull (the beam must stay up for it), so it waits for the
			// pull up to its deadlines.
			stopping := body.Stopping && (parking || recalling)
			recalled := stopping && recalling
			if stopping {
				var (
					res *MissionResult
					err error
				)
				if recalled {
					res, err = recallOrder(ctx, cur, stopFor)
				} else {
					res, err = parkOrder(ctx, cur, stopFor)
				}
				if err != nil || res != nil {
					return deref(res), cur.CrewID, err
				}
				hail, queued, parking, recalling = nil, nil, false, false
			}
			var (
				err error
				lw  loginWaiter
			)
			for {
				lw.mark()
				_, err = dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
					return struct{}{}, pullCheckpoint(c, daemonFrom(c), cur, beam, seq, body)
				}, append([]dbos.StepOption{dbos.WithStepName("pull-checkpoint"),
					// A conflicting checkpoint row or a checkpoint that doesn't
					// match the order's repos won't go away by retrying.
					dbos.WithStepRetryPredicate(func(err error) bool {
						return !errors.Is(err, store.ErrCheckpointConflict) && !errors.Is(err, errWorkPermanent)
					}),
				}, daemonFrom(ctx).copyRetries...)...)
				if err == nil || ctx.Err() != nil {
					break
				}
				again, werr := lw.retry(ctx, "pull-checkpoint", cur.CrewID, in.OrderID)
				if werr != nil {
					return MissionResult{}, cur.CrewID, werr
				}
				if !again {
					break
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return MissionResult{}, cur.CrewID, err
				}
				// The work goes on without this checkpoint.
				text := fmt.Sprintf("checkpoint %d could not be pulled: %v", seq, err)
				code := CodeCheckpointFailed
				if errors.Is(err, errBundleTooLarge) {
					code = CodeBundleTooLarge
				}
				if err := storeStep(ctx, "log-checkpoint-failed", func(c context.Context, d *Daemon) error {
					return d.appendLog(c, logOrder, cur.CrewID, in.OrderID, text,
						map[string]any{"seq": seq, "reason": text, "code": code})
				}); err != nil {
					return MissionResult{}, cur.CrewID, err
				}
			}
			// The beam stopped after this one: parked, on it if it was
			// pulled, else on the earlier ones.
			if stopping {
				if err := notifyTour(ctx, cur.CrewID, parkedSignal(cur, false)); err != nil {
					return MissionResult{}, cur.CrewID, err
				}
				parked = true
				if recalled {
					cur.CrewID, cur.TourID = "", ""
				}
			}

		case comms.TypeReport:
			res, err := completeMission(ctx, cur, beam, hail, msg)
			return res, cur.CrewID, err

		case comms.TypeError:
			body := errorBody(msg)
			// A budget stop is the captain's answer: it fails the order
			// even with a stand_down or recall pending (#119).
			if body.Code == CodeBudgetExceeded && (parking || recalling) {
				what := comms.TypeStandDown
				if recalling {
					what = comms.TypeRecall
				}
				if err := storeStep(ctx, "log-stop-superseded", func(c context.Context, d *Daemon) error {
					return d.appendLog(c, logOrder, cur.CrewID, in.OrderID, what+" superseded by the budget stop",
						map[string]any{"code": body.Code, "stop": what})
				}); err != nil {
					return MissionResult{}, cur.CrewID, err
				}
			}
			if body.Code != CodeDismissed && body.Code != CodeBudgetExceeded && (body.Code == CodeBeamLost || parking || recalling) {
				stop := park
				if recalling {
					stop = recall
				}
				if res, err := stop(body.Reason, false); err != nil || res != nil {
					return deref(res), cur.CrewID, err
				}
				continue
			}
			return failMission(ctx, cur, hail, body.Reason, body.Code)

		default:
			d.logger.Warn("mission: ignoring message", "order", in.OrderID, "type", msg.Type, "seq", msg.Seq)
		}
	}
}

// backstopDrainWait is how long the backstop's belay waits for the
// run's report that may follow the usage report that passed the ceiling.
const backstopDrainWait = 2 * time.Second

// drainReport reads the messages queued behind the budget backstop's belay
// (#119), waiting at most backstopDrainWait for each, and returns the
// order's report from in.CrewID if one comes. Anything else is dropped with
// a log line: the belay ends the order.
func drainReport(ctx dbos.Context, in MissionInput) (*comms.Message, error) {
	d := daemonFrom(ctx)
	for {
		msg, err := dbos.Recv[comms.Message](ctx, fromCrewTopic, backstopDrainWait)
		switch {
		case errors.Is(err, dbos.ErrTimeout):
			return nil, nil
		case err != nil && (ctx.Err() != nil || !isRecvDecodeError(err)):
			return nil, err
		case err != nil:
			continue
		case msg.Type == comms.TypeReport && msg.Crew == in.CrewID:
			if err := storeStep(ctx, "log-ceiling-report", func(c context.Context, d *Daemon) error {
				return d.appendLog(c, logOrder, in.CrewID, in.OrderID, "budget ceiling belay set aside: the order's report came in first",
					map[string]any{"reason": budgetCeilingReason})
			}); err != nil {
				return nil, err
			}
			return &msg, nil
		}
		d.logger.Info("mission: dropping a message behind the budget ceiling's belay", "order", in.OrderID, "type", msg.Type, "seq", msg.Seq)
	}
}

// deref is *res, or the zero result for nil.
func deref(res *MissionResult) MissionResult {
	if res == nil {
		return MissionResult{}
	}
	return *res
}

// sendCorrection is the send-correction step: body to the order on
// in.CrewID's beam, with the hail row's question (#190). The run it
// reaches may never have had the first answer (one resumed after a park),
// and the question lets it name what the captain overrode. The row is
// read inside the step, so a replay doesn't read it again. The question
// is optional: a failed read is logged and the correction goes without
// one, so only the send spends sendRetries.
func sendCorrection(ctx dbos.Context, in MissionInput, body comms.CorrectionBody) error {
	_, err := dbos.RunAsStep(ctx, func(c context.Context) (uint64, error) {
		d := daemonFrom(c)
		if h, err := d.store.GetHail(c, body.HailID); err == nil {
			body.Question = h.Question
		} else {
			d.logger.Warn("mission: sending correction without the hail's question", "order", in.OrderID,
				"hail", body.HailID, "err", err)
		}
		m, err := comms.NewMessage(comms.TypeCorrection, body)
		if err != nil {
			return 0, err
		}
		m.Order = in.OrderID
		return d.SendCommand(in.CrewID, m)
	}, append([]dbos.StepOption{dbos.WithStepName("send-correction")}, sendRetries...)...)
	return err
}

// logCorrectionDropped is the log-correction-dropped step: "correction
// for hail H-0001 not delivered: <why>", with a warning.
func logCorrectionDropped(ctx dbos.Context, crewID, orderID, hailID, why string) error {
	daemonFrom(ctx).logger.Warn("mission: correction not delivered", "order", orderID, "hail", hailID, "why", why)
	return storeStep(ctx, "log-correction-dropped", func(c context.Context, d *Daemon) error {
		e, err := correctionDroppedEntry(crewID, orderID, hailID, why)
		if err != nil {
			return err
		}
		_, err = d.store.AppendLog(c, e)
		return err
	})
}

// deliverHeld sends the held corrections, in hail order, to the order
// just sent to in.CrewID's beam (#167). One for another crew member (a
// recalled order resumed elsewhere), or out of send retries, is logged as
// not delivered. Each leaves *held as it is taken, so a step failing
// partway (bridge_error) leaves missionWorkflow only those not yet
// handled to log.
func deliverHeld(ctx dbos.Context, in MissionInput, held *heldCorrections) error {
	slices.SortFunc(*held, func(a, b heldCorrection) int { return hailIDCompare(a.Body.HailID, b.Body.HailID) })
	for len(*held) > 0 {
		h := (*held)[0]
		*held = (*held)[1:]
		why := ""
		if h.Crew != in.CrewID {
			why = fmt.Sprintf("the order resumed on %s, not %s", in.CrewID, h.Crew)
		} else if err := sendCorrection(ctx, in, h.Body); err != nil {
			if ctx.Err() != nil {
				return err
			}
			why = fmt.Sprintf("could not send it to %s: %v", in.CrewID, err)
		}
		if why != "" {
			if err := logCorrectionDropped(ctx, h.Crew, in.OrderID, h.Body.HailID, why); err != nil {
				return err
			}
		}
	}
	return nil
}

// errorBody decodes an error message's body, with a reason always set.
func errorBody(msg comms.Message) comms.ErrorBody {
	var body comms.ErrorBody
	if err := json.Unmarshal(msg.Body, &body); err != nil {
		body = comms.ErrorBody{Reason: fmt.Sprintf("the crew reported an error with an undecodable body: %v", err)}
	}
	if body.Reason == "" {
		body.Reason = "the crew reported an error without a reason"
	}
	return body
}

// completeMission handles the order's report: pull-work, fetch-work and
// order-done (see runMission).
func completeMission(ctx dbos.Context, in MissionInput, beam string, hail *openHail, msg comms.Message) (MissionResult, error) {
	d := daemonFrom(ctx)
	var report comms.ReportBody
	if err := json.Unmarshal(msg.Body, &report); err != nil {
		d.logger.Warn("mission: report with a bad body", "order", in.OrderID, "seq", msg.Seq, "err", err)
	}
	var (
		landed []landedRepo
		err    error
		lw     loginWaiter
	)
	for {
		lw.mark()
		landed, err = dbos.RunAsStep(ctx, func(c context.Context) ([]landedRepo, error) {
			if h := daemonFrom(c).pullWorkHook; h != nil {
				h(daemonFrom(c), in.OrderID)
			}
			return pullWork(c, daemonFrom(c), in, beam, report.Repos)
		}, append([]dbos.StepOption{dbos.WithStepName("pull-work"),
			dbos.WithStepRetryPredicate(func(err error) bool { return !errors.Is(err, errWorkPermanent) }),
		}, daemonFrom(ctx).copyRetries...)...)
		if err == nil || ctx.Err() != nil {
			break
		}
		again, werr := lw.retry(ctx, "pull-work", in.CrewID, in.OrderID)
		if werr != nil {
			return MissionResult{}, werr
		}
		if !again {
			break
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return MissionResult{}, err
		}
		code := CodeCargoLost
		if errors.Is(err, errBundleTooLarge) {
			code = CodeBundleTooLarge
		}
		return failMissionResult(ctx, in, hail, "the order's work could not be pulled into the cargo bay: "+err.Error(), code)
	}
	// The work is safe in the cargo bay now: a fetch that fails for
	// good is logged and the order is still done.
	fetched, err := dbos.RunAsStep(ctx, func(c context.Context) ([]landedRepo, error) {
		return fetchWork(c, daemonFrom(c), in.OrderID, landed)
	}, append([]dbos.StepOption{dbos.WithStepName("fetch-work")}, daemonFrom(ctx).copyRetries...)...)
	if err != nil {
		if ctx.Err() != nil {
			return MissionResult{}, err
		}
		// Out of retries: no result was recorded, so the error
		// stands for every repo.
		fetched = make([]landedRepo, len(landed))
		for i, l := range landed {
			l.FetchError = firstLine(err.Error())
			fetched[i] = l
		}
	}
	if failed := fetchFailures(fetched); failed != "" {
		text := "the order's work is in the cargo bay but could not be fetched into the bridge's clone: " + failed
		if err := storeStep(ctx, "log-fetch-failed", func(c context.Context, d *Daemon) error {
			return d.appendLog(c, logOrder, in.CrewID, in.OrderID, text, map[string]any{"reason": text, "code": CodeFetchFailed})
		}); err != nil {
			return MissionResult{}, err
		}
	}
	if rejected := workRejections(fetched); rejected != "" {
		text := "the order's work is in the cargo bay but its object check rejected it: " + rejected
		if err := storeStep(ctx, "log-work-rejected", func(c context.Context, d *Daemon) error {
			return d.appendLog(c, logOrder, in.CrewID, in.OrderID, text, map[string]any{"reason": text, "code": CodeWorkRejected})
		}); err != nil {
			return MissionResult{}, err
		}
	}
	payload := donePayload{Beam: beam, Summary: report.Summary, Repos: fetched, Artifacts: report.Artifacts, Usage: report.Usage}
	done, err := dbos.RunAsStep(ctx, func(c context.Context) (doneOutcome, error) {
		d := daemonFrom(c)
		if err := d.closeHail(c, in, hail); err != nil {
			return doneOutcome{}, err
		}
		// An order create-children split settles split, waiting on its
		// children; the store settles it from there (#155). Only this
		// mission creates children of its order, before this step.
		split, err := d.store.HasChildren(c, in.OrderID)
		if err != nil {
			return doneOutcome{}, err
		}
		state, text := store.OrderDone, doneText(report.Summary, fetched)
		if split {
			state, text = store.OrderSplit, "split, waiting on its child orders: "+text
		}
		entry, err := logEntry(logDone, in.CrewID, in.OrderID, text, payload)
		if err != nil {
			return doneOutcome{}, err
		}
		// The tour's backstop may have failed the order while its
		// work was still being pulled (a slow copy can outlast the
		// settle wait). The order then stays failed, and the work,
		// kept in the cargo bay and the clone, gets one entry.
		late, err := logEntry(logOrder, in.CrewID, in.OrderID, lateText(fetched), payload)
		if err != nil {
			return doneOutcome{}, err
		}
		current, _, err := d.store.SettleOrder(c, in.OrderID, state, entry, &late)
		if err != nil {
			return doneOutcome{}, err
		}
		d.wakeReady(c)
		return doneOutcome{Split: state == store.OrderSplit && current == store.OrderSplit}, nil
	}, append([]dbos.StepOption{dbos.WithStepName("order-done")}, storeRetries...)...)
	if err != nil {
		return MissionResult{}, err
	}
	if done.Split {
		if err := belayAfterSplit(ctx, in); err != nil {
			return MissionResult{}, err
		}
	}
	return MissionResult{Status: MissionDone, Report: &report}, nil
}

// doneOutcome is the order-done step's recorded outcome. A mission
// recorded before #146 has "{}": not split.
type doneOutcome struct {
	Split bool `json:"split,omitempty"` // the order is split (this step, or its earlier run, settled it so)
}

// belayAfterSplit takes a belay the captain queued while the mission was
// pulling its report (#146): order-done settled the order split, and the
// mission would end with the belay unread. It drains fromCrewTopic with
// Recv calls of a millisecond until one times out; DBOS records each, so a
// replay reads the same messages. A belay sent while the order was away
// committed before order-done did (the store's single writer): the drain
// sees it. A later one finds the order split and goes through belaySplit.
// Other messages are dropped, as the mission would leave them unread.
// With a belay, step belay-split settles the split order belayed with the
// first belay's reason and cascades to its children
// (store.BelaySplitOrder); an order its last child settled first is left
// alone. The mission still ends done: its report arrived.
func belayAfterSplit(ctx dbos.Context, in MissionInput) error {
	d := daemonFrom(ctx)
	var belay *comms.BelayBody
	for {
		msg, err := dbos.Recv[comms.Message](ctx, fromCrewTopic, time.Millisecond)
		if errors.Is(err, dbos.ErrTimeout) {
			break
		}
		if err != nil {
			if ctx.Err() != nil || !isRecvDecodeError(err) {
				return err
			}
			continue
		}
		if msg.Type != comms.TypeBelay || belay != nil {
			d.logger.Info("mission: split; dropping message", "order", in.OrderID, "type", msg.Type, "seq", msg.Seq)
			continue
		}
		var body comms.BelayBody
		if err := json.Unmarshal(msg.Body, &body); err != nil {
			d.logger.Warn("mission: belay with a bad body", "order", in.OrderID, "err", err)
		}
		belay = &body
	}
	if belay == nil {
		return nil
	}
	return storeStep(ctx, "belay-split", func(c context.Context, d *Daemon) error {
		_, _, _, err := d.belaySplitOrder(c, in.OrderID, in.CrewID, belay.Reason)
		return err
	})
}

// parkOutcome is the park step's recorded outcome.
type parkOutcome struct {
	// Open: the order is still away, now parked. Otherwise it had ended
	// (the tour's backstop) in State.
	Open  bool
	State string `json:",omitempty"`
}

// parkMission runs the park step and, with the order parked, sends
// mission_parked (with stale, StaleBeam) to the current tour of in.CrewID.
// A non-nil result means the order had ended instead: the mission returns
// it, failed.
func parkMission(ctx dbos.Context, in MissionInput, reason string, stale bool) (*MissionResult, error) {
	if res, err := parkOrder(ctx, in, reason); err != nil || res != nil {
		return res, err
	}
	return nil, notifyTour(ctx, in.CrewID, parkedSignal(in, stale))
}

// parkedSignal is the mission_parked signal of a mission running as in. It
// names the tour the mission ran under, so the crew member's next tour,
// which may have resumed the order meanwhile, can tell a late one apart.
func parkedSignal(in MissionInput, stale bool) CrewSignal {
	return CrewSignal{Kind: SignalMissionParked, OrderID: in.OrderID, TourID: in.TourID, StaleBeam: stale}
}

// parkOrder runs the park step (see park). A non-nil result means the order
// had ended instead: the mission returns it, failed.
func parkOrder(ctx dbos.Context, in MissionInput, reason string) (*MissionResult, error) {
	out, err := dbos.RunAsStep(ctx, func(c context.Context) (parkOutcome, error) {
		return daemonFrom(c).park(c, in, reason)
	}, append([]dbos.StepOption{dbos.WithStepName("park")}, storeRetries...)...)
	if err != nil {
		return nil, err
	}
	if !out.Open {
		return &MissionResult{Status: MissionFailed, Reason: "the order was " + out.State + " before its mission could park",
			Code: CodeUnsettled}, nil
	}
	return nil, nil
}

// park is the park step: it closes the order's open hail ("closed
// unanswered: mission parked") and marks the order parked, logging
// "order parked: <reason>" (store.ParkOrder). Both writes are conditional,
// so a re-run writes nothing.
func (d *Daemon) park(ctx context.Context, in MissionInput, reason string) (parkOutcome, error) {
	h, found, err := d.store.OpenHailForOrder(ctx, in.OrderID)
	if err != nil {
		return parkOutcome{}, err
	}
	if found {
		if err := d.closeHailAs(ctx, in, &openHail{ID: h.ID}, ": mission parked"); err != nil {
			return parkOutcome{}, err
		}
	}
	entry, err := logEntry(logOrder, in.CrewID, in.OrderID, "order parked: "+reason, map[string]string{"reason": reason})
	if err != nil {
		return parkOutcome{}, err
	}
	open, err := d.store.ParkOrder(ctx, in.OrderID, entry)
	if err != nil || open {
		return parkOutcome{Open: open}, err
	}
	o, err := d.store.GetOrder(ctx, in.OrderID)
	if err != nil {
		return parkOutcome{}, err
	}
	return parkOutcome{State: o.State}, nil
}

// recallOrder runs the recall-order step (see Daemon.recall). A non-nil
// result means the order had ended instead: the mission returns it, failed.
func recallOrder(ctx dbos.Context, in MissionInput, reason string) (*MissionResult, error) {
	out, err := dbos.RunAsStep(ctx, func(c context.Context) (parkOutcome, error) {
		return daemonFrom(c).recall(c, in, reason)
	}, append([]dbos.StepOption{dbos.WithStepName("recall-order")}, storeRetries...)...)
	if err != nil {
		return nil, err
	}
	if !out.Open {
		return &MissionResult{Status: MissionFailed, Reason: "the order was " + out.State + " before its mission could be recalled",
			Code: CodeUnsettled}, nil
	}
	return nil, nil
}

// recall is the recall-order step (#101): it closes the order's open hail
// ("closed unanswered: order recalled") and returns the order to its
// division's pool, unassigned and parked, logging "order recalled:
// <reason>" (store.RecallOrder, only while in.CrewID is the assignee), then
// wakes the division (wakeDivision), whose idle tours claim it and resume
// the parked mission. Both writes are conditional, so a re-run writes
// nothing; it wakes again, which is harmless. The outcome's Open is false
// when the order had ended instead (the tour's backstop).
func (d *Daemon) recall(ctx context.Context, in MissionInput, reason string) (parkOutcome, error) {
	h, found, err := d.store.OpenHailForOrder(ctx, in.OrderID)
	if err != nil {
		return parkOutcome{}, err
	}
	if found {
		if err := d.closeHailAs(ctx, in, &openHail{ID: h.ID}, ": order recalled"); err != nil {
			return parkOutcome{}, err
		}
	}
	entry, err := logEntry(logOrder, in.CrewID, in.OrderID, "order recalled: "+reason, map[string]string{"reason": reason})
	if err != nil {
		return parkOutcome{}, err
	}
	state, _, err := d.store.RecallOrder(ctx, in.OrderID, in.CrewID, entry)
	if err != nil {
		return parkOutcome{}, err
	}
	switch state {
	case store.OrderPool, store.OrderDirected, store.OrderAway, store.OrderHailing:
		// Recalled, now or by an earlier run (and maybe claimed or routed
		// since).
	default:
		return parkOutcome{State: state}, nil
	}
	o, err := d.store.GetOrder(ctx, in.OrderID)
	if err != nil {
		return parkOutcome{}, err
	}
	d.wakeDivision(ctx, o.Division)
	return parkOutcome{Open: true}, nil
}

// notifyResumer tells tourID, which sent a resume the mission dropped
// because a recall had moved the order first, that the mission isn't its:
// mission_parked, naming that tour. Its claim found the order away with its
// crew member before the recall moved it, and it would otherwise wait on
// the order for good (mission-alive reads the parked mission alive). A send
// to a tour that has ended meanwhile is unread; one that doesn't exist
// counts as delivered, as in notifyTour.
func notifyResumer(ctx dbos.Context, orderID, tourID string) error {
	if tourID == "" {
		return nil
	}
	err := dbos.Send(ctx, tourID, CrewSignal{Kind: SignalMissionParked, OrderID: orderID, TourID: tourID}, crewEventsTopic)
	if err != nil && ctx.Err() == nil && errors.Is(err, dbos.ErrNonExistentWorkflow) {
		return nil
	}
	return err
}

// belayOutcome is the order-belayed step's recorded outcome.
type belayOutcome struct {
	State    string // the order's state afterwards
	Assignee string // its assignee when it was belayed
}

// belayMission ends the mission belayed (#101): the order-belayed step
// closes the order's open hail ("closed unanswered: order belayed") and
// settles the order belayed, logging "order belayed[: <reason>]"
// (store.BelayOrder: from away or hailing, or unclaimed and parked), and
// in the same transaction cascades to the order's child orders (#146):
// withdrawn, or sent a belay of their own (belayChild). An
// order that had ended otherwise (the tour's backstop failed it) returns
// failed.
//
// When the order is on a beam (send), send-belay passes the belay to the
// beam, and the mission waits for the run to end there (awaitBelayed)
// before it returns and frees its tour: a belay that reaches the harness
// late (serve holds it while it checks the repos out) must not find the
// tour's next order sent meanwhile, which serve would refuse as busy. A
// failed send, or no end within belayTimeout, leaves the result StaleBeam.
//
// It returns, with the result, the crew member whose tour to tell: in's,
// or for a mission with none (a recalled order claimed, its resume not yet
// read) the assignee order-belayed found, whose tour holds the order.
func belayMission(ctx dbos.Context, in MissionInput, send bool, reason string) (MissionResult, string, error) {
	out, err := dbos.RunAsStep(ctx, func(c context.Context) (belayOutcome, error) {
		d := daemonFrom(c)
		h, found, err := d.store.OpenHailForOrder(c, in.OrderID)
		if err != nil {
			return belayOutcome{}, err
		}
		if found {
			if err := d.closeHailAs(c, in, &openHail{ID: h.ID}, ": order belayed"); err != nil {
				return belayOutcome{}, err
			}
		}
		text := "order belayed"
		if reason != "" {
			text += ": " + reason
		}
		entry, err := logEntry(logOrder, in.CrewID, in.OrderID, text, map[string]string{"reason": reason})
		if err != nil {
			return belayOutcome{}, err
		}
		state, assignee, _, err := d.store.BelayOrder(c, in.OrderID, entry, d.belayChild)
		return belayOutcome{State: state, Assignee: assignee}, err
	}, append([]dbos.StepOption{dbos.WithStepName("order-belayed")}, storeRetries...)...)
	if err != nil {
		return MissionResult{}, in.CrewID, err
	}
	crew := cmp.Or(in.CrewID, out.Assignee)
	res := MissionResult{Status: MissionBelayed, Reason: reason}
	if out.State != store.OrderBelayed {
		res = MissionResult{Status: MissionFailed, Reason: "the order was " + out.State + " before its mission could be belayed",
			Code: CodeUnsettled}
	}
	if !send {
		return res, crew, nil
	}
	_, err = dbos.RunAsStep(ctx, func(c context.Context) (uint64, error) {
		m, err := comms.NewMessage(comms.TypeBelay, comms.BelayBody{Reason: reason})
		if err != nil {
			return 0, err
		}
		m.Order = in.OrderID
		return daemonFrom(c).SendCommand(in.CrewID, m)
	}, append([]dbos.StepOption{dbos.WithStepName("send-belay")}, sendRetries...)...)
	if err != nil {
		if ctx.Err() != nil {
			return MissionResult{}, crew, err
		}
		// The order is settled all the same; the beam may still be
		// running it.
		res.StaleBeam = true
		return res, crew, nil
	}
	ended, err := awaitBelayed(ctx, in)
	if err != nil {
		return MissionResult{}, crew, err
	}
	res.StaleBeam = !ended
	return res, crew, nil
}

// awaitBelayed waits, up to belayTimeout (a recorded deadline), for the
// belayed order's run to end on in.CrewID's beam: its error (the harness's
// "belayed", or any other), its report (a run that finished before the
// belay reached it), a stopping checkpoint (a stand_down or recall ended
// the run first: serve then drops the belay and emits nothing more), or
// the tour's synthetic beam_lost or dismissed, after which the beam no
// longer matters. Everything else for the order is
// dropped; the order is settled. It reports whether the run ended; only
// recorded outcomes decide, as in the tour's waits.
func awaitBelayed(ctx dbos.Context, in MissionInput) (bool, error) {
	d := daemonFrom(ctx)
	deadline, err := dbos.RunAsStep(ctx, func(c context.Context) (time.Time, error) {
		return time.Now().Add(daemonFrom(c).belayTimeout), nil
	}, dbos.WithStepName("belay-deadline"))
	if err != nil {
		return false, err
	}
	for {
		msg, err := dbos.Recv[comms.Message](ctx, fromCrewTopic, max(time.Until(deadline), time.Millisecond))
		if errors.Is(err, dbos.ErrTimeout) {
			d.logger.Warn("mission: the belayed run did not end on its beam", "order", in.OrderID, "crew", in.CrewID)
			return false, nil
		}
		if err != nil {
			if ctx.Err() != nil || !isRecvDecodeError(err) {
				return false, err
			}
			continue
		}
		if msg.Crew == in.CrewID {
			switch msg.Type {
			case comms.TypeError, comms.TypeReport:
				return true, nil
			case comms.TypeCheckpointReady:
				var body comms.CheckpointReadyBody
				if json.Unmarshal(msg.Body, &body) == nil && body.Stopping {
					return true, nil
				}
			}
		}
		d.logger.Info("mission: belayed; dropping message", "order", in.OrderID, "type", msg.Type, "seq", msg.Seq)
	}
}

// unpark is the unpark step: it clears the order's parked_at, logging
// "order resumed on <crew>", only while the order is away with crewID as
// its assignee (store.UnparkOrder), and reports whether it is resumed on
// crewID.
func unpark(ctx context.Context, d *Daemon, orderID, crewID string) (bool, error) {
	return d.store.UnparkOrder(ctx, orderID, crewID,
		store.LogEntry{Kind: logOrder, CrewID: crewID, OrderID: orderID, Text: "order resumed on " + crewID})
}

// failMission settles the order as failed and returns the failed result,
// with in's crew member.
func failMission(ctx dbos.Context, in MissionInput, hail *openHail, reason, code string) (MissionResult, string, error) {
	res, err := failMissionResult(ctx, in, hail, reason, code)
	return res, in.CrewID, err
}

// failMissionResult is failMission without the crew member.
func failMissionResult(ctx dbos.Context, in MissionInput, hail *openHail, reason, code string) (MissionResult, error) {
	if _, err := settleFailed(ctx, in, hail, reason, code); err != nil {
		return MissionResult{}, err
	}
	return MissionResult{Status: MissionFailed, Reason: reason, Code: code}, nil
}

// settleFailed runs the order-failed step: it closes an open hail (see
// closeHail), marks the order failed and logs reason and code, the last two
// only while the order is away or hailing (store.SettleOrder), or, for a
// mission with no crew member (recalled, not resumed), also while it is
// unclaimed and parked (store.SettleOpenOrder). Every write is idempotent.
//
// With a nil hail the step closes any hail still open for the order
// (closeOpenHail). The lookup runs inside the step, so a replay reuses the
// recorded outcome; a re-run finds the hail already closed. That covers
// bridge_error, where the body's open hail isn't known, including a step
// that failed for good while a hail was open (decide-hail,
// log-checkpoint-failed). Other nil-hail exits (send-order, send-answer
// failing) have no open hail and find none.
//
// For a mission with no crew member it returns the order's assignee
// SettleOpenOrder found: a claimer whose resume the mission never read
// ("" for an order still in the pool).
func settleFailed(ctx dbos.Context, in MissionInput, hail *openHail, reason, code string) (assignee string, err error) {
	// A struct, not a bare string: the step recorded an empty one before
	// #113, which decodes into it.
	out, err := dbos.RunAsStep(ctx, func(c context.Context) (failedOutcome, error) {
		d := daemonFrom(c)
		var err error
		if hail == nil {
			err = d.closeOpenHail(c, in)
		} else {
			err = d.closeHail(c, in, hail)
		}
		if err != nil {
			return failedOutcome{}, err
		}
		entry, err := logEntry(logFail, in.CrewID, in.OrderID, reason, map[string]string{"reason": reason, "code": code})
		if err != nil {
			return failedOutcome{}, err
		}
		if in.CrewID == "" {
			// A recalled mission not resumed (#101 review): its order is
			// unclaimed and parked (or claimed, its resume unread), and no
			// mission is left to run it.
			_, assignee, _, err := d.store.SettleOpenOrder(c, in.OrderID, store.OrderFailed, entry)
			return failedOutcome{Assignee: assignee}, err
		}
		// Only from away or hailing: an order the tour's backstop already
		// failed keeps its one terminal entry.
		_, _, err = d.store.SettleOrder(c, in.OrderID, store.OrderFailed, entry, nil)
		return failedOutcome{}, err
	}, append([]dbos.StepOption{dbos.WithStepName("order-failed")}, storeRetries...)...)
	return out.Assignee, err
}

// failedOutcome is the order-failed step's recorded outcome.
type failedOutcome struct {
	// Assignee, for a mission with no crew member, is the order's
	// assignee SettleOpenOrder found.
	Assignee string `json:",omitempty"`
}

// closeHail closes an open hail (nil-able) the mission is ending with as
// timed_out, the only unanswered-close status, and logs it, in one store
// transaction (store.TimeOutOpenHail): the open -> timed_out update is
// conditional and the "closed unanswered" entry is appended only when it
// changed the row. A hail already closed (decided by an AnswerHail whose
// message the mission never read, or timed out by the tour's backstop) is
// left as it is and not logged again, even when the mission's order-failed
// and the tour's backstop race on it; and a step re-run can't find the hail
// closed but unlogged. The hail's workflow is woken in the same
// transaction (notifyHailClosed). The mission's order-failed and
// order-done steps and the tour's backstop (settle-mission, tour-end) all
// use it.
func (d *Daemon) closeHail(ctx context.Context, in MissionInput, hail *openHail) error {
	return d.closeHailAs(ctx, in, hail, "")
}

// closeHailAs is closeHail with note appended to the entry's text.
func (d *Daemon) closeHailAs(ctx context.Context, in MissionInput, hail *openHail, note string) error {
	if hail == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]string{"hail_id": hail.ID})
	if err != nil {
		return err
	}
	_, err = d.store.TimeOutOpenHailThen(ctx, hail.ID, store.LogEntry{Kind: logHail, CrewID: in.CrewID, OrderID: in.OrderID,
		Text: "hail " + hail.ID + " closed unanswered" + note, Payload: payload},
		func(tx *sql.Tx) error { return d.notifyHailClosed(tx, hail.ID) })
	return err
}

// closeOpenHail closes the hail still open for in.OrderID, if any, as
// closeHail does.
func (d *Daemon) closeOpenHail(ctx context.Context, in MissionInput) error {
	h, found, err := d.store.OpenHailForOrder(ctx, in.OrderID)
	if err != nil || !found {
		return err
	}
	return d.closeHail(ctx, in, &openHail{ID: h.ID})
}

// orderNotSent is send-order's recorded result when the mission's tour
// ended before the order went out (#195). Command ids are the pump's clock
// in microseconds, never 0, so a mission recorded before #195 replays as it
// ran.
const orderNotSent uint64 = 0

// send-order's own retries, as sendRetries': about 2m45s in all.
const (
	sendOrderAttempts = 16
	sendOrderBackoff  = 500 * time.Millisecond
	sendOrderMaxWait  = 15 * time.Second
)

// sendOrderStep is the send-order step: sendOrder, retried while the pump
// (re)connects, as sendRetries would. It gives up with orderNotSent once
// the mission's tour has ended, as ship-repos does (endTour marks it,
// markTourEnded, after queueing the mission its synthetic message and
// before it stops the pump), or once the crew row says so (tourOver: a
// tour that ended before a bridge restart lost the mark). It checks before
// every attempt and wakes from a retry's wait on the mark, so a tour ending
// between ship-repos and send-order parks or fails the order with the
// tour's message within moments, not after the retries run out with a
// send_failed (#195). The step's recorded result decides, so a replay
// takes the same path.
func sendOrderStep(ctx context.Context, d *Daemon, in MissionInput, ship shipped) (uint64, error) {
	if d.sendOrderHook != nil {
		d.sendOrderHook(d, in.OrderID)
	}
	ended := d.tourEnded(in.TourID)
	wait := sendOrderBackoff
	for attempt := 1; ; attempt++ {
		select {
		case <-ended:
			return orderNotSent, nil
		default:
		}
		c, err := d.store.GetCrew(ctx, in.CrewID)
		if err != nil {
			err = fmt.Errorf("crew %s: %w", in.CrewID, err)
		} else if tourOver(c, in) {
			return orderNotSent, nil
		}
		var id uint64
		if err == nil {
			id, err = sendOrder(ctx, d, in, ship)
		}
		if err == nil || attempt == sendOrderAttempts {
			return id, err
		}
		d.logger.Warn("mission: send-order failed; retrying", "order", in.OrderID, "attempt", attempt, "err", err)
		select {
		case <-ended:
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, sendOrderMaxWait)
	}
}

// tourOver reports whether crew row c shows the tour behind in.TourID
// ended, for a mission whose in-memory end mark a bridge restart lost: the
// crew member is in sickbay or dismissed, or on a later tour (re-energized
// or handed over since), whose beam the order must not reach. An empty
// TourID (a mission run outside a tour) only goes by the status.
func tourOver(c store.Crew, in MissionInput) bool {
	if c.Status == store.CrewSickbay || c.Status == store.CrewDismissed {
		return true
	}
	return in.TourID != "" && tourWorkflowID(in.CrewID, c.TourNo) != in.TourID
}

// sendOrder sends the order command for in.OrderID, built from its orders
// row and the ship-repos step's recorded result: the repos, and the
// handover of the checkpoint they were shipped from. The model is the
// order's, else the crew member's, else the division's (resolveModel),
// resolved on every send: a resumed order's re-send resolves it for its new
// crew member. The budget is resolved the same way (resolveBudget), and the
// order carries its current limit and the usage recorded on the order's
// other beams (budgetFor, #118). A re-run sends again under a new command id, which serve
// ignores for an order it is running or has finished.
func sendOrder(ctx context.Context, d *Daemon, in MissionInput, ship shipped) (uint64, error) {
	o, err := d.store.GetOrder(ctx, in.OrderID)
	if err != nil {
		return 0, fmt.Errorf("order %s: %w", in.OrderID, err)
	}
	c, err := d.store.GetCrew(ctx, in.CrewID)
	if err != nil {
		return 0, fmt.Errorf("crew %s: %w", in.CrewID, err)
	}
	model, _ := d.resolveModel(o, c)
	budget, _ := d.resolveBudget(o, c)
	usage, err := d.store.OrderUsage(ctx, in.OrderID)
	if err != nil {
		return 0, fmt.Errorf("order %s usage: %w", in.OrderID, err)
	}
	limit, used := budgetFor(o, budget, usage, c.CurrentBeam)
	msg, err := comms.NewMessage(comms.TypeOrder, comms.OrderBody{
		Title: o.Title, Body: o.Body, Repos: ship.Repos,
		BudgetTokens: uint64(max(limit.Tokens, 0)), BudgetTurns: uint64(max(limit.Turns, 0)),
		UsedTokens: uint64(max(used.Tokens, 0)), UsedTurns: uint64(max(used.Turns, 0)),
		Model: model, Handover: ship.Handover,
	})
	if err != nil {
		return 0, err
	}
	msg.Order = in.OrderID
	return d.SendCommand(in.CrewID, msg)
}

// recordHail records an open hail for the order, sets the crew member
// hailing if it is away and logs it, in one transaction (store.InsertHail,
// which adds hail_id to the payload). A re-run finds the hail open and
// writes nothing. A mission that outlives its tour's settle wait leaves the
// tour's sickbay or dismissed alone (#43), and the next tour's status on
// another order too (#48). An order_split hail is stored with the bridge's
// question (splitQuestion), not the agent's (#155), and so is a tracker
// request (trackerQuestion, #54). A budget hail is stored with the
// bridge's question (budgetQuestion), its context's four counts and the
// options extend and stop (#119).
func recordHail(ctx context.Context, d *Daemon, in MissionInput, body comms.HailBody) (string, error) {
	if body.Category == comms.CategoryOrderSplit {
		o, err := d.store.GetOrder(ctx, in.OrderID)
		if err != nil {
			return "", fmt.Errorf("order %s: %w", in.OrderID, err)
		}
		body.Question = splitQuestion(o, body.Context)
	}
	if isTrackerRequest(body.Category) {
		body.Context = d.canonTrackerContext(body.Category, body.Context)
		body.Question = d.trackerQuestion(body.Category, body.Context)
	}
	if body.Category == comms.CategoryBudget {
		var c comms.BudgetContext
		body.Context, c = canonBudgetContext(body.Context)
		body.Question = budgetQuestion(in.OrderID, c)
		body.Options = []string{comms.OptionExtend, comms.OptionStop}
	}
	entry, err := logEntry(logHail, in.CrewID, in.OrderID, body.Question, map[string]any{
		"category": body.Category, "options": body.Options, "ref": body.Ref,
	})
	if err != nil {
		return "", err
	}
	return d.store.InsertHail(ctx, store.Hail{
		OrderID: in.OrderID, CrewID: in.CrewID, Category: body.Category,
		Question: body.Question, Options: body.Options, Context: body.Context,
	}, entry)
}

// hailDecidedEntry is the captain's log entry for a decision on a hail,
// with its rationale when it has one.
func hailDecidedEntry(crewID, orderID string, body comms.AnswerBody) (store.LogEntry, error) {
	payload := map[string]any{"hail_id": body.HailID, "decided_by": body.DecidedBy, "decision": body.Decision, "option": body.Option}
	if body.Rationale != "" {
		payload["rationale"] = body.Rationale
	}
	return logEntry(logHail, crewID, orderID, "hail "+body.HailID+" decided: "+body.Decision, payload)
}

// decideHail confirms a decision on the hail. Its sender (AnswerHail,
// decide-policy) already decided the hail, set the crew member back away
// and logged it in one transaction, so this is a no-op that writes nothing
// (store.MarkHailDecided with the same decision). A hail still open is
// decided, its crew member set back away (if hailing) and the decision
// logged, in one transaction. It doesn't wake the hail workflow
// (notifyHailClosed): every sender decided the hail first, and woke it or
// is it.
func decideHail(ctx context.Context, d *Daemon, in MissionInput, body comms.AnswerBody) error {
	entry, err := hailDecidedEntry(in.CrewID, in.OrderID, body)
	if err != nil {
		return err
	}
	return d.store.MarkHailDecided(ctx, body.HailID, body.DecidedBy, body.Decision, body.Rationale, entry)
}

// Cargo bay file names in a checkpoint's directory, next to one
// <name>.bundle per repo.
const (
	cargoTranscript = "transcript.jsonl"
	cargoHandover   = "handover.md"
)

// cargoDir is where checkpoint seq of orderID from beam is kept:
// <home>/cargo/<order>/<beam>-<seq>/. Seqs restart on each beam (#100).
func (d *Daemon) cargoDir(orderID, beam string, seq uint64) string {
	return filepath.Join(d.home, "cargo", orderID, beam+"-"+strconv.FormatUint(seq, 10))
}

// pullCheckpoint copies a checkpoint's files out of the crew member's
// current beam into the cargo bay, keyed by that beam and seq, one bundle
// per repo of the order
// (<name>.bundle, matched as a report's are) plus the transcript and the
// note, and records it. A re-run overwrites the files (the copies replace
// atomically) and InsertCheckpoint accepts the identical row again.
func pullCheckpoint(ctx context.Context, d *Daemon, in MissionInput, beamAlias string, seq uint64, body comms.CheckpointReadyBody) error {
	o, err := d.store.GetOrder(ctx, in.OrderID)
	if err != nil {
		return fmt.Errorf("order %s: %w", in.OrderID, err)
	}
	repos, err := matchWork(in.OrderID, o.Repos, body.Repos)
	if err != nil {
		return err
	}
	beam, err := d.missionBeam(ctx, in.CrewID, beamAlias)
	if err != nil {
		return err
	}
	dir := d.cargoDir(in.OrderID, beam.Alias, seq)
	// tsh's CopyFrom needs the local directory to exist.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cp := store.Checkpoint{
		OrderID: in.OrderID, Beam: beam.Alias, Seq: int(seq),
		TranscriptPath: filepath.Join(dir, cargoTranscript),
		HandoverPath:   filepath.Join(dir, cargoHandover),
	}
	files := [][2]string{{body.TranscriptPath, cp.TranscriptPath}, {body.HandoverPath, cp.HandoverPath}}
	commits := map[string]string{}
	for _, r := range repos {
		size, err := d.transporter.Size(ctx, beam, r.BundlePath)
		if err != nil {
			return fmt.Errorf("size repo %s's bundle %s on %s: %w", r.Name, r.BundlePath, beam.Alias, err)
		}
		if size > d.maxBundleBytes {
			return fmt.Errorf("repo %s: bundle %s is %d bytes, over the %d byte cap: %w",
				r.Name, r.BundlePath, size, d.maxBundleBytes, errBundleTooLarge)
		}
		local := filepath.Join(dir, r.Name+".bundle")
		cp.Repos = append(cp.Repos, store.CheckpointRepo{Name: r.Name, Commit: r.Commit, BundlePath: local})
		files = append(files, [2]string{r.BundlePath, local})
		commits[r.Name] = r.Commit
	}
	for _, f := range files {
		if err := d.transporter.CopyFrom(ctx, beam, f[0], f[1]); err != nil {
			return fmt.Errorf("copy %s from %s: %w", f[0], beam.Alias, err)
		}
	}
	for _, r := range cp.Repos {
		if err := d.checkCopiedBundle(r.Name, r.BundlePath); err != nil {
			return err
		}
	}
	if err := d.store.InsertCheckpoint(ctx, cp); err != nil {
		return err
	}
	return d.appendLog(ctx, logOrder, in.CrewID, in.OrderID, fmt.Sprintf("checkpoint %d pulled into the cargo bay", seq),
		map[string]any{"beam": beam.Alias, "seq": seq, "commits": commits, "dir": dir, "stopping": body.Stopping})
}

// logEntry builds a captain's log entry with payload marshalled to JSON
// (nil means {}; a json.RawMessage goes in as is).
func logEntry(kind, crewID, orderID, text string, payload any) (store.LogEntry, error) {
	var raw json.RawMessage
	switch p := payload.(type) {
	case nil:
	case json.RawMessage:
		raw = p
	default:
		b, err := json.Marshal(p)
		if err != nil {
			return store.LogEntry{}, err
		}
		raw = b
	}
	return store.LogEntry{Kind: kind, CrewID: crewID, OrderID: orderID, Text: text, Payload: raw}, nil
}

// appendLog appends a captain's log entry with payload marshalled to JSON
// (nil means {}; a json.RawMessage goes in as is).
func (d *Daemon) appendLog(ctx context.Context, kind, crewID, orderID, text string, payload any) error {
	var raw json.RawMessage
	switch p := payload.(type) {
	case nil:
	case json.RawMessage:
		raw = p
	default:
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		raw = b
	}
	_, err := d.store.AppendLog(ctx, store.LogEntry{Kind: kind, CrewID: crewID, OrderID: orderID, Text: text, Payload: raw})
	return err
}
