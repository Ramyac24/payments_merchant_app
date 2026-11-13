// Package apierr defines the single, Stripe-style error shape returned by the API:
//
//	{"error": {"type": "...", "code": "...", "message": "...", "param": "..."}}
package apierr

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

type Error struct {
	Status  int    `json:"-"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
}

func (e *Error) Error() string { return e.Type + ": " + e.Message }

func InvalidRequest(code, param, msg string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: code, Param: param, Message: msg}
}

func NotFound(msg string) *Error {
	return &Error{Status: http.StatusNotFound, Type: "invalid_request_error", Code: "resource_missing", Message: msg}
}

func Unauthorized(msg string) *Error {
	return &Error{Status: http.StatusUnauthorized, Type: "authentication_error", Message: msg}
}

func RateLimited() *Error {
	return &Error{Status: http.StatusTooManyRequests, Type: "rate_limit_error", Message: "too many requests, slow down"}
}

func Idempotency(status int, code, msg string) *Error {
	return &Error{Status: status, Type: "idempotency_error", Code: code, Message: msg}
}

func Upstream(msg string) *Error {
	return &Error{Status: http.StatusBadGateway, Type: "api_error", Code: "upstream_error", Message: msg}
}

// Write renders err as JSON. Unknown errors become an opaque 500 (and are logged)
// so internal details never leak to clients.
func Write(w http.ResponseWriter, err error) {
	var ae *Error
	if !errors.As(err, &ae) {
		slog.Error("internal error", "err", err)
		ae = &Error{Status: http.StatusInternalServerError, Type: "api_error", Message: "internal server error"}
	}
	WriteJSON(w, ae.Status, map[string]any{"error": ae})
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
