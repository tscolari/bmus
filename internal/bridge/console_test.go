package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/comms"
	"github.com/tscolari/bmus/internal/console"
	"github.com/tscolari/bmus/internal/regs"
	"github.com/tscolari/bmus/internal/store"
)

func TestRunRefusesBadConsolePort(t *testing.T) {
	t.Parallel()
	cfg := testConfig(privateHome(t))
	cfg.NoConsole = false
	cfg.ConsolePort = 65536
	err := Run(t.Context(), cfg)
	require.ErrorContains(t, err, "out of range")
}

// A taken console port fails Run before it opens the store, so before
// DBOS launches and any workflow resumes.
func TestRunConsolePortTaken(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	home := privateHome(t)
	cfg := testConfig(home)
	cfg.NoConsole = false
	cfg.ConsolePort = ln.Addr().(*net.TCPAddr).Port
	cfg.onReady = func(*Daemon) { t.Error("daemon came up") }
	err = Run(t.Context(), cfg)
	require.ErrorContains(t, err, "--no-console")
	require.NoFileExists(t, filepath.Join(home, store.DBFile))
	require.NoFileExists(t, filepath.Join(home, console.TokenFile))
}

// A token file left by a run that didn't close its console is removed
// at start, with or without the console.
func TestRunRemovesStaleToken(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	tokenFile := filepath.Join(home, console.TokenFile)
	require.NoError(t, os.WriteFile(tokenFile, []byte("stale\n"), 0o600))
	require.NoError(t, os.WriteFile(tokenFile+".123", []byte("stale\n"), 0o600)) // a crashed rename
	ctx, cancel := context.WithCancel(t.Context())
	cfg := testConfig(home)
	cfg.onReady = func(*Daemon) {
		require.NoFileExists(t, tokenFile)
		require.NoFileExists(t, tokenFile+".123")
		cancel()
	}
	require.NoError(t, Run(ctx, cfg))
}

func TestRunStartsConsole(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, console.TokenFile), []byte("stale\n"), 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan *Daemon, 1)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.NoConsole = false
	cfg.onReady = func(d *Daemon) { ready <- d }
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	var d *Daemon
	select {
	case d = <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("Run returned before ready: %v", err)
	case <-time.After(daemonReadyTimeout):
		cancel()
		t.Fatal("daemon not ready")
	}

	st, err := d.Status(ctx)
	require.NoError(t, err)
	addr := st.Console
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host)
	require.NotEqual(t, "0", port)

	data, err := os.ReadFile(filepath.Join(home, console.TokenFile))
	require.NoError(t, err)
	require.Len(t, strings.TrimSpace(string(data)), 64, "the new token, not the stale one")
	fi, err := os.Stat(filepath.Join(home, console.TokenFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	// A bare GET reaches the protected catch-all and 401s; this only
	// proves the real Run-wired server is listening, not connection
	// refused.
	resp, err := http.Get("http://" + addr + "/api/session")
	require.NoError(t, err)
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	_, err = http.Get("http://" + addr + "/api/session")
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(home, console.TokenFile))
}

// consoleGet opens a session on the console at addr (token from home) and
// returns a GET of path with its cookie.
func consoleGet(t *testing.T, addr, home, path string) *http.Response {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, console.TokenFile))
	require.NoError(t, err)
	body := `{"token":"` + strings.TrimSpace(string(data)) + `"}`
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/session", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Origin", "http://"+addr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
	require.NoError(t, err)
	for _, c := range resp.Cookies() {
		req.AddCookie(c)
	}
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp
}

type consoleEvent struct{ event, id, data string }

// readEvents parses a server-sent event stream into a channel.
func readEvents(r io.Reader) <-chan consoleEvent {
	ch := make(chan consoleEvent, 1024)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		sc.Buffer(nil, 1<<20)
		var ev consoleEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.event != "" {
					ch <- ev
				}
				ev = consoleEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return ch
}

// The console's stream over the real daemon: each committed change is one
// log event, with the log's ids, followed by the crew, order and hail
// views it names.
func TestConsoleEvents(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.NoConsole = false
	r := startDaemonConfig(t, cfg)
	d := r.d
	ctx := t.Context()
	addr := d.consoleAddr

	events := readEvents(consoleGet(t, addr, home, "/api/events").Body)
	next := func() consoleEvent {
		t.Helper()
		select {
		case ev, ok := <-events:
			require.True(t, ok, "stream ended")
			return ev
		case <-time.After(10 * time.Second):
			t.Fatal("no event within 10s")
			return consoleEvent{}
		}
	}
	snap := next()
	require.Equal(t, "snapshot", snap.event)
	var s struct {
		LastLogID int64          `json:"last_log_id"`
		Status    BridgeStatus   `json:"status"`
		Log       []LogEntryView `json:"log"`
	}
	require.NoError(t, json.Unmarshal([]byte(snap.data), &s))
	require.Equal(t, ConditionGreen, s.Status.Condition)
	require.Equal(t, addr, s.Status.Console)

	// untilLog reads up to the log event whose text has prefix, checking
	// every log event on the way, and returns it with the events behind
	// it up to the next log event. That next event must come whatever the
	// daemon does on its own, so each action is followed by a mark.
	var logIDs []int64
	var pending *consoleEvent
	untilLog := func(prefix string) (LogEntryView, []consoleEvent) {
		t.Helper()
		var found *LogEntryView
		var behind []consoleEvent
		for {
			var ev consoleEvent
			if pending != nil {
				ev, pending = *pending, nil
			} else {
				ev = next()
			}
			if ev.event == "log" {
				if found != nil {
					pending = &ev
					return *found, behind
				}
				var e LogEntryView
				require.NoError(t, json.Unmarshal([]byte(ev.data), &e))
				require.Equal(t, strconv.FormatInt(e.ID, 10), ev.id)
				logIDs = append(logIDs, e.ID)
				if strings.HasPrefix(e.Text, prefix) {
					found = &e
				}
				continue
			}
			if found != nil {
				behind = append(behind, ev)
			}
		}
	}
	// mark appends a log entry of the test's own after an action, for
	// untilLog to stop at. It is a real entry: its log event is counted
	// like any other.
	marks := 0
	mark := func() {
		t.Helper()
		marks++
		_, err := d.store.AppendLog(ctx, store.LogEntry{Kind: "bridge", Text: "marker " + strconv.Itoa(marks)})
		require.NoError(t, err)
	}
	viewOf := func(evs []consoleEvent, kind string, v any) {
		t.Helper()
		for _, ev := range evs {
			if ev.event == kind {
				require.NoError(t, json.Unmarshal([]byte(ev.data), v))
				return
			}
		}
		t.Fatalf("no %s event in %+v", kind, evs)
	}

	require.NoError(t, d.AddCrew(ctx, store.Crew{ID: "sorell", Name: "Sorell"}))
	mark()
	e, behind := untilLog("crew member added: Sorell")
	require.Equal(t, "sorell", e.CrewID)
	require.Equal(t, "crew", behind[0].event)
	var cv CrewView
	viewOf(behind, "crew", &cv)
	require.Equal(t, "sorell", cv.ID)
	// The stream sends a status event once its queue is empty, so a mark
	// already queued puts it behind the mark: read on until it comes,
	// counting the log events passed.
	var st BridgeStatus
	if slices.ContainsFunc(behind, func(e consoleEvent) bool { return e.event == "status" }) {
		viewOf(behind, "status", &st)
	} else {
		for {
			var ev consoleEvent
			if pending != nil {
				ev, pending = *pending, nil
			} else {
				ev = next()
			}
			if ev.event == "log" {
				var le LogEntryView
				require.NoError(t, json.Unmarshal([]byte(ev.data), &le))
				logIDs = append(logIDs, le.ID)
			}
			if ev.event == "status" {
				require.NoError(t, json.Unmarshal([]byte(ev.data), &st))
				break
			}
		}
	}

	id, err := d.CreateOrder(ctx, store.Order{Title: "survey", Assignee: "sorell",
		Repos: []store.OrderRepo{{Name: "app", BaseRef: "main"}}})
	require.NoError(t, err)
	mark()
	e, behind = untilLog("order created: survey")
	require.Equal(t, id, e.OrderID)
	var ov OrderView
	viewOf(behind, "order", &ov)
	require.Equal(t, id, ov.ID)
	require.Equal(t, "survey", ov.Title)

	hid, err := d.store.InsertHail(ctx, store.Hail{OrderID: id, CrewID: "sorell", Question: "left or right?"},
		store.LogEntry{Kind: "hail", CrewID: "sorell", OrderID: id, Text: "hail raised"})
	require.NoError(t, err)
	mark()
	e, behind = untilLog("hail raised")
	var hv HailView
	viewOf(behind, "hail", &hv)
	require.Equal(t, hid, hv.ID)
	require.Equal(t, "left or right?", hv.Question)
	kinds := []string{}
	for _, ev := range behind {
		if ev.event != "status" {
			kinds = append(kinds, ev.event)
		}
	}
	require.Equal(t, []string{"crew", "order", "hail"}, kinds)

	// An entry whose payload isn't JSON goes out with a null payload, and
	// the stream goes on; "after poison" is this step's mark.
	_, err = d.store.AppendLog(ctx, store.LogEntry{Kind: "bridge", Text: "poison", Payload: json.RawMessage("{not json")})
	require.NoError(t, err)
	_, err = d.store.AppendLog(ctx, store.LogEntry{Kind: "bridge", Text: "after poison"})
	require.NoError(t, err)
	e, _ = untilLog("poison")
	require.Equal(t, "null", string(e.Payload))
	// untilLog stopped at the next log event and holds it in pending.
	ev := *pending
	pending = nil
	for {
		if ev.event == "log" {
			require.NoError(t, json.Unmarshal([]byte(ev.data), &e))
			logIDs = append(logIDs, e.ID)
			if e.Text == "after poison" {
				break
			}
		}
		ev = next()
	}

	// One log event per entry, marks included: the stream's ids are the
	// log's, none missing or repeated.
	entries, err := d.store.ListLogLimit(ctx, s.LastLogID, len(logIDs))
	require.NoError(t, err)
	want := []int64{}
	for _, e := range entries {
		want = append(want, e.ID)
	}
	require.Equal(t, want, logIDs)

	// /api/state is the snapshot's body.
	var state struct {
		Crew   []CrewView  `json:"crew"`
		Orders []OrderView `json:"orders"`
		Hails  []HailView  `json:"hails"`
	}
	require.NoError(t, json.NewDecoder(consoleGet(t, addr, home, "/api/state").Body).Decode(&state))
	require.Equal(t, "sorell", state.Crew[0].ID)
	require.Equal(t, id, state.Orders[0].ID)
	require.Equal(t, hid, state.Hails[0].ID)

	// The open stream doesn't hold up shutdown.
	start := time.Now()
	r.stop(t)
	require.Less(t, time.Since(start), 15*time.Second)
}

// consoleSession opens a session on the console at addr (token from
// home) and returns its cookies and CSRF token.
func consoleSession(t *testing.T, addr, home string) ([]*http.Cookie, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, console.TokenFile))
	require.NoError(t, err)
	body := `{"token":"` + strings.TrimSpace(string(data)) + `"}`
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/session", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Origin", "http://"+addr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var s struct {
		CSRF string `json:"csrf"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&s))
	return resp.Cookies(), s.CSRF
}

// consolePost posts body to the console's path as a browser session
// would, and decodes the reply into v on success. It returns the status
// and, on failure, the error code.
func consolePost(t *testing.T, addr string, cookies []*http.Cookie, csrf, path, body string, v any) (int, string) {
	t.Helper()
	return consoleSend(t, addr, cookies, csrf, http.MethodPost, path, body, v)
}

// consoleSend is consolePost with another method.
func consoleSend(t *testing.T, addr string, cookies []*http.Cookie, csrf, method, path, body string, v any) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+addr+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Origin", "http://"+addr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BMUS-CSRF", csrf)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&e))
		return resp.StatusCode, e.Error.Code
	}
	if v != nil {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(v))
	}
	return resp.StatusCode, ""
}

// The console's actions over the real daemon: the same Daemon calls,
// validation, results and error codes as crew.add, order.create and
// hails.answer, and their log entries on the stream.
func TestConsoleActions(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "gone", Name: "Gone", Status: store.CrewDismissed})
	cfg := testConfig(home)
	cfg.Repos = map[string]string{"app": testRepo(t).url}
	cfg.NoConsole = false
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d := r.d
	addr := d.consoleAddr
	cookies, csrf := consoleSession(t, addr, home)
	post := func(path, body string, v any) (int, string) {
		t.Helper()
		return consolePost(t, addr, cookies, csrf, path, body, v)
	}
	events := readEvents(consoleGet(t, addr, home, "/api/events").Body)
	untilLog := func(text string) LogEntryView {
		t.Helper()
		for {
			select {
			case ev, ok := <-events:
				require.True(t, ok, "stream ended")
				if ev.event != "log" {
					continue
				}
				var e LogEntryView
				require.NoError(t, json.Unmarshal([]byte(ev.data), &e))
				if e.Text == text {
					return e
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("no log event %q within 10s", text)
			}
		}
	}

	// POST /api/crew is crew.add: 201 and the CrewView crew.list shows.
	var cv CrewView
	status, _ := post("/api/crew", `{"id":"sorell","name":"Sorell","rank":"Lt"}`, &cv)
	require.Equal(t, http.StatusCreated, status)
	var list CrewListResult
	require.NoError(t, call(t, home, "crew.list", nil, &list))
	// The tour may have moved on since: compare the crew member, not its state.
	listed := map[string]string{}
	for _, c := range list.Crew {
		listed[c.ID] = c.Name
	}
	require.Equal(t, map[string]string{"gone": "Gone", "sorell": "Sorell"}, listed)
	// It starts beaming in (#106), and may be on deck already.
	require.Contains(t, []string{store.CrewBeamingIn, store.CrewOnDeck}, cv.Status)
	require.Equal(t, CrewView{ID: "sorell", Name: "Sorell", Rank: "Lt", Status: cv.Status, CurrentBeam: cv.CurrentBeam,
		HarnessSource: SourceDefault, ModelSource: SourceDefault}, cv)
	require.Equal(t, "sorell", untilLog("crew member added: Sorell").CrewID)
	for body, code := range map[string]string{
		`{"id":"sorell","name":"Again"}`:                 CodeExists,
		`{"id":"kirk"}`:                                  CodeBadRequest,
		`{"id":"kirk","name":"Kirk","dossier":"rel.md"}`: CodeBadRequest,
		`{"id":"kirk","name":"Kirk","harness":"codex"}`:  CodeBadRequest,
		`{"id":"kirk","name":"Kirk","bogus":1}`:          CodeBadRequest,
	} {
		_, got := post("/api/crew", body, nil)
		require.Equal(t, code, got, body)
	}

	// POST /api/orders is order.create: 201 and the OrderView.
	var ov OrderView
	status, _ = post("/api/orders", `{"title":"survey","assignee":"sorell","priority":1,"repos":[{"name":"app","base_ref":"main"}]}`, &ov)
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, OrderView{ID: ov.ID, Kind: store.OrderKindWork, Title: "survey", State: ov.State, Priority: store.PriorityYellow, Assignee: "sorell",
		Repos: []OrderRepo{{Name: "app", BaseRef: "main"}}, BudgetSource: BudgetSource{Tokens: SourceDefault, Turns: SourceDefault},
		CreatedBy: "captain", After: []string{}, WaitingOn: []string{}}, ov)
	require.Equal(t, ov.ID, untilLog("order created: survey").OrderID)
	repos := `"repos":[{"name":"app","base_ref":"main"}]`
	for body, code := range map[string]string{
		`{"title":" ","assignee":"sorell",` + repos + `}`:                    CodeBadRequest,
		`{"title":"x",` + repos + `}`:                                        CodeBadRequest,
		`{"title":"x","assignee":"sorell","budget_tokens":-1,` + repos + `}`: CodeBadRequest,
		`{"title":"x","assignee":"sorell","repos":[]}`:                       CodeBadRequest,
		`{"title":"x","assignee":"sorell","priority":9,` + repos + `}`:       CodeBadRequest,
		`{"title":"x","assignee":"ghost",` + repos + `}`:                     CodeNotFound,
		`{"title":"x","assignee":"gone",` + repos + `}`:                      CodeUnavailable,
		`{"title":"x","assignee":"sorell","id":"ORD-9",` + repos + `}`:       CodeBadRequest,
	} {
		_, got := post("/api/orders", body, nil)
		require.Equal(t, code, got, body)
	}
	for body, status := range map[string]int{
		`{"title":"x","assignee":"ghost",` + repos + `}`: http.StatusNotFound,
		`{"title":"x","assignee":"gone",` + repos + `}`:  http.StatusConflict,
	} {
		got, _ := post("/api/orders", body, nil)
		require.Equal(t, status, got, body)
	}

	// POST /api/hails/{id}/answer is hails.answer: 200 and the HailView.
	requirePending(t, d, missionWorkflowID(ov.ID))
	hailID, err := d.store.InsertHail(t.Context(), store.Hail{OrderID: ov.ID, CrewID: "sorell",
		Category: "tool_approval", Question: "Push?", Options: []string{"allow", "deny"}}, testEntry)
	require.NoError(t, err)
	answer := "/api/hails/" + hailID + "/answer"
	for body, code := range map[string]string{
		`{}`:           CodeBadRequest,
		`{"option":5}`: CodeBadRequest,
		`{"hail_id":"` + hailID + `","option":0}`: CodeBadRequest,
	} {
		_, got := post(answer, body, nil)
		require.Equal(t, code, got, body)
	}
	status, code := post("/api/hails/H-9999/answer", `{"decision":"yes"}`, nil)
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, CodeNotFound, code)
	var hv HailView
	status, _ = post(answer, `{"option":0}`, &hv)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, hailID, hv.ID)
	require.Equal(t, store.HailDecided, hv.Status)
	require.Equal(t, "allow", hv.Decision)
	require.Equal(t, "captain", hv.DecidedBy)
	status, code = post(answer, `{"decision":"deny"}`, nil)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, CodeHailClosed, code)

	// POST /api/hails/{id}/countermand is hails.countermand: the captain's
	// own answer has no window.
	countermand := "/api/hails/" + hailID + "/countermand"
	status, code = post(countermand, `{"option":1,"reason":"no"}`, nil)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, CodeCountermandExpired, code)
	_, code = post(countermand, `{"reason":"no"}`, nil)
	require.Equal(t, CodeBadRequest, code)
}

// Once the daemon is stopping an action is refused with shutting_down,
// and an error from a call cut short by it carries that code too.
func TestConsoleActionsShuttingDown(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemon(t, home)
	defer r.stop(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	src := consoleSource{d: r.d, ctx: ctx, actions: &actionGate{}}
	for name, act := range map[string]func() (any, error){
		"crew":   func() (any, error) { return src.AddCrew(t.Context(), json.RawMessage(`{"id":"a","name":"A"}`)) },
		"orders": func() (any, error) { return src.CreateOrder(t.Context(), json.RawMessage(`{"title":"x"}`)) },
		"hails":  func() (any, error) { return src.AnswerHail(t.Context(), "H-0001", json.RawMessage(`{"option":0}`)) },
		"countermand": func() (any, error) {
			return src.CountermandHail(t.Context(), "H-0001", json.RawMessage(`{"option":0}`))
		},
		"belay":  func() (any, error) { return src.BelayOrder(t.Context(), "ORD-0001", json.RawMessage(`{}`)) },
		"recall": func() (any, error) { return src.RecallOrder(t.Context(), "ORD-0001", json.RawMessage(`{}`)) },
		"withdraw": func() (any, error) {
			return src.WithdrawOrder(t.Context(), "ORD-0001", json.RawMessage(`{}`))
		},
		"route": func() (any, error) {
			return src.RouteOrder(t.Context(), "ORD-0001", json.RawMessage(`{"division":"ops"}`))
		},
		"reenergize": func() (any, error) { return src.ReenergizeCrew(t.Context(), "a", json.RawMessage(`{}`)) },
		"push":       func() (any, error) { return src.PushOrder(t.Context(), "ORD-0001", json.RawMessage(`{}`)) },
		"standing": func() (any, error) {
			return src.SetStandingOrders(t.Context(), json.RawMessage(`{"on":true}`))
		},
	} {
		_, err := act()
		var re *rpcError
		require.ErrorAs(t, err, &re, name)
		require.Equal(t, CodeShutdown, re.Code(), name)
	}
	_, err := r.d.store.GetCrew(t.Context(), "a")
	require.Error(t, err, "nothing added")
	on, err := r.d.standingOrdersNow(t.Context())
	require.NoError(t, err)
	require.False(t, on, "standing orders untouched")

	// A failure seen after the daemon's context ended is shutting_down,
	// as the RPC server reports it.
	re := codedError(ctx, errors.New("context canceled"))
	require.Equal(t, CodeShutdown, re.Code())
	require.Equal(t, "bridge is shutting down: context canceled", re.Error())
}

// The console's codes for the RPC's are spelled the same.
func TestConsoleCodesMatchRPC(t *testing.T) {
	t.Parallel()
	for rpc, cons := range map[string]string{
		CodeBadRequest:         console.CodeBadRequest,
		CodeNotFound:           console.CodeNotFound,
		CodeExists:             console.CodeExists,
		CodeUnavailable:        console.CodeUnavailable,
		CodeHailClosed:         console.CodeHailClosed,
		CodeCountermandExpired: console.CodeCountermandExpired,
		CodeConflict:           console.CodeConflict,
		CodeShutdown:           console.CodeShutdown,
		CodeForbidden:          console.CodeForbidden,
		CodeInternal:           console.CodeInternal,
	} {
		require.Equal(t, rpc, cons)
	}
}

// Closing the gate waits for an action in flight, and refuses new ones.
func TestConsoleActionGate(t *testing.T) {
	t.Parallel()
	src := consoleSource{ctx: t.Context(), actions: &actionGate{}}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := src.act(t.Context(), func(context.Context) (any, error) {
			close(started)
			<-release
			return nil, nil
		})
		done <- err
	}()
	<-started
	closed := make(chan struct{})
	go func() {
		src.actions.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("close returned with an action in flight")
	case <-time.After(50 * time.Millisecond):
	}
	require.Eventually(t, func() bool {
		src.actions.mu.Lock()
		defer src.actions.mu.Unlock()
		return src.actions.closed
	}, 5*time.Second, time.Millisecond)
	_, err := src.act(t.Context(), func(context.Context) (any, error) {
		t.Error("an action ran after close")
		return nil, nil
	})
	var re *rpcError
	require.ErrorAs(t, err, &re)
	require.Equal(t, CodeShutdown, re.Code())
	close(release)
	require.NoError(t, <-done)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not return")
	}
}

// consoleClient is a console session on a running daemon, for the action
// tests: its requests and its event stream.
type consoleClient struct {
	t       *testing.T
	addr    string
	cookies []*http.Cookie
	csrf    string
	events  <-chan consoleEvent
}

func newConsoleClient(t *testing.T, d *Daemon, home string) *consoleClient {
	t.Helper()
	cookies, csrf := consoleSession(t, d.consoleAddr, home)
	return &consoleClient{t: t, addr: d.consoleAddr, cookies: cookies, csrf: csrf,
		events: readEvents(consoleGet(t, d.consoleAddr, home, "/api/events").Body)}
}

func (cc *consoleClient) send(method, path, body string, v any) (int, string) {
	cc.t.Helper()
	return consoleSend(cc.t, cc.addr, cc.cookies, cc.csrf, method, path, body, v)
}

// until reads the stream up to the event of type kind whose data match
// says yes to, and returns that data.
func (cc *consoleClient) until(kind string, match func(data string) bool) string {
	cc.t.Helper()
	for {
		select {
		case ev, ok := <-cc.events:
			require.True(cc.t, ok, "stream ended")
			if ev.event == kind && match(ev.data) {
				return ev.data
			}
		case <-time.After(10 * time.Second):
			cc.t.Fatalf("no %s event within 10s", kind)
		}
	}
}

// untilLog reads the stream up to the log event with text.
func (cc *consoleClient) untilLog(text string) LogEntryView {
	cc.t.Helper()
	var e LogEntryView
	cc.until("log", func(data string) bool {
		require.NoError(cc.t, json.Unmarshal([]byte(data), &e))
		return e.Text == text
	})
	return e
}

// The console's Phase 2 actions over the real daemon (#78): withdraw,
// route, re-energize and standing orders with the results, validation and
// error codes of order.withdraw, order.route, crew.reenergize and
// standing_orders.set, their effects visible to the CLI's RPCs, and belay
// and recall's refusals. TestConsoleBelayRecall covers those two on a
// running mission.
func TestConsolePhase2Actions(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	cfg := testConfig(home)
	cfg.Transporter = &fakeProvider{hello: true}
	cfg.Divisions = map[string]DivisionConfig{"ops": {}, "eng": {}}
	cfg.wakeSend = func(string, CrewSignal) error { return nil }
	cfg.NoConsole = false
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d := r.d
	ctx := t.Context()
	// Straight into the store: no tour claims anything.
	for _, c := range []store.Crew{
		{ID: "kirk", Name: "Kirk", Division: "ops", Status: store.CrewOnDeck},
		{ID: "spock", Name: "Spock", Division: "ops", Status: store.CrewSickbay},
	} {
		require.NoError(t, d.store.InsertCrew(ctx, c, testEntry))
	}
	insert := func(o store.Order) string {
		t.Helper()
		id, err := d.store.InsertOrder(ctx, o, testEntry)
		require.NoError(t, err)
		return id
	}
	// A finished order whose entries drop out of the snapshot's log.
	done := insert(store.Order{Title: "old", Assignee: "kirk", State: store.OrderDone})
	for range console.SnapshotLog {
		_, err := d.store.AppendLog(ctx, store.LogEntry{Kind: "test", Text: "filler"})
		require.NoError(t, err)
	}
	pool := insert(store.Order{Title: "p", Division: "ops"})
	routed := insert(store.Order{Title: "r", Division: "ops"})
	cc := newConsoleClient(t, d, home)
	post := func(path, body string, v any) (int, string) {
		t.Helper()
		return cc.send(http.MethodPost, path, body, v)
	}

	// Withdraw: 200 and the OrderView order.withdraw returns.
	var ov OrderView
	status, _ := post("/api/orders/"+pool+"/withdraw", `{"reason":"duplicate"}`, &ov)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, store.OrderWithdrawn, ov.State)
	require.Equal(t, pool, ov.ID)
	require.Equal(t, pool, cc.untilLog("order withdrawn: duplicate").OrderID)
	var orders OrderListResult
	require.NoError(t, call(t, home, "order.list", nil, &orders))
	states := map[string]string{}
	for _, o := range orders.Orders {
		states[o.ID] = o.State
	}
	require.Equal(t, store.OrderWithdrawn, states[pool], "the CLI sees the withdrawal")

	// Route: 200 and the routed OrderView; the codes order.route gives.
	status, _ = post("/api/orders/"+routed+"/route", `{"assignee":"kirk"}`, &ov)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{store.OrderDirected, "kirk", "ops"}, []string{ov.State, ov.Assignee, ov.Division})
	cc.untilLog("order routed to kirk")
	status, _ = post("/api/orders/"+routed+"/route", `{"assignee":"","division":"eng"}`, &ov)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{store.OrderPool, "", "eng"}, []string{ov.State, ov.Assignee, ov.Division})

	for _, c := range []struct {
		path, body string
		status     int
		code       string
	}{
		{"/api/orders/" + pool + "/withdraw", `{}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/orders/ORD-9999/withdraw", `{}`, http.StatusNotFound, CodeNotFound},
		{"/api/orders/" + routed + "/withdraw", `{"id":"` + routed + `"}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/orders/" + routed + "/withdraw", `{"reason":1}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/orders/" + routed + "/route", `{}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/orders/" + routed + "/route", `{"assignee":"spock"}`, http.StatusConflict, CodeUnavailable},
		{"/api/orders/" + routed + "/route", `{"assignee":"nobody"}`, http.StatusNotFound, CodeNotFound},
		{"/api/orders/" + routed + "/route", `{"division":"science"}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/orders/" + routed + "/route", `{"assignee":"","division":""}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/orders/" + pool + "/route", `{"division":"ops"}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/orders/ORD-9999/route", `{"division":"ops"}`, http.StatusNotFound, CodeNotFound},
		// Belay and recall refuse an order with no mission.
		{"/api/orders/" + routed + "/belay", `{}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/orders/" + routed + "/recall", `{"reason":"x"}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/orders/" + done + "/belay", `{}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/orders/ORD-9999/belay", `{}`, http.StatusNotFound, CodeNotFound},
		{"/api/orders/ORD-9999/recall", `{}`, http.StatusNotFound, CodeNotFound},
		{"/api/orders/" + routed + "/belay", `{"bogus":true}`, http.StatusBadRequest, CodeBadRequest},
		// Re-energize takes a crew member in sickbay, and an empty body.
		{"/api/crew/kirk/reenergize", `{}`, http.StatusBadRequest, CodeBadRequest},
		{"/api/crew/nobody/reenergize", `{}`, http.StatusNotFound, CodeNotFound},
		{"/api/crew/spock/reenergize", `{"id":"spock"}`, http.StatusBadRequest, CodeBadRequest},
	} {
		status, code := post(c.path, c.body, nil)
		require.Equal(t, [2]any{c.status, c.code}, [2]any{status, code}, "%s %s", c.path, c.body)
	}
	o := getOrder(t, d, routed)
	require.Equal(t, []string{store.OrderPool, "", "eng"}, []string{o.State, o.Assignee, o.Division}, "refusals change nothing")
	require.Equal(t, store.CrewSickbay, getCrew(t, d, "spock").Status)

	// Re-energize: 200, the crew member and the outcome crew.reenergize
	// returns; the new tour brings the crew member on deck.
	var res CrewActionResult
	status, _ = post("/api/crew/spock/reenergize", `{}`, &res)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, OutcomeRequested, res.Outcome)
	require.Equal(t, "spock", res.ID)
	waitOnDeck(t, d, "spock", "fake-1")
	status, code := post("/api/crew/spock/reenergize", `{}`, nil)
	require.Equal(t, [2]any{http.StatusBadRequest, CodeBadRequest}, [2]any{status, code}, "on deck now")

	// Standing orders: PUT {on}, the CLI's standing-orders reads it, and
	// the status event and /api/state carry it.
	put := func(body string, v any) (int, string) {
		t.Helper()
		return cc.send(http.MethodPut, "/api/standing-orders", body, v)
	}
	var so StandingOrdersResult
	status, _ = put(`{"on":true}`, &so)
	require.Equal(t, http.StatusOK, status)
	require.True(t, so.On)
	var got StandingOrdersResult
	require.NoError(t, call(t, home, "standing_orders.get", nil, &got))
	require.True(t, got.On)
	cc.until("status", func(data string) bool {
		var st BridgeStatus
		require.NoError(t, json.Unmarshal([]byte(data), &st))
		return st.StandingOrders
	})
	for _, body := range []string{`{}`, `{"on":"yes"}`, `{"on":true,"extra":1}`} {
		status, code := put(body, nil)
		require.Equal(t, [2]any{http.StatusBadRequest, CodeBadRequest}, [2]any{status, code}, body)
	}
	status, _ = put(`{"on":false}`, &so)
	require.Equal(t, http.StatusOK, status)
	require.False(t, so.On)
	cc.until("status", func(data string) bool {
		var st BridgeStatus
		require.NoError(t, json.Unmarshal([]byte(data), &st))
		return !st.StandingOrders
	})

	// The snapshot carries the finished orders its log names: the
	// withdrawn one, not the done one whose entries are older.
	var state struct {
		Orders []OrderView  `json:"orders"`
		Status BridgeStatus `json:"status"`
	}
	require.NoError(t, json.NewDecoder(consoleGet(t, d.consoleAddr, home, "/api/state").Body).Decode(&state))
	got2 := map[string]string{}
	for _, o := range state.Orders {
		got2[o.ID] = o.State
	}
	require.Equal(t, map[string]string{pool: store.OrderWithdrawn, routed: store.OrderPool}, got2)
	require.False(t, state.Status.StandingOrders)
}

// The snapshot's order views carry each order's latest progress (#141):
// after a reload, an away order whose last progress entry is older than
// the snapshot's log window still shows it. An order without a report
// carries no progress fields.
func TestConsoleSnapshotProgress(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemon(t, home)
	defer r.stop(t)
	d := r.d
	ctx := t.Context()
	// Straight into the store: no tour claims anything.
	require.NoError(t, d.store.InsertCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk", Status: store.CrewAway}, testEntry))
	away, err := d.store.InsertOrder(ctx, store.Order{Title: "a", Assignee: "kirk", State: store.OrderAway}, testEntry)
	require.NoError(t, err)
	quiet, err := d.store.InsertOrder(ctx, store.Order{Title: "q", Division: "ops"}, testEntry)
	require.NoError(t, err)
	// What the comms pump does with a progress report.
	_, err = d.store.RecordInbox(ctx, "kirk", 0, 1, func(tx store.InboxTx) error {
		if _, err := tx.SetOrderProgress(away, 40, "surveying"); err != nil {
			return err
		}
		_, err := tx.AppendLog(store.LogEntry{Kind: "order", CrewID: "kirk", OrderID: away, Text: "surveying",
			Payload: json.RawMessage(`{"seq":1,"pct":40}`)})
		return err
	})
	require.NoError(t, err)
	for range console.SnapshotLog {
		_, err := d.store.AppendLog(ctx, store.LogEntry{Kind: "test", Text: "filler"})
		require.NoError(t, err)
	}

	snap, err := consoleSource{d: d}.Snapshot(ctx)
	require.NoError(t, err)
	for _, e := range snap.Log.([]LogEntryView) {
		require.NotEqual(t, away, e.OrderID, "the progress entry is outside the window")
	}
	views := map[string]OrderView{}
	for _, o := range snap.Orders.([]OrderView) {
		views[o.ID] = o
	}
	require.NotNil(t, views[away].ProgressPct)
	require.InDelta(t, 40, *views[away].ProgressPct, 0)
	require.Equal(t, "surveying", views[away].ProgressNote)
	data, err := json.Marshal(views[away])
	require.NoError(t, err)
	require.Contains(t, string(data), `"progress_pct":40,"progress_note":"surveying"`)
	data, err = json.Marshal(views[quiet])
	require.NoError(t, err)
	require.NotContains(t, string(data), "progress_")
}

// The snapshot and /api/state carry the registered repos, sorted (#211),
// and an empty list, not null, on a bridge with none.
func TestConsoleSnapshotRepos(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		repos map[string]string
		want  []string
		json  string
	}{
		"several": {map[string]string{"lib": testRepo(t).url, "app": testRepo(t).url}, []string{"app", "lib"}, `"repos":["app","lib"]`},
		"none":    {nil, []string{}, `"repos":[]`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := privateHome(t)
			cfg := testConfig(home)
			cfg.Repos = tc.repos
			cfg.NoConsole = false
			r := startDaemonConfig(t, cfg)
			defer r.stop(t)

			snap, err := consoleSource{d: r.d}.Snapshot(t.Context())
			require.NoError(t, err)
			require.NotNil(t, snap.Repos)
			require.Equal(t, tc.want, snap.Repos)

			body, err := io.ReadAll(consoleGet(t, r.d.consoleAddr, home, "/api/state").Body)
			require.NoError(t, err)
			require.Contains(t, string(body), tc.json)
		})
	}
}

// The snapshot's hails are the open ones and the decided ones still inside
// their countermand window (#153), with confidence and countermand_until.
func TestConsoleSnapshotCountermandable(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemon(t, home)
	defer r.stop(t)
	d := r.d
	ctx := t.Context()
	require.NoError(t, d.store.InsertCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk"}, testEntry))
	orderID, err := d.store.InsertOrder(ctx, store.Order{Title: "a", Assignee: "kirk"}, testEntry)
	require.NoError(t, err)
	hail := func(dec *store.HailDecision) string {
		t.Helper()
		id, err := d.store.InsertHail(ctx, store.Hail{OrderID: orderID, CrewID: "kirk", Question: "?"}, testEntry)
		require.NoError(t, err)
		if dec != nil {
			require.NoError(t, d.store.DecideOpenHailAs(ctx, id, *dec, testEntry, nil))
		}
		return id
	}
	conf := 0.8
	later, earlier := time.Now().Add(time.Hour), time.Now().Add(-time.Second)
	inWindow := hail(&store.HailDecision{DecidedBy: comms.DecidedByCommand, Decision: "allow", Confidence: &conf, CountermandUntil: &later})
	hail(&store.HailDecision{DecidedBy: comms.DecidedByCommand, Decision: "allow", CountermandUntil: &earlier})
	hail(&store.HailDecision{DecidedBy: comms.DecidedByPolicy, Decision: "allow", Route: "auto"})
	open := hail(nil)

	snap, err := consoleSource{d: d}.Snapshot(ctx)
	require.NoError(t, err)
	var ids []string
	views := map[string]HailView{}
	for _, h := range snap.Hails.([]HailView) {
		ids = append(ids, h.ID)
		views[h.ID] = h
	}
	require.Equal(t, []string{open, inWindow}, ids)
	v := views[inWindow]
	require.NotNil(t, v.Confidence)
	require.InDelta(t, 0.8, *v.Confidence, 0)
	require.NotNil(t, v.CountermandUntil)
	require.WithinDuration(t, later, *v.CountermandUntil, time.Millisecond)
	require.Equal(t, time.UTC, v.CountermandUntil.Location())
	require.Nil(t, views[open].CountermandUntil)
}

// A bridge-raised hail (#151), with no order or crew member, reads through
// every list path beside a beam's hail: hails.list, the console's snapshot
// and its feed, the status count and hails.metrics. Its order and crew
// are empty.
func TestBridgeHailReaders(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemon(t, home)
	defer r.stop(t)
	d := r.d
	ctx := t.Context()
	require.NoError(t, d.store.InsertCrew(ctx, store.Crew{ID: "kirk", Name: "Kirk"}, testEntry))
	orderID, err := d.store.InsertOrder(ctx, store.Order{Title: "a", Assignee: "kirk"}, testEntry)
	require.NoError(t, err)
	bridgeHail, err := d.store.InsertBridgeHail(ctx, store.Hail{DeliverTo: "findings/" + orderID, Category: "tracker_review",
		Question: "review?"}, store.LogEntry{Kind: "hail", OrderID: orderID, Text: "review raised"})
	require.NoError(t, err)
	beamHail, err := d.store.InsertHail(ctx, store.Hail{OrderID: orderID, CrewID: "kirk", Question: "?"}, testEntry)
	require.NoError(t, err)

	raw, err := json.Marshal(HailsListArgs{Status: store.HailOpen})
	require.NoError(t, err)
	res, err := rpcHailsList(ctx, d, raw)
	require.NoError(t, err)
	views := res.(HailsListResult).Hails
	require.Len(t, views, 2)
	require.Equal(t, bridgeHail, views[0].ID)
	require.Empty(t, views[0].OrderID)
	require.Empty(t, views[0].CrewID)
	require.Equal(t, beamHail, views[1].ID)
	require.Equal(t, orderID, views[1].OrderID)

	snap, err := consoleSource{d: d}.Snapshot(ctx)
	require.NoError(t, err)
	var ids []string
	for _, h := range snap.Hails.([]HailView) {
		ids = append(ids, h.ID)
	}
	require.Equal(t, []string{bridgeHail, beamHail}, ids)
	st, err := d.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, st.HailsOpen)

	entries, err := d.store.ListLog(ctx, 0)
	require.NoError(t, err)
	var raised store.LogEntry
	for _, e := range entries {
		if e.Text == "review raised" {
			raised = e
		}
	}
	events, err := consoleSource{d: d}.Views(ctx, raised)
	require.NoError(t, err)
	var hailEvents []string
	for _, ev := range events {
		if ev.Type == "hail" {
			hailEvents = append(hailEvents, ev.Data.(HailView).ID)
		}
	}
	require.Equal(t, []string{bridgeHail}, hailEvents)

	require.NoError(t, d.store.DecideOpenHail(ctx, bridgeHail, store.DeciderCaptain, "keep", "", testEntry, nil))
	raw, err = json.Marshal(HailsMetricsArgs{})
	require.NoError(t, err)
	mres, err := rpcHailsMetrics(ctx, d, raw)
	require.NoError(t, err)
	m := mres.(HailsMetricsResult)
	require.Equal(t, 2, m.Total)
	require.Equal(t, 1, m.Decided)
}

// The console's belay and recall on running missions (#78): each is
// order.belay or order.recall, returning the order's view as the request
// is queued, and the mission carries it out. A belayed order in its hail
// wait settles belayed; a recalled one goes back to the pool, parked, and
// a belay then settles it from there.
func TestConsoleBelayRecall(t *testing.T) {
	t.Parallel()
	b := newHolodeckBridge(t, hailScript(t))
	b.console = true
	hd := b.start(t)
	hd.addCrewWith(t, store.Crew{ID: "sorell", Name: "Sorell", Division: "ops"})
	cc := newConsoleClient(t, hd.d, b.home)
	post := func(path, body string, v any) (int, string) {
		t.Helper()
		return cc.send(http.MethodPost, path, body, v)
	}

	belayed := hd.createDivisionOrder(t, "sorell", "ops")
	hd.waitHail(t, "H-0001")
	var ov OrderView
	status, _ := post("/api/orders/"+belayed+"/belay", `{"reason":"not needed"}`, &ov)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, belayed, ov.ID)
	require.Contains(t, []string{store.OrderAway, store.OrderHailing, store.OrderBelayed}, ov.State)
	require.Equal(t, MissionResult{Status: MissionBelayed, Reason: "not needed"}, missionResult(t, hd.d, belayed))
	cc.until("order", func(data string) bool {
		require.NoError(t, json.Unmarshal([]byte(data), &ov))
		return ov.ID == belayed && ov.State == store.OrderBelayed
	})
	status, code := post("/api/orders/"+belayed+"/belay", `{}`, nil)
	require.Equal(t, [2]any{http.StatusBadRequest, CodeBadRequest}, [2]any{status, code}, "too late")
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, store.CrewOnDeck, hd.crew(ct, "sorell").Status)
	}, 10*time.Second, 50*time.Millisecond)

	recalled := hd.createDivisionOrder(t, "sorell", "ops")
	hd.waitHail(t, "H-0002")
	status, _ = post("/api/orders/"+recalled+"/recall", `{"reason":"reprioritise"}`, &ov)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, recalled, ov.ID)
	cc.until("order", func(data string) bool {
		require.NoError(t, json.Unmarshal([]byte(data), &ov))
		return ov.ID == recalled && ov.State == store.OrderPool
	})
	require.Empty(t, ov.Assignee)
	require.NotNil(t, hd.order(t, recalled).ParkedAt)
	status, code = post("/api/orders/"+recalled+"/recall", `{}`, nil)
	require.Equal(t, [2]any{http.StatusBadRequest, CodeBadRequest}, [2]any{status, code}, "in the pool")
	status, _ = post("/api/orders/"+recalled+"/belay", `{}`, &ov)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, MissionResult{Status: MissionBelayed}, missionResult(t, hd.d, recalled))

	entries := hd.logEntries(t)
	require.Equal(t, []string{"belay requested: not needed"}, orderTexts(entries, belayed, "belay requested"))
	require.Equal(t, []string{"recall requested: reprioritise"}, orderTexts(entries, recalled, "recall requested"))
	require.Equal(t, []string{"order recalled: reprioritise"}, orderTexts(entries, recalled, "order recalled"))
	require.Zero(t, kinds(entries)[logFail])
}

// The console's push (#4) over the real daemon: order.push's checks and
// results through POST /api/orders/{id}/push. An order that isn't done is
// a 400; the push returns each repo's URL, branch and commit, and the
// remote has the branch; a diverged target is a 409, and force then
// overwrites it. The order's base commits are its ship-repos entries'.
func TestConsolePushOrder(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	insertCrew(t, home, store.Crew{ID: "sorell", Name: "Sorell"})
	up := testRepo(t)
	bare := bareOrigin(t, up)
	beams := t.TempDir()
	cfg := testConfig(home)
	cfg.Transporter = &fakeProvider{hello: true, copyRoot: beams}
	cfg.Repos = map[string]string{"app": "file://" + bare}
	cfg.NoConsole = false
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d := r.d
	waitOnDeck(t, d, "sorell", "fake-1")
	cookies, csrf := consoleSession(t, d.consoleAddr, home)
	push := func(id, body string, v any) (int, string) {
		t.Helper()
		return consolePost(t, d.consoleAddr, cookies, csrf, "/api/orders/"+id+"/push", body, v)
	}

	id := startMission(t, d)
	branch := orderBranch(id)
	status, code := push(id, `{}`, nil)
	require.Equal(t, [2]any{http.StatusBadRequest, CodeBadRequest}, [2]any{status, code}, "away")
	status, code = push(id, `{"target_branch":"x","bogus":1}`, nil)
	require.Equal(t, [2]any{http.StatusBadRequest, CodeBadRequest}, [2]any{status, code}, "unknown field")
	status, code = push("ORD-9999", `{}`, nil)
	require.Equal(t, [2]any{http.StatusNotFound, CodeNotFound}, [2]any{status, code})

	work := "/beams/fake-1/work/" + id
	base, commit := workBundle(t, filepath.Join(beams, work, "in", "app.bundle"), id, filepath.Join(beams, work, "out", "app.bundle"))
	sendReport(t, d, id, 2, comms.ReportBody{Summary: "did it",
		Repos: []comms.WorkRepo{{Name: "app", BundlePath: work + "/out/app.bundle", Commit: commit, Branch: branch}}})
	require.Equal(t, MissionDone, missionResult(t, d, id).Status)

	var pushed []PushedRepo
	status, _ = push(id, `{}`, &pushed)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []PushedRepo{{Name: "app", URL: "file://" + bare, Branch: branch, Commit: commit}}, pushed)
	require.Equal(t, commit+"\trefs/heads/"+branch, gitT(t, bare, "ls-remote", bare, branch))

	// A diverged target: 409, the remote untouched; then forced.
	gitT(t, up.url, "checkout", "-q", "-b", "feature", "main~1")
	diverged := up.commit(t, "elsewhere")
	gitT(t, up.url, "push", "-q", bare, "feature")
	status, code = push(id, `{"target_branch":"feature"}`, nil)
	require.Equal(t, [2]any{http.StatusConflict, CodeConflict}, [2]any{status, code})
	require.Equal(t, diverged, gitT(t, bare, "rev-parse", "feature"))
	status, _ = push(id, `{"target_branch":"feature","force":true}`, &pushed)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []PushedRepo{{Name: "app", URL: "file://" + bare, Branch: "feature", Commit: commit}}, pushed)
	require.Equal(t, commit, gitT(t, bare, "rev-parse", "feature"))

	// The snapshot carries the repos' URLs; the order's base commit is
	// the diff's (#104).
	snap, err := consoleSource{d: d}.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"app": "file://" + bare}, snap.RepoURLs)
	bases, err := d.baseCommits(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"app": base}, bases)
}

// The snapshot's repo URLs carry no credentials.
func TestConsoleSnapshotRedactsRepoURLs(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	cfg := testConfig(home)
	cfg.Repos = map[string]string{
		"app": "https://user:secret@example.com/o/app.git",
		"lib": "git@example.com:o/lib.git",
	}
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	snap, err := consoleSource{d: r.d}.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"app": "https://example.com/o/app.git",
		"lib": "git@example.com:o/lib.git",
	}, snap.RepoURLs)
}

// A console action that runs past its time fails with timeout and a
// message that says so, without the error it cut short.
func TestConsoleActionTimeout(t *testing.T) {
	t.Parallel()
	src := consoleSource{ctx: t.Context(), actions: &actionGate{}}
	_, err := src.actFor(time.Millisecond, "push", func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, errors.New("git push: secret detail: " + ctx.Err().Error())
	})
	var re *rpcError
	require.ErrorAs(t, err, &re)
	require.Equal(t, console.CodeTimeout, re.Code())
	require.Equal(t, "push timed out after 1ms; it may have taken effect in part: see the captain's log", re.Error())
}

// The console's answer to a findings review (#157) reaches AnswerHail as
// the page sent it: a bad decision is a 400 whose message names the refs
// and keys the review takes, a mismatched option one too, and a good
// decision is stored in normal form with its option. A triage hail is
// refused with the plan's message.
func TestConsoleAnswerReview(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	cfg := testConfig(home)
	cfg.NoConsole = false
	r := startDaemonConfig(t, cfg)
	defer r.stop(t)
	d := r.d
	ctx := t.Context()
	raw, err := json.Marshal(testReview)
	require.NoError(t, err)
	review, err := d.store.InsertBridgeHail(ctx, store.Hail{DeliverTo: "findings/ORD-0001", Category: regs.CategoryTrackerReview,
		Question: "Review?", Options: reviewOptions, Context: raw}, store.LogEntry{Kind: logHail, Text: "review raised"})
	require.NoError(t, err)
	triage, err := d.store.InsertBridgeHail(ctx, store.Hail{DeliverTo: "findings/ORD-0002", Category: categoryFindingsTriage,
		Question: "Triage?"}, store.LogEntry{Kind: logHail, Text: "triage raised"})
	require.NoError(t, err)

	cookies, csrf := consoleSession(t, d.consoleAddr, home)
	answer := func(id, body string) (int, string, HailView) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+d.consoleAddr+"/api/hails/"+id+"/answer", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Origin", "http://"+d.consoleAddr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-BMUS-CSRF", csrf)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		var out struct {
			HailView
			Error struct{ Code, Message string } `json:"error"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return resp.StatusCode, out.Error.Message, out.HailView
	}

	status, msg, _ := answer(review, `{"decision":"discard #9"}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, `hail `+review+`: "#9" is not a ref of the review; answer keep, discard, discard <ref>..., `+
		`or restore <key>..., the first three optionally followed by "; restore <key>..." (refs: #2, #3, #4; keys: noise, lost)`, msg)
	status, msg, _ = answer(review, `{"option":0,"decision":"discard #2"}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, `hail `+review+`: option 0 (keep) does not match the decision "discard #2"`, msg)
	status, msg, _ = answer(triage, `{"decision":"{}"}`)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "hail "+triage+" is Command's advisory triage; review the issues in its tracker_review hail", msg)

	status, msg, v := answer(review, `{"decision":"discard #4 #2; restore lost noise"}`)
	require.Equal(t, http.StatusOK, status, msg)
	require.Equal(t, "discard #2 #4; restore noise lost", v.Decision)
	require.Equal(t, store.HailDecided, v.Status)
	h, err := d.store.GetHail(ctx, review)
	require.NoError(t, err)
	require.Equal(t, "discard #2 #4; restore noise lost", h.Decision)
}
