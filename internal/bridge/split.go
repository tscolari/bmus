package bridge

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/store"
)

// Caps on an order_split hail (#155): the children one split may create,
// a child's title (runes, one line) and its body (bytes). The schema and
// split_order carry the first two as well.
const (
	maxSplitChildren = 10
	maxSplitTitle    = 200
	maxSplitBody     = 32 << 10
)

// splitCreatedBy is the created_by of the child orders hail id created:
// it marks the batch, so a re-run of create-children creates none twice
// (store.InsertChildOrders).
func splitCreatedBy(hailID string) string { return "hail:" + hailID }

// checkSplit checks order_split hail h, raised by crew member c on order
// o, and returns the child orders it asks for, defaults resolved: each
// child's division and priority are o's when it gives none, and its repos
// are o's. why says what is wrong with a malformed split, which creates
// nothing:
//   - c isn't in the command division;
//   - the options aren't [allow, deny] (option 0 must mean allow);
//   - the context isn't one OrderSplitContext with no unknown fields, or
//     names another parent;
//   - no children, or more than maxSplitChildren;
//   - a child's title (trimmed) is empty, longer than maxSplitTitle runes
//     or holds a control character, or its body (trimmed) is empty or
//     longer than maxSplitBody;
//   - a child has no division, its own or o's, or one the roster doesn't
//     list (checkDivision), or a priority out of range;
//   - o's repos no longer pass checkOrderRepos.
//
// Every check is on the hail's stored context, the one the captain sees.
func (d *Daemon) checkSplit(h store.Hail, o store.Order, c store.Crew) (children []store.Order, why string) {
	if c.Division != DivisionCommand {
		return nil, fmt.Sprintf("only Command splits orders, and %s is not in the %s division", c.ID, DivisionCommand)
	}
	if !slices.Equal(h.Options, []string{comms.OptionAllow, comms.OptionDeny}) {
		return nil, fmt.Sprintf("its options must be [%s %s]", comms.OptionAllow, comms.OptionDeny)
	}
	var sc comms.OrderSplitContext
	dec := json.NewDecoder(bytes.NewReader(h.Context))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return nil, "its context is not a split: " + err.Error()
	}
	if dec.More() {
		return nil, "its context is not a split: more than one JSON value"
	}
	switch n := len(sc.Children); {
	case sc.Parent != "" && sc.Parent != o.ID:
		return nil, fmt.Sprintf("it names parent %s, not its own order %s", sc.Parent, o.ID)
	case n == 0:
		return nil, "it has no children"
	case n > maxSplitChildren:
		return nil, fmt.Sprintf("it has %d children, more than %d", n, maxSplitChildren)
	}
	if err := d.checkOrderRepos(o.Repos); err != nil {
		return nil, fmt.Sprintf("order %s's repos: %v", o.ID, err)
	}
	for i, ch := range sc.Children {
		n := i + 1
		title, body := strings.TrimSpace(ch.Title), strings.TrimSpace(ch.Body)
		switch {
		case title == "":
			return nil, fmt.Sprintf("child %d has no title", n)
		case !utf8.ValidString(title) || utf8.RuneCountInString(title) > maxSplitTitle:
			return nil, fmt.Sprintf("child %d's title is longer than %d characters", n, maxSplitTitle)
		case strings.ContainsFunc(title, unicode.IsControl):
			return nil, fmt.Sprintf("child %d's title is not one line", n)
		case body == "":
			return nil, fmt.Sprintf("child %d has no body", n)
		case len(body) > maxSplitBody:
			return nil, fmt.Sprintf("child %d's body is longer than %d bytes", n, maxSplitBody)
		}
		division := cmp.Or(strings.TrimSpace(ch.Division), o.Division)
		if division == "" {
			return nil, fmt.Sprintf("child %d has no division, and order %s has none", n, o.ID)
		}
		if err := d.checkDivision(division); err != nil {
			return nil, fmt.Sprintf("child %d: %v", n, err)
		}
		priority := o.Priority
		if ch.Priority != nil {
			priority = *ch.Priority
		}
		if priority < store.PriorityRoutine || priority > store.PriorityRed {
			return nil, fmt.Sprintf("child %d's priority %d is out of range (0 routine, 1 yellow, 2 red)", n, priority)
		}
		children = append(children, store.Order{Title: title, Body: body, Division: division, Priority: priority,
			Repos: o.Repos})
	}
	return children, ""
}

// splitInvalid is why order_split hail id is malformed (checkSplit), read
// by classify; "" for a well-formed split.
func (d *Daemon) splitInvalid(ctx context.Context, h store.Hail) (string, error) {
	o, err := d.store.GetOrder(ctx, h.OrderID)
	if err != nil {
		return "", fmt.Errorf("order %s: %w", h.OrderID, err)
	}
	c, err := d.store.GetCrew(ctx, h.CrewID)
	if err != nil {
		return "", fmt.Errorf("crew %s: %w", h.CrewID, err)
	}
	_, why := d.checkSplit(h, o, c)
	return why, nil
}

// denySplit is the deny-split step: it decides open order_split hail id
// by policy, before anyone sees it, with decision "invalid order split:
// <why>", option deny and route auto (decideOpen), logged "hail H-0001
// decided by policy: invalid order split: <why>". It reports false for a
// hail no longer open.
func (d *Daemon) denySplit(ctx context.Context, id, why string) (bool, error) {
	return d.denyByPolicy(ctx, id, "invalid order split: "+why, why)
}

// splitOutcome is the create-children step's recorded result: the answer's
// decision, the children's ids comma separated, or why none were created.
type splitOutcome struct {
	Decision string
	Refused  bool `json:",omitempty"`
}

// createChildren is the create-children step (#155), run by the mission
// for an order_split hail answered with option 0 before it relays the
// answer:
//   - Children this hail already created (store.ChildBatch, by
//     splitCreatedBy) are the result: a re-run after the insert committed
//     never checks again, and so never refuses what it created.
//   - A countermand (#153) to anything but option 0 refuses the split
//     (store.Hail.CountermandedAway). Without one the queued option 0
//     answer stands,
//     whatever its decision text (`bmus hails answer --option 0
//     --decision "ok, go ahead"`).
//   - checkSplit runs again: the roster may have changed, or the captain
//     answered before classify could deny it.
//   - The children and their parent's split entry, "order split into N
//     children: ORD-0002, ORD-0003", commit in one transaction
//     (store.InsertChildOrders); then each child's division is woken.
//
// A refused split creates nothing. It notes "split refused: <why>" on the
// hail's rationale and logs "order split refused: <why>", in one
// transaction, once (store.RefuseSplit), and answers that text with
// option deny.
func createChildren(ctx context.Context, d *Daemon, in MissionInput, hailID string) (splitOutcome, error) {
	ids, err := d.store.ChildBatch(ctx, in.OrderID, splitCreatedBy(hailID))
	if err != nil || len(ids) > 0 {
		return splitOutcome{Decision: strings.Join(ids, ", ")}, err
	}
	h, err := d.store.GetHail(ctx, hailID)
	if err != nil {
		return splitOutcome{}, fmt.Errorf("hail %s: %w", hailID, err)
	}
	o, err := d.store.GetOrder(ctx, in.OrderID)
	if err != nil {
		return splitOutcome{}, fmt.Errorf("order %s: %w", in.OrderID, err)
	}
	c, err := d.store.GetCrew(ctx, h.CrewID)
	if err != nil {
		return splitOutcome{}, fmt.Errorf("crew %s: %w", h.CrewID, err)
	}
	var (
		children []store.Order
		why      string
	)
	switch {
	case h.Status != store.HailDecided:
		why = "hail " + h.ID + " is " + h.Status
	case h.CountermandedAway():
		why = fmt.Sprintf("the captain countermanded it: %s", h.Decision)
	default:
		children, why = d.checkSplit(h, o, c)
	}
	if why == "" {
		ids, err := d.store.InsertChildOrders(ctx, o.ID, splitCreatedBy(h.ID), children, func(ids []string) (store.LogEntry, error) {
			return logEntry(logOrder, in.CrewID, o.ID, fmt.Sprintf("order split into %d children: %s", len(ids), strings.Join(ids, ", ")),
				map[string]any{"hail_id": h.ID, "children": ids})
		})
		switch {
		case errors.Is(err, store.ErrParentEnded):
			why = err.Error()
		case err != nil:
			return splitOutcome{}, err
		default:
			woken := map[string]bool{}
			for _, ch := range children {
				if !woken[ch.Division] {
					woken[ch.Division] = true
					d.wakeDivision(ctx, ch.Division)
				}
			}
			return splitOutcome{Decision: strings.Join(ids, ", ")}, nil
		}
	}
	text := "order split refused: " + why
	entry, err := logEntry(logOrder, in.CrewID, o.ID, text, map[string]any{"hail_id": h.ID, "reason": why})
	if err != nil {
		return splitOutcome{}, err
	}
	if _, err := d.store.RefuseSplit(ctx, h.ID, why, entry); err != nil {
		return splitOutcome{}, err
	}
	return splitOutcome{Decision: text, Refused: true}, nil
}

// Caps on what splitQuestion shows of each child.
const (
	maxSplitQuestionTitle    = 4 * maxSplitTitle // bytes: room for any title checkSplit takes
	maxSplitQuestionDivision = 40
)

// splitQuestion is the question an order_split hail on order o is stored
// with, built by the bridge from the hail's context (#155): "Split
// ORD-0001 into 2 orders: 1) <title> [<division>, <priority>]; 2) ...",
// each child's title cut to maxSplitQuestionTitle and its division to
// maxSplitQuestionDivision, one line each, defaults resolved from o. What
// the captain approves is what the bridge will create; the agent's own
// question is dropped. A context that doesn't decode, or has no children,
// reads as malformed (classify denies it).
func splitQuestion(o store.Order, context json.RawMessage) string {
	var sc comms.OrderSplitContext
	if err := json.Unmarshal(context, &sc); err != nil || len(sc.Children) == 0 {
		return "Split " + o.ID + " into child orders: the request is malformed, and the bridge denies it"
	}
	var parts []string
	for i, ch := range sc.Children {
		if i == maxSplitChildren {
			parts = append(parts, fmt.Sprintf("and %d more", len(sc.Children)-i))
			break
		}
		priority := o.Priority
		if ch.Priority != nil {
			priority = *ch.Priority
		}
		division := cmp.Or(strings.TrimSpace(ch.Division), o.Division, "no division")
		parts = append(parts, fmt.Sprintf("%d) %s [%s, %s]", i+1, clip(oneLine(strings.TrimSpace(ch.Title)), maxSplitQuestionTitle),
			clip(oneLine(division), maxSplitQuestionDivision), priorityName(priority)))
	}
	return fmt.Sprintf("Split %s into %d orders: %s", o.ID, len(sc.Children), strings.Join(parts, "; "))
}

// oneLine is s with each control character, newlines included, and each
// format character (unicode.Cf: bidi overrides such as U+202E, zero-width
// spaces) made a space.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
}

// priorityName is an order priority's name: routine, yellow or red.
func priorityName(p int) string {
	switch p {
	case store.PriorityRoutine:
		return "routine"
	case store.PriorityYellow:
		return "yellow"
	case store.PriorityRed:
		return "red"
	}
	return fmt.Sprintf("priority %d", p)
}
