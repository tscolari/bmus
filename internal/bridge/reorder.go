package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// Caps on an order_reorder hail (#148): the changes one hail may ask
// for, the ids in each of a change's after lists, its onto (bytes) and
// the reason (runes). The schema and reorder_orders carry them as well.
const (
	maxReorderChanges = 10
	maxReorderIDs     = 10
	maxReorderOnto    = 256
	maxReorderReason  = 1000
)

// Marks of the notes an order_reorder hail's rationale gains when the
// bridge refuses it whole, or skips some of its changes.
const (
	reorderRefusedMark = "reorder refused: "
	reorderSkippedMark = "reorder skipped: "
)

// reorderSource says who changes an order's base or after: the captain
// (empty), or Command's crew member through order_reorder hail HailID.
type reorderSource struct {
	CrewID, HailID string
}

// reorderOrder applies ch to order id, which no crew member may have
// started (#148): `bmus order rebase`, `bmus order after` and an allowed
// order_reorder hail. A new base is checked as order.create checks one
// (checkOrderRepos, checkOrderBases); the store then checks, in the
// change's own transaction, that the order is unstarted and the after
// it would have (store.ReorderOrder). Each refusal is a bad request that
// writes nothing, and a missing order ErrNotFound. The change is logged
// once, on the order: "order rebased onto order:ORD-0005 (was
// order:ORD-0001)", or "order after: ORD-0002 (was ORD-0001)". An order
// left with nothing to wait on wakes its assignee's tour or its division,
// best effort, as RouteOrder does. An order with a task is refused first
// (#182): "ORD-0005 is campaign C-0001's work on o/r#12: a campaign order
// can't be reordered".
func (d *Daemon) reorderOrder(ctx context.Context, id string, ch store.OrderChange, src reorderSource) (store.Order, error) {
	o, err := d.store.GetOrder(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Order{}, fmt.Errorf("order %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return store.Order{}, err
	}
	if o.Task != "" {
		// Before the base checks, which would refuse a bad base first;
		// the store refuses it again in the change's transaction.
		return store.Order{}, badRequest("%s: a campaign order can't be reordered", store.CampaignOrderText(o))
	}
	if ch.Base != nil {
		repos, _, err := ch.Apply(o)
		if err != nil {
			return store.Order{}, reorderRefusal(err)
		}
		if err := d.checkOrderRepos(repos); err != nil {
			return store.Order{}, err
		}
		var changed []store.OrderRepo
		for i, r := range repos {
			if i < len(o.Repos) && r.BaseRef != o.Repos[i].BaseRef {
				changed = append(changed, r)
			}
		}
		if _, err := d.checkOrderBases(ctx, changed); err != nil {
			return store.Order{}, err
		}
	}
	o, changed, err := d.store.ReorderOrder(ctx, id, ch, func(before, after store.Order) (store.LogEntry, error) {
		return reorderEntry(src, ch, before, after)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return store.Order{}, fmt.Errorf("order %s: %w", id, ErrNotFound)
	case errors.Is(err, store.ErrReorder), errors.Is(err, store.ErrAfter):
		return store.Order{}, reorderRefusal(err)
	case err != nil:
		return store.Order{}, err
	}
	if changed && len(o.WaitingOn) == 0 {
		d.wakeOrder(ctx, o)
	}
	return o, nil
}

// reorderRefusal is a store refusal as a bad request, its text without
// the sentinel's.
func reorderRefusal(err error) error {
	text := err.Error()
	for _, s := range []error{store.ErrReorder, store.ErrAfter} {
		text = strings.TrimSuffix(text, ": "+s.Error())
	}
	return badRequest("%s", text)
}

// wakeOrder tells whoever may claim unclaimed order o that it may be
// ready: its assignee's tour, or its division. Best effort.
func (d *Daemon) wakeOrder(ctx context.Context, o store.Order) {
	if o.Assignee == "" {
		d.wakeDivision(ctx, o.Division)
		return
	}
	c, err := d.store.GetCrew(ctx, o.Assignee)
	if err != nil {
		d.logger.Warn("order ready but its assignee could not be read", "order", o.ID, "err", err)
		return
	}
	tour := tourWorkflowID(c.ID, c.TourNo)
	if err := d.wakeSend(tour, CrewSignal{Kind: SignalOrderAssigned, OrderID: o.ID}); err != nil {
		d.logger.Warn("order ready but its crew tour was not signalled", "order", o.ID, "tour", tour, "err", err)
	}
}

// afterText is an after list for the log and the captain's question:
// the ids, comma separated, or "none".
func afterText(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

// reorderEntry is the log entry of change ch to an order, before and
// after: "order rebased onto <base> (was <base>)", with " on repo <name>"
// for an order with more than one repo, and "order after: <ids> (was
// <ids>)" for a change to after alone, both joined by "; " when ch asked
// for both. A rebase's own swap of the order: entry in after isn't
// repeated. The payload carries the repo and both bases of a rebase,
// both afters, and the hail it came from (hail_id and reordered_by).
func reorderEntry(src reorderSource, ch store.OrderChange, before, after store.Order) (store.LogEntry, error) {
	payload := map[string]any{"after": orEmpty(after.After), "was_after": orEmpty(before.After)}
	var parts []string
	for i, r := range after.Repos {
		if i >= len(before.Repos) || r.BaseRef == before.Repos[i].BaseRef {
			continue
		}
		on := ""
		if len(after.Repos) > 1 {
			on = " on repo " + r.Name
		}
		parts = append(parts, fmt.Sprintf("order rebased onto %s%s (was %s)", r.BaseRef, on, before.Repos[i].BaseRef))
		payload["repo"], payload["base_ref"], payload["was_base_ref"] = r.Name, r.BaseRef, before.Repos[i].BaseRef
	}
	if len(parts) == 0 || len(ch.AfterAdd) > 0 || len(ch.AfterRemove) > 0 {
		text := fmt.Sprintf("after: %s (was %s)", afterText(after.After), afterText(before.After))
		if len(parts) == 0 {
			text = "order " + text
		}
		parts = append(parts, text)
	}
	if src.HailID != "" {
		payload["hail_id"], payload["reordered_by"] = src.HailID, src.HailID
	}
	return logEntry(logOrder, src.CrewID, before.ID, strings.Join(parts, "; "), payload)
}

// checkReorder checks order_reorder hail h, raised by crew member c, and
// returns its context. why says what is wrong with a malformed one,
// which changes nothing:
//   - c isn't in the command division;
//   - the options aren't [allow, deny];
//   - the context isn't one OrderReorderContext with no unknown fields;
//   - no changes, or more than maxReorderChanges;
//   - the reason (trimmed) is empty or longer than maxReorderReason runes;
//   - a change names no order id, an order another change names, or asks
//     for nothing;
//   - an onto that is blank, starts with '-', holds a control character,
//     is longer than maxReorderOnto bytes, or is order: without an id;
//   - an after list longer than maxReorderIDs, naming something that
//     isn't an order id, naming one twice, or naming one in both lists.
//
// Whether each change applies (the order is unstarted, the base exists,
// no cycle) is checked when it is applied (applyReorder).
func checkReorder(h store.Hail, c store.Crew) (comms.OrderReorderContext, string) {
	var rc comms.OrderReorderContext
	if c.Division != DivisionCommand {
		return rc, fmt.Sprintf("only Command reorders orders, and %s is not in the %s division", c.ID, DivisionCommand)
	}
	if !slices.Equal(h.Options, []string{comms.OptionAllow, comms.OptionDeny}) {
		return rc, fmt.Sprintf("its options must be [%s %s]", comms.OptionAllow, comms.OptionDeny)
	}
	dec := json.NewDecoder(bytes.NewReader(h.Context))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rc); err != nil {
		return rc, "its context is not a reorder: " + err.Error()
	}
	if dec.More() {
		return rc, "its context is not a reorder: more than one JSON value"
	}
	switch n := len(rc.Changes); {
	case n == 0:
		return rc, "it has no changes"
	case n > maxReorderChanges:
		return rc, fmt.Sprintf("it has %d changes, more than %d", n, maxReorderChanges)
	}
	if why := reorderReasonWhy(rc.Reason); why != "" {
		return rc, why
	}
	var seen []string
	for i, ch := range rc.Changes {
		n := i + 1
		if !orderIDRE.MatchString(ch.Order) {
			return rc, fmt.Sprintf("change %d: %q is not an order id", n, ch.Order)
		}
		if slices.Contains(seen, ch.Order) {
			return rc, fmt.Sprintf("change %d names %s again: one change per order", n, ch.Order)
		}
		seen = append(seen, ch.Order)
		if ch.Onto == "" && len(ch.AfterAdd) == 0 && len(ch.AfterRemove) == 0 {
			return rc, fmt.Sprintf("change %d asks for nothing: give onto, after_add or after_remove", n)
		}
		if ch.Onto != "" {
			switch id, isOrder := orderBase(ch.Onto); {
			case strings.TrimSpace(ch.Onto) != ch.Onto:
				return rc, fmt.Sprintf("change %d's onto %q has surrounding space", n, ch.Onto)
			case strings.HasPrefix(ch.Onto, "-"):
				return rc, fmt.Sprintf("change %d's onto %q must not start with '-'", n, ch.Onto)
			case len(ch.Onto) > maxReorderOnto:
				return rc, fmt.Sprintf("change %d's onto is longer than %d bytes", n, maxReorderOnto)
			case !utf8.ValidString(ch.Onto) || strings.ContainsFunc(ch.Onto, unicode.IsControl):
				return rc, fmt.Sprintf("change %d's onto is not one line", n)
			case isOrder && !orderIDRE.MatchString(id):
				return rc, fmt.Sprintf("change %d's onto %q: %s takes an order's id, such as %sORD-0001", n, ch.Onto, orderBasePrefix, orderBasePrefix)
			}
		}
		for _, list := range []struct {
			name string
			ids  []string
		}{{"after_add", ch.AfterAdd}, {"after_remove", ch.AfterRemove}} {
			if len(list.ids) > maxReorderIDs {
				return rc, fmt.Sprintf("change %d's %s has %d ids, more than %d", n, list.name, len(list.ids), maxReorderIDs)
			}
			for j, id := range list.ids {
				switch {
				case !orderIDRE.MatchString(id):
					return rc, fmt.Sprintf("change %d's %s: %q is not an order id", n, list.name, id)
				case slices.Contains(list.ids[:j], id):
					return rc, fmt.Sprintf("change %d's %s names %s twice", n, list.name, id)
				}
			}
		}
		for _, id := range ch.AfterAdd {
			if slices.Contains(ch.AfterRemove, id) {
				return rc, fmt.Sprintf("change %d both adds and removes %s", n, id)
			}
		}
	}
	return rc, ""
}

// reorderReasonWhy says what is wrong with an order_reorder hail's
// reason: blank, not valid UTF-8, or longer than maxReorderReason runes
// once trimmed. encoding/json decodes invalid UTF-8 as U+FFFD, so the
// second can't come from a context; it guards any other caller.
func reorderReasonWhy(reason string) string {
	reason = strings.TrimSpace(reason)
	switch {
	case reason == "":
		return "it gives no reason"
	case !utf8.ValidString(reason):
		return "its reason is not valid UTF-8"
	case utf8.RuneCountInString(reason) > maxReorderReason:
		return fmt.Sprintf("its reason is longer than %d characters", maxReorderReason)
	}
	return ""
}

// orderChange is reorder change ch as the store's OrderChange.
func orderChange(ch comms.OrderReorderChange) store.OrderChange {
	oc := store.OrderChange{AfterAdd: ch.AfterAdd, AfterRemove: ch.AfterRemove}
	if ch.Onto != "" {
		onto := ch.Onto
		oc.Base = &onto
	}
	return oc
}

// reorderInvalid is why order_reorder hail h is malformed (checkReorder),
// read by classify; "" for a well-formed one.
func (d *Daemon) reorderInvalid(ctx context.Context, h store.Hail) (string, error) {
	c, err := d.store.GetCrew(ctx, h.CrewID)
	if err != nil {
		return "", fmt.Errorf("crew %s: %w", h.CrewID, err)
	}
	_, why := checkReorder(h, c)
	return why, nil
}

// denyReorder is the deny-reorder step: it decides open order_reorder
// hail id by policy, before anyone sees it, with decision "invalid order
// reorder: <why>", option deny and route auto. It reports false for a
// hail no longer open.
func (d *Daemon) denyReorder(ctx context.Context, id, why string) (bool, error) {
	return d.denyByPolicy(ctx, id, "invalid order reorder: "+why, why)
}

// Caps on what reorderQuestion shows of each change.
const (
	maxReorderQuestionTitle  = 200
	maxReorderQuestionReason = 4 * maxReorderReason // bytes: room for any reason checkReorder takes
)

// reorderQuestion is the question an order_reorder hail is stored with,
// built by the bridge from its context and the store (#148), never the
// agent's: "Reorder 2 orders: 1) ORD-0003 "<title>" (pool): base main
// -> order:ORD-0005; after ORD-0001 -> ORD-0005; 2) .... Command's
// reason: "<reason>"". Each change shows the order's current base and
// after and the ones it would have (store.OrderChange.Apply), or why it
// can't apply as read now: no such order, or Apply's refusal. A line for
// an order a crew member has started ends "(will be skipped: <why>)"
// (store.StartedWhy). The reason comes last and quoted, so the agent's
// text can't pass for part of the change list. A context that doesn't decode,
// or has no changes, reads as malformed (classify denies it).
func (d *Daemon) reorderQuestion(ctx context.Context, context json.RawMessage) string {
	var rc comms.OrderReorderContext
	if err := json.Unmarshal(context, &rc); err != nil || len(rc.Changes) == 0 {
		return "Reorder orders: the request is malformed, and the bridge denies it"
	}
	var parts []string
	for i, ch := range rc.Changes {
		if i == maxReorderChanges {
			parts = append(parts, fmt.Sprintf("and %d more", len(rc.Changes)-i))
			break
		}
		id := clip(oneLine(ch.Order), 40)
		parts = append(parts, fmt.Sprintf("%d) %s", i+1, d.reorderLine(ctx, id, ch)))
	}
	noun := "orders"
	if len(rc.Changes) == 1 {
		noun = "order"
	}
	return fmt.Sprintf("Reorder %d %s: %s. Command's reason: %q", len(rc.Changes), noun, strings.Join(parts, "; "),
		clip(oneLine(strings.TrimSpace(rc.Reason)), maxReorderQuestionReason))
}

// reorderLine is one change's part of reorderQuestion, for order id.
func (d *Daemon) reorderLine(ctx context.Context, id string, ch comms.OrderReorderChange) string {
	o, err := d.store.GetOrder(ctx, id)
	if err != nil {
		return id + ": no such order"
	}
	head := fmt.Sprintf("%s %q (%s)", o.ID, clip(oneLine(o.Title), maxReorderQuestionTitle), o.State)
	skip := ""
	if why, err := d.store.StartedWhy(ctx, o); err != nil {
		skip = " (could not read whether it started: " + err.Error() + ")"
	} else if why != "" {
		skip = " (will be skipped: " + why + ")"
	}
	oc := orderChange(ch)
	repos, after, err := oc.Apply(o)
	if err != nil {
		return head + ": " + strings.TrimSuffix(err.Error(), ": "+store.ErrReorder.Error()) + skip
	}
	var parts []string
	for i, r := range repos {
		if i < len(o.Repos) && r.BaseRef != o.Repos[i].BaseRef {
			on := ""
			if len(repos) > 1 {
				on = " on repo " + r.Name
			}
			parts = append(parts, fmt.Sprintf("base%s %s -> %s", on, clip(oneLine(o.Repos[i].BaseRef), maxReorderOnto),
				clip(oneLine(r.BaseRef), maxReorderOnto)))
		}
	}
	parts = append(parts, fmt.Sprintf("after %s -> %s", afterText(o.After), clip(oneLine(afterText(after)), 40*maxReorderIDs)))
	return head + ": " + strings.Join(parts, "; ") + skip
}

// reorderOutcome is the apply-reorder step's recorded result: the
// answer's decision, and whether it applied nothing.
type reorderOutcome struct {
	Decision string
	Refused  bool `json:",omitempty"`
}

// applyReorder is the apply-reorder step (#148), run by the mission for
// an order_reorder hail answered with option 0 before it relays the
// answer, once reorder-window has waited out the hail's countermand
// window (awaitCountermandWindow). A hail no longer decided, a window
// still open (longer than the tour's settle wait), a countermand (#153)
// to anything but option 0, or a context checkReorder now refuses
// refuses it whole. Else
// each change goes through reorderOrder in turn, as the captain's own
// would, rechecked in its own transaction: a change that no longer
// applies (its order claimed meanwhile, say) is skipped and named. A
// change already logged with this hail (reordered_by) counts as applied
// without running again, so a re-run of the step applies nothing twice.
//
// The decision reads "applied ORD-0003, ORD-0004; skipped ORD-0005:
// <why>". Skipped changes are noted on the hail's rationale once,
// "reorder skipped: ORD-0005: <why>" (reorderSkippedMark), and logged so
// on Command's order. With nothing
// applied, the outcome is Refused and the answer takes option deny; a
// refusal of the whole hail is noted the same way (reorderRefusedMark).
func applyReorder(ctx context.Context, d *Daemon, in MissionInput, hailID string) (reorderOutcome, error) {
	// Read under the store's write lock, as trackerRequestCall does: a
	// countermand in flight lands first, and one started later is too
	// late.
	h, now, err := d.store.GetHailLocked(ctx, hailID)
	if err != nil {
		return reorderOutcome{}, fmt.Errorf("hail %s: %w", hailID, err)
	}
	c, err := d.store.GetCrew(ctx, h.CrewID)
	if err != nil {
		return reorderOutcome{}, fmt.Errorf("crew %s: %w", h.CrewID, err)
	}
	var (
		rc  comms.OrderReorderContext
		why string
	)
	switch {
	case h.Status != store.HailDecided:
		why = "hail " + h.ID + " is " + h.Status
	case h.CountermandUntil != nil && now.Before(*h.CountermandUntil):
		why = "its countermand window is still open, until " + h.CountermandUntil.UTC().Format(time.RFC3339)
	case h.CountermandedAway():
		why = fmt.Sprintf("the captain countermanded it: %s", h.Decision)
	default:
		rc, why = checkReorder(h, c)
	}
	note := func(mark, text string) error {
		entry, err := logEntry(logOrder, in.CrewID, in.OrderID, mark+text, map[string]any{"hail_id": h.ID, "reason": text})
		if err != nil {
			return err
		}
		_, err = d.store.NoteRefusal(ctx, h.ID, mark, text, entry)
		return err
	}
	if why != "" {
		if err := note(reorderRefusedMark, why); err != nil {
			return reorderOutcome{}, err
		}
		return reorderOutcome{Decision: reorderRefusedMark + why, Refused: true}, nil
	}
	var applied, skipped []string
	src := reorderSource{CrewID: in.CrewID, HailID: h.ID}
	for _, ch := range rc.Changes {
		done, err := d.store.OrderLogMarked(ctx, ch.Order, logOrder, "reordered_by", h.ID)
		if err != nil {
			return reorderOutcome{}, err
		}
		if !done {
			if _, err := d.reorderOrder(ctx, ch.Order, orderChange(ch), src); err != nil {
				if code := errorCode(ctx, err); code != CodeBadRequest && code != CodeNotFound {
					return reorderOutcome{}, err
				}
				skipped = append(skipped, fmt.Sprintf("%s: %v", ch.Order, err))
				continue
			}
		}
		applied = append(applied, ch.Order)
	}
	if len(skipped) > 0 {
		if err := note(reorderSkippedMark, strings.Join(skipped, "; ")); err != nil {
			return reorderOutcome{}, err
		}
	}
	parts := []string{"applied nothing"}
	if len(applied) > 0 {
		parts[0] = "applied " + strings.Join(applied, ", ")
	}
	for _, s := range skipped {
		parts = append(parts, "skipped "+s)
	}
	return reorderOutcome{Decision: strings.Join(parts, "; "), Refused: len(applied) == 0}, nil
}
