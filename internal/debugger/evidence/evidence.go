// Package evidence projects a run's recorded report into bounded answers: the
// compact run summary and paged evidence queries. Projections
// are pure functions over the report; they never execute, replay or read
// anything else.
package evidence

import (
	"errors"

	"github.com/lineahq/linea/internal/debugger/engine"
)

// Run is what the projections read: how a run ended and its report.
type Run struct {
	ID string
	// End is empty while the run is queued or running.
	End    End
	Error  string
	Report *engine.Report
}

// End is how a run ended, as far as its evidence is concerned.
type End string

const (
	EndDone        End = "done"
	EndCancelled   End = "cancelled"
	EndInterrupted End = "interrupted"
	EndError       End = "error"
)

var (
	// ErrInvalidQuery: the query's family, parameters, page or cursor are
	// not acceptable; a cursor issued for another run, family or parameters
	// is invalid.
	ErrInvalidQuery = errors.New("invalid evidence query")
	// ErrNotFound: the run's report has no such stage, record or processor.
	ErrNotFound = errors.New("evidence not found")
)
