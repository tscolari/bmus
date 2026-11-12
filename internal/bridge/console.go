package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/tscolari/bmus/internal/console"
	"github.com/tscolari/bmus/internal/store"
)

// consoleSource is the daemon as the console reads it (console.Source,
// #72): the store's log feed and the views rpc.go returns, and the
// actions (#73, #78). ctx is the daemon's, as an RPC handler's.
type consoleSource struct {
	d       *Daemon
	feed    *store.Feed
	ctx     context.Context
	actions *actionGate
}

// actionGate tracks the console's in-flight actions, for Run to wait for
// them once the console is closed.
type actionGate struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// enter counts an action in, unless the gate is closed.
func (g *actionGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.wg.Add(1)
	return true
}

func (g *actionGate) leave() { g.wg.Done() }

// close refuses new actions and waits for those in flight.
func (g *actionGate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.wg.Wait()
}

func (c consoleSource) Subscribe() *store.Subscription { return c.feed.Subscribe() }

// Snapshot reads the log's last id first and everything else after it
// (console.Snapshot). Its orders are the open and away ones, and any
// other order an entry of its log names. Its hails are the open ones and
// the decided ones still inside their countermand window. Its repos are
// the registry's names.
func (c consoleSource) Snapshot(ctx context.Context) (console.Snapshot, error) {
	d := c.d
	last, err := d.store.LastLogID(ctx)
	if err != nil {
		return console.Snapshot{}, err
	}
	crew, err := d.store.ListCrew(ctx)
	if err != nil {
		return console.Snapshot{}, err
	}
	orders, err := d.store.ListOrders(ctx)
	if err != nil {
		return console.Snapshot{}, err
	}
	hails, err := d.store.ListHails(ctx, store.HailOpen)
	if err != nil {
		return console.Snapshot{}, err
	}
	// Decided hails the captain may still countermand: the console offers
	// the control only on hails it holds (#153).
	countermandable, err := d.store.ListCountermandable(ctx, time.Now())
	if err != nil {
		return console.Snapshot{}, err
	}
	hails = append(hails, countermandable...)
	status, err := c.Status(ctx)
	if err != nil {
		return console.Snapshot{}, err
	}
	entries, err := d.store.ListLogLast(ctx, last, console.SnapshotLog)
	if err != nil {
		return console.Snapshot{}, err
	}
	urls := make(map[string]string, len(d.repos))
	for name, u := range d.repos {
		urls[name] = redactURL(u)
	}
	snap := console.Snapshot{LastLogID: last, Status: status, Repos: d.registeredRepos(), RepoURLs: urls}
	crewViews := []CrewView{}
	for _, cm := range crew {
		crewViews = append(crewViews, d.crewView(cm))
	}
	// Finished orders the log window names: the mission record shows
	// them after a reload, belayed and withdrawn ones included, which
	// have no done or fail entry (#78).
	named := map[string]bool{}
	for _, e := range entries {
		if e.OrderID != "" {
			named[e.OrderID] = true
		}
	}
	orderViews := []OrderView{}
	for _, o := range orders {
		if slices.Contains([]string{store.OrderPool, store.OrderDirected, store.OrderAway, store.OrderHailing, store.OrderSplit}, o.State) || named[o.ID] {
			orderViews = append(orderViews, orderView(o))
		}
	}
	hailViews := []HailView{}
	for _, h := range hails {
		hailViews = append(hailViews, hailView(h))
	}
	logViews := []LogEntryView{}
	for _, e := range entries {
		logViews = append(logViews, logEntryView(e))
	}
	snap.Crew, snap.Orders, snap.Hails, snap.Log = crewViews, orderViews, hailViews, logViews
	return snap, nil
}

// Status is Daemon.Status, keyed on everything but the log id.
func (c consoleSource) Status(ctx context.Context) (console.Status, error) {
	st, err := c.d.Status(ctx)
	if err != nil {
		return console.Status{}, err
	}
	keyed := st
	keyed.LastLogID = 0
	key, err := json.Marshal(keyed)
	if err != nil {
		return console.Status{}, err
	}
	return console.Status{View: st, Key: string(key)}, nil
}

func (c consoleSource) LastLogID(ctx context.Context) (int64, error) {
	return c.d.store.LastLogID(ctx)
}

func (c consoleSource) Log(ctx context.Context, sinceID int64, limit int) ([]store.LogEntry, error) {
	return c.d.store.ListLogLimit(ctx, sinceID, limit)
}

func (c consoleSource) LogBefore(ctx context.Context, beforeID int64, limit int) ([]store.LogEntry, error) {
	return c.d.store.ListLogLast(ctx, beforeID-1, limit)
}

func (c consoleSource) LogView(e store.LogEntry) any { return logEntryView(e) }

// Views reads the crew member, order and hail e names: its crew_id, its
// order_id and its payload's hail_id, and the order's parent, whose open
// children count a child's change may move (#155).
func (c consoleSource) Views(ctx context.Context, e store.LogEntry) ([]console.Event, error) {
	d := c.d
	var out []console.Event
	if e.CrewID != "" {
		cm, err := d.store.GetCrew(ctx, e.CrewID)
		if err == nil {
			out = append(out, console.Event{Type: "crew", Data: d.crewView(cm)})
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if e.OrderID != "" {
		o, err := d.store.GetOrder(ctx, e.OrderID)
		if err == nil {
			out = append(out, console.Event{Type: "order", Data: orderView(o)})
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if o.ParentID != "" {
			p, err := d.store.GetOrder(ctx, o.ParentID)
			if err == nil {
				out = append(out, console.Event{Type: "order", Data: orderView(p)})
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
	}
	var payload struct {
		HailID string `json:"hail_id"`
	}
	if json.Unmarshal(e.Payload, &payload) == nil && payload.HailID != "" {
		h, err := d.store.GetHail(ctx, payload.HailID)
		if err == nil {
			out = append(out, console.Event{Type: "hail", Data: hailView(h)})
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return out, nil
}

// CreateOrder is order.create, returning the new order's view.
func (c consoleSource) CreateOrder(ctx context.Context, args json.RawMessage) (any, error) {
	return c.act(ctx, func(ctx context.Context) (any, error) {
		var a OrderCreateArgs
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		id, err := c.d.createOrder(ctx, a)
		if err != nil {
			return nil, err
		}
		o, err := c.d.store.GetOrder(ctx, id)
		if err != nil {
			return nil, err
		}
		return orderView(o), nil
	})
}

// AnswerHail is hails.answer for hail id. args are HailsAnswerArgs'
// option and decision; the hail's id comes from the path only.
func (c consoleSource) AnswerHail(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return c.act(ctx, func(ctx context.Context) (any, error) {
		var a struct {
			Option   *int   `json:"option,omitempty"`
			Decision string `json:"decision,omitempty"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		return c.d.answerHail(ctx, HailsAnswerArgs{HailID: id, Option: a.Option, Decision: a.Decision})
	})
}

// CountermandHail is hails.countermand for hail id. args are
// HailsCountermandArgs' option, decision and reason; the hail's id comes
// from the path only.
func (c consoleSource) CountermandHail(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return c.act(ctx, func(ctx context.Context) (any, error) {
		var a struct {
			Option   *int   `json:"option,omitempty"`
			Decision string `json:"decision,omitempty"`
			Reason   string `json:"reason,omitempty"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		return c.d.countermandHail(ctx, HailsCountermandArgs{HailID: id, Option: a.Option, Decision: a.Decision, Reason: a.Reason})
	})
}

// AddCrew is crew.add.
func (c consoleSource) AddCrew(ctx context.Context, args json.RawMessage) (any, error) {
	return c.act(ctx, func(ctx context.Context) (any, error) {
		var a CrewAddArgs
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		return c.d.addCrew(ctx, a)
	})
}

// BelayOrder is order.belay for order id: args are OrderActionArgs'
// reason, as for orderAction.
func (c consoleSource) BelayOrder(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return c.act(ctx, func(ctx context.Context) (any, error) {
		var a struct {
			Reason string `json:"reason,omitempty"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		return c.d.belayAction(ctx, OrderActionArgs{ID: id, Reason: a.Reason})
	})
}

// RecallOrder is order.recall for order id.
func (c consoleSource) RecallOrder(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return c.orderAction(ctx, id, args, c.d.RecallOrder)
}

// WithdrawOrder is order.withdraw for order id.
func (c consoleSource) WithdrawOrder(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return c.orderAction(ctx, id, args, c.d.WithdrawOrder)
}

// orderAction runs act as Daemon.orderAction does for order.belay,
// order.recall and order.withdraw. args are OrderActionArgs' reason; the
// order's id comes from the path only.
func (c consoleSource) orderAction(ctx context.Context, id string, args json.RawMessage,
	act func(ctx context.Context, id, reason string) error) (any, error) {
	return c.act(ctx, func(ctx context.Context) (any, error) {
		var a struct {
			Reason string `json:"reason,omitempty"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		return c.d.orderAction(ctx, OrderActionArgs{ID: id, Reason: a.Reason}, act)
	})
}

// RouteOrder is order.route for order id. args are OrderRouteArgs'
// assignee and division.
func (c consoleSource) RouteOrder(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return c.act(ctx, func(ctx context.Context) (any, error) {
		var a struct {
			Assignee *string `json:"assignee,omitempty"`
			Division *string `json:"division,omitempty"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		return c.d.routeOrder(ctx, OrderRouteArgs{ID: id, Assignee: a.Assignee, Division: a.Division})
	})
}

// ReenergizeCrew is crew.reenergize for crew member id; args must be {}.
func (c consoleSource) ReenergizeCrew(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return c.act(ctx, func(ctx context.Context) (any, error) {
		if err := decodeArgs(args, &struct{}{}); err != nil {
			return nil, err
		}
		return c.d.crewAction(ctx, CrewIDArgs{ID: id}, c.d.ReenergizeCrew)
	})
}

// SetStandingOrders is standing_orders.set.
func (c consoleSource) SetStandingOrders(ctx context.Context, args json.RawMessage) (any, error) {
	return c.act(ctx, func(ctx context.Context) (any, error) {
		var a StandingOrdersSetArgs
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		return c.d.switchStandingOrders(ctx, a)
	})
}

// consolePushTimeout bounds the console's push (#4), shorter than
// order.push's gitPushTimeout: the page waits on the request.
const consolePushTimeout = 60 * time.Second

// PushOrder is order.push for order id. args are OrderPushArgs'
// target_branch and force; the order's id comes from the path only. It
// returns the pushed repos.
func (c consoleSource) PushOrder(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return c.actFor(consolePushTimeout, "push", func(ctx context.Context) (any, error) {
		var a struct {
			TargetBranch string `json:"target_branch,omitempty"`
			Force        bool   `json:"force,omitempty"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return nil, err
		}
		pushed, err := c.d.PushOrder(ctx, id, a.TargetBranch, a.Force)
		if err != nil {
			return nil, err
		}
		if pushed == nil {
			pushed = []PushedRepo{}
		}
		return pushed, nil
	})
}

// consoleReportTimeout bounds the console's mission report of an order
// (#104), its diff included: the page waits on the request.
const consoleReportTimeout = 30 * time.Second

// OrderReport is done order id's mission report, each repo's diff capped
// at consoleDiffCap, read as readFor reads.
func (c consoleSource) OrderReport(ctx context.Context, id string) (any, error) {
	return c.readFor(ctx, consoleReportTimeout, "mission report", func(ctx context.Context) (any, error) {
		return c.d.OrderReport(ctx, id, consoleDiffCap)
	})
}

// act runs an action as the RPC server runs a handler, bounded by
// rpcHandlerTimeout: see actFor.
func (c consoleSource) act(_ context.Context, f func(context.Context) (any, error)) (any, error) {
	return c.actFor(rpcHandlerTimeout, "action", f)
}

// actFor runs action what as the RPC server runs a handler: refused once
// the daemon is stopping, on the daemon's context bounded by timeout, and
// with its error as an *rpcError carrying its RPC code. An action that
// runs past timeout fails with console.CodeTimeout and a message that
// says so; the error it ended with is logged. The request's context isn't
// used: a client that goes away doesn't cut an action short. Run waits
// for it before it shuts down.
func (c consoleSource) actFor(timeout time.Duration, what string, f func(context.Context) (any, error)) (any, error) {
	if c.ctx.Err() != nil || !c.actions.enter() {
		return nil, &rpcError{code: CodeShutdown, err: errors.New("bridge is shutting down")}
	}
	defer c.actions.leave()
	return c.runFor(c.ctx, timeout, what, "; it may have taken effect in part: see the captain's log", f)
}

// readFor runs read what as actFor runs an action, except that it takes
// no part in the action gate and runs on the request's context: a read
// changes nothing, so a client that goes away, or the daemon stopping,
// ends it. One that runs past timeout fails with console.CodeTimeout.
func (c consoleSource) readFor(ctx context.Context, timeout time.Duration, what string, f func(context.Context) (any, error)) (any, error) {
	if c.ctx.Err() != nil {
		return nil, &rpcError{code: CodeShutdown, err: errors.New("bridge is shutting down")}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(c.ctx, cancel)()
	return c.runFor(ctx, timeout, what, "", f)
}

// runFor runs f on parent bounded by timeout, for actFor and readFor,
// with its error as an *rpcError carrying its RPC code. Past timeout the
// error is console.CodeTimeout's, with a message naming what and the
// timeout, then note; the error f ended with is logged.
func (c consoleSource) runFor(parent context.Context, timeout time.Duration, what, note string, f func(context.Context) (any, error)) (any, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	v, err := f(ctx)
	if err != nil {
		if c.ctx.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// The message reaches the page: err, which may hold git's
			// output, stays out of it, as for a conflict.
			if c.d != nil {
				c.d.logger.Info("console: "+what+" timed out", "err", err)
			}
			return nil, &rpcError{code: console.CodeTimeout, err: fmt.Errorf("%s timed out after %s%s", what, timeout, note)}
		}
		return nil, codedError(c.ctx, err)
	}
	return v, nil
}
