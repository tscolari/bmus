package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

// maxActionBody caps an action's request body.
const maxActionBody = 64 << 10

// Actions is the bridge's mutations as the console calls them (#73): the
// Daemon calls behind the matching RPC commands, with the same
// validation, defaults and results. args is the request body, a JSON
// object with the RPC args' field names, which the bridge decodes as
// strictly as the RPC does. A returned error with a Code method carries
// its RPC code; any other error is internal.
type Actions interface {
	// CreateOrder is order.create; it returns the new order's view.
	CreateOrder(ctx context.Context, args json.RawMessage) (any, error)
	// AnswerHail is hails.answer for hail id, with args {option,
	// decision}; it returns the decided hail's view.
	AnswerHail(ctx context.Context, id string, args json.RawMessage) (any, error)
	// CountermandHail is hails.countermand for hail id, with args
	// {option, decision, reason}; it returns the hail's view (#153).
	CountermandHail(ctx context.Context, id string, args json.RawMessage) (any, error)
	// AddCrew is crew.add; it returns the new crew member's view.
	AddCrew(ctx context.Context, args json.RawMessage) (any, error)
	// BelayOrder, RecallOrder and WithdrawOrder are order.belay,
	// order.recall and order.withdraw for order id, with args {reason};
	// each returns the order's view (#78).
	BelayOrder(ctx context.Context, id string, args json.RawMessage) (any, error)
	RecallOrder(ctx context.Context, id string, args json.RawMessage) (any, error)
	WithdrawOrder(ctx context.Context, id string, args json.RawMessage) (any, error)
	// RouteOrder is order.route for order id, with args {assignee,
	// division}; it returns the routed order's view.
	RouteOrder(ctx context.Context, id string, args json.RawMessage) (any, error)
	// ReenergizeCrew is crew.reenergize for crew member id, with args {};
	// it returns the crew member's view and the outcome.
	ReenergizeCrew(ctx context.Context, id string, args json.RawMessage) (any, error)
	// SetStandingOrders is standing_orders.set, with args {on}; it
	// returns {on}.
	SetStandingOrders(ctx context.Context, args json.RawMessage) (any, error)
	// PushOrder is order.push for done order id, with args
	// {target_branch, force}; it returns the pushed repos, each with
	// {name, url, branch, commit} and its URL without credentials (#4).
	PushOrder(ctx context.Context, id string, args json.RawMessage) (any, error)
}

// codedError is an Actions error carrying its RPC code.
type codedError interface {
	error
	Code() string
}

// handleGetSession serves GET /api/session: the current session's CSRF
// token, for a page reloaded without the #t= fragment. requireSession
// has put the session in the request context.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, _ := r.Context().Value(sessionContextKey{}).(session)
	writeJSON(w, http.StatusOK, sessionResponse{CSRF: sess.csrf})
}

// handleCreateOrder serves POST /api/orders: 201 and the order's view.
func (s *Server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, http.StatusCreated, "create order", func(src Source, args json.RawMessage) (any, error) {
		return src.CreateOrder(r.Context(), args)
	})
}

// handleAnswerHail serves POST /api/hails/{id}/answer: 200 and the
// decided hail's view.
func (s *Server) handleAnswerHail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.act(w, r, http.StatusOK, "answer hail", func(src Source, args json.RawMessage) (any, error) {
		return src.AnswerHail(r.Context(), id, args)
	})
}

// handleCountermandHail serves POST /api/hails/{id}/countermand: 200 and
// the countermanded hail's view.
func (s *Server) handleCountermandHail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.act(w, r, http.StatusOK, "countermand hail", func(src Source, args json.RawMessage) (any, error) {
		return src.CountermandHail(r.Context(), id, args)
	})
}

// handleAddCrew serves POST /api/crew: 201 and the crew member's view.
func (s *Server) handleAddCrew(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, http.StatusCreated, "add crew", func(src Source, args json.RawMessage) (any, error) {
		return src.AddCrew(r.Context(), args)
	})
}

// handleOrderAction serves POST /api/orders/{id}/<what> with call (#78):
// 200 and the order's view.
func (s *Server) handleOrderAction(what string, call func(Source, context.Context, string, json.RawMessage) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		s.act(w, r, http.StatusOK, what, func(src Source, args json.RawMessage) (any, error) {
			return call(src, r.Context(), id, args)
		})
	}
}

// handlePushOrder serves POST /api/orders/{id}/push (#4): 200 and the
// pushed repos. A diverged target is a 409 (conflict), a push that ran
// past its time a 504 (timeout).
func (s *Server) handlePushOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.act(w, r, http.StatusOK, "push order", func(src Source, args json.RawMessage) (any, error) {
		return src.PushOrder(r.Context(), id, args)
	})
}

// handleReenergize serves POST /api/crew/{id}/reenergize: 200, the crew
// member's view and the outcome.
func (s *Server) handleReenergize(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.act(w, r, http.StatusOK, "re-energize crew", func(src Source, args json.RawMessage) (any, error) {
		return src.ReenergizeCrew(r.Context(), id, args)
	})
}

// handleStandingOrders serves PUT /api/standing-orders: 200 and {on}.
func (s *Server) handleStandingOrders(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, http.StatusOK, "set standing orders", func(src Source, args json.RawMessage) (any, error) {
		return src.SetStandingOrders(r.Context(), args)
	})
}

// act reads an action's JSON body, calls call with it and writes its
// result with status, or its error. It returns once call does: the
// resulting log entries reach the feed on their own.
func (s *Server) act(w http.ResponseWriter, r *http.Request, status int, what string,
	call func(Source, json.RawMessage) (any, error)) {
	src, ok := s.source(w)
	if !ok {
		return
	}
	args, ok := readJSONObject(w, r)
	if !ok {
		return
	}
	v, err := call(src, args)
	if err != nil {
		s.actionError(w, what, err)
		return
	}
	writeJSON(w, status, v)
}

// readJSONObject reads r's body, which must be application/json, at most
// maxActionBody bytes and one JSON object. It answers the request itself
// when it fails.
func readJSONObject(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeAPIError(w, http.StatusUnsupportedMediaType, CodeBadRequest, "Content-Type must be application/json")
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxActionBody))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeAPIError(w, http.StatusRequestEntityTooLarge, CodeBadRequest, "request body too large")
		return nil, false
	}
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "read request body: "+err.Error())
		return nil, false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' || !json.Valid(body) {
		writeAPIError(w, http.StatusBadRequest, CodeBadRequest, "request body must be one JSON object")
		return nil, false
	}
	return body, true
}

// actionError writes an Actions error with its code's status and its
// message, as the RPC replies with them. An error without a code, or
// with one that maps to a 500, is logged and answered as internalError
// does: its message (SQL, paths) isn't sent. Nor is a shutting_down
// error's, which wraps whatever the shutdown cut short, or a conflict's,
// which wraps git's output for a rejected push.
func (s *Server) actionError(w http.ResponseWriter, what string, err error) {
	var ce codedError
	if !errors.As(err, &ce) {
		s.internalError(w, what, err)
		return
	}
	status, ok := codeStatus[ce.Code()]
	switch {
	case !ok || status == http.StatusInternalServerError:
		s.internalError(w, what, err)
	case ce.Code() == CodeShutdown:
		s.logger.Info("console: "+what, "err", err)
		writeAPIError(w, status, CodeShutdown, "bridge is shutting down")
	case ce.Code() == CodeConflict:
		s.logger.Info("console: "+what, "err", err)
		writeAPIError(w, status, CodeConflict, what+" failed: conflict (details in the bridge's log)")
	default:
		writeAPIError(w, status, ce.Code(), err.Error())
	}
}
