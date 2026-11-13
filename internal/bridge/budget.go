package bridge

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// Budget enforcement (#119, spec/phase-4-plan-2026-11-12.md section 6,
// "Enforcing"). The harness checks the order's budget before each tool
// call and raises a budget hail when it is over; the bridge writes the
// hail's question (budgetQuestion), the captain alone answers it
// (regs.CaptainFixed), an extension raises the limit (extendBudget), and a
// stop ends the order failed with CodeBudgetExceeded. budgetCeiling is the
// bridge's backstop for a harness that doesn't enforce.

// budgetCeilingReason is the reason of the belay the backstop sends.
const budgetCeilingReason = "budget ceiling passed"

// budgetBelayText is the log text of the backstop's belay request, as
// requestOrder writes it.
const budgetBelayText = "belay requested: " + budgetCeilingReason

// backstopMark is the payload field and value that mark the backstop's
// belay request in the log. The backstop sends one per order: an order
// whose log holds the mark is not belayed again. A captain's belay with the
// same reason carries no mark.
const (
	backstopField = "backstop"
	backstopValue = "budget_ceiling"
)

// canonBudgetContext is a budget hail's context as the bridge stores it:
// raw's four counts, re-encoded, so the question and the stored context
// agree. A context that doesn't decode counts as all zeros.
func canonBudgetContext(raw json.RawMessage) (json.RawMessage, comms.BudgetContext) {
	var c comms.BudgetContext
	_ = json.Unmarshal(raw, &c)
	out, err := json.Marshal(c)
	if err != nil {
		return raw, c
	}
	return out, c
}

// budgetQuestion is a budget hail's question, which the bridge writes from
// its context: "ORD-0001 has used 1.2M of 1M tokens and 140 of 150 turns.
// Extend by its budget again, or stop?". A field without a limit shows
// its usage alone.
func budgetQuestion(orderID string, c comms.BudgetContext) string {
	part := func(used, limit uint64, unit string) string {
		if limit == 0 {
			return ShortCount(used) + " " + unit
		}
		return ShortCount(used) + " of " + ShortCount(limit) + " " + unit
	}
	return fmt.Sprintf("%s has used %s and %s. Extend by its budget again, or stop?", orderID,
		part(c.Used, c.Limit, "tokens"), part(c.TurnsUsed, c.TurnsLimit, "turns"))
}

// ShortCount is n for a person to read: 950, 3.5k, 420k, 1M, 1.2M. The
// CLI's USAGE column uses it too (#120).
func ShortCount(n uint64) string {
	f := func(v float64, unit string) string {
		return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + unit
	}
	switch {
	case n >= 1_000_000:
		return f(float64(n)/1e6, "M")
	case n >= 1000:
		return f(float64(n)/1e3, "k")
	}
	return strconv.FormatUint(n, 10)
}

// limitText is limit for the log: "2M tokens, 20 turns", "unlimited" for
// a field of 0.
func limitText(limit Budget) string {
	field := func(n int64, unit string) string {
		if n <= 0 {
			return "unlimited " + unit
		}
		return ShortCount(uint64(n)) + " " + unit
	}
	return field(limit.Tokens, "tokens") + ", " + field(limit.Turns, "turns")
}

// budgetLimit is limit on the wire.
func budgetLimit(limit Budget) comms.BudgetLimit {
	return comms.BudgetLimit{Tokens: uint64(max(limit.Tokens, 0)), Turns: uint64(max(limit.Turns, 0))}
}

// extendBudget is the budget-extend step (#119), run when the captain
// answers budget hail hailID extend: store.ExtendBudget increments the
// order's budget_extended, keyed by the hail so a re-run extends nothing
// more, and logs "budget extended for hail H-0001: limit now ...". It
// returns the order's new limit, its budget resolved for in.CrewID
// (resolveBudget) times 1 + budget_extended (budgetFor), which the answer
// carries.
func (d *Daemon) extendBudget(ctx context.Context, in MissionInput, hailID string) (comms.BudgetLimit, error) {
	c, err := d.store.GetCrew(ctx, in.CrewID)
	if err != nil {
		return comms.BudgetLimit{}, fmt.Errorf("crew %s: %w", in.CrewID, err)
	}
	limitOf := func(o store.Order) Budget {
		b, _ := d.resolveBudget(o, c)
		limit, _ := budgetFor(o, b, nil, "")
		return limit
	}
	o, _, err := d.store.ExtendBudget(ctx, in.OrderID, hailID, func(o store.Order) (store.LogEntry, error) {
		limit := limitOf(o)
		return logEntry(logOrder, in.CrewID, in.OrderID, "budget extended for hail "+hailID+": limit now "+limitText(limit),
			map[string]any{"hail_id": hailID, "budget_extended": o.BudgetExtended, "limit": limit})
	})
	if err != nil {
		return comms.BudgetLimit{}, fmt.Errorf("extend the budget of %s: %w", in.OrderID, err)
	}
	return budgetLimit(limitOf(o)), nil
}

// ceilingPassed reports whether used passes 125% of limit in tokens or in
// turns; a field of 0 is unlimited.
func ceilingPassed(used, limit Budget) bool {
	over := func(used, limit int64) bool { return limit > 0 && used*4 > limit*5 }
	return over(used.Tokens, limit.Tokens) || over(used.Turns, limit.Turns)
}

// budgetCeiling is the bridge's backstop (#119), run in the inbox
// transaction of a usage report that changed order id's usage row, from
// crew member crewID's beam. When the order's recorded usage, summed over
// every beam, passes 125% of its current limit (ceilingPassed), it sends
// the order's mission a belay with reason budgetCeilingReason, through
// BelayOrder's path (orderRequest): the same checks, message and log
// entry, in the same transaction, the entry's payload marked
// backstop=budget_ceiling. Only an open order with a mission to belay is
// belayed (belayCheck): usage reports also arrive for ended orders. An
// order whose log already holds the mark is left alone, so a report that
// follows the belay sends no second one. The budget is resolved as
// sendOrder does, for the order's assignee, else for crewID, the crew
// member that reported.
func (d *Daemon) budgetCeiling(ctx context.Context, tx store.InboxTx, crewID, id string) error {
	o, err := tx.Order(id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("order %s: %w", id, err)
	}
	if belayCheck(o) != nil {
		return nil
	}
	crew := cmp.Or(o.Assignee, crewID)
	division, err := store.CrewDivision(ctx, tx.SQL(), crew)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("crew %s: %w", crew, err)
	}
	b, _ := d.resolveBudget(o, store.Crew{ID: crew, Division: division})
	usage, err := tx.OrderUsage(id)
	if err != nil {
		return fmt.Errorf("order %s usage: %w", id, err)
	}
	limit, used := budgetFor(o, b, usage, "")
	if !ceilingPassed(used, limit) {
		return nil
	}
	if sent, err := tx.OrderLogMarked(id, logOrder, backstopField, backstopValue); err != nil || sent {
		return err
	}
	entry, err := d.orderRequest(tx.SQL(), o, comms.TypeBelay, comms.BelayBody{Reason: budgetCeilingReason}, "belay requested",
		budgetCeilingReason)
	if err != nil {
		return err
	}
	if entry.Payload, err = json.Marshal(map[string]any{"reason": budgetCeilingReason, backstopField: backstopValue,
		"used": used, "limit": limit}); err != nil {
		return err
	}
	if _, err := tx.AppendLog(entry); err != nil {
		return err
	}
	d.logger.Warn("budget: recorded usage passed 125% of the order's budget; belaying it", "order", id,
		"tokens", used.Tokens, "token_limit", limit.Tokens, "turns", used.Turns, "turn_limit", limit.Turns)
	return nil
}

// SetOrderBudget sets open order id's own budget (#120): tokens and turns,
// each left as it is when nil, 0 handing the field back to its division's
// budget (resolveBudget). An ended order is refused, and so is a split
// one, whose mission has ended: its child orders carry budgets of their
// own. budget_extended is kept, so the new limit is the new budget times
// 1 + budget_extended; the log entry and the view say so. A change is
// logged "budget set: limit now ..." and reaches a running beam with the
// order's next send (sendOrder), a recall or a watch change; a budget hail
// already raised is answered as before, its extend carrying the new budget.
// A change that changes nothing writes nothing.
func (d *Daemon) SetOrderBudget(ctx context.Context, id string, tokens, turns *int64) (store.Order, error) {
	if tokens == nil && turns == nil {
		return store.Order{}, badRequest("nothing to set: give tokens or turns")
	}
	for _, f := range []struct {
		name string
		v    *int64
	}{{"tokens", tokens}, {"turns", turns}} {
		if f.v != nil && *f.v < 0 {
			return store.Order{}, badRequest("budget %s must not be negative", f.name)
		}
	}
	o, _, err := d.store.SetOrderBudget(ctx, id, tokens, turns, func(o store.Order, tok, tur int64) (store.LogEntry, error) {
		if store.OrderEnded(o.State) || o.State == store.OrderSplit {
			return store.LogEntry{}, badRequest("order %s is %s, too late to change its budget", id, o.State)
		}
		o.BudgetTokens, o.BudgetTurns = tok, tur
		b, src := d.resolveBudget(o, store.Crew{ID: o.Assignee, Division: o.AssigneeDivision})
		limit, _ := budgetFor(o, b, nil, "")
		text := "budget set: limit now " + limitText(limit)
		switch {
		case o.BudgetExtended == 1:
			text += ", with 1 extension"
		case o.BudgetExtended > 1:
			text += fmt.Sprintf(", with %d extensions", o.BudgetExtended)
		}
		return logEntry(logOrder, o.Assignee, id, text, map[string]any{"budget": Budget{Tokens: tok, Turns: tur},
			"source": src, "budget_extended": o.BudgetExtended, "limit": limit})
	})
	if errors.Is(err, sql.ErrNoRows) {
		return store.Order{}, fmt.Errorf("order %s: %w", id, ErrNotFound)
	}
	return o, err
}
