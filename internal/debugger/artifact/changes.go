package artifact

import (
	"context"
	"errors"
	"time"
)

// ErrExpired is a change cursor older than the retained changes: the reader
// reloads what it holds and reads on from a fresh cursor.
var ErrExpired = errors.New("change cursor expired")

// ChangeKind names what a change touched.
type ChangeKind string

const (
	// ChangeRun is a run's record or queue entry: admission, a published
	// state, a requeue or a cancellation request.
	ChangeRun ChangeKind = "run"
	// ChangeEvents is a batch of a running run's live events.
	ChangeEvents ChangeKind = "events"
	// ChangeEvaluation is an evaluation recorded for a run.
	ChangeEvaluation ChangeKind = "evaluation"
	// ChangeSuite is a suite's record.
	ChangeSuite ChangeKind = "suite"
	// ChangeDeletion is a run or suite tombstone.
	ChangeDeletion ChangeKind = "deletion"
	// ChangeJob is an interactive job's row.
	ChangeJob ChangeKind = "job"
	// ChangeBuild is a runtime build's row: admission, a claim, log lines,
	// a cancellation request or its end.
	ChangeBuild ChangeKind = "build"
	// ChangeDefaultRuntime is the default runtime's record.
	ChangeDefaultRuntime ChangeKind = "default_runtime"
	// ChangeRoleBindings is a write to the managed role bindings.
	ChangeRoleBindings ChangeKind = "role_bindings"
	// ChangeProjects is a write to the project set: projects, library
	// audiences or runtime availability.
	ChangeProjects ChangeKind = "projects"
)

// ChangeCursor is a position in the change feed: the
// writing transaction and the change's ID within the feed. The zero cursor
// precedes every change.
type ChangeCursor struct {
	XID uint64
	ID  int64
}

// Change says that the record of Kind named ID changed; the reader reads the
// record again. Run, events and evaluation changes name a run, suite changes a
// suite, deletion changes the run or suite deleted, job changes a job, build
// changes a build, and default runtime changes are named "default".
type Change struct {
	Kind   ChangeKind
	ID     string
	Cursor ChangeCursor
}

// MaxChanges bounds one read of the change feed.
const MaxChanges = 1000

// Changes is the repository's change feed, written in the
// transaction of the change it describes, so instances sharing a repository
// catch up on each other's writes.
type Changes interface {
	// ChangeHead returns a cursor no committed change follows: a reader that
	// takes it before loading what it holds misses no later change.
	ChangeHead(ctx context.Context) (ChangeCursor, error)
	// Changes returns, oldest first, up to limit changes after cursor that
	// no transaction still in progress can precede, and the cursor to read
	// on from. A cursor older than the retained changes is ErrExpired.
	Changes(ctx context.Context, after ChangeCursor, limit int) ([]Change, ChangeCursor, error)
	// ChangeHints returns a channel that receives, best effort, when a change
	// may have committed. Hints are never required for correctness: a reader
	// also reads the feed on its own schedule.
	ChangeHints() <-chan struct{}
}

// EventBatch is a consecutive run of one attempt's live events, encoded by
// the platform, from sequence First to Last.
type EventBatch struct {
	Attempt int
	First   int
	Last    int
	Events  []byte
}

// MaxRunEventBytes bounds the live events kept for one run; a batch beyond
// it is ErrLimit. The terminal evidence archive keeps every event.
const MaxRunEventBytes = 64 << 20

// Upkeep is the periodic housekeeping of a repository that instances share:
// trimming the change feed, tombstones, stale interactive jobs and expired
// worker records, and sweeping unreferenced objects. One handle, the leader,
// does it.
type Upkeep interface {
	// Lead reports whether this handle leads. A leader keeps the lead until
	// it closes or loses its connection; then the next handle to ask leads.
	Lead(ctx context.Context) (bool, error)
	// Housekeep does one round if this handle leads, and reports whether it
	// did. Interactive jobs posted before before go, and worker records that
	// expired before it. A tombstone goes once the change feed no longer
	// reports its deletion, so a deleted ID later reads as unknown.
	Housekeep(ctx context.Context, before time.Time) (bool, error)
}
