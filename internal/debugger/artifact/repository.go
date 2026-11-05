package artifact

import (
	"context"
	"time"
)

// Repository is the storage the platform depends on.
// store/pg (PostgreSQL and object storage) is the production implementation
// and store/memory serves tests; every implementation must
// pass the shared contract suite in internal/debugger/artifact/contract.
// Errors wrap the sentinels in types.go.
type Repository interface {
	Artifacts
	Runs
	Retention
	Queue
	Runtimes
	Presence
	Changes
	Jobs
	Builds
	Captures
	Audit
	Tokens
	Sessions
	Logins
	RoleBindings
	Projects
	// Close releases the repository; later calls fail with ErrClosed.
	Close()
}

// Artifacts holds saved sources: artifacts, their revision chains and the
// blobs they reference.
type Artifacts interface {
	Info() Info
	Limits() Limits
	// Usage is the physical bytes stored, counted against Limits.StorageBytes.
	Usage(ctx context.Context) (int64, error)
	Save(ctx context.Context, req SaveRequest) (*Saved, error)
	Get(ctx context.Context, id string) (Metadata, error)
	Revision(ctx context.Context, ref Ref) (*RevisionData, error)
	Revisions(ctx context.Context, req RevisionsRequest) (*RevisionPage, error)
	List(ctx context.Context, req ListRequest) (*Page, error)
}

// Runs is the run journal: runs with their bundles and evidence, recorded
// evaluations, suites and the tombstones of deleted runs and suites.
type Runs interface {
	// EnableRuns prepares the journal; it is idempotent and must precede
	// every other Runs call.
	EnableRuns(ctx context.Context) error
	// AdmitRun records a run's first publication and queues it, in one
	// transaction.
	AdmitRun(ctx context.Context, a Admission) (*RunRecord, error)
	SaveRun(ctx context.Context, id, project string, expected uint64, terminal bool, index RunIndex, metadata, evidence []byte) (*RunRecord, error)
	Runs(ctx context.Context) ([]RunRecord, error)
	// ListRuns returns a page of runs, newest first, that q selects. A
	// cursor of another query, or a malformed one, is ErrInvalidRequest.
	ListRuns(ctx context.Context, q RunQuery) (*RunPage, error)
	// IndexRun sets the index of a run stored before run indexes existed;
	// it changes nothing else, and an indexed run keeps its index.
	IndexRun(ctx context.Context, id string, index RunIndex) error
	// Run returns one run's record, or ErrNotFound for an unknown or deleted
	// run.
	Run(ctx context.Context, id string) (RunRecord, error)
	// AppendRunEvents keeps a batch of an unfinished run's live events for
	// instances that do not execute it. A batch of a later attempt drops the
	// earlier attempts' batches; a terminal run takes none (ErrConflict).
	AppendRunEvents(ctx context.Context, id string, b EventBatch) error
	// RunEvents returns the live event batches of the run's latest attempt
	// that end after sequence after, in order. A terminal run has none.
	RunEvents(ctx context.Context, id string, after int) ([]EventBatch, error)
	RunBundle(ctx context.Context, id string) ([]byte, error)
	RunEvidence(ctx context.Context, id string) ([]byte, error)
	SaveEvaluation(ctx context.Context, id, runID string, metadata, result, source []byte) (*EvaluationRecord, error)
	Evaluations(ctx context.Context, runID string) ([]EvaluationRecord, error)
	EvaluationResult(ctx context.Context, id string) (*EvaluationRecord, []byte, error)
	CheckSuiteCapacity(ctx context.Context, pending int) error
	SaveSuite(ctx context.Context, id, project string, metadata []byte) (*SuiteRecord, error)
	Suites(ctx context.Context) ([]SuiteRecord, error)
	DeleteRun(ctx context.Context, id string, at time.Time) error
	DeleteSuite(ctx context.Context, id string, at time.Time) error
	Tombstones(ctx context.Context) (runs, suites []Tombstone, err error)
}

// Captures is the capture journal: capture metadata and appended segments.
type Captures interface {
	// EnableCaptures prepares the journal; it is idempotent and must precede
	// every other Captures call.
	EnableCaptures(ctx context.Context) error
	SaveCapture(ctx context.Context, id, project string, expected uint64, closed, memoryOnly bool, metadata, segment []byte) (*CaptureRecord, error)
	Captures(ctx context.Context) ([]CaptureRecord, error)
	CaptureSegments(ctx context.Context, id string) ([][]byte, error)
	DeleteCapture(ctx context.Context, id string) error
}

// Audit is the append-only audit log, bounded by
// Limits.AuditEvents and Limits.AuditAge.
type Audit interface {
	// AppendAudit records an event and returns it with its sequence, one
	// higher than the previous event's.
	AppendAudit(ctx context.Context, e AuditEvent) (AuditEvent, error)
	// AuditEvents returns the events q selects, newest first, within
	// retention.
	AuditEvents(ctx context.Context, q AuditQuery) ([]AuditEvent, error)
}

// Tokens holds API tokens by the SHA-256 of their secret,
// bounded in number. A token's record never holds its secret.
type Tokens interface {
	// SaveToken records a new token; an existing ID or hash is a conflict.
	SaveToken(ctx context.Context, t TokenRecord) error
	// TokenByHash finds a token, revoked and expired ones included.
	TokenByHash(ctx context.Context, hash string) (TokenRecord, error)
	// Tokens lists a principal's tokens, or all for "", newest first.
	Tokens(ctx context.Context, principal string) ([]TokenRecord, error)
	// RevokeToken marks a token revoked; revoking twice changes nothing.
	RevokeToken(ctx context.Context, id string, at time.Time) (TokenRecord, error)
}

// Sessions holds web sign-in sessions by the SHA-256 of their
// secret; expired ones are dropped.
type Sessions interface {
	// SaveSession records a new session; an existing hash is a conflict.
	SaveSession(ctx context.Context, r SessionRecord) error
	// SessionByHash finds a session, an expired one included.
	SessionByHash(ctx context.Context, hash string) (SessionRecord, error)
	// DeleteSession ends a session; an unknown one is not an error.
	DeleteSession(ctx context.Context, hash string) error
}
