package artifact

import (
	"context"
	"strings"
	"time"
)

// QueueState is an unfinished run's place in scheduling. A
// terminal run has none.
type QueueState string

const (
	// QueueQueued waits for a worker of its runtime.
	QueueQueued QueueState = "queued"
	// QueueAssigned is held by a worker's lease and not yet started.
	QueueAssigned QueueState = "assigned"
	// QueueRunning is held by a worker's lease and executing.
	QueueRunning QueueState = "running"
)

// Admission is a run's first record: its metadata, its private input bundle
// (empty for a run with no prepared inputs) and what scheduling needs. The
// run is queued in the same transaction.
type Admission struct {
	ID       string
	Project  string
	Metadata []byte
	Bundle   []byte
	// Runtime names the workers that may take the run.
	Runtime string
	// Binary pins a run of a server-built runtime to the binary it was
	// admitted with, which pool workers fetch; nil for a run
	// only dedicated workers of Runtime take.
	Binary *RunBinary
	// Priority orders the queue, lowest first, then admission order.
	Priority int
	// Index is the run's first index (RunIndex).
	Index RunIndex
}

// RunBinary is the runtime binary a run was admitted with: a stored
// runtime binary's digest and size, and its platform ("linux/amd64"). While
// the run is unfinished the binary is a referenced object.
type RunBinary struct {
	Digest   string
	Bytes    int64
	Platform string
}

// Valid reports whether b names a binary within MaxRuntimeBinary and a
// platform "os/arch".
func (b RunBinary) Valid() bool {
	os, arch, ok := strings.Cut(b.Platform, "/")
	return ValidRuntimeIdentity(b.Digest) && b.Bytes > 0 && b.Bytes <= MaxRuntimeBinary && platformName(os) && platformName(arch) && ok
}

// Assignment is a run held by a worker's lease. Attempt is 1 for the first
// assignment and one more after each requeue.
type Assignment struct {
	RunID   string
	Runtime string
	// Binary is the run's pinned binary (Admission.Binary), if any.
	Binary  *RunBinary
	Worker  string
	State   QueueState
	Attempt int
	// Released counts the attempts a draining instance handed back
	// unfinished (Release); they do not count toward a run's attempt limit.
	Released        int
	Expires         time.Time
	CancelRequested bool
	// Generation is the run record's when the assignment was read; the
	// holder publishes the run's next state against it.
	Generation uint64
}

// Queue schedules admitted runs. Times come from the caller,
// so every instance judges leases against the same clock it renews them with.
// A terminal SaveRun takes a run off the queue.
type Queue interface {
	// Claim assigns up to n queued runs of runtime to worker, lowest priority
	// first and then in admission order, each leased until expires. Runs
	// another claim holds are skipped, never waited for.
	Claim(ctx context.Context, worker, runtime string, n int, expires time.Time) ([]Assignment, error)
	// ClaimBinary is Claim for a pool worker: it assigns only
	// queued runs of runtime pinned to a binary of platform.
	ClaimBinary(ctx context.Context, worker, runtime, platform string, n int, expires time.Time) ([]Assignment, error)
	// Start moves an assignment to running. A lease the worker no longer
	// holds is a conflict.
	Start(ctx context.Context, a Assignment) error
	// Renew extends every lease worker holds until expires and returns them,
	// with any cancellation requested since.
	Renew(ctx context.Context, worker string, expires time.Time) ([]Assignment, error)
	// RequestCancel marks an unfinished run. A run no worker holds leaves the
	// queue at once and is returned with an empty Worker, for the caller to
	// finish; an assigned or running one keeps its lease until its worker
	// stops. A terminal run is a conflict and an unknown one ErrNotFound.
	RequestCancel(ctx context.Context, id string) (Assignment, error)
	// Expired returns the assignments whose lease ended before now.
	Expired(ctx context.Context, now time.Time) ([]Assignment, error)
	// Requeue returns an expired assignment's run to the queue, where its next
	// claim is its next attempt, and returns the run's new generation. It is a conflict unless
	// the same worker and attempt still hold a lease that ended before now,
	// so of several instances only one requeues it.
	Requeue(ctx context.Context, a Assignment, now time.Time) (uint64, error)
	// Release returns a held run to the queue before its lease ends, as a
	// instance that shuts down does, counting the attempt as released, and
	// returns the run's new generation. It is a conflict unless the same
	// worker and attempt still hold the run.
	Release(ctx context.Context, a Assignment) (uint64, error)
	// Assignment returns an unfinished run's queue entry; Worker is empty
	// while it is queued. A terminal or unknown run is ErrNotFound.
	Assignment(ctx context.Context, id string) (Assignment, error)
}
