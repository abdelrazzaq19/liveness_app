package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// ErrorCode is the stable, machine-readable identifier in an error response.
// Clients switch on this. The accompanying message is for humans and may be
// reworded at any time.
type ErrorCode string

const (
	CodeBadRequest       ErrorCode = "bad_request"
	CodeUnauthorized     ErrorCode = "unauthorized"
	CodeForbidden        ErrorCode = "forbidden"
	CodeNotFound         ErrorCode = "not_found"
	CodeMethodNotAllowed ErrorCode = "method_not_allowed"
	CodeConflict         ErrorCode = "conflict"
	CodeGone             ErrorCode = "gone"
	CodePayloadTooLarge  ErrorCode = "payload_too_large"
	CodeUnprocessable    ErrorCode = "unprocessable_entity"
	CodeRateLimited      ErrorCode = "rate_limited"
	CodeInternal         ErrorCode = "internal_error"
	CodeUnavailable      ErrorCode = "service_unavailable"
)

// APIError is a transport-level error carrying the status and code to send.
//
// Domain packages never construct one: they return their own sentinel errors,
// and handlers translate those into an APIError. That translation is the only
// place where a domain concept becomes an HTTP concept.
type APIError struct {
	Status  int
	Code    ErrorCode
	Message string

	// Err is the underlying cause. It is logged and never sent to the client,
	// because internal error text routinely names tables, hosts, and file paths.
	Err error
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s (%d): %s: %v", e.Code, e.Status, e.Message, e.Err)
	}
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *APIError) Unwrap() error { return e.Err }

// Fail builds an APIError without an underlying cause.
func Fail(status int, code ErrorCode, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

// FailWith builds an APIError that wraps a cause for logging.
func FailWith(status int, code ErrorCode, message string, cause error) *APIError {
	return &APIError{Status: status, Code: code, Message: message, Err: cause}
}

// errorEnvelope is the single response shape for every failure in this API.
type errorEnvelope struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	RequestID string    `json:"request_id,omitempty"`
}

// writeJSON serialises v as the response body. It is the only place in this
// package that writes a status code for a success path.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already on the wire, so there is nothing to
		// correct. Record it and move on.
		log.Error("encoding response body failed", slog.String("error", err.Error()))
	}
}

// respond writes err to the client in the standard envelope.
//
// Anything that is not an *APIError becomes a generic 500: an unclassified
// error is by definition one whose text was not written with an external
// audience in mind.
func respond(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = FailWith(http.StatusInternalServerError, CodeInternal,
			"an unexpected error occurred", err)
	}

	reqID := RequestIDFrom(r.Context())

	// Log the full cause; send only the curated message.
	attrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("code", string(apiErr.Code)),
		slog.Int("status", apiErr.Status),
	}
	if apiErr.Err != nil {
		attrs = append(attrs, slog.String("error", apiErr.Err.Error()))
	}
	if apiErr.Status >= http.StatusInternalServerError {
		log.LogAttrs(r.Context(), slog.LevelError, apiErr.Message, attrs...)
	} else {
		log.LogAttrs(r.Context(), slog.LevelWarn, apiErr.Message, attrs...)
	}

	writeJSON(w, log, apiErr.Status, errorEnvelope{Error: errorPayload{
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		RequestID: reqID,
	}})
}
