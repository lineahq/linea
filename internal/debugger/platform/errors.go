package platform

import (
	"context"
	"errors"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/hostfs"
)

// Code classifies an operation failure independently of any adapter. Each
// adapter maps codes with one table. Inspection diagnostics and
// assertion outcomes are results, never errors.
type Code string

const (
	CodeInvalidRequest    Code = "invalid_request"
	CodeValidationFailed  Code = "validation_failed"
	CodeNotFound          Code = "not_found"
	CodeConflict          Code = "conflict"
	CodeResourceExhausted Code = "resource_exhausted"
	CodeUnsupportedBuild  Code = "unsupported_build"
	CodeForbidden         Code = "forbidden"
	// CodeUnauthenticated: the request carries no valid identity.
	CodeUnauthenticated Code = "unauthenticated"
	// CodeUnavailable: the service is closed or has no persistent workspace.
	CodeUnavailable Code = "unavailable"
	// CodeRunFailed: a report was requested from a run that ended in an error.
	CodeRunFailed Code = "run_failed"
	// CodeCancelled: the caller's context ended before the operation finished.
	CodeCancelled    Code = "cancelled"
	CodeStorageError Code = "storage_error"
	CodeInternal     Code = "internal_error"
)

// Error is the failure of a platform operation. Message is safe to show to the
// caller; the cause stays available to errors.Is for in-process callers.
type Error struct {
	Code    Code
	Message string
	// Diagnostics are the structured findings of the cause. Every
	// validation_failed error has at least one; other codes carry them only
	// when the cause supplies them. Adapters need not import config.
	Diagnostics []Diagnostic
	// Retryable is set when the same request can succeed later unchanged, for
	// example when the run queue is full.
	Retryable bool
	// Current is the artifact's current metadata on an edit conflict.
	Current *ArtifactMetadata
	// CurrentStatement is the statement's current text on a stale statement edit.
	CurrentStatement string
	cause            error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.cause }

// ErrorCode returns the code of an operation error, CodeInternal for an
// unclassified error and "" for nil.
func ErrorCode(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

func newError(code Code, err error) *Error {
	e := &Error{Code: code, Message: err.Error(), cause: err}
	var diagnostics *config.DiagnosticsError
	switch {
	case errors.As(err, &diagnostics):
		e.Diagnostics = append([]Diagnostic(nil), diagnostics.Diagnostics...)
	case code == CodeValidationFailed:
		// Contract §9: a validation failure always carries diagnostics. A
		// cause without findings becomes one, as inspection reports it.
		e.Diagnostics = config.AsDiagnostics(err)
	}
	return e
}

// classify gives an internal failure its code. Unrecognized errors take the
// operation's fallback: validation_failed where the error describes the
// caller's sources (configuration, scenario, files), internal_error elsewhere.
func classify(err error, fallback Code) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	var conflict *artifact.Conflict
	switch {
	case errors.Is(err, ErrQueueFull):
		e = newError(CodeResourceExhausted, err)
		e.Retryable = true
	case errors.Is(err, engine.ErrInputLimit), errors.Is(err, config.ErrCaptureLimit), errors.Is(err, artifact.ErrLimit):
		e = newError(CodeResourceExhausted, err)
	case errors.Is(err, ErrPersistence), errors.Is(err, artifact.ErrStorage), errors.Is(err, artifact.ErrCorrupt),
		errors.Is(err, artifact.ErrUnsupportedSchema), errors.Is(err, artifact.ErrLocked):
		e = newError(CodeStorageError, err)
	case errors.Is(err, ErrClosed), errors.Is(err, artifact.ErrClosed), errors.Is(err, ErrWorkspaceRequired),
		errors.Is(err, engine.ErrRuntimeUnavailable), errors.Is(err, engine.ErrNoCollector):
		e = newError(CodeUnavailable, err)
	case errors.Is(err, ErrUnknownRun), errors.Is(err, artifact.ErrNotFound), errors.Is(err, ErrBundleUnavailable):
		e = newError(CodeNotFound, err)
	case errors.As(err, &conflict):
		e = newError(CodeConflict, err)
		e.Current = conflict.Current
	case errors.Is(err, artifact.ErrConflict):
		e = newError(CodeConflict, err)
	case errors.Is(err, engine.ErrDistribution), errors.Is(err, engine.ErrBundleSchema):
		e = newError(CodeUnsupportedBuild, err)
	case errors.Is(err, ErrUnauthenticated):
		e = newError(CodeUnauthenticated, err)
	case errors.Is(err, engine.ErrInvalidRequest), errors.Is(err, artifact.ErrInvalidRequest), errors.Is(err, errInvalidCaller):
		e = newError(CodeInvalidRequest, err)
	case config.Refused(err):
		// The message is the path error: it names the path the caller sent
		// and the reason, never file contents, and how to allow it.
		e = newError(CodeForbidden, err)
		if errors.Is(err, hostfs.ErrOutsideRoots) || errors.Is(err, hostfs.ErrNoRoots) {
			// The composition may name how its owner allows a path; a
			// team server names none, since it reads no host file.
			e.Message += "; send the content inline"
			if grant := hostfs.GrantOf(err); grant != "" {
				e.Message += ", or " + grant
			}
		}
	case errors.Is(err, config.ErrEnvironmentPolicy), errors.Is(err, hostfs.ErrInvalidPolicy):
		e = newError(CodeInternal, err) // Trusted composition policy, not request content.
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		e = newError(CodeCancelled, err)
	default:
		e = newError(fallback, err)
	}
	return e
}
