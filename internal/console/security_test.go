package console

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// newSessionFor exchanges token for a real session cookie and CSRF token
// over the wire, the way a browser would.
func newSessionFor(t *testing.T, s *Server, token string) (cookie *http.Cookie, csrf string) {
	t.Helper()
	host := s.Addr()
	body, err := json.Marshal(sessionRequest{Token: token})
	require.NoError(t, err)
	resp := postSession(t, s, string(body), host, "http://"+host)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	for _, c := range resp.Cookies() {
		if c.Name == s.cookieName {
			cookie = c
		}
	}
	require.NotNil(t, cookie)
	var sr sessionResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sr))
	return cookie, sr.CSRF
}

// doProtected issues method to s's protected /api/x, optionally with a
// session cookie and/or CSRF header.
func doProtected(t *testing.T, s *Server, method, host string, cookie *http.Cookie, csrf string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+s.Addr()+"/api/x", bytes.NewReader(nil))
	require.NoError(t, err)
	if host != "" {
		req.Host = host
	}
	req.Header.Set("Origin", "http://"+s.Addr())
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set(csrfHeader, csrf)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestProtectedRoutes(t *testing.T) {
	s, token := newTestServer(t)
	host := s.Addr()

	t.Run("GET, no cookie", func(t *testing.T) {
		resp := doProtected(t, s, http.MethodGet, host, nil, "")
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		require.Equal(t, CodeUnauthorized, decodeAPIError(t, resp).Error.Code)
	})

	t.Run("GET, wrong Host, no cookie", func(t *testing.T) {
		resp := doProtected(t, s, http.MethodGet, "evil.example:4096", nil, "")
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, CodeForbidden, decodeAPIError(t, resp).Error.Code)
	})

	cookie, csrf := newSessionFor(t, s, token)

	t.Run("GET with valid cookie, correct Host", func(t *testing.T) {
		resp := doProtected(t, s, http.MethodGet, host, cookie, "")
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("POST with valid cookie, no CSRF", func(t *testing.T) {
		resp := doProtected(t, s, http.MethodPost, host, cookie, "")
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, CodeForbidden, decodeAPIError(t, resp).Error.Code)
	})

	t.Run("POST with valid cookie, wrong CSRF", func(t *testing.T) {
		resp := doProtected(t, s, http.MethodPost, host, cookie, "wrong-csrf")
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, CodeForbidden, decodeAPIError(t, resp).Error.Code)
	})

	t.Run("POST with valid cookie, correct CSRF", func(t *testing.T) {
		resp := doProtected(t, s, http.MethodPost, host, cookie, csrf)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// A lowercase method isn't GET or HEAD, so it is a mutation.
func TestProtectedRoutesMethodCaseInsensitive(t *testing.T) {
	s, token := newTestServer(t)
	host := s.Addr()
	cookie, csrf := newSessionFor(t, s, token)

	t.Run("lowercase put, no CSRF", func(t *testing.T) {
		resp := doProtected(t, s, "put", host, cookie, "")
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, CodeForbidden, decodeAPIError(t, resp).Error.Code)
	})

	t.Run("lowercase put, correct CSRF", func(t *testing.T) {
		resp := doProtected(t, s, "put", host, cookie, csrf)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// Every method but GET and HEAD needs an Origin.
func TestCheckOriginMethods(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.checkOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cases := []struct {
		method string
		status int
	}{
		{http.MethodGet, http.StatusOK},
		{http.MethodHead, http.StatusOK},
		{http.MethodPost, http.StatusForbidden},
		{"PROPPATCH", http.StatusForbidden},
		{"LOCK", http.StatusForbidden},
		{"get", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(c.method, "http://"+s.Addr()+"/api/x", nil))
			require.Equal(t, c.status, rec.Code)
		})
	}
}

// A read with a foreign Origin is refused too; one without an Origin isn't.
func TestCheckOriginReads(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.checkOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cases := []struct {
		name   string
		origin []string
		status int
	}{
		{"no Origin", nil, http.StatusOK},
		{"own Origin", []string{"http://" + s.Addr()}, http.StatusOK},
		{"foreign Origin", []string{"http://evil.example"}, http.StatusForbidden},
		{"null Origin", []string{"null"}, http.StatusForbidden},
		{"empty Origin", []string{""}, http.StatusForbidden},
		{"two Origins", []string{"http://" + s.Addr(), "http://evil.example"}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+s.Addr()+"/api/x", nil)
			if c.origin != nil {
				req.Header["Origin"] = c.origin
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, c.status, rec.Code)
		})
	}
}

// A path outside /api/ still gets the Host check and the headers.
func TestNonAPIPathChecked(t *testing.T) {
	s, _ := newTestServer(t)
	req, err := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/", nil)
	require.NoError(t, err)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NotEmpty(t, resp.Header.Get("Content-Security-Policy"))
}

func TestCheckHostForms(t *testing.T) {
	s, _ := newTestServer(t)
	_, port, err := net.SplitHostPort(s.Addr())
	require.NoError(t, err)
	cases := []struct {
		host   string
		status int
	}{
		{"127.0.0.1:" + port, http.StatusUnauthorized},
		{"localhost:" + port, http.StatusUnauthorized},
		{"LOCALHOST:" + port, http.StatusForbidden},
		{"localhost.:" + port, http.StatusForbidden},
		{"[::1]:" + port, http.StatusForbidden},
		{"127.0.0.1", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			require.Equal(t, c.status, doProtected(t, s, http.MethodGet, c.host, nil, "").StatusCode)
		})
	}
}

// The localhost form of the console's origin passes checkOrigin, on a
// read and a mutation.
func TestCheckOriginLocalhost(t *testing.T) {
	s, _ := newTestServer(t)
	_, port, err := net.SplitHostPort(s.Addr())
	require.NoError(t, err)
	h := s.checkOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "http://localhost:"+port+"/api/x", nil)
			req.Header.Set("Origin", "http://localhost:"+port)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

// An absolute-form request URI must name one of the console's hosts too.
func TestCheckHostAbsoluteURI(t *testing.T) {
	s, _ := newTestServer(t)
	// A foreign absolute-form URI with a matching Host header.
	conn, err := net.Dial("tcp", s.Addr())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "GET http://evil.example/api/x HTTP/1.1\r\nHost: %s\r\n\r\n", s.Addr())
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}
