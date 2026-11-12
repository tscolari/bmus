package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tscolari/bmus/internal/store"
)

// actionCall is one Actions call a testSource saw.
type actionCall struct {
	Action string          `json:"action"`
	ID     string          `json:"id,omitempty"`
	Args   json.RawMessage `json:"args"`
}

// act records an Actions call and, unless actErr is set, logs it and
// returns it as its result.
func (ts *testSource) act(ctx context.Context, action, id string, args json.RawMessage) (any, error) {
	ts.mu.Lock()
	call := actionCall{Action: action, ID: id, Args: args}
	ts.actions = append(ts.actions, call)
	err := ts.actErr
	ts.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if _, err := ts.st.AppendLog(ctx, store.LogEntry{Kind: "test", Text: action}); err != nil {
		return nil, err
	}
	return call, nil
}

func (ts *testSource) CreateOrder(ctx context.Context, args json.RawMessage) (any, error) {
	return ts.act(ctx, "create_order", "", args)
}

func (ts *testSource) AnswerHail(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return ts.act(ctx, "answer_hail", id, args)
}

func (ts *testSource) CountermandHail(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return ts.act(ctx, "countermand_hail", id, args)
}

func (ts *testSource) AddCrew(ctx context.Context, args json.RawMessage) (any, error) {
	return ts.act(ctx, "add_crew", "", args)
}

func (ts *testSource) BelayOrder(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return ts.act(ctx, "belay_order", id, args)
}

func (ts *testSource) RecallOrder(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return ts.act(ctx, "recall_order", id, args)
}

func (ts *testSource) WithdrawOrder(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return ts.act(ctx, "withdraw_order", id, args)
}

func (ts *testSource) RouteOrder(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return ts.act(ctx, "route_order", id, args)
}

func (ts *testSource) ReenergizeCrew(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return ts.act(ctx, "reenergize_crew", id, args)
}

func (ts *testSource) SetStandingOrders(ctx context.Context, args json.RawMessage) (any, error) {
	return ts.act(ctx, "set_standing_orders", "", args)
}

func (ts *testSource) PushOrder(ctx context.Context, id string, args json.RawMessage) (any, error) {
	return ts.act(ctx, "push_order", id, args)
}

// testCodedError is an Actions error with an RPC code.
type testCodedError struct{ code, msg string }

func (e testCodedError) Error() string { return e.msg }
func (e testCodedError) Code() string  { return e.code }

type actionEndpoint struct {
	method, path, action, id string
	status                   int
	// what is the action's name in a 500's message.
	what string
}

var actionEndpoints = []actionEndpoint{
	{http.MethodPost, "/api/orders", "create_order", "", http.StatusCreated, "create order"},
	{http.MethodPost, "/api/hails/H-0001/answer", "answer_hail", "H-0001", http.StatusOK, "answer hail"},
	{http.MethodPost, "/api/hails/H-0001/countermand", "countermand_hail", "H-0001", http.StatusOK, "countermand hail"},
	{http.MethodPost, "/api/crew", "add_crew", "", http.StatusCreated, "add crew"},
	{http.MethodPost, "/api/orders/ORD-0001/belay", "belay_order", "ORD-0001", http.StatusOK, "belay order"},
	{http.MethodPost, "/api/orders/ORD-0001/recall", "recall_order", "ORD-0001", http.StatusOK, "recall order"},
	{http.MethodPost, "/api/orders/ORD-0001/withdraw", "withdraw_order", "ORD-0001", http.StatusOK, "withdraw order"},
	{http.MethodPost, "/api/orders/ORD-0001/route", "route_order", "ORD-0001", http.StatusOK, "route order"},
	{http.MethodPost, "/api/orders/ORD-0001/push", "push_order", "ORD-0001", http.StatusOK, "push order"},
	{http.MethodPost, "/api/crew/sorell/reenergize", "reenergize_crew", "sorell", http.StatusOK, "re-energize crew"},
	{http.MethodPut, "/api/standing-orders", "set_standing_orders", "", http.StatusOK, "set standing orders"},
}

// post issues a POST of body to path with the session cookie, the
// console's Origin, csrf (unless empty) and contentType (unless empty).
func (fs *feedServer) post(t *testing.T, path, contentType, csrf, body string) *http.Response {
	t.Helper()
	return fs.send(t, http.MethodPost, path, contentType, csrf, body)
}

// send is post with another method.
func (fs *feedServer) send(t *testing.T, method, path, contentType, csrf, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+fs.s.Addr()+path, strings.NewReader(body))
	require.NoError(t, err)
	req.AddCookie(fs.cookie)
	req.Header.Set("Origin", "http://"+fs.s.Addr())
	if csrf != "" {
		req.Header.Set(csrfHeader, csrf)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (fs *feedServer) postJSON(t *testing.T, path, body string) *http.Response {
	t.Helper()
	return fs.post(t, path, "application/json", fs.csrf, body)
}

// call sends body to ep as JSON, as postJSON does.
func (fs *feedServer) call(t *testing.T, ep actionEndpoint, body string) *http.Response {
	t.Helper()
	return fs.send(t, ep.method, ep.path, "application/json", fs.csrf, body)
}

func TestActionsSuccess(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	for _, ep := range actionEndpoints {
		t.Run(ep.action, func(t *testing.T) {
			body := `{"title":"survey","option":1}`
			resp := fs.call(t, ep, body)
			require.Equal(t, ep.status, resp.StatusCode)
			require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
			require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
			var got actionCall
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
			require.Equal(t, actionCall{Action: ep.action, ID: ep.id, Args: json.RawMessage(body)}, got)
		})
	}
	// A charset parameter is fine, and surrounding space is trimmed.
	resp := fs.post(t, "/api/crew", "application/json; charset=utf-8", fs.csrf, "  {\"id\":\"x\"}\n")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var got actionCall
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.JSONEq(t, `{"id":"x"}`, string(got.Args))
}

// Each RPC code an action returns maps to its HTTP status, with the
// error's message; an error without a code is a 500 that hides it.
func TestActionsErrorCodes(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	cases := []struct {
		code   string
		status int
	}{
		{CodeBadRequest, http.StatusBadRequest},
		{CodeNotFound, http.StatusNotFound},
		{CodeExists, http.StatusConflict},
		{CodeHailClosed, http.StatusConflict},
		{CodeUnavailable, http.StatusConflict},
		{CodeTimeout, http.StatusGatewayTimeout},
	}
	for _, ep := range actionEndpoints {
		what := ep.what
		for _, c := range cases {
			t.Run(ep.action+"/"+c.code, func(t *testing.T) {
				fs.src.mu.Lock()
				fs.src.actErr = testCodedError{code: c.code, msg: "boom: " + c.code}
				fs.src.mu.Unlock()
				resp := fs.call(t, ep, `{}`)
				require.Equal(t, c.status, resp.StatusCode)
				require.Equal(t, apiErrorBody{Error: apiError{Code: c.code, Message: "boom: " + c.code}}, decodeAPIError(t, resp))
			})
		}
		// conflict sends a fixed message: the error's wraps git's output.
		t.Run(ep.action+"/conflict", func(t *testing.T) {
			fs.src.mu.Lock()
			fs.src.actErr = testCodedError{code: CodeConflict, msg: "push rejected: secret " + ep.action}
			fs.src.mu.Unlock()
			resp := fs.call(t, ep, `{}`)
			require.Equal(t, http.StatusConflict, resp.StatusCode)
			require.Equal(t, apiErrorBody{Error: apiError{Code: CodeConflict, Message: what + " failed: conflict (details in the bridge's log)"}},
				decodeAPIError(t, resp))
			require.Contains(t, fs.logs.String(), "push rejected: secret "+ep.action)
		})
		// shutting_down sends a fixed message; the error's is logged.
		t.Run(ep.action+"/shutting_down", func(t *testing.T) {
			fs.src.mu.Lock()
			fs.src.actErr = testCodedError{code: CodeShutdown, msg: "bridge is shutting down: secret " + ep.action}
			fs.src.mu.Unlock()
			resp := fs.call(t, ep, `{}`)
			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			require.Equal(t, apiErrorBody{Error: apiError{Code: CodeShutdown, Message: "bridge is shutting down"}}, decodeAPIError(t, resp))
			require.Contains(t, fs.logs.String(), "bridge is shutting down: secret "+ep.action)
		})
		// A 500 (internal, a code with no status, or no code at all)
		// sends "<action> failed", never the error's own message.
		for name, err := range map[string]error{
			"internal": testCodedError{code: CodeInternal, msg: "secret detail"},
			"unknown":  testCodedError{code: "some_new_code", msg: "secret detail"},
			"uncoded":  errors.New("secret detail"),
		} {
			t.Run(ep.action+"/"+name, func(t *testing.T) {
				fs.src.mu.Lock()
				fs.src.actErr = err
				fs.src.mu.Unlock()
				resp := fs.call(t, ep, `{}`)
				require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
				require.Equal(t, apiErrorBody{Error: apiError{Code: CodeInternal, Message: what + " failed"}}, decodeAPIError(t, resp))
				require.Contains(t, fs.logs.String(), "secret detail")
			})
		}
	}
	// A wrapped coded error keeps its code.
	fs.src.mu.Lock()
	fs.src.actErr = errors.Join(testCodedError{code: CodeNotFound, msg: "hail H-9: not found"})
	fs.src.mu.Unlock()
	resp := fs.postJSON(t, "/api/hails/H-9/answer", `{}`)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// A body that isn't one application/json object, at most 64KB, never
// reaches the Source.
func TestActionsBodyRules(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	big := `{"title":"` + strings.Repeat("x", maxActionBody) + `"}`
	cases := []struct {
		name, contentType, body string
		status                  int
	}{
		{"no Content-Type", "", `{}`, http.StatusUnsupportedMediaType},
		{"text/plain", "text/plain", `{}`, http.StatusUnsupportedMediaType},
		{"form", "application/x-www-form-urlencoded", `a=1`, http.StatusUnsupportedMediaType},
		{"bad media type", "application/", `{}`, http.StatusUnsupportedMediaType},
		{"empty", "application/json", ``, http.StatusBadRequest},
		{"malformed", "application/json", `{"title":`, http.StatusBadRequest},
		{"array", "application/json", `[{}]`, http.StatusBadRequest},
		{"null", "application/json", `null`, http.StatusBadRequest},
		{"string", "application/json", `"x"`, http.StatusBadRequest},
		{"two objects", "application/json", `{} {}`, http.StatusBadRequest},
		{"too large", "application/json", big, http.StatusRequestEntityTooLarge},
	}
	for _, ep := range actionEndpoints {
		for _, c := range cases {
			t.Run(ep.action+"/"+c.name, func(t *testing.T) {
				resp := fs.send(t, ep.method, ep.path, c.contentType, fs.csrf, c.body)
				require.Equal(t, c.status, resp.StatusCode)
				require.Equal(t, CodeBadRequest, decodeAPIError(t, resp).Error.Code)
			})
		}
	}
	fs.src.mu.Lock()
	defer fs.src.mu.Unlock()
	require.Empty(t, fs.src.actions)
}

// Every action sits behind the session, Origin and CSRF checks.
func TestActionsProtected(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	addr := fs.s.Addr()
	cases := []struct {
		name         string
		cookie       bool
		origin, csrf string
		status       int
		code         string
	}{
		{"no session", false, "http://" + addr, fs.csrf, http.StatusUnauthorized, CodeUnauthorized},
		{"no Origin", true, "", fs.csrf, http.StatusForbidden, CodeForbidden},
		{"foreign Origin", true, "http://evil.example", fs.csrf, http.StatusForbidden, CodeForbidden},
		{"no CSRF", true, "http://" + addr, "", http.StatusForbidden, CodeForbidden},
		{"wrong CSRF", true, "http://" + addr, "wrong", http.StatusForbidden, CodeForbidden},
	}
	for _, ep := range actionEndpoints {
		for _, c := range cases {
			t.Run(ep.action+"/"+c.name, func(t *testing.T) {
				req, err := http.NewRequest(ep.method, "http://"+addr+ep.path, strings.NewReader(`{}`))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				if c.cookie {
					req.AddCookie(fs.cookie)
				}
				if c.origin != "" {
					req.Header.Set("Origin", c.origin)
				}
				if c.csrf != "" {
					req.Header.Set(csrfHeader, c.csrf)
				}
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, c.status, resp.StatusCode)
				require.Equal(t, c.code, decodeAPIError(t, resp).Error.Code)
			})
		}
	}
	fs.src.mu.Lock()
	defer fs.src.mu.Unlock()
	require.Empty(t, fs.src.actions)
}

func TestActionsWithoutSource(t *testing.T) {
	s, token := newTestServer(t)
	cookie, csrf := newSessionFor(t, s, token)
	for _, ep := range actionEndpoints {
		req, err := http.NewRequest(ep.method, "http://"+s.Addr()+ep.path, strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://"+s.Addr())
		req.Header.Set(csrfHeader, csrf)
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, ep.path)
		require.Equal(t, CodeUnavailable, decodeAPIError(t, resp).Error.Code, ep.path)
	}
}

// An action returns once the Source call does; the feed then carries its
// log entry.
func TestActionLoggedOnStream(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	ss := fs.openStream(t, "")
	requireSnapshot(t, ss.next(t), 0)
	resp := fs.postJSON(t, "/api/orders", `{"title":"survey"}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	requireLog(t, ss.next(t), 1, "create_order")
}

// GET /api/session returns the session's CSRF token, for a reloaded page.
func TestGetSession(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	addr := fs.s.Addr()

	resp := fs.get(t, "/api/session", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	var sr sessionResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sr))
	require.Equal(t, fs.csrf, sr.CSRF)

	// Same-origin, the browser sends its Origin on a fetch.
	resp = fs.get(t, "/api/session", map[string]string{"Origin": "http://" + addr})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Another session gets its own token.
	cookie, csrf := newSessionFor(t, fs.s, fs.token)
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/session", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&sr))
	require.Equal(t, csrf, sr.CSRF)
	require.NotEqual(t, fs.csrf, csrf)

	t.Run("no session", func(t *testing.T) {
		resp, err := http.Get("http://" + addr + "/api/session")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		require.Equal(t, CodeUnauthorized, decodeAPIError(t, resp).Error.Code)
	})
	t.Run("stale cookie", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/session", nil)
		require.NoError(t, err)
		req.AddCookie(&http.Cookie{Name: fs.cookie.Name, Value: "stale"})
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
	t.Run("foreign Origin", func(t *testing.T) {
		resp := fs.get(t, "/api/session", map[string]string{"Origin": "http://evil.example"})
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	})
	t.Run("foreign Host", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/session", nil)
		require.NoError(t, err)
		req.Host = "evil.example:" + strings.Split(addr, ":")[1]
		req.AddCookie(fs.cookie)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

// The #78 endpoints answer only their own method; another gets the 404
// every unknown /api/ route does.
func TestActionsWrongMethod(t *testing.T) {
	fs := newFeedServer(t, 0, nil)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/standing-orders"},
		{http.MethodGet, "/api/standing-orders"},
		{http.MethodPut, "/api/orders/ORD-0001/belay"},
		{http.MethodGet, "/api/crew/sorell/reenergize"},
		{http.MethodPost, "/api/orders/ORD-0001/dismiss"},
	} {
		resp := fs.send(t, c.method, c.path, "application/json", fs.csrf, `{}`)
		require.Equal(t, http.StatusNotFound, resp.StatusCode, c)
	}
	fs.src.mu.Lock()
	defer fs.src.mu.Unlock()
	require.Empty(t, fs.src.actions)
}
