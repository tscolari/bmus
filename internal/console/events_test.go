package console

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
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

// testSource is a Source over a real store and feed, with views that
// carry just ids, and a status the test sets.
type testSource struct {
	st   *store.Store
	feed *store.Feed

	mu        sync.Mutex
	condition string
	// beforeRead, if set, runs at the start of the next Snapshot or Log
	// call (then is cleared); viewGate, if set, blocks Views until closed.
	// skipPublish drops the next commit's entries on their way to the
	// feed; failSnapshot fails Snapshot.
	beforeRead   func()
	viewGate     chan struct{}
	lastLimit    int
	skipPublish  bool
	failSnapshot bool
	// actions records the Actions calls; actErr, if set, fails them.
	actions []actionCall
	actErr  error
	// reportErr, if set, fails OrderReport.
	reportErr error
}

// publish is the store's log hook: the feed's Publish, unless
// skipPublish is set.
func (ts *testSource) publish(entries []store.LogEntry) {
	ts.mu.Lock()
	skip := ts.skipPublish
	ts.skipPublish = false
	ts.mu.Unlock()
	if !skip {
		ts.feed.Publish(entries)
	}
}

type idView struct {
	ID string `json:"id"`
}

type logView struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
}

func (ts *testSource) takeBeforeRead() {
	ts.mu.Lock()
	f := ts.beforeRead
	ts.beforeRead = nil
	ts.mu.Unlock()
	if f != nil {
		f()
	}
}

func (ts *testSource) setCondition(c string) {
	ts.mu.Lock()
	ts.condition = c
	ts.mu.Unlock()
}

func (ts *testSource) Subscribe() *store.Subscription { return ts.feed.Subscribe() }

func (ts *testSource) Snapshot(ctx context.Context) (Snapshot, error) {
	ts.takeBeforeRead()
	ts.mu.Lock()
	fail := ts.failSnapshot
	ts.mu.Unlock()
	if fail {
		return Snapshot{}, errors.New("snapshot failed")
	}
	last, err := ts.st.LastLogID(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	crew, err := ts.st.ListCrew(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	entries, err := ts.st.ListLogLast(ctx, last, SnapshotLog)
	if err != nil {
		return Snapshot{}, err
	}
	status, _ := ts.Status(ctx)
	crewViews := []idView{}
	for _, c := range crew {
		crewViews = append(crewViews, idView{c.ID})
	}
	logViews := []any{}
	for _, e := range entries {
		logViews = append(logViews, ts.LogView(e))
	}
	return Snapshot{LastLogID: last, Crew: crewViews, Orders: []idView{}, Hails: []idView{}, Status: status, Log: logViews}, nil
}

func (ts *testSource) Status(context.Context) (Status, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return Status{View: map[string]string{"condition": ts.condition}, Key: ts.condition}, nil
}

func (ts *testSource) LastLogID(ctx context.Context) (int64, error) { return ts.st.LastLogID(ctx) }

func (ts *testSource) Log(ctx context.Context, sinceID int64, limit int) ([]store.LogEntry, error) {
	ts.takeBeforeRead()
	ts.mu.Lock()
	ts.lastLimit = limit
	ts.mu.Unlock()
	return ts.st.ListLogLimit(ctx, sinceID, limit)
}

func (ts *testSource) LogBefore(ctx context.Context, beforeID int64, limit int) ([]store.LogEntry, error) {
	return ts.st.ListLogLast(ctx, beforeID-1, limit)
}

func (ts *testSource) LogView(e store.LogEntry) any { return logView{ID: e.ID, Text: e.Text} }

func (ts *testSource) Views(ctx context.Context, e store.LogEntry) ([]Event, error) {
	ts.mu.Lock()
	gate := ts.viewGate
	ts.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var out []Event
	if e.CrewID != "" {
		out = append(out, Event{Type: "crew", Data: idView{e.CrewID}})
	}
	if e.OrderID != "" {
		out = append(out, Event{Type: "order", Data: idView{e.OrderID}})
	}
	return out, nil
}

// feedServer is a serving console over a testSource, with a session.
type feedServer struct {
	s      *Server
	st     *store.Store
	src    *testSource
	cookie *http.Cookie
	csrf   string
	token  string
	logs   *syncBuffer
}

// newFeedServer starts a console Server over a fresh store whose feed
// buffers feedBuf entries; tune, if set, adjusts the Server before it
// serves.
func newFeedServer(t *testing.T, feedBuf int, tune func(*Server)) *feedServer {
	t.Helper()
	ctx := t.Context()
	home := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(home, store.DBFile))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	src := &testSource{st: st, feed: store.NewFeed(feedBuf), condition: "green"}
	require.NoError(t, st.SetLogHook(ctx, src.publish, nil))

	logs := &syncBuffer{}
	s, err := Listen(Config{Addr: "127.0.0.1:0", Home: home, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	if tune != nil {
		tune(s)
	}
	require.NoError(t, s.Serve(src))
	data, err := os.ReadFile(filepath.Join(home, TokenFile))
	require.NoError(t, err)
	token := strings.TrimSpace(string(data))
	cookie, csrf := newSessionFor(t, s, token)
	return &feedServer{s: s, st: st, src: src, cookie: cookie, csrf: csrf, token: token, logs: logs}
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// appendLog appends a log entry; it may run in a handler's goroutine.
func (fs *feedServer) appendLog(t *testing.T, text string) {
	if _, err := fs.st.AppendLog(context.Background(), store.LogEntry{Kind: "test", Text: text}); err != nil {
		t.Errorf("append log: %v", err)
	}
}

// get issues a GET to path with the session cookie and headers.
func (fs *feedServer) get(t *testing.T, path string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+fs.s.Addr()+path, nil)
	require.NoError(t, err)
	req.AddCookie(fs.cookie)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

type sseEvent struct {
	Event, ID, Data string
	HasID           bool
}

// sseStream reads a /api/events response's events in the background.
type sseStream struct {
	resp   *http.Response
	events chan sseEvent
	retry  chan string // the retry fields
}

// openStream opens /api/events, resuming from lastID unless it is empty.
func (fs *feedServer) openStream(t *testing.T, lastID string) *sseStream {
	t.Helper()
	header := map[string]string{}
	if lastID != "" {
		header["Last-Event-ID"] = lastID
	}
	resp := fs.get(t, "/api/events", header)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	ss := &sseStream{resp: resp, events: make(chan sseEvent, 1024), retry: make(chan string, 16)}
	go func() {
		defer close(ss.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 1<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.Event != "" {
					ss.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, ":"):
			case line == "id":
				ev.ID, ev.HasID = "", true
			case strings.HasPrefix(line, "id: "):
				ev.ID, ev.HasID = strings.TrimPrefix(line, "id: "), true
			case strings.HasPrefix(line, "event: "):
				ev.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, "retry: "):
				ss.retry <- strings.TrimPrefix(line, "retry: ")
			}
		}
	}()
	return ss
}

// next returns the stream's next event, failing after 5s or at its end.
func (ss *sseStream) next(t *testing.T) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-ss.events:
		require.True(t, ok, "stream ended")
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
		return sseEvent{}
	}
}

// none checks no event arrives within d.
func (ss *sseStream) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-ss.events:
		if ok {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-time.After(d):
	}
}

// ended checks the stream ends within 5s, returning the events before
// its end.
func (ss *sseStream) ended(t *testing.T) []sseEvent {
	t.Helper()
	var out []sseEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ss.events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("stream did not end within 5s")
		}
	}
}

// requireLog checks ev is the log event of entry id with text.
func requireLog(t *testing.T, ev sseEvent, id int64, text string) {
	t.Helper()
	require.Equal(t, "log", ev.Event)
	require.Equal(t, strconv.FormatInt(id, 10), ev.ID)
	var v logView
	require.NoError(t, json.Unmarshal([]byte(ev.Data), &v))
	require.Equal(t, logView{ID: id, Text: text}, v)
}

func requireSnapshot(t *testing.T, ev sseEvent, last int64) Snapshot {
	t.Helper()
	require.Equal(t, "snapshot", ev.Event)
	require.True(t, ev.HasID)
	if last > 0 {
		require.Equal(t, strconv.FormatInt(last, 10), ev.ID)
	} else {
		require.Empty(t, ev.ID)
	}
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(ev.Data), &raw))
	var snap Snapshot
	require.NoError(t, json.Unmarshal(raw["last_log_id"], &snap.LastLogID))
	require.Equal(t, last, snap.LastLogID)
	for _, k := range []string{"crew", "orders", "hails", "status", "log"} {
		require.Contains(t, raw, k)
	}
	return snap
}

// A stream starts with a snapshot, then carries one log event per
// committed change, each followed by the views it names, and a status
// event only when the status changed.
func TestEventsSnapshotThenLive(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	ctx := t.Context()
	fs.appendLog(t, "before")
	require.NoError(t, fs.st.InsertCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell"}, store.LogEntry{Kind: "crew", CrewID: "sorell", Text: "added"}))

	ss := fs.openStream(t, "")
	ev := ss.next(t)
	requireSnapshot(t, ev, 2)
	require.Equal(t, "1000", <-ss.retry)
	require.Contains(t, ev.Data, `"crew":[{"id":"sorell"}]`)
	require.Contains(t, ev.Data, `"status":{"condition":"green"}`)
	require.Contains(t, ev.Data, `"log":[{"id":1,"text":"before"},{"id":2,"text":"added"}]`)

	id, err := fs.st.InsertOrder(ctx, store.Order{Title: "t", Assignee: "sorell"}, store.LogEntry{Kind: "order", CrewID: "sorell", Text: "created"})
	require.NoError(t, err)
	requireLog(t, ss.next(t), 3, "created")
	ev = ss.next(t)
	require.Equal(t, sseEvent{Event: "crew", Data: `{"id":"sorell"}`}, ev)
	ev = ss.next(t)
	require.Equal(t, sseEvent{Event: "order", Data: `{"id":"` + id + `"}`}, ev)

	// A no-op commits nothing and sends nothing.
	require.NoError(t, fs.st.SetCrewStatus(ctx, "sorell", store.CrewOnDeck, "", store.LogEntry{Kind: "crew", Text: "noop"}))
	ss.none(t, 100*time.Millisecond)

	fs.src.setCondition("yellow")
	fs.appendLog(t, "hailing")
	requireLog(t, ss.next(t), 4, "hailing")
	require.Equal(t, sseEvent{Event: "status", Data: `{"condition":"yellow"}`}, ss.next(t))

	fs.appendLog(t, "unchanged")
	requireLog(t, ss.next(t), 5, "unchanged")
	ss.none(t, 100*time.Millisecond)
}

// A status change with no log entry (a tsh login's) goes out at the next
// heartbeat.
func TestEventsStatusOnHeartbeat(t *testing.T) {
	fs := newFeedServer(t, 0, func(s *Server) { s.heartbeat = 20 * time.Millisecond })
	ss := fs.openStream(t, "")
	requireSnapshot(t, ss.next(t), 0)
	fs.src.setCondition("yellow")
	require.Equal(t, sseEvent{Event: "status", Data: `{"condition":"yellow"}`}, ss.next(t))
}

// Last-Event-ID replays the entries above it, then goes live.
func TestEventsReplay(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	for i := 1; i <= 5; i++ {
		fs.appendLog(t, "e"+strconv.Itoa(i))
	}
	ss := fs.openStream(t, "2")
	requireLog(t, ss.next(t), 3, "e3")
	requireLog(t, ss.next(t), 4, "e4")
	requireLog(t, ss.next(t), 5, "e5")
	require.Equal(t, "status", ss.next(t).Event, "a resumed stream gets the status once")
	fs.appendLog(t, "e6")
	requireLog(t, ss.next(t), 6, "e6")

	// Caught up: nothing to replay.
	ss = fs.openStream(t, "6")
	require.Equal(t, "status", ss.next(t).Event)
	fs.appendLog(t, "e7")
	requireLog(t, ss.next(t), 7, "e7")
}

// A gap wider than the replay limit, or an id past the log's end, gets a
// snapshot instead.
func TestEventsGapSnapshot(t *testing.T) {
	fs := newFeedServer(t, 0, func(s *Server) { s.maxReplay = 3 })
	for i := 1; i <= 6; i++ {
		fs.appendLog(t, "e"+strconv.Itoa(i))
	}
	ss := fs.openStream(t, "2")
	requireSnapshot(t, ss.next(t), 6)
	fs.appendLog(t, "e7")
	requireLog(t, ss.next(t), 7, "e7")

	ss = fs.openStream(t, "4")
	requireLog(t, ss.next(t), 5, "e5")

	ss = fs.openStream(t, "99")
	requireSnapshot(t, ss.next(t), 7)
}

func TestEventsDefaultMaxReplay(t *testing.T) {
	s, _ := newTestServer(t)
	require.Equal(t, int64(10000), s.maxReplay)
}

// An entry committed while a stream reads its replay or snapshot reaches
// it once: from the read, or from the feed, never both or neither.
func TestEventsReplayLiveRace(t *testing.T) {
	t.Run("replay", func(t *testing.T) {
		fs := newFeedServer(t, 0, nil)
		fs.appendLog(t, "e1")
		fs.appendLog(t, "e2")
		// Committed after the subscription, before the replay's read:
		// both the read and the feed have it.
		fs.src.beforeRead = func() { fs.appendLog(t, "during") }
		ss := fs.openStream(t, "1")
		requireLog(t, ss.next(t), 2, "e2")
		requireLog(t, ss.next(t), 3, "during")
		require.Equal(t, "status", ss.next(t).Event)
		fs.appendLog(t, "after")
		requireLog(t, ss.next(t), 4, "after")
		ss.none(t, 100*time.Millisecond)
	})
	t.Run("snapshot", func(t *testing.T) {
		fs := newFeedServer(t, 0, nil)
		fs.appendLog(t, "e1")
		fs.src.beforeRead = func() { fs.appendLog(t, "during") }
		ss := fs.openStream(t, "")
		ev := ss.next(t)
		requireSnapshot(t, ev, 2)
		require.Contains(t, ev.Data, `{"id":2,"text":"during"}`)
		fs.appendLog(t, "after")
		requireLog(t, ss.next(t), 3, "after")
		ss.none(t, 100*time.Millisecond)
	})
}

// A subscriber that falls behind is dropped: its stream sends what it had
// buffered and ends, and a reconnect with Last-Event-ID catches up, with
// no entry lost or repeated.
func TestEventsSlowSubscriberDropped(t *testing.T) {
	fs := newFeedServer(t, 2, nil)
	ss := fs.openStream(t, "")
	requireSnapshot(t, ss.next(t), 0)

	gate := make(chan struct{})
	fs.src.mu.Lock()
	fs.src.viewGate = gate
	fs.src.mu.Unlock()
	// The stream blocks in Views on the first entry it takes; its buffer
	// of two fills, and a later entry drops it.
	for i := 1; i <= 6; i++ {
		fs.appendLog(t, "e"+strconv.Itoa(i))
	}
	fs.src.mu.Lock()
	fs.src.viewGate = nil
	fs.src.mu.Unlock()
	close(gate)

	// The stream sends the entries it had, each once and in order, and
	// ends; a reconnect from the last one sends the rest.
	var last int64
	for _, ev := range ss.ended(t) {
		requireLog(t, ev, last+1, "e"+strconv.FormatInt(last+1, 10))
		last++
	}
	require.Less(t, last, int64(6), "dropped before e6")

	ss = fs.openStream(t, strconv.FormatInt(last, 10))
	for id := last + 1; id <= 6; id++ {
		requireLog(t, ss.next(t), id, "e"+strconv.FormatInt(id, 10))
	}
	require.Equal(t, "status", ss.next(t).Event)
	ss.none(t, 100*time.Millisecond)
}

// Close ends open streams at once, not at Shutdown's 5s fallback.
func TestEventsEndOnClose(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	ss := fs.openStream(t, "")
	requireSnapshot(t, ss.next(t), 0)
	start := time.Now()
	require.NoError(t, fs.s.Close())
	require.Less(t, time.Since(start), 2*time.Second)
	ss.ended(t)
}

func TestEventsBadLastEventID(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	for _, v := range []string{"x", "-1", "1.5", "99999999999999999999"} {
		resp := fs.get(t, "/api/events", map[string]string{"Last-Event-ID": v})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, v)
		require.Equal(t, CodeBadRequest, decodeAPIError(t, resp).Error.Code)
	}
}

func TestState(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	fs.appendLog(t, "e1")
	resp := fs.get(t, "/api/state", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	requireSnapshot(t, sseEvent{Event: "snapshot", ID: "1", HasID: true, Data: string(body)}, 1)
}

func TestLogPages(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	for i := 1; i <= 5; i++ {
		fs.appendLog(t, "e"+strconv.Itoa(i))
	}
	page := func(query string) (logPage, int) {
		resp := fs.get(t, "/api/log"+query, nil)
		var p struct {
			Entries []logView `json:"entries"`
			LastID  int64     `json:"last_id"`
		}
		if resp.StatusCode != http.StatusOK {
			require.Equal(t, CodeBadRequest, decodeAPIError(t, resp).Error.Code)
			return logPage{}, resp.StatusCode
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
		out := logPage{Entries: []any{}, LastID: p.LastID}
		for _, e := range p.Entries {
			out.Entries = append(out.Entries, e)
		}
		return out, resp.StatusCode
	}

	p, code := page("?since=1&limit=2")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, logPage{Entries: []any{logView{2, "e2"}, logView{3, "e3"}}, LastID: 3}, p)

	p, _ = page("?since=5")
	require.Equal(t, logPage{Entries: []any{}, LastID: 5}, p)

	_, _ = page("")
	require.Equal(t, defaultLogLimit, fs.src.lastLimit)
	_, _ = page("?limit=5000")
	require.Equal(t, maxLogLimit, fs.src.lastLimit)

	// before is exclusive, and pages back.
	p, _ = page("?before=4&limit=2")
	require.Equal(t, logPage{Entries: []any{logView{2, "e2"}, logView{3, "e3"}}, LastID: 3}, p)
	p, _ = page("?before=3")
	require.Equal(t, logPage{Entries: []any{logView{1, "e1"}, logView{2, "e2"}}, LastID: 2}, p)
	p, _ = page("?before=99&limit=1")
	require.Equal(t, logPage{Entries: []any{logView{5, "e5"}}, LastID: 5}, p)
	for _, q := range []string{"?before=1", "?before=0"} {
		p, _ = page(q)
		require.Equal(t, logPage{Entries: []any{}, LastID: 0}, p, q)
	}

	for _, q := range []string{"?since=x", "?since=-1", "?limit=-1", "?limit=1e3", "?before=x", "?before=-1", "?since=1&before=3", "?since=&before="} {
		_, code := page(q)
		require.Equal(t, http.StatusBadRequest, code, q)
	}
}

// The data endpoints sit behind the session and Origin checks.
func TestDataEndpointsProtected(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	for _, path := range []string{"/api/events", "/api/state", "/api/log"} {
		req, err := http.NewRequest(http.MethodGet, "http://"+fs.s.Addr()+path, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, path)

		resp = fs.get(t, path, map[string]string{"Origin": "http://evil.example"})
		require.Equal(t, http.StatusForbidden, resp.StatusCode, path)

		req, err = http.NewRequest(http.MethodPost, "http://"+fs.s.Addr()+path, nil)
		require.NoError(t, err)
		req.Header.Set("Origin", "http://"+fs.s.Addr())
		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "a POST without a session never reaches the mux's 405: "+path)
	}
}

// Without a Source the data endpoints answer 503.
func TestDataEndpointsWithoutSource(t *testing.T) {
	s, token := newTestServer(t)
	cookie, _ := newSessionFor(t, s, token)
	req, err := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/api/state", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, CodeUnavailable, decodeAPIError(t, resp).Error.Code)
}

// A commit the feed missed (a failed hook read) reaches open streams at
// their next heartbeat, once.
func TestEventsHeartbeatCatchUp(t *testing.T) {
	fs := newFeedServer(t, 0, func(s *Server) { s.heartbeat = 30 * time.Millisecond })
	ss := fs.openStream(t, "")
	requireSnapshot(t, ss.next(t), 0)
	fs.src.mu.Lock()
	fs.src.skipPublish = true
	fs.src.mu.Unlock()
	fs.appendLog(t, "missed")
	requireLog(t, ss.next(t), 1, "missed")
	fs.appendLog(t, "fed")
	requireLog(t, ss.next(t), 2, "fed")
	ss.none(t, 200*time.Millisecond)
}

// A client that stops reading ends its stream at the write deadline.
func TestEventsStalledClientEnds(t *testing.T) {
	fs := newFeedServer(t, 1024, func(s *Server) { s.heartbeat = 50 * time.Millisecond })
	conn, err := net.Dial("tcp", fs.s.Addr())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "GET /api/events HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\n\r\n",
		fs.s.Addr(), fs.cookie.Name, fs.cookie.Value)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond) // the stream is live
	big := strings.Repeat("x", 256<<10)
	for range 100 {
		fs.appendLog(t, big)
	}
	require.Eventually(t, func() bool { return strings.Contains(fs.logs.String(), "event stream ended") },
		5*time.Second, 20*time.Millisecond, fs.logs.String())
}

// The first read happens before the response starts: its failure is a
// 500.
func TestEventsFirstReadFails(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	fs.src.failSnapshot = true
	resp := fs.get(t, "/api/events", nil)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	require.Equal(t, CodeInternal, decodeAPIError(t, resp).Error.Code)
}
