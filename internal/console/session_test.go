package console

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// postSession posts body to s's /api/session, with host and origin set on
// the request (empty means omit the header / use the real Host).
func postSession(t *testing.T, s *Server, body, host, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/api/session", bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeAPIError(t *testing.T, resp *http.Response) apiErrorBody {
	t.Helper()
	var body apiErrorBody
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

func TestSessionExchange(t *testing.T) {
	s, token := newTestServer(t)
	host := s.Addr()
	origin := "http://" + host

	t.Run("valid token, correct Host and Origin", func(t *testing.T) {
		body, err := json.Marshal(sessionRequest{Token: token})
		require.NoError(t, err)
		resp := postSession(t, s, string(body), host, origin)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var cookie *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == s.cookieName {
				cookie = c
			}
		}
		require.NotNil(t, cookie, "session cookie not set")
		_, port, err := net.SplitHostPort(s.Addr())
		require.NoError(t, err)
		require.Equal(t, "bmus_console_"+port, cookie.Name)
		require.True(t, cookie.HttpOnly)
		require.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
		require.Equal(t, "/", cookie.Path)

		var sr sessionResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&sr))
		require.NotEmpty(t, sr.CSRF)
	})

	t.Run("empty token", func(t *testing.T) {
		body, err := json.Marshal(sessionRequest{Token: ""})
		require.NoError(t, err)
		resp := postSession(t, s, string(body), host, origin)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		require.Equal(t, CodeUnauthorized, decodeAPIError(t, resp).Error.Code)
	})

	t.Run("wrong token", func(t *testing.T) {
		body, err := json.Marshal(sessionRequest{Token: "wrong"})
		require.NoError(t, err)
		resp := postSession(t, s, string(body), host, origin)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		require.Equal(t, CodeUnauthorized, decodeAPIError(t, resp).Error.Code)
	})

	t.Run("malformed JSON body", func(t *testing.T) {
		resp := postSession(t, s, "{not json", host, origin)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.Equal(t, CodeBadRequest, decodeAPIError(t, resp).Error.Code)
	})

	t.Run("wrong Host", func(t *testing.T) {
		body, err := json.Marshal(sessionRequest{Token: token})
		require.NoError(t, err)
		resp := postSession(t, s, string(body), "evil.example:4096", origin)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, CodeForbidden, decodeAPIError(t, resp).Error.Code)
	})

	t.Run("missing Origin", func(t *testing.T) {
		body, err := json.Marshal(sessionRequest{Token: token})
		require.NoError(t, err)
		resp := postSession(t, s, string(body), host, "")
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, CodeForbidden, decodeAPIError(t, resp).Error.Code)
	})

	t.Run("wrong Origin", func(t *testing.T) {
		body, err := json.Marshal(sessionRequest{Token: token})
		require.NoError(t, err)
		resp := postSession(t, s, string(body), host, "http://evil.example:4096")
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, CodeForbidden, decodeAPIError(t, resp).Error.Code)
	})
}

// Past maxSessions, a new session evicts the oldest.
func TestSessionCap(t *testing.T) {
	s, token := newTestServer(t)
	first, _ := newSessionFor(t, s, token)
	for range maxSessions - 1 {
		newSessionFor(t, s, token)
	}
	require.Equal(t, http.StatusNotFound, doProtected(t, s, http.MethodGet, s.Addr(), first, "").StatusCode)

	last, _ := newSessionFor(t, s, token)
	s.sessionsMu.Lock()
	require.Len(t, s.sessions, maxSessions)
	s.sessionsMu.Unlock()
	require.Equal(t, http.StatusUnauthorized, doProtected(t, s, http.MethodGet, s.Addr(), first, "").StatusCode)
	require.Equal(t, http.StatusNotFound, doProtected(t, s, http.MethodGet, s.Addr(), last, "").StatusCode)
}
