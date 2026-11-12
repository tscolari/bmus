package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

// hailWorkflowID is the workflow id of a hail's workflow
// (spec/phase-3-plan-2026-10-29.md §3).
func hailWorkflowID(hailID string) string { return "hail/" + hailID }

// consultReplyTopic is the topic the hail workflow Recvs Command's
// consult_reply on (spec/phase-3-plan-2026-10-29.md §3 and §4). A hail
// workflow reads it at most once: a reply carries no consult attempt id.
// A reply that comes after the workflow stopped waiting is acked and never
// read. notifyHailClosed also sends a message with no type on it for a
// hail routed to Command, so a workflow waiting for Command learns its
// hail closed. Only Command's crew members' replies are delivered
// (pumpSink.ToHail).
const consultReplyTopic = "consult-reply"

// DefaultConsultTimeout is how long a hail waits for Command's reply
// before it goes to the captain (Config.ConsultTimeout).
const DefaultConsultTimeout = 10 * time.Minute

// consultRecentLog is how many of the order's latest log lines a consult
// carries (comms.ConsultBody.RecentLog).
const consultRecentLog = 20

// Caps on what a consult carries (consultBody) and on Command's rationale
// in the hail, its log lines and the answer (decideCommand): longer text
// is cut and marked truncated.
const (
	maxConsultLogLine   = 2 << 10
	maxConsultSpec      = 32 << 10
	maxConsultContext   = 32 << 10
	maxCommandRationale = 2 << 10
)

// truncated marks text clip cut.
const truncated = " [truncated]"

// clip cuts s to at most n bytes, on a rune boundary, marked truncated.
// An n with no room for the mark cuts without it.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n = max(n, 0)
	mark := truncated
	if n <= len(truncated) {
		mark = ""
	}
	cut := n - len(mark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// whyCommandNotOnDeck is why a hail for Command goes to the captain when
// nobody in the command division is on deck.
const whyCommandNotOnDeck = "Command not on deck"

// hailClosedTopic is the topic a hail workflow Recvs on to learn its hail
// closed: AnswerHail and closeHailAs send the hail's id on it in the
// transaction that closes the hail (notifyHailClosed). The workflow reads
// the hail's row on every wake; the message only wakes it.
const hailClosedTopic = "hail-closed"

// DivisionCommand is Command's division (D7): its first crew member on
// deck answers consults (commandCrew), and hails from its crew members go
// to the captain. A roster with divisions must list it for crew members to
// join it (checkDivision).
const DivisionCommand = "command"

// RoleCommand is the serve --role a crew member of DivisionCommand runs
// with: its sdk runs get split_order (#155).
const RoleCommand = "command"

// hailRecheckEvery caps a captain-routed hail's wait between reads of its
// row when no reminder or timeout is due sooner.
const hailRecheckEvery = missionWaitTimeout

// HailInput is the input of a hail workflow, id hail/<HailID>.
type HailInput struct {
	HailID string
}

// HailResult is how a hail workflow ended: whether its hail was decided,
// and by whom. Nothing reads it yet.
type HailResult struct {
	Decided bool
	By      string `json:",omitempty"`
}

// hailRouting is the classify step's recorded result. It carries the
// escalation settings the workflow runs on, so a regs.yaml changed across a
// restart applies to new hails only and a replay takes the same path.
type hailRouting struct {
	Open  bool   // the hail was still open
	Route string // regs.RouteAuto, RouteCommand or RouteCaptain
	// Why is the auto_allow rule, or why the hail is the captain's; empty
	// for RouteCommand until Command's consult falls back to the captain.
	Why string
	Tag string
	// CaptainOnly: Classify routed the hail to the captain, so
	// on_timeout default_option holds instead.
	CaptainOnly bool
	// DefaultBlocked is why regs.CheckAnswer rejects DefaultOption as an
	// answer to the hail (a captain_only option rule, say): on_timeout
	// default_option then holds instead.
	DefaultBlocked string `json:",omitempty"`
	// InvalidSplit is why an order_split hail is malformed (checkSplit):
	// deny-split decides it by policy at once.
	InvalidSplit string `json:",omitempty"`
	// InvalidRequest is why the bridge refuses a tracker request
	// (checkTrackerRequest), no tracker included: deny-request decides it
	// by policy at once.
	InvalidRequest string `json:",omitempty"`
	Start          time.Time

	RemindEvery       time.Duration
	Timeout           time.Duration
	OnTimeout         string
	DefaultOption     *int `json:",omitempty"`
	CountermandWindow time.Duration
	ConsultTimeout    time.Duration `json:",omitempty"`
}

// hailCheck is the check-hail step's recorded result.
type hailCheck struct {
	Open      bool
	Status    string
	DecidedBy string
	Now       time.Time
}

func (c hailCheck) result() HailResult {
	if c.Status != store.HailDecided {
		return HailResult{}
	}
	return HailResult{Decided: true, By: c.DecidedBy}
}

// hailWorkflow classifies, decides or routes one hail
// (spec/phase-3-plan-2026-10-29.md §3). The mission starts it right after
// record-hail and never waits on it; it talks to the mission only through
// fromCrewTopic.
//
//   - classify reads the hail, its order and its crew member. A malformed
//     order_split hail (checkSplit) is denied at once by policy
//     (deny-split), with or without regs.yaml, and so is a tracker
//     request the bridge can't run (checkTrackerRequest: no tracker, a
//     malformed context) (deny-request). Without regs.yaml every
//     other hail is the captain's ("regs.yaml not configured").
//     Otherwise regs.Classify routes it: RouteAuto is decided at once
//     (decide-policy); RouteCaptain and a hail from the command division
//     go to the captain (route-captain logs why).
//   - RouteCommand consults Command once (consultCommand), which decides
//     the hail or hands it to the captain.
//   - A captain-routed hail waits for its hail to close (awaitCaptain),
//     with reminders and the on_timeout policy.
//
// It never decides a captain's answer itself: AnswerHail does, and tells
// the workflow (notifyHailClosed). A hail closed before or while the
// workflow runs (belay, recall, park, the mission's end, the tour's
// backstop) ends it the same way.
func hailWorkflow(ctx dbos.Context, in HailInput) (HailResult, error) {
	r, err := dbos.RunAsStep(ctx, func(c context.Context) (hailRouting, error) {
		return daemonFrom(c).classifyHail(c, in.HailID)
	}, append([]dbos.StepOption{dbos.WithStepName("classify")}, storeRetries...)...)
	if err != nil {
		return HailResult{}, err
	}
	if r.Open && r.InvalidSplit != "" {
		decided, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
			return daemonFrom(c).denySplit(c, in.HailID, r.InvalidSplit)
		}, append([]dbos.StepOption{dbos.WithStepName("deny-split")}, storeRetries...)...)
		if err != nil || decided {
			return HailResult{Decided: decided, By: comms.DecidedByPolicy}, err
		}
		r.Open = false
	}
	if r.Open && r.InvalidRequest != "" {
		decided, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
			return daemonFrom(c).denyTrackerRequest(c, in.HailID, r.InvalidRequest)
		}, append([]dbos.StepOption{dbos.WithStepName("deny-request")}, storeRetries...)...)
		if err != nil || decided {
			return HailResult{Decided: decided, By: comms.DecidedByPolicy}, err
		}
		r.Open = false
	}
	if r.Open && r.Route == regs.RouteAuto {
		decided, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
			return daemonFrom(c).decideByPolicy(c, in.HailID, 0, r.Why, regs.RouteAuto, 0, false)
		}, append([]dbos.StepOption{dbos.WithStepName("decide-policy")}, storeRetries...)...)
		if err != nil || decided {
			return HailResult{Decided: decided, By: comms.DecidedByPolicy}, err
		}
		r.Open = false
	}
	if r.Open && r.Route == regs.RouteCommand {
		res, done, err := consultCommand(ctx, in.HailID, &r)
		if err != nil || done {
			return res, err
		}
	}
	if r.Open {
		r.Open, err = dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
			return daemonFrom(c).routeCaptain(c, in.HailID, r)
		}, append([]dbos.StepOption{dbos.WithStepName("route-captain")}, storeRetries...)...)
		if err != nil {
			return HailResult{}, err
		}
	}
	if !r.Open {
		chk, err := checkHail(ctx, in.HailID)
		return chk.result(), err
	}
	return awaitCaptain(ctx, in.HailID, r)
}

// classifyHail is the classify step.
func (d *Daemon) classifyHail(ctx context.Context, id string) (hailRouting, error) {
	h, err := d.store.GetHail(ctx, id)
	if err != nil {
		return hailRouting{}, fmt.Errorf("hail %s: %w", id, err)
	}
	r := hailRouting{Open: h.Status == store.HailOpen, Route: regs.RouteCaptain, Start: time.Now(),
		ConsultTimeout: d.consultTimeout}
	if !r.Open {
		return r, nil
	}
	if h.Category == comms.CategoryOrderSplit {
		// A malformed split is denied whatever regs.yaml says (#155).
		if r.InvalidSplit, err = d.splitInvalid(ctx, h); err != nil || r.InvalidSplit != "" {
			return r, err
		}
	}
	if isTrackerRequest(h.Category) {
		// A tracker request the bridge can't run (no tracker, a malformed
		// context) is refused whatever regs.yaml says (#54).
		if _, r.InvalidRequest = d.checkTrackerRequest(h); r.InvalidRequest != "" {
			return r, nil
		}
	}
	if !d.regsConfigured {
		r.Why = infoRegsNotConfigured
		return r, nil
	}
	esc := d.regs.Escalation
	r.RemindEvery, r.Timeout, r.OnTimeout, r.DefaultOption = esc.CaptainReminderEvery, esc.CaptainTimeout, esc.OnTimeout, esc.DefaultOption
	if w := d.regs.CommandMayAnswer.CountermandWindow; w != nil {
		r.CountermandWindow = *w
	}
	o, err := d.store.GetOrder(ctx, h.OrderID)
	if err != nil {
		return hailRouting{}, fmt.Errorf("order %s: %w", h.OrderID, err)
	}
	c, err := d.store.GetCrew(ctx, h.CrewID)
	if err != nil {
		return hailRouting{}, fmt.Errorf("crew %s: %w", h.CrewID, err)
	}
	rh := regsHail(h, o)
	route, tag, rule := d.regs.Classify(rh)
	r.Tag = tag
	switch {
	case route == regs.RouteAuto && (len(h.Options) == 0 || h.Options[0] != comms.OptionAllow):
		// Option 0 allows a tool (comms.AnswerBody); without it, auto-allow
		// can't answer.
		r.Why, r.CaptainOnly = rule+", but option 0 is not "+comms.OptionAllow, true
	case route == regs.RouteAuto:
		r.Route, r.Why = regs.RouteAuto, rule
	case route == regs.RouteCaptain:
		r.Why, r.CaptainOnly = rule, true
	case c.Division == DivisionCommand:
		r.Why = "from Command"
	default:
		r.Route = regs.RouteCommand
	}
	// Classify skips captain_only option rules: the default option's label
	// gets CheckAnswer's checks, as Command's answer would.
	if opt := r.DefaultOption; r.OnTimeout == regs.OnTimeoutDefaultOption && opt != nil && !r.CaptainOnly && *opt < len(h.Options) {
		if ok, why := d.regs.CheckAnswer(rh, regs.Reply{Decision: regs.DecisionAnswer, Option: opt, Confidence: 1}); !ok {
			r.DefaultBlocked = why
		}
	}
	return r, nil
}

// regsHail is what the Regulations engine reads of hail h on order o.
func regsHail(h store.Hail, o store.Order) regs.Hail {
	rh := regs.Hail{Category: h.Category, Question: h.Question, Options: h.Options, Priority: o.Priority}
	if h.Category == regs.CategoryToolApproval {
		rh.SetToolContext(h.Context)
	}
	if isTrackerRequest(h.Category) {
		// The question shows the first 2 KiB of a comment; the rules read
		// all of it.
		rh.Values = trackerRequestText(h.Category, h.Context)
	}
	return rh
}

// routeCaptain is the route-captain step: it sets the hail's route to
// captain and logs why, only while the hail is open (store.UpdateOpenHail),
// and reports whether it was.
func (d *Daemon) routeCaptain(ctx context.Context, id string, r hailRouting) (bool, error) {
	h, err := d.store.GetHail(ctx, id)
	if err != nil {
		return false, err
	}
	entry, err := logEntry(logHail, h.CrewID, h.OrderID, "hail "+id+" for the captain: "+r.Why,
		map[string]any{"hail_id": id, "route": regs.RouteCaptain, "why": r.Why, "tag": r.Tag})
	if err != nil {
		return false, err
	}
	return d.store.UpdateOpenHail(ctx, id, regs.RouteCaptain, entry)
}

// decideByPolicy decides open hail id with its option, decided_by policy
// and rationale, as AnswerHail does: the decision (with route, if set),
// its crew member and order back away, its log entry and the answer to
// the mission commit in one transaction (store.DecideOpenHailAs). onTimeout
// marks the captain timeout's default_option: logged as such, with
// countermand_until now+window when window is positive. It reports false
// for a hail no longer open, and for an option out of range.
func (d *Daemon) decideByPolicy(ctx context.Context, id string, option int, rationale, route string, window time.Duration,
	onTimeout bool) (bool, error) {
	h, err := d.store.GetHail(ctx, id)
	if err != nil {
		return false, err
	}
	if h.Status != store.HailOpen || option < 0 || option >= len(h.Options) {
		return false, nil
	}
	body := comms.AnswerBody{HailID: id, DecidedBy: comms.DecidedByPolicy, Decision: h.Options[option], Option: &option,
		Rationale: rationale}
	text := ""
	if onTimeout {
		text = "hail " + id + " decided on the captain timeout: " + body.Decision
	}
	return d.decideOpen(ctx, h, body, store.HailDecision{Route: route}, text, window)
}

// decideOpen decides open hail h with body, as AnswerHail does: dec (whose
// DecidedBy, Decision and Rationale are body's), its crew member and order
// back away, the decided log entry (text, if set, in place of the usual
// one) and the answer to the mission commit in one
// transaction (store.DecideOpenHailAs). A positive window sets
// countermand_until to the decision's time plus window. It reports false
// for a hail no longer open.
func (d *Daemon) decideOpen(ctx context.Context, h store.Hail, body comms.AnswerBody, dec store.HailDecision, text string,
	window time.Duration) (bool, error) {
	msg, err := syntheticMessage(comms.TypeAnswer, body, h.CrewID, h.OrderID)
	if err != nil {
		return false, err
	}
	entry, err := hailDecidedEntry(h.CrewID, h.OrderID, body)
	if err != nil {
		return false, err
	}
	entry.TS = time.Now()
	if text != "" {
		entry.Text = text
	}
	dec.DecidedBy, dec.Decision, dec.Rationale = body.DecidedBy, body.Decision, body.Rationale
	if window > 0 {
		until := entry.TS.Add(window)
		dec.CountermandUntil = &until
	}
	err = d.store.DecideOpenHailAs(ctx, h.ID, dec, entry, func(tx *sql.Tx) error {
		if err := dbos.Send(d.dbos, missionWorkflowID(h.OrderID), msg, fromCrewTopic, dbos.WithSendTransaction(tx)); err != nil {
			return fmt.Errorf("deliver answer to %s: %w", missionWorkflowID(h.OrderID), err)
		}
		return nil
	})
	if errors.Is(err, store.ErrHailClosed) {
		return false, nil
	}
	return err == nil, err
}

// commandFound is the find-command step's recorded result.
type commandFound struct {
	Crew string `json:",omitempty"` // Command's crew member; empty for none on deck
	Open bool   // the hail was still open
}

// commandOutcome is the decide-command step's recorded result: whether it
// decided the hail, found it closed, or why the captain gets it instead.
type commandOutcome struct {
	Decided bool   `json:",omitempty"`
	Closed  bool   `json:",omitempty"`
	Why     string `json:",omitempty"`
	// Objected: Command escalated or flagged risks. on_timeout
	// default_option then holds rather than overrule it.
	Objected bool `json:",omitempty"`
}

// consultCommand consults Command on hail id, once (spec/phase-3-plan
// §3, steps 3 to 5), and reports whether that ended the workflow, with its
// result. Otherwise the hail goes to the captain with r.Why set to why, or
// with r.Open false when it closed meanwhile.
//
//   - find-command picks Command's crew member (commandCrew), sets the
//     hail's route to command and logs it; nobody on deck: "Command not
//     on deck".
//   - transmit-consult sends it the consult (consultBody).
//   - One Recv on consultReplyTopic, for r.ConsultTimeout: a timeout is
//     "Command did not reply within <t>". A message with no type is
//     notifyHailClosed's: the hail closed while Command thought.
//   - decide-command checks the reply (regs.CheckAnswer) and decides the
//     hail with it (decideCommand). When Command objected (escalated or
//     flagged risks), on_timeout default_option holds (r.DefaultBlocked).
func consultCommand(ctx dbos.Context, id string, r *hailRouting) (HailResult, bool, error) {
	found, err := dbos.RunAsStep(ctx, func(c context.Context) (commandFound, error) {
		return daemonFrom(c).findCommand(c, id)
	}, append([]dbos.StepOption{dbos.WithStepName("find-command")}, storeRetries...)...)
	switch {
	case err != nil:
		return HailResult{}, true, err
	case !found.Open:
		r.Open = false
		return HailResult{}, false, nil
	case found.Crew == "":
		r.Why = whyCommandNotOnDeck
		return HailResult{}, false, nil
	}
	sent, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
		return daemonFrom(c).transmitConsult(c, id, found.Crew, *r)
	}, append([]dbos.StepOption{dbos.WithStepName("transmit-consult")}, sendRetries...)...)
	switch {
	case err != nil && ctx.Err() != nil:
		return HailResult{}, true, err
	case err != nil:
		r.Why = "could not send the consult to " + found.Crew + ": " + err.Error()
		return HailResult{}, false, nil
	case !sent:
		r.Open = false
		return HailResult{}, false, nil
	}
	msg, err := dbos.Recv[comms.Message](ctx, consultReplyTopic, r.ConsultTimeout)
	switch {
	case errors.Is(err, dbos.ErrTimeout):
		r.Why = "Command did not reply within " + shortDuration(r.ConsultTimeout)
		return HailResult{}, false, nil
	case err != nil && (ctx.Err() != nil || !isRecvDecodeError(err)):
		return HailResult{}, true, err
	case err != nil:
		r.Why = "Command's reply did not decode"
		return HailResult{}, false, nil
	case msg.Type == "":
		// notifyHailClosed's wake: check-hail reads how it closed.
		r.Open = false
		return HailResult{}, false, nil
	}
	out, err := dbos.RunAsStep(ctx, func(c context.Context) (commandOutcome, error) {
		return daemonFrom(c).decideCommand(c, id, found.Crew, *r, msg)
	}, append([]dbos.StepOption{dbos.WithStepName("decide-command")}, storeRetries...)...)
	switch {
	case err != nil:
		return HailResult{}, true, err
	case out.Decided:
		return HailResult{Decided: true, By: comms.DecidedByCommand}, true, nil
	case out.Closed:
		r.Open = false
	default:
		r.Why = out.Why
		if out.Objected {
			r.DefaultBlocked = out.Why
		}
	}
	return HailResult{}, false, nil
}

// commandCrew returns the id of the first crew member of the command
// division on deck (on_deck, away or hailing: anyone with a live beam), by
// id, preferring one whose pump is connected, or "" for none.
func (d *Daemon) commandCrew(ctx context.Context) (string, error) {
	crew, err := d.store.ListCrew(ctx)
	if err != nil {
		return "", err
	}
	first := ""
	for _, c := range crew {
		if c.Division != DivisionCommand || !pumpCrewStatuses[c.Status] {
			continue
		}
		if p, ok := d.Pump(c.ID); ok && p.Connected() {
			return c.ID, nil
		}
		if first == "" {
			first = c.ID
		}
	}
	return first, nil
}

// commandDown reports whether crew, every crew member, has someone in the
// command division but nobody of it on deck (commandCrew).
func commandDown(crew []store.Crew) bool {
	down := false
	for _, c := range crew {
		if c.Division != DivisionCommand || c.Status == store.CrewDismissed {
			continue
		}
		if pumpCrewStatuses[c.Status] {
			return false
		}
		down = true
	}
	return down
}

// findCommand is the find-command step: Command's crew member
// (commandCrew). With one, it sets hail id's route to command and logs
// it, only while the hail is open (store.UpdateOpenHail).
func (d *Daemon) findCommand(ctx context.Context, id string) (commandFound, error) {
	crew, err := d.commandCrew(ctx)
	if err != nil {
		return commandFound{}, err
	}
	h, err := d.store.GetHail(ctx, id)
	if err != nil {
		return commandFound{}, err
	}
	if crew == "" {
		return commandFound{Open: h.Status == store.HailOpen}, nil
	}
	entry, err := logEntry(logHail, h.CrewID, h.OrderID, "hail "+id+" for Command: "+crew,
		map[string]any{"hail_id": id, "route": regs.RouteCommand, "command": crew})
	if err != nil {
		return commandFound{}, err
	}
	open, err := d.store.UpdateOpenHail(ctx, id, regs.RouteCommand, entry)
	return commandFound{Crew: crew, Open: open}, err
}

// transmitConsult is the transmit-consult step: it sends hail id's consult
// (consultBody) to crew, Command, while the hail is open, and reports
// whether it was. A consult carries no order. Its model is crew's, else
// crew's division's default (resolveModel without an order, #162).
func (d *Daemon) transmitConsult(ctx context.Context, id, crew string, r hailRouting) (bool, error) {
	body, open, err := d.consultBody(ctx, id, r)
	if err != nil || !open {
		return false, err
	}
	c, err := d.store.GetCrew(ctx, crew)
	if err != nil {
		return false, fmt.Errorf("crew %s: %w", crew, err)
	}
	body.Model, _ = d.resolveModel(store.Order{}, c)
	msg, err := comms.NewMessage(comms.TypeConsult, body)
	if err != nil {
		return false, err
	}
	if _, err := d.SendCommand(crew, msg); err != nil {
		return false, err
	}
	return true, nil
}

// consultBody builds hail id's consult: the hail's fields; its order's
// title and body (OrderSpec) and latest consultRecentLog log lines; the
// command_guidance setting; and the regulations that apply (consultRegs).
// open is false, with no body, for a hail no longer open.
func (d *Daemon) consultBody(ctx context.Context, id string, r hailRouting) (body comms.ConsultBody, open bool, err error) {
	h, err := d.store.GetHail(ctx, id)
	if err != nil || h.Status != store.HailOpen {
		return comms.ConsultBody{}, false, err
	}
	o, err := d.store.GetOrder(ctx, h.OrderID)
	if err != nil {
		return comms.ConsultBody{}, false, fmt.Errorf("order %s: %w", h.OrderID, err)
	}
	entries, err := d.store.ListOrderLogLast(ctx, h.OrderID, consultRecentLog)
	if err != nil {
		return comms.ConsultBody{}, false, err
	}
	guidance, err := d.CommandGuidance(ctx)
	if err != nil {
		return comms.ConsultBody{}, false, err
	}
	body = comms.ConsultBody{HailID: id, Crew: h.CrewID, Order: h.OrderID, Category: h.Category, Question: h.Question,
		Options: h.Options, OrderSpec: o.Title, Guidance: guidance, Regulations: d.consultRegs(r)}
	if o.Body != "" {
		body.OrderSpec += "\n\n" + o.Body
	}
	body.OrderSpec = clip(body.OrderSpec, maxConsultSpec)
	switch c := string(h.Context); {
	case c == "" || c == "null" || c == "{}":
	case len(c) <= maxConsultContext:
		body.Context = h.Context
	default:
		// Cut JSON isn't JSON: the cut text goes as a string.
		if body.Context, err = json.Marshal(map[string]string{"truncated": clip(c, maxConsultContext)}); err != nil {
			return comms.ConsultBody{}, false, err
		}
	}
	for _, e := range entries {
		body.RecentLog = append(body.RecentLog, clip(e.TS.UTC().Format(time.RFC3339)+" "+e.Kind+": "+e.Text, maxConsultLogLine))
	}
	return body, true, nil
}

// consultRegs is the excerpt of regs.yaml a consult carries: why the hail
// is Command's, its classify tag, the confidence an answer needs and the
// captain_only option rules CheckAnswer reads the answer with.
func (d *Daemon) consultRegs(r hailRouting) string {
	lines := []string{"No captain_only or auto_allow rule fits this hail: Command may answer it."}
	if r.Tag != "" {
		lines = append(lines, "classify tag: "+r.Tag)
	}
	if mc := d.regs.CommandMayAnswer.MinConfidence; mc != nil {
		lines = append(lines, fmt.Sprintf("min_confidence: %v (a less confident answer goes to the captain)", *mc))
	}
	for i, rule := range d.regs.CaptainOnly {
		if rule.Option != "" {
			lines = append(lines, fmt.Sprintf("captain_only[%d]: %s (an answer this fits goes to the captain)", i, rule))
		}
	}
	return strings.Join(lines, "\n")
}

// decideCommand is the decide-command step: it decides hail id with
// Command's reply msg, if it came from crew (Command's crew member, the
// consult's recipient) and regs.CheckAnswer takes it, as decided_by command
// with route command, the reply's rationale and confidence, and
// countermand_until now+r.CountermandWindow (decideOpen). The answer is the
// picked option's label, else the reply's answer. A reply for another
// hail, or one CheckAnswer rejects, returns why the captain gets the hail.
func (d *Daemon) decideCommand(ctx context.Context, id, crew string, r hailRouting, msg comms.Message) (commandOutcome, error) {
	h, err := d.store.GetHail(ctx, id)
	if err != nil {
		return commandOutcome{}, err
	}
	if h.Status != store.HailOpen {
		return commandOutcome{Closed: true}, nil
	}
	var reply comms.ConsultReplyBody
	switch err := json.Unmarshal(msg.Body, &reply); {
	case msg.Type != comms.TypeConsultReply || err != nil:
		return commandOutcome{Why: fmt.Sprintf("Command's reply is not a consult_reply (%s)", msg.Type)}, nil
	case msg.Crew != crew:
		return commandOutcome{Why: fmt.Sprintf("Command's reply came from %s, not %s", msg.Crew, crew)}, nil
	case reply.HailID != id:
		return commandOutcome{Why: "Command's reply is for hail " + reply.HailID}, nil
	}
	o, err := d.store.GetOrder(ctx, h.OrderID)
	if err != nil {
		return commandOutcome{}, fmt.Errorf("order %s: %w", h.OrderID, err)
	}
	ok, why := d.regs.CheckAnswer(regsHail(h, o), regs.Reply{Decision: reply.Decision, Option: reply.Option,
		Answer: reply.Answer, Confidence: reply.Confidence, RiskFlags: reply.RiskFlags})
	rationale := clip(reply.Rationale, maxCommandRationale)
	objected := reply.Decision == regs.DecisionEscalate || len(reply.RiskFlags) > 0
	switch {
	case ok:
	case strings.HasPrefix(why, "captain-only"):
		// The Regulations' reason outranks Command's own.
		return commandOutcome{Why: "Regulations rejected Command's answer: " + why, Objected: objected}, nil
	case reply.Decision == regs.DecisionEscalate && rationale != "":
		return commandOutcome{Why: "Command escalated: " + rationale, Objected: true}, nil
	case reply.Decision == regs.DecisionEscalate:
		return commandOutcome{Why: "Command escalated", Objected: true}, nil
	default:
		return commandOutcome{Why: "Regulations rejected Command's answer: " + why, Objected: objected}, nil
	}
	body := comms.AnswerBody{HailID: id, DecidedBy: comms.DecidedByCommand, Decision: reply.Answer, Option: reply.Option,
		Rationale: rationale}
	if reply.Option != nil {
		body.Decision = h.Options[*reply.Option]
	}
	conf := reply.Confidence
	decided, err := d.decideOpen(ctx, h, body, store.HailDecision{Route: regs.RouteCommand, Confidence: &conf},
		"hail "+id+" decided by Command: "+body.Decision, r.CountermandWindow)
	return commandOutcome{Decided: decided, Closed: err == nil && !decided}, err
}

// checkHail runs the check-hail step: the hail's status, and the time.
func checkHail(ctx dbos.Context, id string) (hailCheck, error) {
	return dbos.RunAsStep(ctx, func(c context.Context) (hailCheck, error) {
		h, err := daemonFrom(c).store.GetHail(c, id)
		if err != nil {
			return hailCheck{}, err
		}
		return hailCheck{Open: h.Status == store.HailOpen, Status: h.Status, DecidedBy: h.DecidedBy, Now: time.Now()}, nil
	}, append([]dbos.StepOption{dbos.WithStepName("check-hail")}, storeRetries...)...)
}

// awaitCaptain waits for captain-routed hail id to close, and returns how
// it closed. It wakes on hailClosedTopic, at each reminder, at the captain
// timeout and at least every hailRecheckEvery, and reads the hail's row on
// every wake (check-hail): a closed hail ends the wait. Every r.RemindEvery
// (0 never) remind logs that the hail is still open. Past r.Timeout (0
// never), on-timeout applies r.OnTimeout once (applyTimeout). Only
// recorded results decide: a wake's time is check-hail's.
func awaitCaptain(ctx dbos.Context, id string, r hailRouting) (HailResult, error) {
	var nextRemind, timeoutAt time.Time
	if r.RemindEvery > 0 {
		nextRemind = r.Start.Add(r.RemindEvery)
	}
	if r.Timeout > 0 {
		timeoutAt = r.Start.Add(r.Timeout)
	}
	for {
		next := nextRemind
		if !timeoutAt.IsZero() && (next.IsZero() || timeoutAt.Before(next)) {
			next = timeoutAt
		}
		wait := hailRecheckEvery
		if !next.IsZero() {
			// Only the wait: what is due is decided by check-hail's time.
			wait = min(wait, max(time.Until(next), time.Millisecond))
		}
		_, err := dbos.Recv[string](ctx, hailClosedTopic, wait)
		if err != nil && !errors.Is(err, dbos.ErrTimeout) && (ctx.Err() != nil || !isRecvDecodeError(err)) {
			return HailResult{}, err
		}
		chk, err := checkHail(ctx, id)
		if err != nil {
			return HailResult{}, err
		}
		if !chk.Open {
			return chk.result(), nil
		}
		if !timeoutAt.IsZero() && !chk.Now.Before(timeoutAt) {
			timeoutAt = time.Time{}
			out, err := dbos.RunAsStep(ctx, func(c context.Context) (timeoutOutcome, error) {
				return daemonFrom(c).applyTimeout(c, id, r, chk.Now.Sub(r.Start))
			}, append([]dbos.StepOption{dbos.WithStepName("on-timeout")}, storeRetries...)...)
			if err != nil {
				return HailResult{}, err
			}
			if out.Decided {
				return HailResult{Decided: true, By: comms.DecidedByPolicy}, nil
			}
			if out.Closed {
				// Closed meanwhile, or decided by a run of this step whose
				// result a crash lost: read how.
				chk, err := checkHail(ctx, id)
				return chk.result(), err
			}
		}
		if !nextRemind.IsZero() && !chk.Now.Before(nextRemind) {
			// The first beat after chk.Now; missed ones coalesce.
			nextRemind = nextRemind.Add((chk.Now.Sub(nextRemind)/r.RemindEvery + 1) * r.RemindEvery)
			if err := storeStep(ctx, "remind", func(c context.Context, d *Daemon) error {
				_, err := d.noteOpenHail(c, id, "still open after "+shortDuration(chk.Now.Sub(r.Start)), nil)
				return err
			}); err != nil {
				return HailResult{}, err
			}
		}
	}
}

// timeoutOutcome is the on-timeout step's recorded result: whether it
// decided the hail, or found it closed.
type timeoutOutcome struct {
	Decided bool `json:",omitempty"`
	Closed  bool `json:",omitempty"`
}

// applyTimeout is the on-timeout step: the hail, open after waited, gets
// r.OnTimeout. hold logs that the hail waits on; belay belays its order
// (BelayOrder), whose mission then closes the hail; default_option decides
// it by policy with r.DefaultOption (decideByPolicy), countermandable for
// r.CountermandWindow. default_option holds instead for a captain-only
// hail, for a default option CheckAnswer rejects (r.DefaultBlocked), and
// for a missing default_option or one the hail has no option for.
func (d *Daemon) applyTimeout(ctx context.Context, id string, r hailRouting, waited time.Duration) (timeoutOutcome, error) {
	after := "unanswered after " + shortDuration(waited)
	hold := func(why string) (timeoutOutcome, error) {
		open, err := d.noteOpenHail(ctx, id, after+": "+why, map[string]any{"on_timeout": r.OnTimeout})
		return timeoutOutcome{Closed: err == nil && !open}, err
	}
	h, err := d.store.GetHail(ctx, id)
	if err != nil {
		return timeoutOutcome{}, err
	}
	if h.Status != store.HailOpen {
		return timeoutOutcome{Closed: true}, nil
	}
	switch r.OnTimeout {
	case regs.OnTimeoutBelay:
		err = d.BelayOrder(ctx, h.OrderID, "hail "+id+" "+after)
		if re := (*rpcError)(nil); errors.As(err, &re) && re.code == CodeBadRequest {
			// The order moved on (settled, parked): its hail is closing
			// too.
			d.logger.Info("hail: captain timeout could not belay the order", "hail", id, "order", h.OrderID, "err", err)
			return timeoutOutcome{}, nil
		}
		return timeoutOutcome{}, err
	case regs.OnTimeoutDefaultOption:
		const holding = "holding for the captain ("
		switch {
		case r.DefaultOption == nil:
			return hold(holding + "no default_option)")
		case r.CaptainOnly:
			return hold(holding + "default_option does not apply to a captain-only hail)")
		case r.DefaultBlocked != "":
			return hold(fmt.Sprintf("%sdefault_option %d: %s)", holding, *r.DefaultOption, r.DefaultBlocked))
		case *r.DefaultOption >= len(h.Options):
			return hold(fmt.Sprintf("%sdefault_option %d is not one of the hail's %d options)", holding, *r.DefaultOption, len(h.Options)))
		}
		opt := *r.DefaultOption
		decided, err := d.decideByPolicy(ctx, id, opt, "captain timeout: "+after+", default_option "+fmt.Sprint(opt), "",
			r.CountermandWindow, true)
		return timeoutOutcome{Decided: decided, Closed: err == nil && !decided}, err
	}
	return hold("holding for the captain")
}

// noteOpenHail logs "hail <id> <text>" for hail id, only while it is open
// (store.UpdateOpenHail), with payload's fields and hail_id. It reports
// whether the hail was open.
func (d *Daemon) noteOpenHail(ctx context.Context, id, text string, payload map[string]any) (bool, error) {
	h, err := d.store.GetHail(ctx, id)
	if err != nil {
		return false, err
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["hail_id"] = id
	entry, err := logEntry(logHail, h.CrewID, h.OrderID, "hail "+id+" "+text, payload)
	if err != nil {
		return false, err
	}
	return d.store.UpdateOpenHail(ctx, id, "", entry)
}

// notifyHailClosed wakes hail id's workflow, if it is live, in tx, the
// transaction that closes the hail: on hailClosedTopic, and, for a hail
// routed to Command, with a message with no type on consultReplyTopic. A workflow not started yet finds the
// hail closed when it starts: under SQLite's single writer (the store's
// _txlock=immediate) no workflow can start between the check and the
// commit.
func (d *Daemon) notifyHailClosed(tx *sql.Tx, id string) error {
	wf := hailWorkflowID(id)
	live, _, err := workflowLive(d, wf)
	if err != nil || !live {
		return err
	}
	if err := dbos.Send(d.dbos, wf, id, hailClosedTopic, dbos.WithSendTransaction(tx)); err != nil {
		return fmt.Errorf("tell %s: %w", wf, err)
	}
	// A workflow waiting for Command's reply Recvs only consultReplyTopic.
	// find-command routes the hail to command before the consult goes out,
	// and route-captain away from it after.
	route, err := store.HailRoute(context.Background(), tx, id)
	if err != nil || route != regs.RouteCommand {
		return err
	}
	if err := dbos.Send(d.dbos, wf, comms.Message{}, consultReplyTopic, dbos.WithSendTransaction(tx)); err != nil {
		return fmt.Errorf("tell %s: %w", wf, err)
	}
	return nil
}

// shortDuration is d to the second, without zero minutes or seconds after
// a larger unit: 30m, 1h, 1h30m, 5s.
func shortDuration(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
