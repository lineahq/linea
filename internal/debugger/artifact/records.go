package artifact

import (
	"encoding/json"
	"time"
)

// AuditEvent is one operation's record: who, through which client and
// request, which operation and permission, in which scope, on which
// resources, and the outcome. It never holds request content.
type AuditEvent struct {
	Seq           uint64    `json:"seq"`
	Time          time.Time `json:"time"`
	Principal     string    `json:"principal"`
	PrincipalKind string    `json:"principal_kind"`
	Client        string    `json:"client"`
	RequestID     string    `json:"request_id,omitempty"`
	Operation     string    `json:"operation"`
	Permission    string    `json:"permission,omitempty"`
	Scope         string    `json:"scope,omitempty"`
	Resources     []string  `json:"resources,omitempty"`
	// Outcome is "allowed" or "denied"; Code is an allowed operation's error
	// code, empty on success.
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
}

// Audit outcomes.
const (
	AuditAllowed = "allowed"
	AuditDenied  = "denied"
)

// AuditQuery selects events newest first. Empty fields match every event;
// Before, when nonzero, keeps events with a lower sequence (the next page).
type AuditQuery struct {
	Before    uint64
	Limit     int
	Principal string
	Operation string
	Outcome   string
	// Scope selects the events that decided in one scope, such as
	// "project:payments".
	Scope string
	Since time.Time
}

// CaptureAllowance is the storage an open capture reserves until it closes.
const CaptureAllowance int64 = 16 << 20

// CaptureHistory bounds the captures a store keeps. Captures have no
// retention period: they stay until deleted.
const CaptureHistory = 1024

// CaptureSegment is one stored blob of a capture's records.
type CaptureSegment struct {
	Digest            string `json:"digest"`
	Bytes             int64  `json:"bytes"` // Stored bytes; digest covers this representation.
	Encoding          string `json:"encoding,omitempty"`
	UncompressedBytes int64  `json:"uncompressed_bytes,omitempty"`
}

// CaptureRecord is a capture session in the capture journal: its
// platform metadata and the segments of its records.
type CaptureRecord struct {
	ID         string           `json:"id"`
	Project    string           `json:"project_id"`
	Generation uint64           `json:"generation"`
	Closed     bool             `json:"closed"`
	MemoryOnly bool             `json:"memory_only"`
	Metadata   json.RawMessage  `json:"metadata"`
	Segments   []CaptureSegment `json:"segments"`
	Bytes      int64            `json:"bytes"` // Uncompressed bytes, including JSON/base64 overhead.
}

// Tombstone is a deleted run or suite: its ID and deletion time only.
// A deleted ID is never reused.
type Tombstone struct {
	ID      string    `json:"id"`
	Deleted time.Time `json:"deleted"`
}

// EvaluationRecord is independent of the immutable run record. Metadata holds
// application listing facts only; the original assertion source is a private blob.
type EvaluationRecord struct {
	ID           string          `json:"id"`
	RunID        string          `json:"run_id"`
	Ordinal      uint64          `json:"ordinal"`
	Metadata     json.RawMessage `json:"metadata"`
	ResultDigest string          `json:"result_digest"`
	ResultBytes  int64           `json:"result_bytes"`
	SourceDigest string          `json:"source_digest"`
	SourceBytes  int64           `json:"source_bytes"`
}

// RevisionsRequest pages one artifact's revisions, newest first.
type RevisionsRequest struct {
	ArtifactID string
	Limit      int
	Cursor     string
}

// RevisionPage is one page of an artifact's revision metadata, newest first.
type RevisionPage struct {
	Artifact   Metadata   `json:"artifact"`
	Items      []Revision `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// RunRecord is a storage envelope. Application lifecycle and evidence schemas
// belong to the platform, not this repository. Terminal records are immutable.
type RunRecord struct {
	ID           string          `json:"id"`
	Project      string          `json:"project_id"`
	Generation   uint64          `json:"generation"`
	Ordinal      uint64          `json:"ordinal"`
	Terminal     bool            `json:"terminal"`
	Metadata     json.RawMessage `json:"metadata"`
	Digest       string          `json:"evidence_digest,omitempty"`
	Bytes        int64           `json:"evidence_bytes,omitempty"`
	BundleDigest string          `json:"bundle_digest,omitempty"`
	BundleBytes  int64           `json:"bundle_bytes,omitempty"`
	// Kept protects the run from retention (Retention.KeepRun).
	Kept bool `json:"kept,omitempty"`
	// Index is what run lists filter on; nil for a run stored before run
	// indexes existed until the platform indexes it (IndexRun).
	Index *RunIndex `json:"index,omitempty"`
}

// RunIndex is what a run list filters on, written by the
// platform with each publication: the scenario and runtime a run executed,
// and its result. Each field is at most MaxRunIndexField bytes; Result is
// at most 64. The repository stores and matches them; it gives them no
// meaning.
type RunIndex struct {
	Scenario string `json:"scenario,omitempty"`
	Runtime  string `json:"runtime,omitempty"`
	Result   string `json:"result,omitempty"`
}

// MaxRunIndexField bounds RunIndex.Scenario and RunIndex.Runtime.
const MaxRunIndexField = 256

// Valid reports whether the index fits its bounds.
func (x RunIndex) Valid() bool {
	return len(x.Scenario) <= MaxRunIndexField && len(x.Runtime) <= MaxRunIndexField && len(x.Result) <= 64
}

// RunQuery selects a page of runs, newest first. Projects limits it to
// these projects (nil is every project); a non-empty Scenario, Runtime or
// Result must equal the run's index field. Unfinished selects only runs
// not yet terminal, and Unindexed only runs stored without an index: what a
// server reads when it opens. Limit is 1 to MaxRunPage
// (nonpositive is DefaultRunPage); Cursor continues an earlier page of the
// same query.
type RunQuery struct {
	Projects   []string
	Scenario   string
	Runtime    string
	Result     string
	Unfinished bool
	Unindexed  bool
	Limit      int
	Cursor     string
}

// Run page sizes.
const (
	DefaultRunPage = 50
	MaxRunPage     = 500
)

// RunPage is one page of runs, newest first. NextCursor is empty on the
// last page.
type RunPage struct {
	Runs       []RunRecord
	NextCursor string
}

// SessionRecord is a signed-in user's web session without its secret.
// Groups are the identity provider's when the user signed in.
type SessionRecord struct {
	Hash      string    `json:"hash"`
	Principal string    `json:"principal"`
	Name      string    `json:"name,omitempty"`
	Groups    []string  `json:"groups,omitempty"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
}

// SuiteRecord is an immutable journal envelope. Outcomes live on member runs,
// so there is no terminal publication or separate suite evidence blob.
type SuiteRecord struct {
	ID       string          `json:"id"`
	Project  string          `json:"project_id"`
	Ordinal  uint64          `json:"ordinal"`
	Metadata json.RawMessage `json:"metadata"`
}

// Token kinds: a user's personal token, or a service principal's.
const (
	TokenUser    = "user"
	TokenService = "service"
)

// TokenRecord is an API token without its secret. Groups are the user's
// identity-provider groups when the token was created; a service token has
// none.
type TokenRecord struct {
	ID        string     `json:"id"`
	Hash      string     `json:"hash"`
	Principal string     `json:"principal"`
	Kind      string     `json:"kind"`
	Name      string     `json:"name,omitempty"`
	Groups    []string   `json:"groups,omitempty"`
	Label     string     `json:"label"`
	CreatedBy string     `json:"created_by"`
	Created   time.Time  `json:"created"`
	Expires   time.Time  `json:"expires"`
	Revoked   *time.Time `json:"revoked,omitempty"`
}
