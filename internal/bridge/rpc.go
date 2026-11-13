package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tscolari/bmus/internal/store"
	"github.com/tscolari/bmus/internal/tracker"
)

// The local RPC socket (plan §9): <home>/bridge.sock, one newline-delimited
// JSON request and one reply per connection.
//
//	-> {"cmd":"crew.list","args":{}}
//	<- {"ok":true,"result":{"crew":[...]}}
//	<- {"ok":false,"error":"hail H-0009: not found","code":"not_found"}
//
// The commands, their args and results are the rpc* types below. Unknown
// fields in a request or its args are an error.

// SocketFile is the RPC socket's name under the bridge home.
const SocketFile = "bridge.sock"

const (
	// rpcConnTimeout bounds reading a request and writing its reply.
	rpcConnTimeout = 10 * time.Second
	// rpcHandlerTimeout bounds a handler, but those in rpcHandlerTimeouts.
	rpcHandlerTimeout = 30 * time.Second
	// maxRPCConns caps connections served at once; more get unavailable.
	maxRPCConns = 64
	// MaxRPCRequest caps a request line, newline included.
	MaxRPCRequest = 1 << 20
	// maxSocketPath bounds the socket path (sun_path is 108 bytes on Linux,
	// 104 on macOS).
	maxSocketPath = 104

	// Default and maximum log.tail page size.
	defaultLogTailLimit = 500
	maxLogTailLimit     = 1000
)

// RPC error codes, in a failed Response's Code.
const (
	CodeBadRequest         = "bad_request"         // malformed request, unknown arg, invalid value
	CodeUnknownCmd         = "unknown_cmd"         // no such command
	CodeNotFound           = "not_found"           // ErrNotFound
	CodeExists             = "exists"              // ErrExists
	CodeUnavailable        = "unavailable"         // ErrUnavailable
	CodeHailClosed         = "hail_closed"         // store.ErrHailClosed
	CodeCountermandExpired = "countermand_expired" // store.ErrCountermandExpired
	CodeConflict           = "conflict"            // ErrPushRejected: the server refused a push (e.g. not a fast-forward)
	CodeShutdown           = "shutting_down"       // the daemon is stopping
	CodeForbidden          = "forbidden"           // the peer isn't the bridge's user
	CodeInternal           = "internal"            // anything else
)

// Request is an RPC request.
type Request struct {
	Cmd  string          `json:"cmd"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Response is an RPC reply.
type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

// Command args and results.
type (
	// CrewAddArgs are crew.add's args. Dossier is an absolute path.
	// Harness is one of Harnesses, or empty for serve's default (sdk);
	// Model is opaque to the bridge, empty for the harness's default.
	// Division must be in the roster, if it has divisions.
	CrewAddArgs struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Rank     string `json:"rank,omitempty"`
		Division string `json:"division,omitempty"`
		Spec     string `json:"spec,omitempty"`
		Dossier  string `json:"dossier,omitempty"`
		Harness  string `json:"harness,omitempty"`
		Model    string `json:"model,omitempty"`
	}
	// CrewIDArgs are crew.dismiss's and crew.reenergize's args. Both
	// return a CrewActionResult.
	CrewIDArgs struct {
		ID string `json:"id"`
	}
	// CrewActionResult is the crew member afterwards and the action's
	// Outcome: "done", "requested" (a tour finishes it on its own) or
	// "under_way" (an earlier call's; nothing done). See Daemon.DismissCrew
	// and Daemon.ReenergizeCrew.
	CrewActionResult struct {
		CrewView
		Outcome string `json:"outcome"`
	}
	// CrewListResult is crew.list's result; crew.add returns one CrewView.
	CrewListResult struct {
		Crew []CrewView `json:"crew"`
	}

	// OrderCreateArgs are order.create's args. Title is required unless
	// Source names a tracker item, whose title it then defaults to; and
	// an assignee (a directed order) or a division (a pool order) or
	// both.
	// Priority is store.PriorityRoutine (the default), PriorityYellow or
	// PriorityRed.
	OrderCreateArgs struct {
		Title        string      `json:"title"`
		Body         string      `json:"body,omitempty"`
		Division     string      `json:"division,omitempty"`
		Assignee     string      `json:"assignee"`
		Priority     int         `json:"priority,omitempty"`
		Repos        []OrderRepo `json:"repos"`
		BudgetTokens int64       `json:"budget_tokens,omitempty"`
		// Model overrides the assignee's model for this order; opaque
		// to the bridge.
		Model string `json:"model,omitempty"`
		// Source is where the order comes from, free-form. "tracker:<ref>"
		// puts the tracker item's text in the body (#54).
		Source string `json:"source,omitempty"`
	}
	// OrderRepo is a repo of an order: a name from the bridge's registry
	// and the git ref the work starts from. Phase 1 orders name exactly
	// one.
	OrderRepo struct {
		Name    string `json:"name"`
		BaseRef string `json:"base_ref"`
	}
	OrderCreateResult struct {
		ID string `json:"id"`
	}
	// OrderActionArgs are order.belay's, order.recall's and
	// order.withdraw's args: the order's id and an optional reason, which
	// goes into the captain's log (and, for belay and recall, to the
	// beam). Each returns the order as an OrderView (belay as an
	// OrderBelayResult): for belay and recall, as it is when the request
	// is queued for the order's mission, which carries it out.
	OrderActionArgs struct {
		ID     string `json:"id"`
		Reason string `json:"reason,omitempty"`
	}
	// OrderBelayResult is order.belay's result: the order as an
	// OrderView, as for any order action, and Children, how many of its
	// open child orders the belay reaches (#146): withdrawn, belayed or
	// sent a belay of their own. Belayed is true when the call belayed
	// the order itself (a split order, which has no mission), false when
	// it queued the belay for the order's mission.
	OrderBelayResult struct {
		OrderView
		Children int  `json:"children"`
		Belayed  bool `json:"belayed"`
	}
	// OrderRouteArgs are order.route's args. An absent assignee or
	// division keeps the order's, and an empty one clears it; at least one
	// must be given. It returns the routed OrderView.
	OrderRouteArgs struct {
		ID       string  `json:"id"`
		Assignee *string `json:"assignee,omitempty"`
		Division *string `json:"division,omitempty"`
	}
	// OrderPushArgs are order.push's args: a done order's id, the branch
	// to push its work to (default bmus/<order>) and whether to force it.
	OrderPushArgs struct {
		ID           string `json:"id"`
		TargetBranch string `json:"target_branch,omitempty"`
		Force        bool   `json:"force,omitempty"`
	}
	// OrderPushResult is the repos order.push pushed, each to
	// TargetBranch of its registered URL.
	OrderPushResult struct {
		ID           string       `json:"id"`
		TargetBranch string       `json:"target_branch"`
		Repos        []PushedRepo `json:"repos"`
	}
	OrderListResult struct {
		Orders []OrderView `json:"orders"`
	}

	// HailsListArgs are hails.list's args: Status "open", "decided" or
	// "timed_out", or empty for every hail.
	HailsListArgs struct {
		Status string `json:"status,omitempty"`
	}
	HailsListResult struct {
		Hails []HailView `json:"hails"`
	}
	// HailsAnswerArgs are hails.answer's args: Option, Decision or both
	// (see Daemon.AnswerHail). It returns the decided HailView.
	HailsAnswerArgs struct {
		HailID   string `json:"hail_id"`
		Option   *int   `json:"option,omitempty"`
		Decision string `json:"decision,omitempty"`
	}
	// HailsCountermandArgs are hails.countermand's args: Option, Decision
	// or both, as hails.answer's, and an optional Reason (see
	// Daemon.CountermandHail). It returns a HailsCountermandResult.
	HailsCountermandArgs struct {
		HailID   string `json:"hail_id"`
		Option   *int   `json:"option,omitempty"`
		Decision string `json:"decision,omitempty"`
		Reason   string `json:"reason,omitempty"`
	}
	// HailsCountermandResult is the countermanded hail's view and, when
	// its order has no live run, a Note saying whether the agent is told.
	HailsCountermandResult struct {
		HailView
		Note string `json:"note,omitempty"`
	}

	// HailsMetricsArgs are hails.metrics's args: the hails created at or
	// after Since count (default a week before the call).
	HailsMetricsArgs struct {
		Since *time.Time `json:"since,omitempty"`
	}
	// HailsMetricsResult is hails.metrics's result: HailMetrics as JSON
	// (#156). The rates and times are null when nothing was decided;
	// times are in milliseconds.
	HailsMetricsResult struct {
		Since             time.Time      `json:"since"`
		Total             int            `json:"total"`
		Decided           int            `json:"decided"`
		Countermanded     int            `json:"countermanded"`
		ByOriginalDecider map[string]int `json:"by_original_decider"`
		// AutoResolvedShare is (policy + command) / decided.
		AutoResolvedShare *float64 `json:"auto_resolved_share"`
		// CaptainOverrides is countermanded / decided.
		CaptainOverrides     *float64 `json:"captain_overrides"`
		MedianTimeToAnswerMS *int64   `json:"median_time_to_answer_ms"`
		P90TimeToAnswerMS    *int64   `json:"p90_time_to_answer_ms"`
	}

	// StandingOrdersSetArgs are standing_orders.set's args. On is
	// required. standing_orders.get and .set return a StandingOrdersResult.
	StandingOrdersSetArgs struct {
		On *bool `json:"on"`
	}
	StandingOrdersResult struct {
		On bool `json:"on"`
	}

	// CommandGuidanceSetArgs are command.guidance.set's args. Guidance is
	// required; empty clears it. command.guidance.get and .set return a
	// CommandGuidanceResult.
	CommandGuidanceSetArgs struct {
		Guidance *string `json:"guidance"`
	}
	CommandGuidanceResult struct {
		Guidance string `json:"guidance"`
	}

	// LogTailArgs are log.tail's args: the entries above SinceID, oldest
	// first, at most Limit (default 500, capped at 1000).
	LogTailArgs struct {
		SinceID int64 `json:"since_id"`
		Limit   int   `json:"limit,omitempty"`
	}
	// LogTailResult: LastID is the last entry's id, or SinceID when there
	// are none, so a follower passes it back as the next SinceID.
	LogTailResult struct {
		Entries []LogEntryView `json:"entries"`
		LastID  int64          `json:"last_id"`
	}
)

// rpcError is a handler error with its RPC code.
type rpcError struct {
	code string
	err  error
}

func (e *rpcError) Error() string { return e.err.Error() }
func (e *rpcError) Unwrap() error { return e.err }

// Code returns e's RPC code; the console maps it to an HTTP status (#73).
func (e *rpcError) Code() string { return e.code }

func badRequest(format string, args ...any) error {
	return &rpcError{code: CodeBadRequest, err: fmt.Errorf(format, args...)}
}

// errorCode maps a handler error to its RPC code.
func errorCode(ctx context.Context, err error) string {
	var re *rpcError
	switch {
	case errors.As(err, &re):
		return re.code
	case errors.Is(err, ErrNotFound):
		return CodeNotFound
	case errors.Is(err, ErrExists):
		return CodeExists
	case errors.Is(err, ErrUnavailable):
		return CodeUnavailable
	case errors.Is(err, store.ErrHailClosed):
		return CodeHailClosed
	case errors.Is(err, store.ErrCountermandExpired):
		return CodeCountermandExpired
	case ctx.Err() != nil:
		return CodeShutdown
	}
	return CodeInternal
}

// codedError returns err as an *rpcError carrying errorCode(ctx, err),
// where ctx is the daemon's context. A shutdown's message says so.
func codedError(ctx context.Context, err error) *rpcError {
	code := errorCode(ctx, err)
	if code == CodeShutdown && ctx.Err() != nil {
		err = fmt.Errorf("bridge is shutting down: %w", err)
	}
	return &rpcError{code: code, err: err}
}

type rpcHandler func(ctx context.Context, d *Daemon, args json.RawMessage) (any, error)

var rpcHandlers = map[string]rpcHandler{
	"crew.add":          rpcCrewAdd,
	"crew.list":         rpcCrewList,
	"crew.dismiss":      rpcCrewDismiss,
	"crew.reenergize":   rpcCrewReenergize,
	"order.create":      rpcOrderCreate,
	"order.list":        rpcOrderList,
	"order.belay":       rpcOrderBelay,
	"order.recall":      rpcOrderRecall,
	"order.withdraw":    rpcOrderWithdraw,
	"order.route":       rpcOrderRoute,
	"order.push":        rpcOrderPush,
	"hails.list":        rpcHailsList,
	"hails.answer":      rpcHailsAnswer,
	"hails.countermand": rpcHailsCountermand,
	"hails.metrics":     rpcHailsMetrics,
	"log.tail":          rpcLogTail,
	"bridge.status":     rpcBridgeStatus,

	"standing_orders.get": rpcStandingOrdersGet,
	"standing_orders.set": rpcStandingOrdersSet,

	"command.guidance.get": rpcCommandGuidanceGet,
	"command.guidance.set": rpcCommandGuidanceSet,

	"bridge.jettison": rpcBridgeJettison,
}

// rpcHandlerTimeouts are the handlers that may take longer than
// rpcHandlerTimeout.
var rpcHandlerTimeouts = map[string]time.Duration{
	"order.push":      gitPushTimeout,
	"bridge.jettison": jettisonTimeout,
}

// handlerTimeout bounds command cmd's handler.
func handlerTimeout(cmd string) time.Duration {
	if t, ok := rpcHandlerTimeouts[cmd]; ok {
		return t
	}
	return rpcHandlerTimeout
}

// decodeArgs strictly decodes args (absent means {}) into v.
func decodeArgs(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("bad args: %v", err)
	}
	if dec.More() {
		return badRequest("bad args: trailing data")
	}
	return nil
}

func rpcCrewAdd(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a CrewAddArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	v, err := d.addCrew(ctx, a)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// addCrew adds the crew member a describes and returns its view: crew.add
// and the console's POST /api/crew.
func (d *Daemon) addCrew(ctx context.Context, a CrewAddArgs) (CrewView, error) {
	if a.ID == "" || a.Name == "" {
		return CrewView{}, badRequest("crew id and name are required")
	}
	if a.Dossier != "" && !filepath.IsAbs(a.Dossier) {
		return CrewView{}, badRequest("dossier must be an absolute path, got %q", a.Dossier)
	}
	c := store.Crew{ID: a.ID, Name: a.Name, Rank: a.Rank, Division: a.Division, Spec: a.Spec, DossierPath: a.Dossier,
		Harness: a.Harness, Model: a.Model}
	if err := d.AddCrew(ctx, c); err != nil {
		return CrewView{}, err
	}
	c, err := d.store.GetCrew(ctx, a.ID)
	if err != nil {
		return CrewView{}, err
	}
	return d.crewView(c), nil
}

func rpcCrewList(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	crew, err := d.store.ListCrew(ctx)
	if err != nil {
		return nil, err
	}
	res := CrewListResult{Crew: []CrewView{}}
	for _, c := range crew {
		res.Crew = append(res.Crew, d.crewView(c))
	}
	return res, nil
}

func rpcCrewDismiss(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	return rpcCrewAction(ctx, d, raw, d.DismissCrew)
}

func rpcCrewReenergize(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	return rpcCrewAction(ctx, d, raw, d.ReenergizeCrew)
}

func rpcCrewAction(ctx context.Context, d *Daemon, raw json.RawMessage, act func(context.Context, string) (string, error)) (any, error) {
	var a CrewIDArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	v, err := d.crewAction(ctx, a, act)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// crewAction runs act on the crew member a names and returns a
// CrewActionResult: crew.dismiss, crew.reenergize and the console's POST
// /api/crew/{id}/reenergize.
func (d *Daemon) crewAction(ctx context.Context, a CrewIDArgs, act func(context.Context, string) (string, error)) (CrewActionResult, error) {
	if a.ID == "" {
		return CrewActionResult{}, badRequest("crew id is required")
	}
	outcome, err := act(ctx, a.ID)
	if err != nil {
		return CrewActionResult{}, err
	}
	c, err := d.store.GetCrew(ctx, a.ID)
	if err != nil {
		return CrewActionResult{}, err
	}
	return CrewActionResult{CrewView: d.crewView(c), Outcome: outcome}, nil
}

func rpcOrderCreate(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a OrderCreateArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	id, err := d.createOrder(ctx, a)
	if err != nil {
		return nil, err
	}
	return OrderCreateResult{ID: id}, nil
}

// createOrder creates the captain's order a describes and returns its id:
// order.create and the console's POST /api/orders.
func (d *Daemon) createOrder(ctx context.Context, a OrderCreateArgs) (string, error) {
	if _, ok := tracker.SourceRef(a.Source); !ok && strings.TrimSpace(a.Title) == "" {
		return "", badRequest("order title is required")
	}
	if a.BudgetTokens < 0 {
		return "", badRequest("budget_tokens must not be negative")
	}
	repos := make([]store.OrderRepo, len(a.Repos))
	for i, r := range a.Repos {
		repos[i] = store.OrderRepo{Name: r.Name, BaseRef: r.BaseRef}
	}
	return d.CreateOrder(ctx, store.Order{Title: a.Title, Body: a.Body, Division: a.Division,
		Assignee: a.Assignee, Priority: a.Priority, Repos: repos, BudgetTokens: a.BudgetTokens, CreatedBy: "captain", Model: a.Model,
		Source: a.Source})
}

func rpcOrderList(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	orders, err := d.store.ListOrders(ctx)
	if err != nil {
		return nil, err
	}
	res := OrderListResult{Orders: []OrderView{}}
	for _, o := range orders {
		res.Orders = append(res.Orders, orderView(o))
	}
	return res, nil
}

func rpcOrderBelay(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a OrderActionArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	v, err := d.belayAction(ctx, a)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// belayAction is orderAction for a belay: order.belay and the console's
// POST /api/orders/{id}/belay. It adds the open child orders the belay
// reaches (#146).
func (d *Daemon) belayAction(ctx context.Context, a OrderActionArgs) (OrderBelayResult, error) {
	var (
		n       int
		belayed bool
	)
	v, err := d.orderAction(ctx, a, func(ctx context.Context, id, reason string) (err error) {
		n, belayed, err = d.belayOrder(ctx, id, reason)
		return err
	})
	if err != nil {
		return OrderBelayResult{}, err
	}
	return OrderBelayResult{OrderView: v, Children: n, Belayed: belayed}, nil
}

func rpcOrderRecall(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	return rpcOrderAction(ctx, d, raw, d.RecallOrder)
}

func rpcOrderWithdraw(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	return rpcOrderAction(ctx, d, raw, d.WithdrawOrder)
}

func rpcOrderAction(ctx context.Context, d *Daemon, raw json.RawMessage, act func(ctx context.Context, id, reason string) error) (any, error) {
	var a OrderActionArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	v, err := d.orderAction(ctx, a, act)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// orderAction runs act on the order a names, with its reason, and returns
// the order's OrderView: order.belay, order.recall, order.withdraw and the
// console's POST /api/orders/{id}/belay, /recall and /withdraw.
func (d *Daemon) orderAction(ctx context.Context, a OrderActionArgs, act func(ctx context.Context, id, reason string) error) (OrderView, error) {
	if a.ID == "" {
		return OrderView{}, badRequest("order id is required")
	}
	if err := act(ctx, a.ID, a.Reason); err != nil {
		return OrderView{}, err
	}
	o, err := d.store.GetOrder(ctx, a.ID)
	if err != nil {
		return OrderView{}, err
	}
	return orderView(o), nil
}

func rpcOrderRoute(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a OrderRouteArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	v, err := d.routeOrder(ctx, a)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// routeOrder routes the order a names and returns its OrderView:
// order.route and the console's POST /api/orders/{id}/route.
func (d *Daemon) routeOrder(ctx context.Context, a OrderRouteArgs) (OrderView, error) {
	if a.ID == "" {
		return OrderView{}, badRequest("order id is required")
	}
	if err := d.RouteOrder(ctx, a.ID, a.Assignee, a.Division); err != nil {
		return OrderView{}, err
	}
	o, err := d.store.GetOrder(ctx, a.ID)
	if err != nil {
		return OrderView{}, err
	}
	return orderView(o), nil
}

func rpcOrderPush(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a OrderPushArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, badRequest("order id is required")
	}
	pushed, err := d.PushOrder(ctx, a.ID, a.TargetBranch, a.Force)
	if err != nil {
		return nil, err
	}
	target := a.TargetBranch
	if target == "" {
		target = orderBranch(a.ID)
	}
	return OrderPushResult{ID: a.ID, TargetBranch: target, Repos: pushed}, nil
}

func rpcHailsList(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a HailsListArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	switch a.Status {
	case "", store.HailOpen, store.HailDecided, store.HailTimedOut:
	default:
		return nil, badRequest("unknown hail status %q (want open, decided or timed_out)", a.Status)
	}
	hails, err := d.store.ListHails(ctx, a.Status)
	if err != nil {
		return nil, err
	}
	res := HailsListResult{Hails: []HailView{}}
	for _, h := range hails {
		res.Hails = append(res.Hails, hailView(h))
	}
	return res, nil
}

func rpcHailsAnswer(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a HailsAnswerArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	v, err := d.answerHail(ctx, a)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// answerHail answers the hail a names and returns its decided view:
// hails.answer and the console's POST /api/hails/{id}/answer.
func (d *Daemon) answerHail(ctx context.Context, a HailsAnswerArgs) (HailView, error) {
	if a.HailID == "" {
		return HailView{}, badRequest("hail_id is required")
	}
	if a.Option == nil && a.Decision == "" {
		return HailView{}, badRequest("an option or a decision is required")
	}
	if err := d.AnswerHail(ctx, a.HailID, a.Decision, a.Option); err != nil {
		return HailView{}, err
	}
	h, err := d.store.GetHail(ctx, a.HailID)
	if err != nil {
		return HailView{}, err
	}
	return hailView(h), nil
}

func rpcHailsCountermand(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a HailsCountermandArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	v, err := d.countermandHail(ctx, a)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// DefaultMetricsPeriod is how far back hails.metrics counts when no since
// is given: the exit criterion's week of use.
const DefaultMetricsPeriod = 7 * 24 * time.Hour

// HailMetrics are the hail rates of spec/phase-3-plan-2026-10-29.md
// section 9.
type HailMetrics = store.HailMetrics

// HailMetrics computes the hail rates over the hails created at or after
// since (store.HailMetrics).
func (d *Daemon) HailMetrics(ctx context.Context, since time.Time) (HailMetrics, error) {
	return d.store.HailMetrics(ctx, since)
}

func rpcHailsMetrics(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a HailsMetricsArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	since := time.Now().Add(-DefaultMetricsPeriod)
	if a.Since != nil {
		since = *a.Since
	}
	m, err := d.HailMetrics(ctx, since)
	if err != nil {
		return nil, err
	}
	return hailsMetricsResult(since, m), nil
}

// hailsMetricsResult is m, over the hails created at or after since, as
// hails.metrics's result.
func hailsMetricsResult(since time.Time, m HailMetrics) HailsMetricsResult {
	res := HailsMetricsResult{Since: since.UTC(), Total: m.Total, Decided: m.Decided, Countermanded: m.Countermanded,
		ByOriginalDecider: m.ByOriginalDecider}
	if share, ok := m.AutoResolvedShare(); ok {
		res.AutoResolvedShare = &share
	}
	if rate, ok := m.CaptainOverrides(); ok {
		res.CaptainOverrides = &rate
	}
	if m.Decided > 0 {
		median, p90 := m.MedianTimeToAnswer.Milliseconds(), m.P90TimeToAnswer.Milliseconds()
		res.MedianTimeToAnswerMS, res.P90TimeToAnswerMS = &median, &p90
	}
	return res
}

// countermandHail countermands the hail a names and returns its view:
// hails.countermand and the console's POST /api/hails/{id}/countermand.
// Note is set when the hail's order has no live run: it isn't away or
// hailing, its mission has ended, or it is parked, when the correction
// waits for the resume (#167). One parked on another crew member than the
// hailer, or recalled (unclaimed, parked), is told only if the hailer
// resumes it.
func (d *Daemon) countermandHail(ctx context.Context, a HailsCountermandArgs) (HailsCountermandResult, error) {
	if a.HailID == "" {
		return HailsCountermandResult{}, badRequest("hail_id is required")
	}
	if a.Option == nil && a.Decision == "" {
		return HailsCountermandResult{}, badRequest("an option or a decision is required")
	}
	ended, err := d.CountermandHail(ctx, a.HailID, a.Decision, a.Option, a.Reason)
	if err != nil {
		return HailsCountermandResult{}, err
	}
	h, err := d.store.GetHail(ctx, a.HailID)
	if err != nil {
		return HailsCountermandResult{}, err
	}
	res := HailsCountermandResult{HailView: hailView(h)}
	o, err := d.store.GetOrder(ctx, h.OrderID)
	if err != nil {
		return HailsCountermandResult{}, err
	}
	active := o.State == store.OrderAway || o.State == store.OrderHailing
	recalled := o.ParkedAt != nil && (o.State == store.OrderPool || o.State == store.OrderDirected)
	switch {
	case !active && !recalled:
		res.Note = fmt.Sprintf("order %s is %s: no live run, the agent won't be told", o.ID, o.State)
	case ended:
		// An order its mission left away, e.g. one that ended in error.
		res.Note = fmt.Sprintf("order %s's mission has ended: no live run, the agent won't be told", o.ID)
	case recalled:
		res.Note = fmt.Sprintf("order %s is recalled: the agent is told only if %s resumes it", o.ID, h.CrewID)
	case o.ParkedAt != nil && o.Assignee == h.CrewID:
		res.Note = fmt.Sprintf("order %s is parked: the agent is told on resume", o.ID)
	case o.ParkedAt != nil:
		// Parked on another crew member: it reaches the hailer only
		// after a recall that the hailer claims.
		res.Note = fmt.Sprintf("order %s is parked on %s: the agent is told only if %s resumes it", o.ID, o.Assignee, h.CrewID)
	}
	return res, nil
}

func rpcLogTail(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a LogTailArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.SinceID < 0 || a.Limit < 0 {
		return nil, badRequest("since_id and limit must not be negative")
	}
	if a.Limit == 0 {
		a.Limit = defaultLogTailLimit
	}
	a.Limit = min(a.Limit, maxLogTailLimit)
	entries, err := d.store.ListLogLimit(ctx, a.SinceID, a.Limit)
	if err != nil {
		return nil, err
	}
	res := LogTailResult{Entries: []LogEntryView{}, LastID: a.SinceID}
	for _, e := range entries {
		res.Entries = append(res.Entries, logEntryView(e))
		res.LastID = e.ID
	}
	return res, nil
}

func rpcBridgeStatus(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	return d.Status(ctx)
}

func rpcStandingOrdersGet(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	on, err := d.StandingOrders(ctx)
	if err != nil {
		return nil, err
	}
	return StandingOrdersResult{On: on}, nil
}

func rpcStandingOrdersSet(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a StandingOrdersSetArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	v, err := d.switchStandingOrders(ctx, a)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// switchStandingOrders turns standing orders on or off as a says:
// standing_orders.set and the console's PUT /api/standing-orders.
func (d *Daemon) switchStandingOrders(ctx context.Context, a StandingOrdersSetArgs) (StandingOrdersResult, error) {
	if a.On == nil {
		return StandingOrdersResult{}, badRequest("on is required")
	}
	if err := d.SetStandingOrders(ctx, *a.On); err != nil {
		return StandingOrdersResult{}, err
	}
	return StandingOrdersResult{On: *a.On}, nil
}

func rpcCommandGuidanceGet(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	g, err := d.CommandGuidance(ctx)
	if err != nil {
		return nil, err
	}
	return CommandGuidanceResult{Guidance: g}, nil
}

func rpcCommandGuidanceSet(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a CommandGuidanceSetArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Guidance == nil {
		return nil, badRequest("guidance is required")
	}
	g, err := d.SetCommandGuidance(ctx, *a.Guidance)
	if err != nil {
		return nil, err
	}
	return CommandGuidanceResult{Guidance: g}, nil
}

// rpcBridgeJettison is bridge.jettison (#108): JettisonArgs, a
// JettisonResult.
func rpcBridgeJettison(ctx context.Context, d *Daemon, raw json.RawMessage) (any, error) {
	var a JettisonArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.OlderThan == "" {
		a.OlderThan = DefaultJettisonAge
	}
	age, err := ParseAge(a.OlderThan)
	if err != nil {
		return nil, badRequest("older_than: %v", err)
	}
	res, err := d.Jettison(ctx, age, a.Yes)
	if err != nil {
		return nil, err
	}
	res.OlderThan = a.OlderThan
	return res, nil
}

// rpcServer serves the RPC socket.
type rpcServer struct {
	d      *Daemon
	ctx    context.Context
	ln     net.Listener
	path   string
	logger *slog.Logger
	sem    chan struct{} // one slot per connection being served

	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

// CheckSocketPath fails if path is too long for a Unix socket.
func CheckSocketPath(path string) error {
	if len(path) >= maxSocketPath {
		return fmt.Errorf("socket path %s is %d bytes, too long for a Unix socket (limit %d): use a shorter --home",
			path, len(path), maxSocketPath-1)
	}
	return nil
}

// startRPC listens on path and serves requests until Close. Handlers run on
// ctx, the daemon's context. A stale socket at path is removed first (the
// bridge lock, already held, guarantees no other daemon serves it), but
// anything at path that isn't a socket is refused.
func (d *Daemon) startRPC(ctx context.Context, path string) (*rpcServer, error) {
	if err := CheckSocketPath(path); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != os.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket (%s); move it away", path, fi.Mode().Type())
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale RPC socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on RPC socket: %w", err)
	}
	// The socket is created with the umask's mode. Run has checked the home
	// is ours and private, so nobody else can reach it before this chmod;
	// serveConn also checks every peer's uid (peercred_linux.go).
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod RPC socket: %w", err)
	}
	s := &rpcServer{d: d, ctx: ctx, ln: ln, path: path, logger: d.logger,
		sem: make(chan struct{}, maxRPCConns), conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

func (s *rpcServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.logger.Error("RPC accept", "err", err)
			}
			return
		}
		full := false
		select {
		case s.sem <- struct{}{}:
		default:
			full = true
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		// Set here, under mu and before the closed check, so it can't
		// overwrite the deadline Close sets to cut an idle read short.
		conn.SetDeadline(time.Now().Add(rpcConnTimeout))
		if s.closed {
			// Close already unblocked the connections it knew about.
			conn.SetReadDeadline(time.Now())
		}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
				conn.Close()
				if !full {
					<-s.sem
				}
			}()
			if full {
				s.reply(conn, errResponse(s.ctx, &rpcError{code: CodeUnavailable,
					err: fmt.Errorf("bridge busy: %d connections in flight", maxRPCConns)}))
				return
			}
			s.serveConn(conn)
		}()
	}
}

func (s *rpcServer) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close stops accepting, waits for in-flight requests (their handlers see
// the daemon's context cancelled) and removes the socket.
func (s *rpcServer) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	err := s.ln.Close() // a Unix listener unlinks its socket on Close
	// Unblock connections still waiting for a request line; they are told
	// the bridge is shutting down.
	s.mu.Lock()
	for c := range s.conns {
		c.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()
	s.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	return err
}

func (s *rpcServer) serveConn(conn net.Conn) {
	if err := checkPeer(conn); err != nil {
		s.logger.Warn("RPC connection refused", "err", err)
		s.reply(conn, Response{Error: err.Error(), Code: CodeForbidden})
		return
	}
	resp, unread := s.handle(conn)
	s.reply(conn, resp)
	if unread {
		// Closing with unread input resets the connection, which can
		// discard the reply before the client reads it. Drain a while.
		if uc, ok := conn.(*net.UnixConn); ok {
			uc.CloseWrite()
		}
		// This can overwrite Close's deadline, holding shutdown for at
		// most this second.
		conn.SetReadDeadline(time.Now().Add(time.Second))
		io.Copy(io.Discard, io.LimitReader(conn, 16*MaxRPCRequest))
	}
}

func (s *rpcServer) reply(conn net.Conn, resp Response) {
	b, err := json.Marshal(resp)
	if err != nil {
		b, _ = json.Marshal(Response{Error: "encode reply: " + err.Error(), Code: CodeInternal})
	}
	// Refresh the write deadline: a slow handler may have used up the first.
	conn.SetWriteDeadline(time.Now().Add(rpcConnTimeout))
	if _, err := conn.Write(append(b, '\n')); err != nil {
		s.logger.Debug("RPC write reply", "err", err)
	}
}

// readRequest reads one request line of at most MaxRPCRequest bytes. A line
// ended by EOF instead of a newline is accepted. A read error is returned
// as is; the other failures are bad requests.
func readRequest(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(io.LimitReader(r, MaxRPCRequest+1))
	line, err := br.ReadBytes('\n')
	if len(line) > MaxRPCRequest {
		return nil, badRequest("request larger than %d bytes", MaxRPCRequest)
	}
	if err == io.EOF {
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, badRequest("empty request")
		}
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return line, nil
}

// handle reads and runs one request. unread reports that the request was
// cut short and input may be left unread.
func (s *rpcServer) handle(conn net.Conn) (resp Response, unread bool) {
	line, err := readRequest(conn)
	var re *rpcError
	switch {
	case err == nil:
		return s.run(line), false
	case errors.As(err, &re):
		return errResponse(s.ctx, err), true
	case s.isClosed() || s.ctx.Err() != nil:
		// Close cut the read short.
		return Response{Error: "bridge is shutting down", Code: CodeShutdown}, false
	default:
		return errResponse(s.ctx, badRequest("read request: %v", err)), true
	}
}

func (s *rpcServer) run(line []byte) Response {
	var req Request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return errResponse(s.ctx, badRequest("malformed request: %v", err))
	}
	if dec.More() {
		return errResponse(s.ctx, badRequest("malformed request: one JSON object per connection"))
	}
	h, ok := rpcHandlers[req.Cmd]
	if !ok {
		return errResponse(s.ctx, &rpcError{code: CodeUnknownCmd, err: fmt.Errorf("unknown command %q", req.Cmd)})
	}
	if s.ctx.Err() != nil {
		return Response{Error: "bridge is shutting down", Code: CodeShutdown}
	}
	ctx, cancel := context.WithTimeout(s.ctx, handlerTimeout(req.Cmd))
	defer cancel()
	start := time.Now()
	res, err := h(ctx, s.d, req.Args)
	if err != nil {
		re := codedError(s.ctx, err)
		s.logger.Info("RPC", "cmd", req.Cmd, "err", re, "code", re.code, "took", time.Since(start))
		return Response{Error: re.Error(), Code: re.code}
	}
	s.logger.Debug("RPC", "cmd", req.Cmd, "took", time.Since(start))
	b, err := json.Marshal(res)
	if err != nil {
		return errResponse(s.ctx, err)
	}
	return Response{OK: true, Result: b}
}

func errResponse(ctx context.Context, err error) Response {
	return Response{Error: err.Error(), Code: errorCode(ctx, err)}
}

// ErrBridgeNotRunning is returned by Call when nothing serves the socket.
var ErrBridgeNotRunning = errors.New("bridge not running (start it with `bmus bridge run`)")

// ErrConnLost wraps Call's failures to send a request or read its reply:
// the bridge went away mid-request, typically because it is stopping.
var ErrConnLost = errors.New("connection to the bridge lost")

// RPCError is a failed reply (ok:false) returned by Call.
type RPCError struct {
	Code    string
	Message string
}

func (e *RPCError) Error() string { return e.Message }

// IsBridgeDown reports whether err from Call means the bridge is not
// running or is shutting down (as opposed to rejecting the request), so a
// poller can wait for it to come back.
func IsBridgeDown(err error) bool {
	var re *RPCError
	return errors.Is(err, ErrBridgeNotRunning) || errors.Is(err, ErrConnLost) ||
		(errors.As(err, &re) && re.Code == CodeShutdown)
}

// Call sends one request to the bridge at socket and decodes the result
// into result (if non-nil). A missing socket, or one nobody listens on (a
// bridge that was killed), returns ErrBridgeNotRunning; a connection lost
// mid-request an error wrapping ErrConnLost; an ok:false reply an *RPCError.
// IsBridgeDown tells the first two, and a shutting_down reply, apart from
// the rest.
func Call(ctx context.Context, socket, cmd string, args, result any) error {
	if err := CheckSocketPath(socket); err != nil {
		return err
	}
	if args == nil {
		args = struct{}{}
	}
	rawArgs, err := json.Marshal(args)
	if err != nil {
		return err
	}
	req, err := json.Marshal(Request{Cmd: cmd, Args: rawArgs})
	if err != nil {
		return err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return ErrBridgeNotRunning
		}
		return fmt.Errorf("connect to bridge: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(handlerTimeout(cmd) + rpcConnTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()
	if _, err := conn.Write(append(req, '\n')); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("send %s: %w: %w", cmd, ErrConnLost, err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == io.EOF && len(line) == 0 {
			return fmt.Errorf("%s: %w: closed without a reply", cmd, ErrConnLost)
		}
		return fmt.Errorf("read %s reply: %w: %w", cmd, ErrConnLost, err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("decode %s reply: %w", cmd, err)
	}
	if !resp.OK {
		return &RPCError{Code: resp.Code, Message: resp.Error}
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, result); err != nil {
		return fmt.Errorf("decode %s result: %w", cmd, err)
	}
	return nil
}
