package api

import (
	"errors"
	"net/http"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Error is the body of every error response.
type Error struct {
	Error string `json:"error"`
}

// statusOf is the one mapping from platform error codes to HTTP v1 statuses.
// A full queue is retryable and therefore 503; other exhausted
// resources are sizes (413).
func statusOf(err error) int {
	var e *platform.Error
	if !errors.As(err, &e) {
		return http.StatusInternalServerError
	}
	switch e.Code {
	case platform.CodeInvalidRequest:
		return http.StatusBadRequest
	case platform.CodeValidationFailed, platform.CodeRunFailed:
		return http.StatusUnprocessableEntity
	case platform.CodeNotFound:
		return http.StatusNotFound
	case platform.CodeConflict, platform.CodeUnsupportedBuild:
		return http.StatusConflict
	case platform.CodeResourceExhausted:
		if e.Retryable {
			return http.StatusServiceUnavailable
		}
		return http.StatusRequestEntityTooLarge
	case platform.CodeUnavailable:
		return http.StatusServiceUnavailable
	case platform.CodeForbidden:
		return http.StatusForbidden
	case platform.CodeUnauthenticated:
		return http.StatusUnauthorized
	}
	return http.StatusInternalServerError
}

func writeFailure(w http.ResponseWriter, err error) {
	writeError(w, statusOf(err), err)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, Error{Error: err.Error()})
}

// writeSourceFailure keeps v1's statuses for operations over the caller's
// sources (inspection, preview, assertions): a malformed request is a 400, a
// host path outside the allowed roots a 403, an unknown saved revision a 404,
// artifact references without a workspace a 503 and every other failure a 422.
func writeSourceFailure(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	switch platform.ErrorCode(err) {
	case platform.CodeInvalidRequest:
		status = http.StatusBadRequest
	case platform.CodeForbidden:
		status = http.StatusForbidden
	case platform.CodeNotFound:
		status = http.StatusNotFound
	case platform.CodeUnavailable:
		status = http.StatusServiceUnavailable
	}
	writeError(w, status, err)
}

// OperationError is the body of failures of the candidate and
// scenario-from-run endpoints: the platform code beside the message and, on
// a stale statement edit, the statement's current text.
type OperationError struct {
	Code             string `json:"code"`
	Error            string `json:"error"`
	CurrentStatement string `json:"current_statement,omitempty"`
	// Diagnostics are the findings of a validation_failed failure, such as
	// a manifest refused before building.
	Diagnostics []platform.Diagnostic `json:"diagnostics,omitempty"`
}

func writeOperationError(w http.ResponseWriter, err error) {
	body := OperationError{Code: string(platform.ErrorCode(err)), Error: err.Error()}
	var e *platform.Error
	if errors.As(err, &e) {
		body.Error, body.CurrentStatement, body.Diagnostics = e.Message, e.CurrentStatement, e.Diagnostics
	}
	writeJSON(w, statusOf(err), body)
}
