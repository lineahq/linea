package livecapture

import (
	"errors"

	"github.com/lineahq/linea/internal/fleetplugins/livecapture/translog"
)

// causeError carries the enum cause of a failure beside its error, so the
// cause survives to the transition log without any error text. It unwraps to
// err, so errors.Is still sees the sentinel.
type causeError struct {
	cause translog.Reason
	err   error
}

func (e *causeError) Error() string { return e.err.Error() }
func (e *causeError) Unwrap() error { return e.err }

// WithCause attaches an enum cause to err. A nil err stays nil.
func WithCause(cause translog.Reason, err error) error {
	if err == nil {
		return nil
	}
	return &causeError{cause: cause, err: err}
}

// CauseOf is the enum cause of a failure: the one attached with WithCause,
// otherwise the one the plugin's sentinel errors imply. A nil error is an
// orderly shutdown. It never reads error text.
func CauseOf(err error) translog.Reason {
	if err == nil {
		return translog.ReasonShutdown
	}
	var c *causeError
	switch {
	case errors.As(err, &c):
		return c.cause
	case errors.Is(err, ErrOwnerBusy):
		return translog.ReasonOwnerBusy
	case errors.Is(err, ErrInvalid):
		return translog.ReasonInvalidInput
	case errors.Is(err, ErrUnavailable):
		return translog.ReasonUnavailable
	}
	return translog.ReasonFailed
}
