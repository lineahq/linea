package artifact

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// BuildState is where a runtime build is.
type BuildState string

const (
	// BuildQueued waits for a builder.
	BuildQueued BuildState = "queued"
	// BuildRunning is held by a builder's lease.
	BuildRunning BuildState = "running"
	// BuildDone produced a binary, or reused an identical build's.
	BuildDone BuildState = "done"
	// BuildFailed ended without a binary.
	BuildFailed BuildState = "failed"
	// BuildCancelled was cancelled before it ended.
	BuildCancelled BuildState = "cancelled"
)

// Terminal reports whether a build in state s has ended.
func (s BuildState) Terminal() bool {
	return s == BuildDone || s == BuildFailed || s == BuildCancelled
}

// Build bounds: a build's log keeps its last MaxBuildLogLines lines of at
// most MaxBuildLogLine bytes each; the inputs of one build are at most
// MaxBuildInputBytes together.
const (
	MaxBuildLogLines   = 2000
	MaxBuildLogLine    = 4096
	MaxBuildInputBytes = 64<<20 + 1<<20
	MaxBuildSources    = 32
	MaxBuildPage       = 500
	DefaultBuildPage   = 50
	MaxBuildSpec       = 64 << 10
	MaxBuildResult     = 1 << 20
)

// BuildInput is one stored input of a build: a module's source archive, or
// the manifest (Module empty).
type BuildInput struct {
	Module string
	Digest string
	Bytes  int64
}

// BuildAdmission is a build's first record. Key identifies its inputs (the
// manifest's and sources' digests, the Go image and the builder version):
// builds of the same Key produce the same binary. Spec is the platform's
// encoded build description, which never holds a credential.
type BuildAdmission struct {
	ID        string
	Principal string
	Name      string
	Key       string
	Spec      []byte
	Manifest  []byte
	// Sources are the uploaded module archives, by module.
	Sources []BuildSource
	// Created is when the build was requested.
	Created time.Time
}

// BuildSource is an uploaded module archive.
type BuildSource struct {
	Module  string
	Archive []byte
}

// BuildLimits bound unfinished builds: per principal and in total.
type BuildLimits struct {
	PerPrincipal int
	Total        int
}

// BuildRecord is a build as stored.
type BuildRecord struct {
	ID        string
	Principal string
	Name      string
	Key       string
	Spec      []byte
	State     BuildState
	// Inputs are the manifest (Module empty) and the sources, by module.
	Inputs []BuildInput
	// ReusedFrom is the build whose output a reused build took.
	ReusedFrom string
	// Builder holds a running build's lease until Expires; Attempt counts
	// its claims.
	Builder         string
	Attempt         int
	Expires         time.Time
	CancelRequested bool
	// LogLines counts every line the build logged; the store keeps the
	// last MaxBuildLogLines.
	LogLines int
	// Result is the platform's encoded outcome; Binary the built binary's
	// digest and size once done.
	Result       []byte
	BinaryDigest string
	BinaryBytes  int64
	Created      time.Time
	Started      time.Time
	Finished     time.Time
}

// BuildOutcome is how a builder ends a build it holds: done with a binary
// already stored, failed, or cancelled.
type BuildOutcome struct {
	State        BuildState
	Result       []byte
	BinaryDigest string
	BinaryBytes  int64
}

// BuildQuery selects builds, newest first: Principal empty is everyone's.
// Cursor is the previous page's NextCursor.
type BuildQuery struct {
	Principal string
	Limit     int
	Cursor    string
}

// BuildPage is a page of builds and the cursor of the next, empty after
// the last.
type BuildPage struct {
	Builds     []BuildRecord
	NextCursor string
}

// BuildCounts are how many builds are queued and running, for a builder
// autoscaler.
type BuildCounts struct {
	Queued, Running int
}

// Builds is the runtime build queue: durable builds that
// builders claim under leases, as workers claim runs. Times come from the
// caller. Every write records a ChangeBuild for the build.
type Builds interface {
	// AdmitBuild stores the inputs and queues the build. When a done build
	// of the same Key exists, the build is recorded done at once and takes
	// that build's binary and result (ReusedFrom). An existing ID is
	// ErrConflict; more unfinished builds than limits allow, for the
	// principal or in total, is ErrLimit.
	AdmitBuild(ctx context.Context, a BuildAdmission, limits BuildLimits) (BuildRecord, error)
	// Build returns a build, or ErrNotFound.
	Build(ctx context.Context, id string) (BuildRecord, error)
	// BuildManifest returns the manifest a build was admitted with, as
	// sent, or ErrNotFound.
	BuildManifest(ctx context.Context, id string) ([]byte, error)
	// ListBuilds returns a page of the builds q selects. A cursor of
	// another query is ErrInvalidRequest.
	ListBuilds(ctx context.Context, q BuildQuery) (*BuildPage, error)
	// BuildLog returns the kept log lines from line from on (counting every
	// line the build logged) and the number of the first one returned.
	BuildLog(ctx context.Context, id string, from int) (lines []string, first int, err error)
	// ClaimBuilds leases up to n queued builds to builder, oldest first,
	// until expires; each claim counts an attempt.
	ClaimBuilds(ctx context.Context, builder string, n int, now, expires time.Time) ([]BuildRecord, error)
	// RenewBuilds extends builder's leases until expires and returns its
	// builds, with any cancellation requested since.
	RenewBuilds(ctx context.Context, builder string, expires time.Time) ([]BuildRecord, error)
	// AppendBuildLog adds lines to a build that builder holds in attempt,
	// keeping the last MaxBuildLogLines. A lease it no longer holds is
	// ErrConflict.
	AppendBuildLog(ctx context.Context, id, builder string, attempt int, lines []string) error
	// FinishBuild ends a build that builder holds in attempt. A lease it no
	// longer holds is ErrConflict.
	FinishBuild(ctx context.Context, id, builder string, attempt int, o BuildOutcome, now time.Time) (BuildRecord, error)
	// CancelBuild cancels a queued build at once and flags a running one
	// for its builder. A terminal build is ErrConflict, an unknown one
	// ErrNotFound.
	CancelBuild(ctx context.Context, id string, now time.Time) (BuildRecord, error)
	// ExpireBuilds returns the running builds whose lease ended before now
	// to the queue, or fails them once they used maxAttempts, with result.
	// It returns the builds it changed.
	ExpireBuilds(ctx context.Context, now time.Time, maxAttempts int, failed []byte) ([]BuildRecord, error)
	// TrimBuilds deletes the terminal builds beyond the newest keep and
	// returns how many it deleted; their inputs become unreferenced.
	TrimBuilds(ctx context.Context, keep int) (int, error)
	// CountBuilds counts the queued and running builds.
	CountBuilds(ctx context.Context) (BuildCounts, error)
}

// ValidateBuildAdmission reports why a repository refuses a, as
// ErrInvalidRequest or ErrLimit.
func ValidateBuildAdmission(a BuildAdmission) error {
	switch {
	case a.ID == "" || len(a.ID) > 128 || !printable(a.ID):
		return fmt.Errorf("%w: build ID must be 1 to 128 printable characters", ErrInvalidRequest)
	case a.Principal == "" || len(a.Principal) > 256 || !printable(a.Principal):
		return fmt.Errorf("%w: build principal must be 1 to 256 printable characters", ErrInvalidRequest)
	case a.Name == "" || len(a.Name) > 128 || !printable(a.Name):
		return fmt.Errorf("%w: build name must be 1 to 128 printable characters", ErrInvalidRequest)
	case !ValidRuntimeIdentity(a.Key):
		return fmt.Errorf("%w: build key must be sha256:<64 lowercase hex digits>", ErrInvalidRequest)
	case len(a.Spec) == 0 || len(a.Spec) > MaxBuildSpec:
		return fmt.Errorf("%w: build spec must be 1 to %d bytes", ErrInvalidRequest, MaxBuildSpec)
	case len(a.Manifest) == 0:
		return fmt.Errorf("%w: a build needs a manifest", ErrInvalidRequest)
	case len(a.Sources) > MaxBuildSources:
		return fmt.Errorf("%w: at most %d sources per build", ErrLimit, MaxBuildSources)
	case a.Created.IsZero():
		return fmt.Errorf("%w: a build needs a creation time", ErrInvalidRequest)
	}
	total := len(a.Manifest)
	seen := map[string]bool{}
	for _, s := range a.Sources {
		if s.Module == "" || len(s.Module) > 512 || !printable(s.Module) || seen[s.Module] {
			return fmt.Errorf("%w: each source names one module once", ErrInvalidRequest)
		}
		if len(s.Archive) == 0 {
			return fmt.Errorf("%w: the source of %s is empty", ErrInvalidRequest, s.Module)
		}
		seen[s.Module] = true
		total += len(s.Archive)
	}
	if total > MaxBuildInputBytes {
		return fmt.Errorf("%w: a build's inputs exceed %d bytes", ErrLimit, MaxBuildInputBytes)
	}
	return nil
}

// ValidateBuildOutcome reports why a repository refuses o.
func ValidateBuildOutcome(o BuildOutcome) error {
	switch {
	case !o.State.Terminal():
		return fmt.Errorf("%w: a build ends done, failed or cancelled", ErrInvalidRequest)
	case len(o.Result) > MaxBuildResult:
		return fmt.Errorf("%w: build result exceeds %d bytes", ErrInvalidRequest, MaxBuildResult)
	case (o.State == BuildDone) != (o.BinaryDigest != ""):
		return fmt.Errorf("%w: exactly a done build has a binary", ErrInvalidRequest)
	case o.BinaryDigest != "" && (!ValidRuntimeIdentity(o.BinaryDigest) || o.BinaryBytes <= 0):
		return fmt.Errorf("%w: build binary must be a sha256 digest with a size", ErrInvalidRequest)
	}
	return nil
}

// ClipBuildLine makes a log line storable: valid UTF-8 without NUL, at
// most MaxBuildLogLine bytes.
func ClipBuildLine(line string) string {
	if len(line) > MaxBuildLogLine {
		line = line[:MaxBuildLogLine]
	}
	return strings.ReplaceAll(strings.ToValidUTF8(line, "\uFFFD"), "\x00", "")
}
