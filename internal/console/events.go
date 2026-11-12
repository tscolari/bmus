package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/tscolari/bmus/internal/store"
)

// Feed limits (#72).
const (
	// defaultMaxReplay is the widest Last-Event-ID gap /api/events
	// replays; past it the stream starts with a snapshot instead.
	defaultMaxReplay = 10000
	// replayPage is how many entries a replay reads at a time.
	replayPage = 1000
	// SnapshotLog is how many log entries a snapshot carries.
	SnapshotLog = 200
	// defaultLogLimit and maxLogLimit are /api/log's limit default and
	// cap, as log.tail's.
	defaultLogLimit = 500
	maxLogLimit     = 1000
	// defaultHeartbeat is how often an idle stream sends a comment,
	// checks the status and catches up from the log. Each write's
	// deadline is two heartbeats away.
	defaultHeartbeat = 15 * time.Second
	// retryMillis is the reconnect delay a stream asks the browser for.
	retryMillis = 1000
)

// Source is the bridge as the console reads it, and its Actions: the
// bridge's daemon implements it. The console never reads the store
// itself; it uses only the store's LogEntry and Feed types. View values
// are the bridge's JSON shapes, shared with its RPC results.
type Source interface {
	Actions
	// Subscribe subscribes to the captain's log entries committed from
	// now on.
	Subscribe() *store.Subscription
	// Snapshot reads the console's whole state.
	Snapshot(ctx context.Context) (Snapshot, error)
	// Status reads the bridge's status.
	Status(ctx context.Context) (Status, error)
	// LastLogID returns the captain's log's last id, 0 when it is empty.
	LastLogID(ctx context.Context) (int64, error)
	// Log returns at most limit entries above sinceID, oldest first.
	Log(ctx context.Context, sinceID int64, limit int) ([]store.LogEntry, error)
	// LogBefore returns the newest limit entries below beforeID, oldest
	// first.
	LogBefore(ctx context.Context, beforeID int64, limit int) ([]store.LogEntry, error)
	// LogView returns an entry's view. It must marshal for any entry,
	// one with a payload that isn't JSON included.
	LogView(e store.LogEntry) any
	// Views reads the current views of the entities e names (crew
	// member, order, hail, in that order), as events. An entity that
	// doesn't exist is left out.
	Views(ctx context.Context, e store.LogEntry) ([]Event, error)
	// OrderDiff reads done order id's work as unified diffs (#104):
	// {id, title, branch, repos: [{name, base, diff, truncated}]}. Its
	// errors carry their RPC codes as Actions errors do.
	OrderDiff(ctx context.Context, id string) (any, error)
}

// Snapshot is the snapshot event's body, and /api/state's. LastLogID is
// the log id it reflects: Log holds the entries up to it, and the other
// fields were read after it, so they may already reflect later entries.
// Those entries' events follow on the stream; views are whole, so
// applying one twice is harmless.
type Snapshot struct {
	LastLogID int64  `json:"last_log_id"`
	Crew      any    `json:"crew"`
	Orders    any    `json:"orders"` // open and away, and finished ones Log names
	Hails     any    `json:"hails"`  // open, and decided ones still in their countermand window
	Status    Status `json:"status"`
	Log       any    `json:"log"` // the last SnapshotLog entries, oldest first
	// Repos are the names registered with bridge run --repo, sorted: the
	// repos an order may name (#211). The registry is fixed for the
	// bridge's lifetime, so no event updates it.
	Repos []string `json:"repos"`
	// RepoURLs are the registered repos' URLs without credentials, by
	// name: where a push sends an order's work (#4). Fixed, as Repos.
	RepoURLs map[string]string `json:"repo_urls"`
}

// Status is the bridge's status. View is what the status event carries;
// Key changes exactly when a status event is worth sending (condition,
// reasons, counters; not the log id).
type Status struct {
	View any
	Key  string
}

func (s Status) MarshalJSON() ([]byte, error) { return json.Marshal(s.View) }

// Event is one server-sent event: crew, order or hail, from Views.
type Event struct {
	Type string
	Data any
}

// logPage is /api/log's body: LastID is the last entry's id, or since
// when there are none, as log.tail's. With before it is the newest
// entry's id, or 0 when there are none.
type logPage struct {
	Entries []any `json:"entries"`
	LastID  int64 `json:"last_id"`
}

// apiRoutes are the /api/ routes behind the session checks: the reads
// (#72, the diff #104), GET /api/session and the actions (#73, #78,
// actions.go).
func (s *Server) apiRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/log", s.handleLog)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/session", s.handleGetSession)
	mux.HandleFunc("POST /api/orders", s.handleCreateOrder)
	mux.HandleFunc("POST /api/hails/{id}/answer", s.handleAnswerHail)
	mux.HandleFunc("POST /api/hails/{id}/countermand", s.handleCountermandHail)
	mux.HandleFunc("POST /api/crew", s.handleAddCrew)
	mux.HandleFunc("POST /api/orders/{id}/belay", s.handleOrderAction("belay order", Source.BelayOrder))
	mux.HandleFunc("POST /api/orders/{id}/recall", s.handleOrderAction("recall order", Source.RecallOrder))
	mux.HandleFunc("POST /api/orders/{id}/withdraw", s.handleOrderAction("withdraw order", Source.WithdrawOrder))
	mux.HandleFunc("POST /api/orders/{id}/route", s.handleOrderAction("route order", Source.RouteOrder))
	mux.HandleFunc("POST /api/orders/{id}/push", s.handlePushOrder)
	mux.HandleFunc("GET /api/orders/{id}/diff", s.handleOrderDiff)
	mux.HandleFunc("POST /api/crew/{id}/reenergize", s.handleReenergize)
	mux.HandleFunc("PUT /api/standing-orders", s.handleStandingOrders)
	mux.Handle("/api/", http.NotFoundHandler())
	return mux
}

// source returns the Source Serve was given, or answers 503 without one.
func (s *Server) source(w http.ResponseWriter) (Source, bool) {
	if s.src == nil {
		writeAPIError(w, http.StatusServiceUnavailable, CodeUnavailable, "no data source")
		return nil, false
	}
	return s.src, true
}

// handleState serves GET /api/state: the snapshot event's body.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	src, ok := s.source(w)
	if !ok {
		return
	}
	snap, err := src.Snapshot(r.Context())
	if err != nil {
		s.internalError(w, "read state", err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleLog serves GET /api/log: with since=<id>, the entries above it
// (log.tail's paging); with before=<id>, the newest entries below it, for
// scrolling back; with neither, the entries from the start. Either way at
// most limit (default 500, capped at 1000), oldest first.
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	src, ok := s.source(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	if q.Has("since") && q.Has("before") {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "since and before are exclusive")
		return
	}
	since, err := queryInt(q.Get("since"), 0)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "since: "+err.Error())
		return
	}
	before, err := queryInt(q.Get("before"), 0)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "before: "+err.Error())
		return
	}
	limit, err := queryInt(q.Get("limit"), 0)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "limit: "+err.Error())
		return
	}
	if limit == 0 {
		limit = defaultLogLimit
	}
	limit = min(limit, maxLogLimit)
	var entries []store.LogEntry
	page := logPage{Entries: []any{}, LastID: since}
	switch {
	case !q.Has("before"):
		entries, err = src.Log(r.Context(), since, int(limit))
	case before > 1:
		page.LastID = 0
		entries, err = src.LogBefore(r.Context(), before, int(limit))
	default:
		page.LastID = 0 // nothing below id 1
	}
	if err != nil {
		s.internalError(w, "read log", err)
		return
	}
	for _, e := range entries {
		page.Entries = append(page.Entries, src.LogView(e))
		page.LastID = e.ID
	}
	writeJSON(w, http.StatusOK, page)
}

// handleOrderDiff serves GET /api/orders/{id}/diff (#104): 200 and the
// done order's diff. Errors map as an action's do: an order that isn't
// done is a 400, an unknown one a 404, a diff that ran past its time a
// 504 (timeout).
func (s *Server) handleOrderDiff(w http.ResponseWriter, r *http.Request) {
	src, ok := s.source(w)
	if !ok {
		return
	}
	v, err := src.OrderDiff(r.Context(), r.PathValue("id"))
	if err != nil {
		s.actionError(w, "diff order", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// queryInt parses v as a non-negative integer; empty means def.
func queryInt(v string, def int64) (int64, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a non-negative integer", v)
	}
	return n, nil
}

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.logger.Error("console: "+what, "err", err)
	writeAPIError(w, http.StatusInternalServerError, CodeInternal, what+" failed")
}

// handleEvents serves GET /api/events, the server-sent event stream.
//
// It subscribes to the feed before it reads anything. It then sends a
// snapshot or, with Last-Event-ID, the log entries above that id. A gap
// wider than 10,000 entries, or an id past the log's end, gets a
// snapshot instead. The first read happens before the response starts,
// so its failure is a 500. Then the stream goes live. A live entry at or
// below the last id sent was already sent, and is skipped. Each entry
// goes out as a log event with its id as the event id, followed by the
// views it names. A status event follows when the status's Key changed.
// Each heartbeat also catches up from the log, in case the feed missed
// an entry (a failed hook read). The stream ends when the subscriber is
// dropped (the client reconnects with Last-Event-ID and catches up),
// when the client goes or stops reading, and when the server closes.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	src, ok := s.source(w)
	if !ok {
		return
	}
	resume, resuming := int64(0), false
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		n, err := queryInt(v, 0)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "Last-Event-ID: "+err.Error())
			return
		}
		resume, resuming = n, true
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.streams, cancel)
	defer stop()

	sub := src.Subscribe()
	defer sub.Close()

	if resuming {
		head, err := src.LastLogID(ctx)
		if err != nil {
			s.internalError(w, "read log", err)
			return
		}
		resuming = resume <= head && head-resume <= s.maxReplay
	}
	var (
		snap  Snapshot
		first []store.LogEntry
		err   error
	)
	if resuming {
		first, err = src.Log(ctx, resume, replayPage)
		if first == nil {
			first = []store.LogEntry{} // read, and empty: catchUp mustn't read again
		}
	} else {
		snap, err = src.Snapshot(ctx)
	}
	if err != nil {
		s.internalError(w, "read state", err)
		return
	}

	st := &stream{w: w, rc: http.NewResponseController(w), src: src, timeout: 2 * s.heartbeat}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	err = st.write(fmt.Sprintf("retry: %d\n\n", retryMillis))
	if err == nil {
		if resuming {
			st.sent = resume
			err = st.catchUp(ctx, first)
		} else {
			err = st.snapshot(snap)
		}
	}
	if err == nil {
		err = st.checkStatus(ctx)
	}
	if err == nil {
		err = st.flush()
	}
	tick := time.NewTicker(s.heartbeat)
	defer tick.Stop()
	for err == nil {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-sub.C():
			if !ok {
				return // dropped: the client catches up on reconnect
			}
			if e.ID <= st.sent {
				continue
			}
			err = st.entry(ctx, e)
			if err == nil && len(sub.C()) == 0 {
				if err = st.checkStatus(ctx); err == nil {
					err = st.flush()
				}
			}
		case <-tick.C:
			err = st.heartbeat(ctx)
		}
	}
	s.logStreamEnd(r, err)
}

// logStreamEnd logs the error that ended a stream: at Warn, unless the
// client went away or the server is closing. A failed write cancels the
// request's context too, so a write deadline is checked first.
func (s *Server) logStreamEnd(r *http.Request, err error) {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		s.logger.Warn("console: event stream ended: client stopped reading", "err", err)
	case s.streams.Err() != nil, r.Context().Err() != nil, errors.Is(err, context.Canceled),
		errors.Is(err, syscall.EPIPE), errors.Is(err, syscall.ECONNRESET):
		s.logger.Debug("console: event stream ended", "err", err)
	default:
		s.logger.Warn("console: event stream ended", "err", err)
	}
}

// stream is one /api/events response.
type stream struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	src Source
	// timeout is each write's deadline, from just before it.
	timeout time.Duration
	// sent is the last log id sent; status is the Key of the last status
	// sent.
	sent   int64
	status string
}

// write writes text under a fresh write deadline: a client that stops
// reading ends the stream at the deadline. Heartbeats write often enough
// to keep an idle stream's deadline ahead.
func (st *stream) write(text string) error {
	if err := st.rc.SetWriteDeadline(time.Now().Add(st.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	_, err := io.WriteString(st.w, text)
	return err
}

// send writes one event; id 0 means none.
func (st *stream) send(event string, id int64, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("%s event: %w", event, err)
	}
	if id > 0 {
		return st.write(fmt.Sprintf("event: %s\nid: %d\ndata: %s\n\n", event, id, b))
	}
	return st.write(fmt.Sprintf("event: %s\ndata: %s\n\n", event, b))
}

func (st *stream) flush() error {
	if err := st.rc.SetWriteDeadline(time.Now().Add(st.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return st.rc.Flush()
}

// snapshot sends snap as a snapshot event, with its log id as the event
// id.
func (st *stream) snapshot(snap Snapshot) error {
	// "id: 0" would be a real id; an empty one resets the client's.
	if snap.LastLogID == 0 {
		if err := st.write("id\n"); err != nil {
			return err
		}
	}
	st.sent, st.status = snap.LastLogID, snap.Status.Key
	return st.send("snapshot", snap.LastLogID, snap)
}

// catchUp sends page, the entries above st.sent already read (nil to
// read them), and the entries after it, a page at a time and flushing
// after each, until a page comes back short.
func (st *stream) catchUp(ctx context.Context, page []store.LogEntry) error {
	for {
		if page == nil {
			var err error
			if page, err = st.src.Log(ctx, st.sent, replayPage); err != nil {
				return err
			}
		}
		for _, e := range page {
			if e.ID <= st.sent {
				continue
			}
			if err := st.entry(ctx, e); err != nil {
				return err
			}
		}
		if err := st.flush(); err != nil {
			return err
		}
		if len(page) < replayPage {
			return nil
		}
		page = nil
	}
}

// heartbeat catches up from the log if it is ahead of what was sent,
// checks the status and sends a comment.
func (st *stream) heartbeat(ctx context.Context) error {
	head, err := st.src.LastLogID(ctx)
	if err != nil {
		return err
	}
	if head > st.sent {
		if err := st.catchUp(ctx, nil); err != nil {
			return err
		}
	}
	if err := st.checkStatus(ctx); err != nil {
		return err
	}
	if err := st.write(": heartbeat\n\n"); err != nil {
		return err
	}
	return st.flush()
}

// entry sends a log event for e and the views it names.
func (st *stream) entry(ctx context.Context, e store.LogEntry) error {
	if err := st.send("log", e.ID, st.src.LogView(e)); err != nil {
		return err
	}
	st.sent = e.ID
	views, err := st.src.Views(ctx, e)
	if err != nil {
		return err
	}
	for _, v := range views {
		if err := st.send(v.Type, 0, v.Data); err != nil {
			return err
		}
	}
	return nil
}

// checkStatus sends a status event if the status changed since the last
// one sent.
func (st *stream) checkStatus(ctx context.Context) error {
	status, err := st.src.Status(ctx)
	if err != nil || status.Key == st.status {
		return err
	}
	st.status = status.Key
	return st.send("status", 0, status)
}
