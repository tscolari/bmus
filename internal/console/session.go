package console

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
)

// cookiePrefix plus the console's port names its session cookie, so two
// bridges on one machine keep separate sessions.
const cookiePrefix = "bmus_console_"
const csrfHeader = "X-BMUS-CSRF"
const maxSessionBody = 4 << 10

// maxSessions caps the sessions kept; a new one past it evicts the oldest.
const maxSessions = 64

// session is the per-session state kept server-side; the cookie only
// carries the opaque id that indexes into Server.sessions.
type session struct{ csrf string }

// sessionContextKey is the context key requireSession stores the matched
// session under, for requireCSRF to read.
type sessionContextKey struct{}

type sessionRequest struct {
	Token string `json:"token"`
}

type sessionResponse struct {
	CSRF string `json:"csrf"`
}

// handleSession serves POST /api/session: exchanges the per-start token
// for a session cookie and a CSRF token.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	var req sessionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "malformed request body")
		return
	}
	if req.Token == "" || subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.token)) != 1 {
		writeAPIError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid token")
		return
	}
	id, csrf, err := s.newSession()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, CodeInternal, "could not start a session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName, Value: id, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, sessionResponse{CSRF: csrf})
}

// newSession creates a session and returns its id (the cookie value) and
// its CSRF token.
func (s *Server) newSession() (id, csrf string, err error) {
	if id, err = randomHex(16); err != nil {
		return "", "", err
	}
	if csrf, err = randomHex(16); err != nil {
		return "", "", err
	}
	s.sessionsMu.Lock()
	if len(s.sessionIDs) == maxSessions {
		delete(s.sessions, s.sessionIDs[0])
		s.sessionIDs = s.sessionIDs[1:]
	}
	s.sessions[id] = session{csrf: csrf}
	s.sessionIDs = append(s.sessionIDs, id)
	s.sessionsMu.Unlock()
	return id, csrf, nil
}

// sessionFor returns the session named by r's cookie, if any.
func (s *Server) sessionFor(r *http.Request) (session, bool) {
	c, err := r.Cookie(s.cookieName)
	if err != nil {
		return session{}, false
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	sess, ok := s.sessions[c.Value]
	return sess, ok
}
