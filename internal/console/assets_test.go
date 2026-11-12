package console

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

// serveAssets starts a console serving assets and returns its address.
func serveAssets(t *testing.T, assets fs.FS) string {
	t.Helper()
	s, err := Listen(Config{Addr: "127.0.0.1:0", Home: t.TempDir(), Assets: assets, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.Serve(nil))
	return s.Addr()
}

// get fetches path from addr and returns the response and its body.
func get(t *testing.T, addr, path string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

// The committed build is embedded: / serves its index.html, which loads
// app.js and app.css from the same origin.
func TestEmbeddedIndexServed(t *testing.T) {
	addr := serveAssets(t, nil)

	resp, body := get(t, addr, "/")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	require.Equal(t, "default-src 'self'; connect-src 'self'", resp.Header.Get("Content-Security-Policy"))
	require.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Contains(t, body, `<script type="module" src="/app.js"></script>`)

	for _, path := range []string{"/app.js", "/app.css", "/fonts/syncopate-latin-400-normal.woff2", "/preact-LICENSE.txt"} {
		resp, body := get(t, addr, path)
		require.Equal(t, http.StatusOK, resp.StatusCode, path)
		require.NotEmpty(t, body, path)
	}
}

// The mission report (#104), /report/<order>, is index.html under the
// same headers as /; no other path under /report/ serves anything, nor
// does the old /diff/<order>.
func TestReportPageServed(t *testing.T) {
	t.Parallel()
	addr := serveAssets(t, fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><title>index</title>")},
		"app.js":     {Data: []byte("js")},
	})
	resp, body := get(t, addr, "/report/ORD-0007")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	require.Equal(t, "default-src 'self'; connect-src 'self'", resp.Header.Get("Content-Security-Policy"))
	require.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	require.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	require.Equal(t, "<!doctype html><title>index</title>", body)

	for _, path := range []string{"/report", "/report/", "/report/ORD-0007/", "/report/ORD-0007/x", "/report/app.js", "/report/.ORD", "/report/ORD%2F1", "/reportx/ORD-0007", "/diff/ORD-0007"} {
		resp, _ := get(t, addr, path)
		require.Equal(t, http.StatusNotFound, resp.StatusCode, path)
	}
}

func TestAssetsHead(t *testing.T) {
	addr := serveAssets(t, nil)
	resp, err := http.Head("http://" + addr + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Empty(t, body)
}

// A static file goes through the Host check like /api/.
func TestAssetsBadHost(t *testing.T) {
	addr := serveAssets(t, nil)
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/app.js", nil)
	require.NoError(t, err)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestMissingBuildPage(t *testing.T) {
	addr := serveAssets(t, fstest.MapFS{".gitkeep": {}})
	for _, path := range []string{"/", "/app.js", "/report/ORD-0007"} {
		resp, body := get(t, addr, path)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, path)
		require.Contains(t, resp.Header.Get("Content-Type"), "text/html")
		require.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
		require.Contains(t, body, "npm run build")
		require.NotContains(t, body, "<script")
		require.NotContains(t, body, "style")
	}
}

func TestAssetsNoListingsOrDotfiles(t *testing.T) {
	addr := serveAssets(t, fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html>")},
		".gitkeep":          {},
		"fonts/a.woff2":     {Data: []byte("font")},
		"fonts/.hidden":     {Data: []byte("x")},
		"fonts/LICENSE.txt": {Data: []byte("OFL")},
	})
	for _, path := range []string{"/fonts/", "/.gitkeep", "/fonts/.hidden", "/nope.js"} {
		resp, _ := get(t, addr, path)
		require.Equal(t, http.StatusNotFound, resp.StatusCode, path)
	}
	resp, body := get(t, addr, "/fonts/a.woff2")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "font", body)
}

// A mutation outside /api/ still needs an Origin, and with one is refused
// as a method the frontend doesn't serve.
func TestAssetsGetOnly(t *testing.T) {
	addr := serveAssets(t, nil)
	resp, err := http.Post("http://"+addr+"/", "text/plain", strings.NewReader(""))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "http://"+addr)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}
