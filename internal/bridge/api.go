package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// The daemon API: what the local RPC handlers (#17) call. Each method runs
// outside any workflow, on the daemon's DBOS context.

// API errors, returned wrapped.
var (
	// ErrNotFound: the crew member or hail doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrExists: AddCrew with an id already taken.
	ErrExists = errors.New("already exists")
	// ErrUnavailable: CreateOrder for a crew member in sickbay or
	// dismissed, who has no tour to take it.
	ErrUnavailable = errors.New("crew member unavailable")
)

// Harnesses are the harness names combadge serve accepts (--harness), and
// so a crew member's possible harnesses.
var Harnesses = []string{"sdk", "holodeck", "echo"}

// DefaultHarnesses are the harnesses any bridge can run: all but holodeck,
// which needs a script (Phase 1: a holodeck bridge's --holodeck-script, see
// Config.Harnesses).
var DefaultHarnesses = []string{"sdk", "echo"}

// AddCrew adds a crew member and starts its first tour,
// crew/<id>/tour/0, which provisions a beam and puts the crew member
// on_deck at its combadge's hello. The crew member is inserted beaming_in
// (#106): c.Status must be empty or beaming_in and c.TourNo 0 (later tours
// come from Phase 2's watch change); c.CurrentBeam is ignored. c.Harness
// must be empty or one of Harnesses. c.Division must be empty or in the
// roster (checkDivision). The resolved harness (resolveHarness: c's, else
// the division's, else sdk) must be one this bridge can run
// (checkHarness); the tour starts combadge serve with it, and passes no
// --harness for an empty one. c.Model is opaque, empty for the division's
// or the harness's default. An id already taken returns ErrExists. Once the crew members
// not dismissed reach TransporterLimits.MaxBeams, a new one is a bad
// request (#97).
func (d *Daemon) AddCrew(ctx context.Context, c store.Crew) error {
	if c.ID == "" || c.Name == "" {
		return badRequest("crew id and name are required")
	}
	if c.Harness != "" && !slices.Contains(Harnesses, c.Harness) {
		return badRequest("unknown harness %q (want one of %s)", c.Harness, strings.Join(Harnesses, ", "))
	}
	if err := d.checkDivision(c.Division); err != nil {
		return err
	}
	if err := d.checkHarness(c); err != nil {
		return err
	}
	if c.Status != "" && c.Status != store.CrewBeamingIn {
		return badRequest("a new crew member starts beaming_in, not %s", c.Status)
	}
	c.Status = store.CrewBeamingIn
	if c.TourNo != 0 {
		return badRequest("a new crew member starts on tour 0, not %d", c.TourNo)
	}
	c.CurrentBeam = ""
	// The daemon is the store's only writer, so check-then-insert can't race
	// another process; the insert's own error covers anything else.
	if _, err := d.store.GetCrew(ctx, c.ID); err == nil {
		return fmt.Errorf("crew %s: %w", c.ID, ErrExists)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Each crew member not dismissed holds at most one beam (spec
	// phase-2-plan §5), so the cap on beams caps them.
	err := d.store.InsertCrewWithin(ctx, c, d.limits.MaxBeams, store.LogEntry{Kind: logCrew, CrewID: c.ID, Text: "crew member added: " + c.Name})
	if errors.Is(err, store.ErrRosterFull) {
		return badRequest("beam cap reached (transporter.max_beams %d): dismiss a crew member or raise max_beams in the roster",
			d.limits.MaxBeams)
	}
	if err != nil {
		return fmt.Errorf("add crew %s: %w", c.ID, err)
	}
	if _, err := dbos.RunWorkflow(d.dbos, crewTourWorkflow, CrewTourInput{CrewID: c.ID, TourNo: c.TourNo},
		dbos.WithWorkflowID(tourWorkflowID(c.ID, c.TourNo))); err != nil {
		return fmt.Errorf("start tour of %s: %w", c.ID, err)
	}
	return nil
}

// CreateOrder inserts an order (see store.InsertOrder: directed when
// o.Assignee is set, else a pool order) and returns its id. For a directed
// order it signals the assignee's tour with order_assigned; an idle tour
// claims it at once, a busy one after its mission. For a pool order it
// wakes the division (wakeDivision), whose idle tours claim it if standing
// orders are on. The assignee must exist (ErrNotFound) and not be in
// sickbay or dismissed (ErrUnavailable). o.Division must be empty or in the
// roster (checkDivision). An order with neither an assignee nor a division
// (it could never be claimed), or with a priority other than
// store.PriorityRoutine, PriorityYellow or PriorityRed, is a bad request.
//
// o.Repos must name exactly one repo of the registry (Config.Repos), with
// a base_ref (checkOrderRepos); anything else is a bad request. A
// base_ref of order:<id> (#143) must name an order that can still bring
// back work on that repo (checkOrderBases), and adds <id> to o.After when
// it isn't there.
//
// o.After are the orders it waits on (#142): no claim takes it until each
// has ended done. An after naming no order, the order itself, or one that
// ended failed, belayed or withdrawn is a bad request.
//
// o.Source is free-form. One naming a tracker item ("tracker:<ref>", #54)
// is read from the tracker into the order's title and body once every
// other check has passed (resolveSource); without a tracker it is a bad
// request. An order still without a title then is a bad request.
//
// If the signal can't be sent (no tour is running, say) the order is still
// created and nil is returned: the tour claims it after its next mission or
// wait timeout (missionCheckEvery).
func (d *Daemon) CreateOrder(ctx context.Context, o store.Order) (string, error) {
	id, _, err := d.createOrderAs(ctx, o, false)
	return id, err
}

// sourceError is a tracker error createOrderAs met reading the order's
// source (resolveSource), as a campaign's order steps tell it apart
// (#184): one wrapping neither ErrNotFound nor a bad request is worth a
// retry.
type sourceError struct{ err error }

func (e *sourceError) Error() string { return e.err.Error() }
func (e *sourceError) Unwrap() error { return e.err }

// createOrderAs is CreateOrder. With once, the order is inserted keyed on
// o.CreatedBy (store.InsertOrderOnce, #184): an order already created
// under the key is returned with created false, and nothing is written,
// so a re-run of a campaign's order step creates no second order. The
// crew member's tour is signalled, or the division woken, either way. A
// resolveSource error comes back wrapped in a sourceError.
func (d *Daemon) createOrderAs(ctx context.Context, o store.Order, once bool) (id string, created bool, err error) {
	if o.Assignee == "" && o.Division == "" {
		return "", false, badRequest("order assignee or division is required")
	}
	if o.Priority < store.PriorityRoutine || o.Priority > store.PriorityRed {
		return "", false, badRequest("order priority %d out of range (0 routine, 1 yellow, 2 red)", o.Priority)
	}
	var crew store.Crew
	if o.Assignee != "" {
		var err error
		crew, err = d.store.GetCrew(ctx, o.Assignee)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, fmt.Errorf("assignee %s: %w", o.Assignee, ErrNotFound)
		}
		if err != nil {
			return "", false, err
		}
		if crew.Status == store.CrewSickbay || crew.Status == store.CrewDismissed {
			return "", false, fmt.Errorf("assignee %s is %s: %w", crew.ID, crew.Status, ErrUnavailable)
		}
	}
	if err := d.checkDivision(o.Division); err != nil {
		return "", false, err
	}
	if err := d.checkOrderRepos(o.Repos); err != nil {
		return "", false, err
	}
	bases, err := d.checkOrderBases(ctx, o.Repos)
	if err != nil {
		return "", false, err
	}
	for _, id := range bases {
		if !slices.Contains(o.After, id) {
			o.After = append(o.After, id)
		}
	}
	if err := d.resolveSource(ctx, &o); err != nil {
		return "", false, &sourceError{err}
	}
	if strings.TrimSpace(o.Title) == "" {
		return "", false, badRequest("order title is required")
	}
	entry := store.LogEntry{Kind: logOrder, CrewID: o.Assignee, Text: "order created: " + o.Title}
	if o.Source != "" {
		var err error
		if entry, err = logEntry(logOrder, o.Assignee, "", entry.Text, map[string]any{"source": o.Source}); err != nil {
			return "", false, err
		}
	}
	if once {
		id, created, err = d.store.InsertOrderOnce(ctx, o, entry)
	} else {
		id, err = d.store.InsertOrder(ctx, o, entry)
		created = err == nil
	}
	if errors.Is(err, store.ErrAfter) {
		return "", false, badRequest("%s", strings.TrimSuffix(err.Error(), ": "+store.ErrAfter.Error()))
	}
	if errors.Is(err, store.ErrOrderKind) {
		return "", false, badRequest("%s", err.Error())
	}
	if err != nil {
		return "", false, fmt.Errorf("create order: %w", err)
	}
	if o.Assignee == "" {
		d.wakeDivision(ctx, o.Division)
		return id, created, nil
	}
	tour := tourWorkflowID(crew.ID, crew.TourNo)
	if err := dbos.Send(d.dbos, tour, CrewSignal{Kind: SignalOrderAssigned, OrderID: id}, crewEventsTopic); err != nil {
		d.logger.Warn("order created but its crew tour was not signalled", "order", id, "tour", tour, "err", err)
	}
	return id, created, nil
}

// AnswerHail delivers the captain's decision on an open hail to the mission
// waiting on it (order/<order>, topic from-crew, as a comms answer). option,
// if set, indexes the hail's options; an empty decision then defaults to that
// option's text. A budget hail (#119) needs an option: a decision naming
// one, in any case, picks it, and any other text is a bad request. It
// fails with store.ErrHailClosed (wrapped) if the hail isn't open and
// ErrNotFound if it doesn't exist.
//
// The decision (hails.status = decided), the crew member back away, its
// captain's log entry and the message to the mission commit in one store
// transaction, the comms pump's pattern
// (dbos.WithSendTransaction): of two concurrent calls exactly one succeeds
// and the other gets ErrHailClosed, and a decided hail always has its answer
// queued. The hail's workflow is woken in the same transaction
// (notifyHailClosed). It returns once the message is queued for the
// mission, not once the beam has it; the mission sends the answer command.
// A bridge-raised hail (#152) has no mission: nothing is sent to one, and
// notifyHailClosed wakes the workflow that raised it (deliver_to). A
// tracker_review hail's decision is checked against its context and
// stored in normal form, option 1 when it discards anything, else 0
// (checkReviewAnswer, #156); option 0 alone keeps every issue and option 1
// discards every one. A findings_triage hail is Command's alone and is
// refused. A campaign hail's decision is checked against its context and
// options and stored in normal form with its option (checkCampaignAnswer,
// #183).
func (d *Daemon) AnswerHail(ctx context.Context, hailID, decision string, option *int) error {
	h, err := d.store.GetHail(ctx, hailID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("hail %s: %w", hailID, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if h.Status != store.HailOpen {
		return fmt.Errorf("hail %s is %s: %w", hailID, h.Status, store.ErrHailClosed)
	}
	if h.Category == categoryFindingsTriage && h.DeliverTo != "" {
		return badRequest("hail %s is Command's advisory triage; review the issues in its tracker_review hail", hailID)
	}
	written := option
	if option != nil {
		if *option < 0 || *option >= len(h.Options) {
			return badRequest("hail %s: option %d out of range (%d options)", hailID, *option, len(h.Options))
		}
		if decision == "" {
			decision, written = h.Options[*option], nil
		}
	}
	if decision == "" {
		return badRequest("a decision or an option is required")
	}
	if h.Category == regs.CategoryTrackerReview && h.DeliverTo != "" {
		// Checked against the review's context, and stored in normal
		// form with its option (#156). An option alone is its text.
		if decision, option, err = d.checkReviewAnswer(h, decision, written); err != nil {
			return err
		}
	}
	if isCampaignHail(h) {
		// Checked against the hail's context and options, and stored in
		// normal form with its option (#183).
		if decision, option, err = checkCampaignAnswer(h, decision, written); err != nil {
			return err
		}
	}
	if h.Category == comms.CategoryBudget && option == nil {
		// The harness acts on the option: extend or stop (#119).
		i := slices.IndexFunc(h.Options, func(o string) bool { return strings.EqualFold(o, strings.TrimSpace(decision)) })
		if i < 0 {
			return badRequest("hail %s is a budget hail: answer with an option (%s)", hailID, strings.Join(h.Options, " or "))
		}
		option, decision = &i, h.Options[i]
	}
	msg, err := comms.NewMessage(comms.TypeAnswer, comms.AnswerBody{
		HailID: hailID, DecidedBy: comms.DecidedByCaptain, Decision: decision, Option: option,
	})
	if err != nil {
		return err
	}
	msg.Crew, msg.Order = h.CrewID, h.OrderID
	entry, err := hailDecidedEntry(h.CrewID, h.OrderID, comms.AnswerBody{
		HailID: hailID, DecidedBy: comms.DecidedByCaptain, Decision: decision, Option: option,
	})
	if err != nil {
		return err
	}
	return d.store.DecideOpenHail(ctx, hailID, comms.DecidedByCaptain, decision, "", entry, func(tx *sql.Tx) error {
		if h.DeliverTo != "" {
			// A bridge-raised hail (#152) has no mission: notifyHailClosed
			// wakes its raiser.
			return d.notifyHailClosed(tx, hailID)
		}
		if err := dbos.Send(d.dbos, missionWorkflowID(h.OrderID), msg, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
			return fmt.Errorf("deliver answer to %s: %w", missionWorkflowID(h.OrderID), err)
		}
		return d.notifyHailClosed(tx, hailID)
	})
}

// CountermandHail overrides a decision made without the captain on hail
// hailID, Command's or a captain timeout's default_option, while its
// countermand window is open (spec/phase-3-plan-2026-10-29.md section 6).
// option and decision are as AnswerHail's; regs.CheckAnswer doesn't apply:
// the captain is who its captain-only rules reserve the answer for.
//
// The agent already acted on the first answer, so it is sent a correction,
// not an answer: CorrectionBody{HailID, Ref: the order, Note: the decision
// and reason}. As AnswerHail's answer, it goes to the order's mission
// (fromCrewTopic, dbos.WithSendTransaction) in the store transaction that
// records the countermand (store.CountermandHail): decided_by captain, the
// decision, reason as rationale, countermanded_at and the log entry. The
// mission passes it to the beam (send-correction) behind anything queued
// before it, the first decision's answer included, and adds the hail's
// question as it sends it (#190). A mission that has ended is sent
// nothing: the countermand stands, "correction for hail H-0001 not
// delivered: ..." is logged after the commit, and ended is true. A hail
// that isn't countermandable fails with store.ErrCountermandExpired
// (wrapped), an open one is a bad request (answer it instead), and a
// missing one
// ErrNotFound.
//
// A bridge-raised hail (#152) has no order or agent to correct: the row and
// its log entry are written and nothing is sent, the raiser included. A
// campaign hail's countermand is checked and stored in normal form as
// AnswerHail's answer is (checkCampaignAnswer, #183). A
// raiser that acts on a countermandable decision must sleep past
// countermand_until and re-read the row before acting
// (spec/campaigns-stage-2-plan-2026-11-14.md section 2); awaitBridgeHail
// returns at the first decision and does not wait out the window.
func (d *Daemon) CountermandHail(ctx context.Context, hailID, decision string, option *int, reason string) (ended bool, err error) {
	h, err := d.store.GetHail(ctx, hailID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("hail %s: %w", hailID, ErrNotFound)
	}
	if err != nil {
		return false, err
	}
	if h.Status == store.HailOpen {
		return false, badRequest("hail %s is open: answer it (`bmus hails answer`)", hailID)
	}
	written := option
	if option != nil {
		if *option < 0 || *option >= len(h.Options) {
			return false, badRequest("hail %s: option %d out of range (%d options)", hailID, *option, len(h.Options))
		}
		if decision == "" {
			decision, written = h.Options[*option], nil
		}
	}
	if decision == "" {
		return false, badRequest("a decision or an option is required")
	}
	if isCampaignHail(h) {
		// The task acts on the countermand as on any decision (#183).
		if decision, option, err = checkCampaignAnswer(h, decision, written); err != nil {
			return false, err
		}
	}
	note := decision
	if reason != "" {
		note += " (" + reason + ")"
	}
	msg, err := syntheticMessage(comms.TypeCorrection, comms.CorrectionBody{HailID: hailID, Ref: h.OrderID, Note: note},
		h.CrewID, h.OrderID)
	if err != nil {
		return false, err
	}
	why := reason
	if why == "" {
		why = decision
	}
	payload := map[string]any{"hail_id": hailID, "decided_by": comms.DecidedByCaptain, "decision": decision, "option": option,
		"countermanded": h.DecidedBy}
	if reason != "" {
		payload["rationale"] = reason
	}
	entry, err := logEntry(logHail, h.CrewID, h.OrderID, "hail "+hailID+" countermanded: "+oneLine(why), payload)
	if err != nil {
		return false, err
	}
	if h.DeliverTo != "" {
		return false, d.store.CountermandHailOption(ctx, hailID, decision, option, reason, entry, nil)
	}
	mission := missionWorkflowID(h.OrderID)
	err = d.store.CountermandHailOption(ctx, hailID, decision, option, reason, entry, func(tx *sql.Tx) error {
		// Checked in the transaction, as notifyHailClosed checks the hail
		// workflow: the store's single writer keeps the mission from
		// ending between the check and the commit.
		live, _, err := workflowLive(d, mission)
		if err != nil {
			return err
		}
		if !live {
			ended = true
			return nil
		}
		if err := dbos.Send(d.dbos, mission, msg, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
			return fmt.Errorf("deliver correction to %s: %w", mission, err)
		}
		return nil
	})
	if err != nil || !ended {
		return ended, err
	}
	d.logger.Info("countermand recorded; the order's mission has ended, so no correction is sent", "hail", hailID, "order", h.OrderID)
	e, err := correctionDroppedEntry(h.CrewID, h.OrderID, hailID, "the order's mission has ended")
	if err == nil {
		_, err = d.store.AppendLog(ctx, e)
	}
	if err != nil {
		d.logger.Warn("could not log the undelivered correction", "hail", hailID, "err", err)
	}
	return true, nil
}

// correctionDroppedEntry is the captain's log entry for a correction that
// never reached the agent, and why.
func correctionDroppedEntry(crewID, orderID, hailID, why string) (store.LogEntry, error) {
	return logEntry(logHail, crewID, orderID, fmt.Sprintf("correction for hail %s not delivered: %s", hailID, why),
		map[string]any{"hail_id": hailID})
}

// BelayOrder abandons order id (spec/phase-2-plan-2026-10-19.md section
// 9): it logs "belay requested" and sends the order's mission a synthetic
// belay, in one store transaction (dbos.WithSendTransaction, as AnswerHail
// does). The mission settles the order belayed and, when the order is on a
// beam, passes the belay on; the settle cascades to the order's child
// orders (store.BelayOrder, #146). It takes an order that is away or
// hailing, or unclaimed with its mission parked (a recalled order, in the
// pool or routed to a crew member); an unclaimed order without one is
// withdrawn instead. A split order has no mission: it is belayed at once,
// its children with it (belaySplit). Any other state is a bad request,
// and a missing order ErrNotFound. It returns once the belay is queued
// for the mission, or done.
func (d *Daemon) BelayOrder(ctx context.Context, id, reason string) error {
	_, err := d.belayOrder(ctx, id, reason)
	return err
}

// errBelaySplit is belayOrder's check refusing a split order, which
// belaySplit takes instead.
var errBelaySplit = errors.New("split order")

// belayReach is what a belay reaches besides the order: Children, how
// many open child orders (#146), and Waiting, the orders waiting on it
// that it withdraws (#142). Settled is true when the call belayed the
// order itself (a split order) rather than queueing the belay for its
// mission.
type belayReach struct {
	Children int
	Waiting  []string
	Settled  bool
}

// belayOrder is BelayOrder, returning what the belay reaches. For a belay
// queued for a mission, those are the order's open children and the
// orders waiting on it, down the chain (store.WaitingChain), as the
// request read them; for a split order, what its cascades did.
func (d *Daemon) belayOrder(ctx context.Context, id, reason string) (belayReach, error) {
	var r belayReach
	err := d.store.RequestOrder(ctx, id, func(tx *sql.Tx, o store.Order, _ string) (store.LogEntry, error) {
		if err := belayCheck(o); err != nil {
			return store.LogEntry{}, err
		}
		waiting, err := store.WaitingChain(ctx, tx, id)
		if err != nil {
			return store.LogEntry{}, err
		}
		r = belayReach{Children: o.OpenChildren, Waiting: waiting}
		return d.orderRequest(tx, o, comms.TypeBelay, comms.BelayBody{Reason: reason}, "belay requested", reason)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return belayReach{}, fmt.Errorf("order %s: %w", id, ErrNotFound)
	case errors.Is(err, errBelaySplit):
		return d.belaySplit(ctx, id, reason)
	case err != nil:
		return belayReach{}, err
	}
	return r, nil
}

// belayCheck is BelayOrder's check on order o: nil for an order whose
// mission can take a belay (away or hailing, or unclaimed and parked: a
// recalled order), errBelaySplit for a split order, and a bad request for
// any other.
func belayCheck(o store.Order) error {
	switch {
	case o.State == store.OrderAway || o.State == store.OrderHailing:
	case (o.State == store.OrderPool || o.State == store.OrderDirected) && o.ParkedAt != nil:
	case o.State == store.OrderPool || o.State == store.OrderDirected:
		return badRequest("order %s is %s and has no mission to belay: withdraw it (`bmus order withdraw`)", o.ID, o.State)
	case o.State == store.OrderSplit:
		return errBelaySplit
	default:
		return badRequest("order %s is %s, too late to belay", o.ID, o.State)
	}
	return nil
}

// belaySplit belays split order id (#146): its mission has ended, so
// belaySplitOrder settles it belayed and cascades. An order its last child
// settled meanwhile is a bad request ("too late to belay"). It returns the
// children the cascade reached, those it did not find already ended, and
// the orders waiting on any order it ended that it withdrew (#142).
func (d *Daemon) belaySplit(ctx context.Context, id, reason string) (belayReach, error) {
	o, err := d.store.GetOrder(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return belayReach{}, fmt.Errorf("order %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return belayReach{}, err
	}
	cctx, col := store.WithAfterCascade(ctx)
	current, cascade, belayed, err := d.belaySplitOrder(cctx, id, o.Assignee, reason)
	if err != nil {
		return belayReach{}, err
	}
	if !belayed {
		return belayReach{}, badRequest("order %s is %s, too late to belay", id, current)
	}
	r := belayReach{Waiting: col.IDs(), Settled: true}
	for _, c := range cascade {
		if c.Action != store.CascadeAlready {
			r.Children++
		}
	}
	return r, nil
}

// belaySplitOrder settles split order id belayed, logged "order
// belayed[: <reason>]" on assignee, and cascades to its children in the
// same transaction, sending in-flight ones a belay (store.BelaySplitOrder
// with belayChild): belaySplit, and a split order's mission when a belay
// was waiting for it (belayAfterSplit).
func (d *Daemon) belaySplitOrder(ctx context.Context, id, assignee, reason string) (string, []store.ChildBelay, bool, error) {
	text := "order belayed"
	if reason != "" {
		text += ": " + reason
	}
	entry, err := logEntry(logOrder, assignee, id, text, map[string]string{"reason": reason})
	if err != nil {
		return "", nil, false, err
	}
	return d.store.BelaySplitOrder(ctx, id, entry, d.belayChild)
}

// belayChild is the store.BelayFunc of a cascading belay (#146): it sends
// child order o's mission a synthetic belay with reason, in tx, as
// BelayOrder does (store.BelaySent). A mission not started yet is sent
// nothing (store.BelayNoMission). One that has ended would never read it:
// nothing is sent, and the cascade settles the child (store.BelayMissionEnded).
func (d *Daemon) belayChild(tx *sql.Tx, o store.Order, reason string) (store.BelayReach, error) {
	mission := missionWorkflowID(o.ID)
	live, found, err := workflowLive(d, mission)
	switch {
	case err != nil:
		return store.BelayNoMission, err
	case !found:
		return store.BelayNoMission, nil
	case !live:
		return store.BelayMissionEnded, nil
	}
	msg, err := syntheticMessage(comms.TypeBelay, comms.BelayBody{Reason: reason}, "", o.ID)
	if err != nil {
		return store.BelayNoMission, err
	}
	if err := dbos.Send(d.dbos, mission, msg, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
		return store.BelayNoMission, fmt.Errorf("deliver belay to %s: %w", mission, err)
	}
	return store.BelaySent, nil
}

// RecallOrder returns order id to its division's pool, from its last
// checkpoint (spec/phase-2-plan-2026-10-19.md section 9): it logs "recall
// requested" and sends the order's mission a synthetic recall, in one store
// transaction. The mission passes the recall to the beam and, at its
// stopping checkpoint, moves the order to the pool (unassigned, its mission
// parked) and wakes the division; any of its crew members may claim it
// next, the recalled one included. It takes an order that is away or
// hailing and has a division (a directed order without one has no pool to
// return to). A parked order (watch change, beam loss) qualifies only while
// its crew member is in sickbay or beaming in (#106: its new tour claims
// nothing until its hello, which can take a whole tsh login wait): one
// being handed to a new beam is a bad request ("being resumed by <crew>;
// try again"), and one whose crew member
// was dismissed is being failed by the dismissal. Any other state is a bad
// request, and a missing order ErrNotFound. It returns once the recall is
// queued for the mission.
func (d *Daemon) RecallOrder(ctx context.Context, id, reason string) error {
	return d.requestOrder(ctx, id, comms.TypeRecall, comms.RecallBody{Reason: reason}, "recall requested", reason,
		func(o store.Order, assigneeStatus string) error {
			switch {
			case o.State == store.OrderPool || o.State == store.OrderDirected:
				return badRequest("order %s is %s, not claimed yet: route it instead (`bmus order route`)", o.ID, o.State)
			case o.State == store.OrderSplit:
				return badRequest("order %s is split and its mission has ended; it waits on %d open child orders: "+
					"recall those instead, or belay it to end them all", o.ID, o.OpenChildren)
			case o.State != store.OrderAway && o.State != store.OrderHailing:
				return badRequest("order %s is %s, too late to recall", o.ID, o.State)
			case o.Division == "":
				return badRequest("order %s has no division, so there is no pool to recall it to: "+
					"belay it (`bmus order belay`), and give orders a division to recall them (`bmus order route --division`)", o.ID)
			case o.ParkedAt != nil && assigneeStatus == store.CrewDismissed:
				return badRequest("order %s is being failed: its crew member %s was dismissed", o.ID, o.Assignee)
			case o.ParkedAt != nil && assigneeStatus != store.CrewSickbay && assigneeStatus != store.CrewBeamingIn:
				return badRequest("order %s is being resumed by %s; try again", o.ID, o.Assignee)
			}
			return nil
		})
}

// requestOrder runs check on order id and its assignee's status, then logs
// "<what>[: <reason>]" and sends the order's mission a synthetic message of
// type typ with body, in one store transaction (store.RequestOrder): of a
// request check refuses, nothing is logged or sent. The message names no
// crew member; the mission takes it whoever it runs on.
func (d *Daemon) requestOrder(ctx context.Context, id, typ string, body any, what, reason string,
	check func(o store.Order, assigneeStatus string) error) error {
	err := d.store.RequestOrder(ctx, id, func(tx *sql.Tx, o store.Order, assigneeStatus string) (store.LogEntry, error) {
		if err := check(o, assigneeStatus); err != nil {
			return store.LogEntry{}, err
		}
		return d.orderRequest(tx, o, typ, body, what, reason)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("order %s: %w", id, ErrNotFound)
	}
	return err
}

// orderRequest sends order o's mission a synthetic message of type typ
// with body in tx, and returns the log entry "<what>[: <reason>]" for the
// caller to append in tx: requestOrder's, and the budget backstop's in
// its inbox transaction (budgetCeiling).
func (d *Daemon) orderRequest(tx *sql.Tx, o store.Order, typ string, body any, what, reason string) (store.LogEntry, error) {
	msg, err := syntheticMessage(typ, body, "", o.ID)
	if err != nil {
		return store.LogEntry{}, err
	}
	text := what
	if reason != "" {
		text += ": " + reason
	}
	entry, err := logEntry(logOrder, o.Assignee, o.ID, text, map[string]string{"reason": reason})
	if err != nil {
		return store.LogEntry{}, err
	}
	if err := dbos.Send(d.dbos, missionWorkflowID(o.ID), msg, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
		return store.LogEntry{}, fmt.Errorf("deliver %s to %s: %w", typ, missionWorkflowID(o.ID), err)
	}
	return entry, nil
}

// WithdrawOrder takes back order id before any crew member claims it
// (spec/phase-2-plan-2026-10-19.md section 3): store.WithdrawOrder moves it
// to withdrawn and logs "order withdrawn[: <reason>]". Only an unclaimed
// order (pool or directed) has no mission to tell. A recalled order still
// has its parked mission: "parked; use belay". Any other state is a bad
// request naming it, and a missing order ErrNotFound.
func (d *Daemon) WithdrawOrder(ctx context.Context, id, reason string) error {
	_, err := d.withdrawOrder(ctx, id, reason)
	return err
}

// withdrawOrder is WithdrawOrder. It also returns the orders waiting on
// it, down the chain, that the withdraw withdrew in its transaction
// (#142).
func (d *Daemon) withdrawOrder(ctx context.Context, id, reason string) ([]string, error) {
	text := "order withdrawn"
	if reason != "" {
		text += ": " + reason
	}
	entry, err := logEntry(logOrder, "", id, text, map[string]string{"reason": reason})
	if err != nil {
		return nil, err
	}
	cctx, col := store.WithAfterCascade(ctx)
	o, withdrawn, err := d.store.WithdrawOrder(cctx, id, entry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("order %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if withdrawn {
		return col.IDs(), nil
	}
	if (o.State == store.OrderPool || o.State == store.OrderDirected) && o.ParkedAt != nil {
		return nil, badRequest("order %s is parked; use belay (`bmus order belay`)", id)
	}
	return nil, badRequest("order %s is %s, too late to withdraw", id, o.State)
}

// RouteOrder changes the assignee or division of order id before any crew
// member claims it (spec/phase-2-plan-2026-10-19.md section 3). A nil
// assignee or division keeps the order's, and an empty one clears it, both
// resolved against the order as store.RouteOrder reads it in its
// transaction. With an assignee the order is directed to that crew member,
// who must exist (ErrNotFound) and not be in sickbay or dismissed
// (ErrUnavailable), as in CreateOrder: a kept assignee is checked too, so
// moving an order directed to a crew member in sickbay to a division takes
// an empty assignee. Without one it goes to the division's pool. The
// division must be empty or in the roster (checkDivision), and the order
// must keep an assignee or a division. A review routed to a crew member
// who worked on its task is a bad request, "ORD-0009 reviews task o/r#12,
// which sorell worked on" (store.ReviewBarredError, #182). The route logs "order routed to
// ..."; one that changes nothing writes and logs nothing. A claimed or
// settled order is a bad request ("too late to route"), and a missing one
// ErrNotFound. A routed recalled order keeps its parked mission, which its
// next claimer resumes. Last, as CreateOrder does, the assignee's tour is
// signalled, or the division woken (wakeDivision), best effort.
func (d *Daemon) RouteOrder(ctx context.Context, id string, assignee, division *string) error {
	if assignee == nil && division == nil {
		return badRequest("nothing to route: give an assignee or a division")
	}
	var crew store.Crew
	o, routed, err := d.store.RouteOrder(ctx, id, assignee, division,
		func(o store.Order, to, div string, c *store.Crew) (store.LogEntry, error) {
			if to == "" && div == "" {
				return store.LogEntry{}, badRequest("order %s would have neither an assignee nor a division", id)
			}
			if to != "" {
				if c == nil {
					return store.LogEntry{}, fmt.Errorf("assignee %s: %w", to, ErrNotFound)
				}
				if c.Status == store.CrewSickbay || c.Status == store.CrewDismissed {
					return store.LogEntry{}, fmt.Errorf("assignee %s is %s: %w", c.ID, c.Status, ErrUnavailable)
				}
				crew = *c
			}
			if err := d.checkDivision(div); err != nil {
				return store.LogEntry{}, err
			}
			text := "order routed to " + to
			if to == "" {
				text = "order routed to the " + div + " pool"
			}
			return logEntry(logOrder, to, id, text, map[string]string{"assignee": to, "division": div})
		})
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("order %s: %w", id, ErrNotFound)
	}
	if errors.Is(err, store.ErrReviewBarred) {
		return badRequest("%s", err.Error())
	}
	if err != nil {
		return err
	}
	if o.State != store.OrderPool && o.State != store.OrderDirected {
		return badRequest("order %s is %s, too late to route", id, o.State)
	}
	if !routed {
		return nil
	}
	if o.Assignee == "" {
		d.wakeDivision(ctx, o.Division)
		return nil
	}
	tour := tourWorkflowID(crew.ID, crew.TourNo)
	if err := dbos.Send(d.dbos, tour, CrewSignal{Kind: SignalOrderAssigned, OrderID: id}, crewEventsTopic); err != nil {
		d.logger.Warn("order routed but its crew tour was not signalled", "order", id, "tour", tour, "err", err)
	}
	return nil
}

// settingStandingOrders is the settings key of the standing orders switch,
// "on" or "off".
const settingStandingOrders = "standing_orders"

// StandingOrders reports whether standing orders are on. The first read
// seeds the setting from the roster's default (Config.StandingOrders),
// logging it; from then on the setting, which SetStandingOrders changes,
// wins over the roster.
func (d *Daemon) StandingOrders(ctx context.Context) (bool, error) {
	d.settingsMu.Lock()
	defer d.settingsMu.Unlock()
	v, err := d.store.GetSetting(ctx, settingStandingOrders)
	if errors.Is(err, sql.ErrNoRows) {
		on := d.standingOrdersDefault
		return on, d.setStandingOrders(ctx, on, " (roster default)")
	}
	if err != nil {
		return false, err
	}
	return v == "on", nil
}

// standingOrdersNow is what StandingOrders returns, without seeding the
// setting: for bridge.status, which only reads.
func (d *Daemon) standingOrdersNow(ctx context.Context) (bool, error) {
	v, err := d.store.GetSetting(ctx, settingStandingOrders)
	if errors.Is(err, sql.ErrNoRows) {
		return d.standingOrdersDefault, nil
	}
	if err != nil {
		return false, err
	}
	return v == "on", nil
}

// SetStandingOrders turns standing orders on or off, logging "standing
// orders on" or "standing orders off". Setting the current value changes
// and logs nothing. Turning them on (or setting on again) then wakes every
// division (wakeDivision), so idle tours claim from their pools at once.
func (d *Daemon) SetStandingOrders(ctx context.Context, on bool) error {
	d.settingsMu.Lock()
	err := d.setStandingOrders(ctx, on, "")
	// Not under settingsMu: every claim reads StandingOrders.
	d.settingsMu.Unlock()
	if err != nil || !on {
		return err
	}
	d.wakeDivision(ctx, "")
	return nil
}

func (d *Daemon) setStandingOrders(ctx context.Context, on bool, note string) error {
	v := "off"
	if on {
		v = "on"
	}
	return d.store.SetSetting(ctx, settingStandingOrders, v, store.LogEntry{Kind: logBridge, Text: "standing orders " + v + note})
}

// settingCommandGuidance is the settings key of the captain's guidance for
// Command's consults (#147, spec phase-3-plan section 1).
const settingCommandGuidance = "command_guidance"

// MaxCommandGuidance caps the guidance's length in bytes: every consult
// carries it.
const MaxCommandGuidance = 4096

// CommandGuidance returns the captain's guidance for Command, empty if none
// is set.
func (d *Daemon) CommandGuidance(ctx context.Context) (string, error) {
	v, err := d.store.GetSetting(ctx, settingCommandGuidance)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetCommandGuidance sets the captain's guidance for Command, trimmed of
// surrounding space; empty clears it. It logs "command guidance set", with
// the text in the payload, or "command guidance cleared". Setting the
// current value, or empty when none was ever set, changes and logs
// nothing. Guidance longer than MaxCommandGuidance is a bad request.
func (d *Daemon) SetCommandGuidance(ctx context.Context, guidance string) (string, error) {
	guidance = strings.TrimSpace(guidance)
	if len(guidance) > MaxCommandGuidance {
		return "", badRequest("guidance is %d bytes, over the %d limit", len(guidance), MaxCommandGuidance)
	}
	current, err := d.CommandGuidance(ctx)
	if err != nil || current == guidance {
		return guidance, err
	}
	entry := store.LogEntry{Kind: logBridge, Text: "command guidance cleared"}
	if guidance != "" {
		entry.Text = "command guidance set"
		if entry.Payload, err = json.Marshal(map[string]string{"guidance": guidance}); err != nil {
			return "", err
		}
	}
	return guidance, d.store.SetSetting(ctx, settingCommandGuidance, guidance, entry)
}

// wakeDivision sends order_assigned to the tour of every on-deck crew member
// of division, or of every division when division is empty (crew members
// without one have no pool and are left alone): the standing-orders
// dispatcher's wake-up (spec phase-2-plan section 4). Idle tours claim
// (claimOrder) and those that lose the race find nothing. It runs outside
// any workflow and is best effort: a failed send is logged, and the tour
// claims on its next wait timeout (missionCheckEvery). Crew members away
// claim after their mission.
//
// CreateOrder calls it for a pool order, SetStandingOrders when turning
// standing orders on, RouteOrder for an order routed to a pool, and the
// mission's recall-order for a recalled order (#101).
func (d *Daemon) wakeDivision(ctx context.Context, division string) {
	crew, err := d.store.ListCrew(ctx)
	if err != nil {
		d.logger.Warn("division not woken: list crew", "division", division, "err", err)
		return
	}
	for _, c := range crew {
		if c.Status != store.CrewOnDeck || c.Division == "" || (division != "" && c.Division != division) {
			continue
		}
		tour := tourWorkflowID(c.ID, c.TourNo)
		if err := d.wakeSend(tour, CrewSignal{Kind: SignalOrderAssigned}); err != nil {
			d.logger.Warn("crew tour not woken for its division's pool", "division", c.Division, "tour", tour, "err", err)
		}
	}
}

// settleReport is the order-done step's settle (store.SettleOrder) of
// order id as state, done or split, then wakeReady for it.
func (d *Daemon) settleReport(ctx context.Context, id, state string, entry, late store.LogEntry) (string, error) {
	current, _, err := d.store.SettleOrder(ctx, id, state, entry, &late)
	if err != nil {
		return "", err
	}
	d.wakeReady(ctx, id)
	return current, nil
}

// wakeReady wakes every crew member on deck (wakeOnDeck) when an
// unclaimed order waiting on order id, or on a split parent id's settle
// made done, now has each order in its after done (#142,
// store.HasReadyDependents). Its own crew member, still away, claims
// after its mission. Best effort, as wakeDivision.
func (d *Daemon) wakeReady(ctx context.Context, id string) {
	ready, err := d.store.HasReadyDependents(ctx, id)
	if err != nil {
		d.logger.Warn("waiting orders not woken", "err", err)
		return
	}
	if ready {
		d.wakeOnDeck(ctx, "for an order whose after is done")
	}
}

// Outcomes of DismissCrew and ReenergizeCrew (CrewActionResult.Outcome).
const (
	// OutcomeDone: the action is complete.
	OutcomeDone = "done"
	// OutcomeRequested: a tour was signalled or started and finishes the
	// action on its own (the dismissed tour removes the beam; the new
	// tour provisions one).
	OutcomeRequested = "requested"
	// OutcomeUnderWay: an earlier call's action is still under way, and
	// this one did nothing.
	OutcomeUnderWay = "under_way"
)

// DismissCrew dismisses crew member id, freeing its beam slot (spec
// phase-2-plan §8). One transaction sets the crew member dismissed, logs
// it, sends the dismissed error to the mission of a parked order still
// assigned to it, which fails the order, and reads the tour to signal
// (crew.tour_no, #113). Dismissed is terminal in the store and every tour
// step that ends or hands over a tour reads it, so no path brings the crew
// member back (#100). A live tour (PENDING or ENQUEUED) is then sent the
// dismissed signal, to end at once: a running mission fails its order and
// remove-beam removes the beam; the outcome is OutcomeRequested.
//
// A tour whose terminal step read the crew member before the dismissal
// committed ends without removing the beam, and reads live until it does
// (#113). So DismissCrew waits up to dismissWait for the tour's result:
// one that ended dismissed has removed the beam, and one still running
// will. Without a live tour, or once it ended otherwise (in sickbay, a
// watch change, an error), the crew member's beam is removed here, best
// effort: OutcomeDone. Dismissing a crew member already dismissed does
// nothing (OutcomeDone). A missing one is ErrNotFound.
func (d *Daemon) DismissCrew(ctx context.Context, id string) (string, error) {
	msg, err := syntheticMessage(comms.TypeError, comms.ErrorBody{Reason: "crew member dismissed", Code: CodeDismissed}, id, "")
	if err != nil {
		return "", err
	}
	if d.dismissHook != nil {
		d.dismissHook(d, id)
	}
	changed, tourNo, err := d.store.DismissCrew(ctx, id, store.LogEntry{Kind: logCrew, CrewID: id, Text: "crew dismissed"},
		func(tx *sql.Tx, order string) error {
			if order == "" {
				return nil
			}
			msg.Order = order
			if err := dbos.Send(d.dbos, missionWorkflowID(order), msg, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
				return fmt.Errorf("tell %s: %w", missionWorkflowID(order), err)
			}
			return nil
		})
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("crew %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	if !changed {
		return OutcomeDone, nil
	}
	tour := tourWorkflowID(id, tourNo)
	live, _, err := workflowLive(d, tour)
	if err != nil {
		return "", err
	}
	if live {
		if err := dbos.Send(d.dbos, tour, CrewSignal{Kind: SignalDismissed}, crewEventsTopic); err != nil {
			return "", fmt.Errorf("signal tour %s: %w", tour, err)
		}
		// A send to a workflow that has just ended succeeds unread, and a
		// tour past its terminal step won't read it: then remove the beam
		// here. A tour still live after the wait reads the signal, or
		// finds the dismissal in its last step.
		h, err := dbos.RetrieveWorkflow[CrewTourResult](d.dbos, tour)
		if err != nil {
			return "", err
		}
		// Only the wait: what ended it is read once it has.
		_, _ = h.GetResult(dbos.WithHandleTimeout(d.dismissWait), dbos.WithHandlePollingInterval(d.dismissWait/20))
		if live, _, err = workflowLive(d, tour); err != nil || live {
			return OutcomeRequested, err
		}
		if res, err := h.GetResult(); err == nil && res.Ended == store.CrewDismissed {
			// The tour removed the beam.
			return OutcomeRequested, nil
		}
	}
	// The crew member's beam, if any: the latest it had, whatever tour
	// made it (each crew member holds at most one).
	beams, err := d.store.ListBeams(ctx, id)
	if err != nil {
		return "", err
	}
	if len(beams) > 0 {
		if err := d.removeBeam(ctx, id, beams[len(beams)-1]); err != nil {
			return "", err
		}
	}
	return OutcomeDone, nil
}

// ReenergizeCrew gives a crew member in sickbay a new tour, and so a new
// beam (spec phase-2-plan §8); the tour resumes the crew member's parked
// order, if it has one. A crew member not in sickbay is a bad request, and a
// missing one ErrNotFound. While tour crew.tour_no is PENDING or ENQUEUED
// re-energize is under way (or the tour is still ending) and it does
// nothing: OutcomeUnderWay. A tour that doesn't exist (a crash after an
// earlier call's NextTour) is started. Otherwise NextTour moves the crew
// member to tour tour_no+1, beaming_in (#106), and that tour is started
// under its deterministic id, so repeated or concurrent calls start one
// tour. A crew member beaming in is handled the same way: its tour is under
// way (OutcomeUnderWay), doesn't exist (an earlier call's NextTour moved it
// out of sickbay before a crash, and its tour is started), or ended without
// a hello (ERROR or CANCELLED: NextTour and a new tour, as from sickbay). A started tour
// provisions its beam on its own: OutcomeRequested. A resolved harness
// this bridge can't run (checkHarness) is the bad request AddCrew gives,
// before anything starts: the tour would go straight back to sickbay.
func (d *Daemon) ReenergizeCrew(ctx context.Context, id string) (string, error) {
	c, err := d.store.GetCrew(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("crew %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	if c.Status != store.CrewSickbay && c.Status != store.CrewBeamingIn {
		return "", badRequest("crew %s is %s, not in sickbay", id, c.Status)
	}
	live, found, err := workflowLive(d, tourWorkflowID(id, c.TourNo))
	if err != nil {
		return "", err
	}
	if live {
		return OutcomeUnderWay, nil
	}
	if err := d.checkHarness(c); err != nil {
		return "", err
	}
	tourNo := c.TourNo
	if found {
		// Unchanged: a concurrent call moved it already, or a dismissal
		// landed; the tour then provisions nothing.
		if _, err := d.store.NextTour(ctx, id, c.TourNo, store.LogEntry{Kind: logCrew, CrewID: id,
			Text: fmt.Sprintf("crew re-energized: tour %d", c.TourNo+1)}); err != nil {
			return "", err
		}
		tourNo++
	}
	if _, err := dbos.RunWorkflow(d.dbos, crewTourWorkflow, CrewTourInput{CrewID: id, TourNo: tourNo},
		dbos.WithWorkflowID(tourWorkflowID(id, tourNo))); err != nil {
		return "", fmt.Errorf("start tour %d of %s: %w", tourNo, id, err)
	}
	return OutcomeRequested, nil
}

// workflowLive reports whether workflow id exists (found) and whether it is
// PENDING, ENQUEUED or DELAYED (live): running, or waiting to run, as during
// recovery. SUCCESS, ERROR and CANCELLED are finished, and so is
// MAX_RECOVERY_ATTEMPTS_EXCEEDED, which missions, tours and hails can't reach:
// they register with WithMaxRecoveryAttempts(-1) (register).
func workflowLive(d *Daemon, id string) (live, found bool, err error) {
	wfs, err := dbos.ListWorkflows(d.dbos, dbos.WithFilterWorkflowIDs(id),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil || len(wfs) == 0 {
		return false, false, err
	}
	switch wfs[0].Status {
	case dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued, dbos.WorkflowStatusDelayed:
		return true, true, nil
	}
	return false, true, nil
}
