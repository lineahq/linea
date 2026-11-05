package artifact

import (
	"context"
	"fmt"
	"time"
)

// JobKind is what an interactive job asks of a worker.
type JobKind string

const (
	// JobInspect completes an inspection; Input is an engine.InspectionInput.
	JobInspect JobKind = "inspect"
	// JobOTTL completes an OTTL preview; Input is an engine.OTTLInput.
	JobOTTL JobKind = "ottl"
)

// JobState is where an interactive job is.
type JobState string

const (
	JobQueued  JobState = "queued"
	JobClaimed JobState = "claimed"
	JobDone    JobState = "done"
)

// JobRecord is an interactive request that an instance without
// a free worker of Runtime posts for any instance that has one. It keeps no
// history: the requester deletes it once answered.
type JobRecord struct {
	ID      string
	Runtime string
	Kind    JobKind
	Input   []byte
	State   JobState
	// Worker claimed the job; Result or Error is its answer once done.
	Worker  string
	Result  []byte
	Error   []byte
	Created time.Time
}

// MaxJobBytes bounds a job's input and each of its answers.
const MaxJobBytes = 16 << 20

// Jobs holds interactive jobs. Posting and finishing a job record a job
// change, which wakes the instances that claim and the requester that waits.
type Jobs interface {
	// PostJob queues j by ID, Runtime, Kind and Input. An existing ID is
	// ErrConflict.
	PostJob(ctx context.Context, j JobRecord) error
	// ClaimJobs claims up to n queued jobs of runtime for worker, oldest first.
	ClaimJobs(ctx context.Context, worker, runtime string, n int) ([]JobRecord, error)
	// FinishJob records worker's answer to a job it claimed: result, or an
	// encoded error. A job worker does not hold is ErrConflict; a deleted
	// one ErrNotFound.
	FinishJob(ctx context.Context, id, worker string, result, failure []byte) error
	// Job returns a job, or ErrNotFound.
	Job(ctx context.Context, id string) (JobRecord, error)
	// WithdrawJob deletes a job no worker claimed yet: a claimed one is
	// ErrConflict, a deleted one ErrNotFound.
	WithdrawJob(ctx context.Context, id string) error
	// DeleteJob deletes a job in any state; a deleted one is not an error.
	DeleteJob(ctx context.Context, id string) error
	// TrimJobs deletes the jobs posted before before, whose requesters gave up.
	TrimJobs(ctx context.Context, before time.Time) error
}

// ValidateJob reports why a repository refuses to post j, as
// ErrInvalidRequest.
func ValidateJob(j JobRecord) error {
	switch {
	case j.ID == "" || len(j.ID) > 128 || !printable(j.ID):
		return fmt.Errorf("%w: job ID must be 1 to 128 printable characters", ErrInvalidRequest)
	case j.Runtime == "" || len(j.Runtime) > 256 || !printable(j.Runtime):
		return fmt.Errorf("%w: job runtime must be 1 to 256 printable characters", ErrInvalidRequest)
	case j.Kind != JobInspect && j.Kind != JobOTTL:
		return fmt.Errorf("%w: unknown job kind %q", ErrInvalidRequest, j.Kind)
	case len(j.Input) == 0 || len(j.Input) > MaxJobBytes:
		return fmt.Errorf("%w: job input must be 1 to %d bytes", ErrInvalidRequest, MaxJobBytes)
	}
	return nil
}

// ValidateJobAnswer reports why a repository refuses a job's answer.
func ValidateJobAnswer(result, failure []byte) error {
	if (len(result) == 0) == (len(failure) == 0) || len(result) > MaxJobBytes || len(failure) > MaxJobBytes {
		return fmt.Errorf("%w: a job's answer is a result or an error of at most %d bytes", ErrInvalidRequest, MaxJobBytes)
	}
	return nil
}
