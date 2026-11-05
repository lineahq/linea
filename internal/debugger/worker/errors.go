package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// wireError carries an engine error across the process boundary: its
// message unchanged, the sentinel it wraps as a kind, and its diagnostics.
// The debugger rebuilds an error that errors.Is and errors.As classify as
// the in-process error would be, so platform codes stay the same.
type wireError struct {
	Kind        string              `json:"kind,omitempty"`
	Message     string              `json:"message"`
	Diagnostics []config.Diagnostic `json:"diagnostics,omitempty"`
	// Suspect says a pool worker found the runtime binary itself wrong
	// (ErrSuspectBinary); older workers never set it.
	Suspect bool `json:"suspect,omitempty"`
}

// ErrSuspectBinary marks a pool worker's runtime_unavailable error caused by
// the runtime binary itself: it failed its digest, did not start, or
// announced another runtime than the one assigned. The server
// stops offering runs on it.
var ErrSuspectBinary = errors.New("the runtime binary is suspect")

// suspectError marks err as caused by the binary without changing its
// message.
type suspectError struct{ error }

func (e suspectError) Unwrap() error        { return e.error }
func (e suspectError) Is(target error) bool { return target == ErrSuspectBinary }

// kinds are the sentinels a remote error may wrap, by wire name. Order
// matters: the first match names the error.
var kinds = []struct {
	name string
	err  error
}{
	{"invalid_request", engine.ErrInvalidRequest},
	{"unsupported_build", engine.ErrDistribution},
	{"bundle_schema", engine.ErrBundleSchema},
	{"bundle", engine.ErrBundle},
	{"input_limit", engine.ErrInputLimit},
	{"capture_limit", config.ErrCaptureLimit},
	{"runtime_unavailable", engine.ErrRuntimeUnavailable},
	{"deadline_exceeded", context.DeadlineExceeded},
	{"cancelled", context.Canceled},
}

func encodeError(err error) *wireError {
	if err == nil {
		return nil
	}
	w := &wireError{Message: err.Error(), Suspect: errors.Is(err, ErrSuspectBinary)}
	var de *config.DiagnosticsError
	if errors.As(err, &de) {
		w.Kind, w.Diagnostics = "diagnostics", de.Diagnostics
		return w
	}
	for _, k := range kinds {
		if errors.Is(err, k.err) {
			w.Kind = k.name
			break
		}
	}
	return w
}

// remoteError is an error a worker returned.
type remoteError struct {
	message string
	cause   error
}

func (e *remoteError) Error() string { return e.message }
func (e *remoteError) Unwrap() error { return e.cause }

// MarshalError encodes err as a worker sends it, for a server that relays a
// worker's answer to another instance; UnmarshalError rebuilds it, so
// platform codes stay the same.
func MarshalError(err error) ([]byte, error) { return json.Marshal(encodeError(err)) }

// UnmarshalError rebuilds an error MarshalError encoded.
func UnmarshalError(raw []byte) error {
	var w wireError
	if err := json.Unmarshal(raw, &w); err != nil {
		return fmt.Errorf("unreadable worker error: %w", err)
	}
	return decodeError(&w)
}

func decodeError(w *wireError) error {
	if w == nil {
		return nil
	}
	e := &remoteError{message: w.Message}
	if w.Kind == "diagnostics" {
		e.cause = &config.DiagnosticsError{Diagnostics: w.Diagnostics}
		return e
	}
	for _, k := range kinds {
		if k.name == w.Kind {
			e.cause = k.err
			break
		}
	}
	if w.Suspect {
		e.cause = errors.Join(e.cause, ErrSuspectBinary)
	}
	return e
}
