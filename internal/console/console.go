// Package console is the bmus console's HTTP server: the listener, the
// per-start token, the /api/session exchange, and the Host/Origin/CSRF
// request checks and security headers (D9, issue #71), and the data
// endpoints behind them: /api/state, /api/log and the /api/events stream
// (#72, events.go), and the action endpoints (#73, actions.go). This
// package has no dependency on internal/bridge, which imports it to start
// the server and hands Serve a Source instead.
package console

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Host is the only address the console binds.
const Host = "127.0.0.1"

// CheckPort validates port as a console port; 0 picks a free one.
func CheckPort(port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("console port %d: out of range 0-65535", port)
	}
	return nil
}

// Config configures a console Server.
type Config struct {
	// Addr is the address the console listens on: Host and a port.
	Addr string
	// Home is the bridge home directory the per-start token is written
	// under.
	Home string
	// Logger receives the console's logs. Nil means slog.Default().
	Logger *slog.Logger
	// Assets is the frontend served outside /api/. Nil means the build
	// embedded in the binary.
	Assets fs.FS
}

// Server is a console HTTP server: listening after Listen, serving after
// Serve.
type Server struct {
	ln     net.Listener
	http   *http.Server
	logger *slog.Logger
	home   string

	token      string
	cookieName string

	allowedHosts   map[string]bool
	allowedOrigins map[string]bool

	sessionsMu sync.Mutex
	sessions   map[string]session
	sessionIDs []string // sessions' ids, oldest first

	// src is Serve's Source. streams is cancelled first thing in Close,
	// ending every /api/events stream: Shutdown waits for handlers, and a
	// stream's never returns on its own.
	src         Source
	streams     context.Context
	stopStreams context.CancelFunc
	heartbeat   time.Duration
	maxReplay   int64

	serving   bool
	closeOnce sync.Once
}

// Listen opens the console's listener on cfg.Addr, which must be on
// Host, and generates the per-start token. It writes nothing; Serve does.
func Listen(cfg Config) (*Server, error) {
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || host != Host {
		return nil, fmt.Errorf("console: %s: not %s:<port>", cfg.Addr, Host)
	}
	token, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("console: %w", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("console: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	hosts := []string{Host + ":" + port, "localhost:" + port}
	allowedHosts := make(map[string]bool, len(hosts))
	allowedOrigins := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		allowedHosts[h] = true
		allowedOrigins["http://"+h] = true
	}
	s := &Server{
		ln:             ln,
		logger:         logger,
		home:           cfg.Home,
		token:          token,
		cookieName:     cookiePrefix + port,
		allowedHosts:   allowedHosts,
		allowedOrigins: allowedOrigins,
		sessions:       map[string]session{},
		heartbeat:      defaultHeartbeat,
		maxReplay:      defaultMaxReplay,
	}
	s.streams, s.stopStreams = context.WithCancel(context.Background())
	assets := cfg.Assets
	if assets == nil {
		assets = embeddedAssets()
	}
	s.http = &http.Server{Handler: s.routes(assets), ReadHeaderTimeout: 10 * time.Second}
	return s, nil
}

// Serve writes the token file and starts serving in the background, with
// src behind the data endpoints. With a nil src they answer 503.
func (s *Server) Serve(src Source) error {
	if err := writeTokenFile(s.home, s.token); err != nil {
		return fmt.Errorf("console: %w", err)
	}
	s.src = src
	s.serving = true
	go func() {
		if err := s.http.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("console serve", "err", err)
		}
	}()
	return nil
}

// Addr returns the server's resolved listen address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close ends the event streams, stops accepting requests, waits up to 5
// seconds for in-flight ones (then drops them), closes the listener and
// removes the token file. Calls after the first do nothing.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.stopStreams()
		if s.serving {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err = s.http.Shutdown(ctx); err != nil {
				err = errors.Join(err, s.http.Close())
			}
		} else {
			err = s.ln.Close()
		}
		err = errors.Join(err, RemoveTokenFile(s.home))
	})
	return err
}

// routes builds the console's HTTP routing. Every request, the mux's own
// redirects and 404s included, gets the security headers and the Host
// check. POST /api/session is exempt from requireSession/requireCSRF
// (that is how a session is created at all) but still goes through
// checkOrigin; every other /api/ path is protected end to end, and
// reaches apiRoutes (#72, #73) only through the chain, GET /api/session
// included. Every other path serves the frontend from assets ("/", not
// "GET /": that pattern would conflict with "/api/").
func (s *Server) routes(assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", s.checkOrigin(assetsHandler(assets)))
	mux.Handle("POST /api/session", s.checkOrigin(http.HandlerFunc(s.handleSession)))
	mux.Handle("/api/", s.checkOrigin(s.requireSession(s.requireCSRF(s.apiRoutes()))))
	return securityHeaders(s.checkHost(mux))
}
