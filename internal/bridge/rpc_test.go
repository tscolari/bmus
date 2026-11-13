package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

func call(t *testing.T, home, cmd string, args, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	return Call(ctx, filepath.Join(home, SocketFile), cmd, args, result)
}

// requireRPCError requires err to be a failed reply with code.
func requireRPCError(t *testing.T, err error, code string) *RPCError {
	t.Helper()
	var re *RPCError
	require.ErrorAs(t, err, &re)
	require.Equal(t, code, re.Code, "message: %s", re.Message)
	return re
}

// rawCall writes req as is, half-closes, and decodes the reply.
func rawCall(t *testing.T, home string, req []byte) Response {
	t.Helper()
	conn, err := net.Dial("unix", filepath.Join(home, SocketFile))
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(20*time.Second)))
	// The server may reply and close before reading all of an oversize
	// request, so write concurrently and ignore the write's error.
	go func() {
		conn.Write(req)
		conn.(*net.UnixConn).CloseWrite()
	}()
	b, err := io.ReadAll(conn)
	require.NoError(t, err)
	var resp Response
	require.NoError(t, json.Unmarshal(b, &resp), "reply %q", b)
	require.True(t, strings.HasSuffix(string(b), "\n"), "reply is one line")
	return resp
}

func TestRPCCommands(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemon(t, home)
	defer r.stop(t)

	var crew CrewListResult
	require.NoError(t, call(t, home, "crew.list", nil, &crew))
	require.Empty(t, crew.Crew)
	require.NotNil(t, crew.Crew, "an empty list is [], not null")

	// crew.add inserts the crew member and starts its tour.
	var added CrewView
	require.NoError(t, call(t, home, "crew.add", CrewAddArgs{ID: "sorell", Name: "Sorell", Rank: "Lieutenant", Division: "science"}, &added))
	// The reply re-reads the row after the tour starts, and the tour may
	// already have provisioned its beam, and had its hello. It starts
	// beaming in (#106).
	require.Contains(t, []string{"", "fake-1"}, added.CurrentBeam)
	require.Contains(t, []string{store.CrewBeamingIn, store.CrewOnDeck}, added.Status)
	added.CurrentBeam, added.Status = "", store.CrewBeamingIn
	require.Equal(t, CrewView{ID: "sorell", Name: "Sorell", Rank: "Lieutenant", Division: "science", Status: store.CrewBeamingIn,
		HarnessSource: SourceDefault, ModelSource: SourceDefault}, added)
	requirePending(t, r.d, tourWorkflowID("sorell", 0))
	requireRPCError(t, call(t, home, "crew.add", CrewAddArgs{ID: "sorell", Name: "Again"}, nil), CodeExists)
	requireRPCError(t, call(t, home, "crew.add", CrewAddArgs{ID: "nameless"}, nil), CodeBadRequest)
	requireRPCError(t, call(t, home, "crew.add", CrewAddArgs{ID: "x", Name: "X", Dossier: "rel/path.md"}, nil), CodeBadRequest)
	requireRPCError(t, call(t, home, "crew.add", CrewAddArgs{ID: "x", Name: "X", Harness: "codex"}, nil), CodeBadRequest)
	requireRPCError(t, call(t, home, "crew.add", CrewAddArgs{ID: "x", Name: "X", Harness: "holodeck"}, nil), CodeBadRequest)
	var tuvok CrewView
	require.NoError(t, call(t, home, "crew.add", CrewAddArgs{ID: "tuvok", Name: "Tuvok", Harness: "echo", Model: "model-crew"}, &tuvok))
	require.Equal(t, [4]string{"echo", SourceCrew, "model-crew", SourceCrew},
		[4]string{tuvok.Harness, tuvok.HarnessSource, tuvok.Model, tuvok.ModelSource})
	require.NoError(t, call(t, home, "crew.list", nil, &crew))
	require.Len(t, crew.Crew, 2)
	require.Equal(t, "sorell", crew.Crew[0].ID)
	require.Equal(t, "model-crew", crew.Crew[1].Model)

	// order.create: an assignee, if any, is a known crew member; a pool
	// order needs a division; a priority is 0-2.
	re := requireRPCError(t, call(t, home, "order.create", OrderCreateArgs{Title: "t"}, nil), CodeBadRequest)
	require.Equal(t, "order assignee or division is required", re.Message)
	re = requireRPCError(t, call(t, home, "order.create", OrderCreateArgs{Title: "t", Division: "ops", Priority: 3}, nil), CodeBadRequest)
	require.Equal(t, "order priority 3 out of range (0 routine, 1 yellow, 2 red)", re.Message)
	requireRPCError(t, call(t, home, "order.create", OrderCreateArgs{Title: "t", Division: "ops", Priority: -1}, nil), CodeBadRequest)
	requireRPCError(t, call(t, home, "order.create", OrderCreateArgs{Assignee: "sorell"}, nil), CodeBadRequest)
	requireRPCError(t, call(t, home, "order.create", OrderCreateArgs{Title: " \t", Assignee: "sorell"}, nil), CodeBadRequest)
	re = requireRPCError(t, call(t, home, "order.create", OrderCreateArgs{Title: "t", Assignee: "nobody"}, nil), CodeNotFound)
	require.Contains(t, re.Message, "nobody")
	var created OrderCreateResult
	// Repos: exactly one, registered (startDaemon registers app).
	re = requireRPCError(t, call(t, home, "order.create", OrderCreateArgs{Title: "t", Assignee: "sorell",
		Repos: []OrderRepo{{Name: "app", BaseRef: "main"}, {Name: "app", BaseRef: "dev"}}}, nil), CodeBadRequest)
	require.Equal(t, "an order takes exactly one repo in Phase 1, got 2", re.Message)
	re = requireRPCError(t, call(t, home, "order.create", OrderCreateArgs{Title: "t", Assignee: "sorell",
		Repos: []OrderRepo{{Name: "web", BaseRef: "main"}}}, nil), CodeBadRequest)
	require.Equal(t, `unknown repo "web" (repos: app)`, re.Message)
	requireRPCError(t, call(t, home, "order.create", OrderCreateArgs{Title: "t", Assignee: "sorell"}, nil), CodeBadRequest)
	re = requireRPCError(t, call(t, home, "order.create", OrderCreateArgs{Title: "t", Assignee: "sorell",
		Repos: []OrderRepo{{Name: "app", BaseRef: "main"}}, BudgetTurns: -1}, nil), CodeBadRequest)
	require.Equal(t, "budget_turns must not be negative", re.Message)
	require.NoError(t, call(t, home, "order.create", OrderCreateArgs{Title: "survey", Body: "look around", Division: "science",
		Assignee: "sorell", Priority: store.PriorityYellow, Repos: []OrderRepo{{Name: "app", BaseRef: "main"}}, BudgetTokens: 1000,
		BudgetTurns: 40, Model: "model-x"}, &created))
	require.Equal(t, "ORD-0001", created.ID)
	stored, err := r.d.store.GetOrder(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, [2]int64{1000, 40}, [2]int64{stored.BudgetTokens, stored.BudgetTurns})
	var orders OrderListResult
	require.NoError(t, call(t, home, "order.list", nil, &orders))
	require.Len(t, orders.Orders, 1)
	o := orders.Orders[0]
	require.Equal(t, OrderView{ID: "ORD-0001", Title: "survey", Body: "look around", Division: "science", State: o.State,
		Priority: store.PriorityYellow, Assignee: "sorell", Repos: []OrderRepo{{Name: "app", BaseRef: "main"}}, BudgetTokens: 1000,
		CreatedBy: "captain", Model: "model-x"}, o)
	// The tour claims it and starts its mission.
	requirePending(t, r.d, missionWorkflowID(created.ID))

	// The log is still being written in the background: each tour logs its
	// beam as created and then as up, then its combadge's hello (#173), and
	// the mission's ship-repos step logs the repo it ships. Wait for all of
	// that, so the "nothing new" check below can't see an entry landing
	// between two log.tail calls. The mission then sends its order, which
	// the fake beam discards, and waits, logging nothing for minutes. The
	// first read of the standing orders switch logs its
	// roster default: make it here rather than in the background.
	_, err = r.d.StandingOrders(t.Context())
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, shipped := stepOutput(c, r.d, missionWorkflowID(created.ID), "ship-repos")
		require.True(c, shipped, "ship-repos not recorded yet")
		entries, err := r.d.store.ListLog(context.Background(), 0)
		require.NoError(c, err)
		up := map[string]bool{}
		for _, e := range entries {
			// The tour's hello entry, not a beam entry: provisionBeam logs
			// "created" and "up" first, and the hello lands after them
			// (#184, #173).
			if e.Kind == logCrew && e.Text == "crew on deck: combadge said hello" {
				up[e.CrewID] = true
			}
		}
		require.Equal(c, map[string]bool{"sorell": true, "tuvok": true}, up)
	}, 20*time.Second, 20*time.Millisecond)

	// log.tail pages through the log from since_id.
	var tail LogTailResult
	require.NoError(t, call(t, home, "log.tail", LogTailArgs{}, &tail))
	require.NotEmpty(t, tail.Entries)
	require.Equal(t, tail.Entries[len(tail.Entries)-1].ID, tail.LastID)
	var texts []string
	for _, e := range tail.Entries {
		texts = append(texts, e.Text)
	}
	require.Contains(t, texts, "order created: survey")
	var page LogTailResult
	require.NoError(t, call(t, home, "log.tail", LogTailArgs{Limit: 1}, &page))
	require.Len(t, page.Entries, 1)
	require.Equal(t, tail.Entries[0], page.Entries[0])
	require.NoError(t, call(t, home, "log.tail", LogTailArgs{SinceID: tail.LastID}, &page))
	require.Empty(t, page.Entries)
	require.Equal(t, tail.LastID, page.LastID, "last_id stays put with nothing new")
	requireRPCError(t, call(t, home, "log.tail", LogTailArgs{SinceID: -1}, nil), CodeBadRequest)

	// hails.list and hails.answer, on a hail recorded for the running
	// mission (the fake beam never hails on its own).
	var hails HailsListResult
	require.NoError(t, call(t, home, "hails.list", nil, &hails))
	require.Empty(t, hails.Hails)
	hailID, err := r.d.store.InsertHail(t.Context(), store.Hail{OrderID: created.ID, CrewID: "sorell",
		Category: "tool_approval", Question: "Push?", Options: []string{"allow", "deny"}}, testEntry)
	require.NoError(t, err)
	require.NoError(t, call(t, home, "hails.list", HailsListArgs{Status: store.HailOpen}, &hails))
	require.Len(t, hails.Hails, 1)
	require.Equal(t, hailID, hails.Hails[0].ID)
	require.Equal(t, []string{"allow", "deny"}, hails.Hails[0].Options)
	require.NotNil(t, hails.Hails[0].CreatedAt)
	require.Nil(t, hails.Hails[0].DecidedAt, "omitted until decided")
	require.Empty(t, hails.Hails[0].OriginalDecidedBy)
	requireRPCError(t, call(t, home, "hails.list", HailsListArgs{Status: "bogus"}, nil), CodeBadRequest)

	// hails.metrics (#156) over the last week by default: the open hail
	// counts in the total; with nothing decided the rates and times are
	// null, not 0 or NaN.
	var raw json.RawMessage
	require.NoError(t, call(t, home, "hails.metrics", nil, &raw))
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.Equal(t, map[string]any{"policy": 0.0, "command": 0.0, "captain": 0.0}, fields["by_original_decider"])
	require.InDelta(t, 1, fields["total"], 0)
	require.InDelta(t, 0, fields["decided"], 0)
	for _, k := range []string{"auto_resolved_share", "captain_overrides", "median_time_to_answer_ms", "p90_time_to_answer_ms"} {
		v, ok := fields[k]
		require.True(t, ok, "%s present", k)
		require.Nil(t, v, k)
	}
	var metrics HailsMetricsResult
	require.NoError(t, json.Unmarshal(raw, &metrics))
	require.WithinDuration(t, time.Now().Add(-DefaultMetricsPeriod), metrics.Since, time.Minute)
	require.Equal(t, time.UTC, metrics.Since.Location())

	five := 5
	requireRPCError(t, call(t, home, "hails.answer", HailsAnswerArgs{HailID: hailID, Option: &five}, nil), CodeBadRequest)
	requireRPCError(t, call(t, home, "hails.answer", HailsAnswerArgs{HailID: hailID}, nil), CodeBadRequest)
	requireRPCError(t, call(t, home, "hails.answer", HailsAnswerArgs{HailID: "H-9999", Decision: "yes"}, nil), CodeNotFound)
	zero := 0
	var answered HailView
	require.NoError(t, call(t, home, "hails.answer", HailsAnswerArgs{HailID: hailID, Option: &zero}, &answered))
	require.Equal(t, store.HailDecided, answered.Status)
	require.Equal(t, "allow", answered.Decision)
	require.Equal(t, "captain", answered.DecidedBy)
	require.Equal(t, "captain", answered.OriginalDecidedBy)
	require.NotNil(t, answered.DecidedAt)
	require.False(t, answered.DecidedAt.Before(*answered.CreatedAt))
	require.Equal(t, time.UTC, answered.DecidedAt.Location())
	requireRPCError(t, call(t, home, "hails.answer", HailsAnswerArgs{HailID: hailID, Decision: "deny"}, nil), CodeHailClosed)
	require.NoError(t, call(t, home, "hails.list", HailsListArgs{Status: store.HailOpen}, &hails))
	require.Empty(t, hails.Hails)
	require.NoError(t, call(t, home, "hails.list", nil, &hails))
	require.Len(t, hails.Hails, 1)

	// The captain's answer: decided, none auto-resolved or overridden.
	since := answered.CreatedAt.Add(-time.Second)
	require.NoError(t, call(t, home, "hails.metrics", HailsMetricsArgs{Since: &since}, &metrics))
	ms := answered.DecidedAt.Sub(*answered.CreatedAt).Milliseconds()
	zeroRate := 0.0
	require.Equal(t, HailsMetricsResult{Since: since.UTC(), Total: 1, Decided: 1,
		ByOriginalDecider: map[string]int{"policy": 0, "command": 0, "captain": 1},
		AutoResolvedShare: &zeroRate, CaptainOverrides: &zeroRate, MedianTimeToAnswerMS: &ms, P90TimeToAnswerMS: &ms}, metrics)
	after := answered.CreatedAt.Add(time.Second)
	require.NoError(t, call(t, home, "hails.metrics", HailsMetricsArgs{Since: &after}, &metrics))
	require.Zero(t, metrics.Total)
}

func TestRPCBadRequests(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemon(t, home)
	defer r.stop(t)

	for name, tc := range map[string]struct {
		req      string
		code     string
		contains string
	}{
		"not json":       {"not json\n", CodeBadRequest, "malformed request"},
		"two objects":    {`{"cmd":"crew.list"} {"cmd":"crew.list"}` + "\n", CodeBadRequest, "one JSON object"},
		"unknown field":  {`{"cmd":"crew.list","extra":1}` + "\n", CodeBadRequest, "malformed request"},
		"unknown cmd":    {`{"cmd":"crew.dance"}` + "\n", CodeUnknownCmd, `"crew.dance"`},
		"unknown arg":    {`{"cmd":"crew.list","args":{"bogus":1}}` + "\n", CodeBadRequest, "bogus"},
		"bad arg type":   {`{"cmd":"log.tail","args":{"since_id":"x"}}` + "\n", CodeBadRequest, "bad args"},
		"empty":          {"", CodeBadRequest, "empty request"},
		"no newline":     {`{"cmd":"crew.list"}`, "", ""},
		"oversize":       {`{"cmd":"crew.list","args":{"x":"` + strings.Repeat("a", MaxRPCRequest) + `"}}` + "\n", CodeBadRequest, "larger than"},
		"oversize no nl": {strings.Repeat(" ", MaxRPCRequest+1), CodeBadRequest, "larger than"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := rawCall(t, home, []byte(tc.req))
			if tc.code == "" {
				require.True(t, resp.OK, resp.Error)
				return
			}
			require.False(t, resp.OK)
			require.Equal(t, tc.code, resp.Code, resp.Error)
			require.Contains(t, resp.Error, tc.contains)
		})
	}

	// The daemon still serves after all that.
	require.NoError(t, call(t, home, "crew.list", nil, nil))
}

func TestRPCSocketLifecycle(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "h")
	sock := filepath.Join(home, SocketFile)
	require.ErrorContains(t, Call(t.Context(), "/"+strings.Repeat("x", 200), "crew.list", nil, nil), "too long")

	// No daemon: a clear error.
	require.ErrorIs(t, call(t, home, "crew.list", nil, nil), ErrBridgeNotRunning)

	// The daemon creates its home private and the socket owner-only.
	r := startDaemon(t, home)
	require.NoError(t, call(t, home, "crew.list", nil, nil))
	fi, err := os.Stat(home)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	fi, err = os.Stat(sock)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	// A stopped daemon removes it.
	r.stop(t)
	_, err = os.Stat(sock)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorIs(t, call(t, home, "crew.list", nil, nil), ErrBridgeNotRunning)

	// A killed bridge leaves its socket file behind: still "not running",
	// and the next daemon replaces it.
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, ln.Close())
	_, err = os.Stat(sock)
	require.NoError(t, err, "stale socket left behind")
	require.ErrorIs(t, call(t, home, "crew.list", nil, nil), ErrBridgeNotRunning)
	r = startDaemon(t, home)
	require.NoError(t, call(t, home, "crew.list", nil, nil))
	r.stop(t)
}

// An existing home open to others is refused and left as it is (#40);
// anything but a socket at bridge.sock is refused, not removed.
func TestRPCHomeAndSocketChecks(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "h")
	require.NoError(t, os.Mkdir(home, 0o755))
	require.NoError(t, os.Chmod(home, 0o755))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := Run(ctx, testConfig(home))
	require.ErrorContains(t, err, "chmod 700 "+home)
	fi, err := os.Stat(home)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), fi.Mode().Perm(), "left alone")

	require.NoError(t, os.Chmod(home, 0o700))
	r := startDaemon(t, home)
	r.stop(t)

	sock := filepath.Join(home, SocketFile)
	require.NoError(t, os.WriteFile(sock, []byte("not a socket"), 0o600))
	cfg := testConfig(home)
	err = Run(t.Context(), cfg)
	require.ErrorContains(t, err, "is not a socket")
	b, err := os.ReadFile(sock)
	require.NoError(t, err)
	require.Equal(t, "not a socket", string(b), "left alone")

	sticky := filepath.Join(t.TempDir(), "shared")
	require.NoError(t, os.Mkdir(sticky, 0o700))
	require.NoError(t, os.Chmod(sticky, 0o700|os.ModeSticky))
	require.ErrorContains(t, Run(t.Context(), testConfig(sticky)), "sticky")
}

// Every error a client sees while the daemon shuts down says the bridge is
// down (IsBridgeDown), never a bad request or an opaque transport error, so
// `log tail --follow` can wait for it to come back.
func TestRPCShutdownErrorsAreDown(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemon(t, home)
	sock := filepath.Join(home, SocketFile)

	var (
		mu   sync.Mutex
		errs = map[string]int{}
		oks  int
		quit = make(chan struct{})
		wg   sync.WaitGroup
	)
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-quit:
					return
				default:
				}
				err := Call(context.Background(), sock, "log.tail", LogTailArgs{}, nil)
				mu.Lock()
				if err == nil {
					oks++
				} else {
					errs[err.Error()]++
					if !IsBridgeDown(err) {
						t.Errorf("not a bridge-down error: %v", err)
					}
				}
				mu.Unlock()
			}
		})
	}
	// A client that connects and sends nothing is told the bridge is
	// shutting down when Close cuts its read short.
	idle, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer idle.Close()
	time.Sleep(200 * time.Millisecond)
	r.stop(t)
	time.Sleep(100 * time.Millisecond)
	close(quit)
	wg.Wait()
	b, err := io.ReadAll(idle)
	require.NoError(t, err)
	var resp Response
	require.NoError(t, json.Unmarshal(b, &resp))
	require.Equal(t, CodeShutdown, resp.Code, resp.Error)
	require.Positive(t, oks)
	require.Positive(t, errs[ErrBridgeNotRunning.Error()], "%v", errs)
	t.Logf("ok=%d errors=%v", oks, errs)
}

// lateListener forces the late-connection order of #40: Close's sweep of
// s.conns runs while the map is still empty, and only then does accept get
// the connection, see closed, and set its read deadline to now. Before the
// fix, serveConn then overwrote that deadline with its own 10s one.
type lateListener struct {
	net.Listener
	release chan struct{} // closed by Close
	once    sync.Once
	first   bool // only accept's goroutine touches it
}

// Accept waits for Close, gives Close's sweep time to finish, and then
// returns the queued connection. Later calls close the real listener.
func (l *lateListener) Accept() (net.Conn, error) {
	<-l.release
	if l.first {
		l.first = false
		time.Sleep(100 * time.Millisecond)
		return l.Listener.Accept()
	}
	l.Listener.Close()
	return nil, net.ErrClosed
}

func (l *lateListener) Close() error {
	l.once.Do(func() { close(l.release) })
	return nil
}

// A connection accepted while Close runs doesn't hold shutdown for
// rpcConnTimeout: Close's deadline cuts its read short.
func TestRPCLateConnectionDoesNotHoldClose(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), SocketFile)
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	late := &lateListener{Listener: ln, release: make(chan struct{}), first: true}
	s := &rpcServer{ctx: t.Context(), ln: late, path: sock, logger: slog.New(slog.DiscardHandler),
		sem: make(chan struct{}, maxRPCConns), conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.accept()

	// Queued in the backlog; it sends nothing.
	idle, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer idle.Close()

	start := time.Now()
	require.NoError(t, s.Close())
	require.Less(t, time.Since(start), 500*time.Millisecond)
	b, err := io.ReadAll(idle)
	require.NoError(t, err)
	var resp Response
	require.NoError(t, json.Unmarshal(b, &resp))
	require.Equal(t, CodeShutdown, resp.Code, resp.Error)
}

// Past maxRPCConns connections in flight, new ones are refused at once.
func TestRPCConnectionLimit(t *testing.T) {
	t.Parallel()
	home := privateHome(t)
	r := startDaemon(t, home)
	defer r.stop(t)
	sock := filepath.Join(home, SocketFile)
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for range maxRPCConns {
		c, err := net.Dial("unix", sock)
		require.NoError(t, err)
		held = append(held, c)
	}
	// Let the server pick them all up.
	require.Eventually(t, func() bool {
		re := requireRPCErrorOrNil(call(t, home, "crew.list", nil, nil))
		return re != nil && re.Code == CodeUnavailable
	}, 5*time.Second, 20*time.Millisecond)
	held[0].Close()
	held = held[1:]
	require.Eventually(t, func() bool { return call(t, home, "crew.list", nil, nil) == nil },
		5*time.Second, 20*time.Millisecond, "a slot frees up")
}

func requireRPCErrorOrNil(err error) *RPCError {
	var re *RPCError
	if errors.As(err, &re) {
		return re
	}
	return nil
}
