package console

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// securityHeaders sets the headers every console response carries: a
// strict CSP (the console serves no third-party content), clickjacking,
// MIME sniffing and referrer protections, and no-store on API responses so a shared
// machine's browser cache never keeps a copy of console data.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// checkHost rejects a request whose Host isn't one of the console's own
// (DNS rebinding protection). For an absolute-form request URI, net/http
// takes r.Host from the URI.
func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHosts[r.Host] {
			writeAPIError(w, http.StatusForbidden, CodeForbidden, "invalid Host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isMutation reports whether a request with method is a mutation, which
// checkOrigin and requireCSRF gate on: anything but GET and HEAD.
func isMutation(method string) bool {
	return method != http.MethodGet && method != http.MethodHead
}

// checkOrigin rejects a request whose Origin header is present and isn't
// one of the console's own origins, and a mutation without one.
func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin, present := r.Header["Origin"]
		if present && (len(origin) != 1 || !s.allowedOrigins[origin[0]]) ||
			!present && isMutation(r.Method) {
			writeAPIError(w, http.StatusForbidden, CodeForbidden, "missing or invalid Origin")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireSession rejects a request without a valid session cookie, and
// otherwise hands the matched session to the next handler through the
// request context, for requireCSRF to read.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.sessionFor(r)
		if !ok {
			writeAPIError(w, http.StatusUnauthorized, CodeUnauthorized, "missing or invalid session")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, sess)))
	})
}

// requireCSRF rejects a mutating request whose X-BMUS-CSRF header doesn't
// match the session's CSRF token. It must run after requireSession, which
// puts the session in the request context.
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMutation(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		sess, _ := r.Context().Value(sessionContextKey{}).(session)
		got := r.Header.Get(csrfHeader)
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) != 1 {
			writeAPIError(w, http.StatusForbidden, CodeForbidden, "missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
