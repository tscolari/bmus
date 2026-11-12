package console

import (
	"encoding/json"
	"net/http"
)

// Error codes the console's HTTP API returns in apiErrorBody.Error.Code.
// The action endpoints (#73) pass on the bridge's RPC codes, which these
// spell the same.
const (
	CodeBadRequest         = "bad_request"
	CodeUnauthorized       = "unauthorized" // missing/invalid token or session cookie
	CodeForbidden          = "forbidden"    // wrong Host/Origin, or missing/invalid CSRF
	CodeUnavailable        = "unavailable"  // no data source behind the console, or the RPC's
	CodeInternal           = "internal"
	CodeNotFound           = "not_found"
	CodeExists             = "exists"
	CodeHailClosed         = "hail_closed"
	CodeCountermandExpired = "countermand_expired" // no countermand window open
	CodeConflict           = "conflict"
	CodeShutdown           = "shutting_down"
	// CodeTimeout is the console's own: an action that ran past its
	// time, such as a push to a server that doesn't answer (#4).
	CodeTimeout = "timeout"
)

// codeStatus is the HTTP status of an action's error code. A code not
// here is a 500.
var codeStatus = map[string]int{
	CodeBadRequest:         http.StatusBadRequest,
	CodeForbidden:          http.StatusForbidden,
	CodeNotFound:           http.StatusNotFound,
	CodeExists:             http.StatusConflict,
	CodeHailClosed:         http.StatusConflict,
	CodeCountermandExpired: http.StatusConflict,
	CodeConflict:           http.StatusConflict,
	CodeUnavailable:        http.StatusConflict, // the RPC's: an assignee in sickbay or dismissed
	CodeShutdown:           http.StatusServiceUnavailable,
	CodeTimeout:            http.StatusGatewayTimeout,
	CodeInternal:           http.StatusInternalServerError,
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type apiErrorBody struct {
	Error apiError `json:"error"`
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiErrorBody{Error: apiError{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
