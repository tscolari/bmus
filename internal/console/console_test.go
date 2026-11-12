package console

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListenOnlyOnHost(t *testing.T) {
	for _, addr := range []string{"localhost:0", "[::1]:0", "0.0.0.0:0", "192.168.1.5:0", "4096"} {
		t.Run(addr, func(t *testing.T) {
			_, err := Listen(Config{Addr: addr, Home: t.TempDir()})
			require.ErrorContains(t, err, "not 127.0.0.1:<port>")
		})
	}
}

func TestCheckPort(t *testing.T) {
	require.NoError(t, CheckPort(0))
	require.NoError(t, CheckPort(65535))
	require.Error(t, CheckPort(-1))
	require.Error(t, CheckPort(65536))
}

// startTestServer listens on a free 127.0.0.1 port under home and serves.
func startTestServer(t *testing.T, home string) *Server {
	t.Helper()
	s, err := Listen(Config{Addr: "127.0.0.1:0", Home: home, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.Serve(nil))
	return s
}

// newTestServer starts a console Server on a free 127.0.0.1 port and
// returns it with its per-start token.
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	s := startTestServer(t, home)
	data, err := os.ReadFile(filepath.Join(home, TokenFile))
	require.NoError(t, err)
	return s, strings.TrimSpace(string(data))
}

func TestTokenFile(t *testing.T) {
	_, token := newTestServer(t)
	require.Len(t, token, 64)

	home := t.TempDir()
	startTestServer(t, home)
	fi, err := os.Stat(filepath.Join(home, TokenFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func TestLifecycleCloseStopsListening(t *testing.T) {
	s, _ := newTestServer(t)
	addr := s.Addr()
	resp, err := http.Get("http://" + addr + "/api/session")
	require.NoError(t, err)
	resp.Body.Close()

	require.NoError(t, s.Close())
	_, err = http.Get("http://" + addr + "/api/session")
	require.Error(t, err)
}

func TestCloseRemovesTokenFile(t *testing.T) {
	home := t.TempDir()
	s := startTestServer(t, home)
	require.FileExists(t, filepath.Join(home, TokenFile))
	require.NoError(t, s.Close())
	require.NoFileExists(t, filepath.Join(home, TokenFile))
}

// A Server closed before it serves frees its port and writes nothing.
func TestCloseBeforeServe(t *testing.T) {
	home := t.TempDir()
	s, err := Listen(Config{Addr: "127.0.0.1:0", Home: home})
	require.NoError(t, err)
	addr := s.Addr()
	require.NoError(t, s.Close())
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	ln.Close()
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestSecurityHeaders(t *testing.T) {
	s, _ := newTestServer(t)
	resp, err := http.Get("http://" + s.Addr() + "/api/anything")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NotEmpty(t, resp.Header.Get("Content-Security-Policy"))
	require.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"))
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
}
