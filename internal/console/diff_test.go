package console

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// testDiff is the diff testSource.OrderDiff returns.
type testDiff struct {
	ID    string `json:"id"`
	Repos []struct {
		Name string `json:"name"`
		Diff string `json:"diff"`
	} `json:"repos"`
}

func (ts *testSource) OrderDiff(_ context.Context, id string) (any, error) {
	ts.mu.Lock()
	err := ts.diffErr
	ts.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "repos": []map[string]any{{"name": "app", "diff": "diff --git a/x b/x\n"}}}, nil
}

// GET /api/orders/{id}/diff (#104): 200 and the source's diff, with the
// API's headers.
func TestOrderDiff(t *testing.T) {
	t.Parallel()
	fs := newFeedServer(t, 0, nil)
	resp := fs.get(t, "/api/orders/ORD-0007/diff", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	require.Equal(t, "default-src 'self'; connect-src 'self'", resp.Header.Get("Content-Security-Policy"))
	var got testDiff
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "ORD-0007", got.ID)
	require.Len(t, got.Repos, 1)
	require.Equal(t, "diff --git a/x b/x\n", got.Repos[0].Diff)
}

// Without a session the diff is a 401, and the source is never asked.
func TestOrderDiffNoSession(t *testing.T) {
	t.Parallel()
	fs := newFeedServer(t, 0, nil)
	fs.src.mu.Lock()
	fs.src.diffErr = testCodedError{code: CodeInternal, msg: "must not be called"}
	fs.src.mu.Unlock()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+fs.s.Addr()+"/api/orders/ORD-0007/diff", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, CodeUnauthorized, decodeAPIError(t, resp).Error.Code)
	require.NotContains(t, fs.logs.String(), "must not be called")
}

// The diff's errors map as an action's: an order that isn't done is a
// 400, an unknown one a 404, a diff past its time a 504, and an uncoded
// error a 500 that hides its message.
func TestOrderDiffErrors(t *testing.T) {
	t.Parallel()
	fs := newFeedServer(t, 0, nil)
	for _, c := range []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{testCodedError{code: CodeBadRequest, msg: "order ORD-0007 is away"}, http.StatusBadRequest, CodeBadRequest, "order ORD-0007 is away"},
		{testCodedError{code: CodeNotFound, msg: "order ORD-0007: not found"}, http.StatusNotFound, CodeNotFound, "order ORD-0007: not found"},
		{testCodedError{code: CodeTimeout, msg: "diff timed out after 30s"}, http.StatusGatewayTimeout, CodeTimeout, "diff timed out after 30s"},
		{testCodedError{code: CodeInternal, msg: "secret detail"}, http.StatusInternalServerError, CodeInternal, "diff order failed"},
	} {
		t.Run(c.code, func(t *testing.T) {
			fs.src.mu.Lock()
			fs.src.diffErr = c.err
			fs.src.mu.Unlock()
			resp := fs.get(t, "/api/orders/ORD-0007/diff", nil)
			require.Equal(t, c.status, resp.StatusCode)
			require.Equal(t, apiErrorBody{Error: apiError{Code: c.code, Message: c.message}}, decodeAPIError(t, resp))
		})
	}
}
