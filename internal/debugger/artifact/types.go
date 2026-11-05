// Package artifact defines the debugger's storage contract: the Repository
// interface, its records and errors, and the limits every implementation
// enforces (store/pg in production, store/memory in tests). The shared
// contract suite is artifact/contract.
package artifact

import (
	"encoding/json"
	"errors"
	"time"
)

// SchemaVersion 3 gives every artifact a project.
const SchemaVersion = 3

type Kind string

const (
	Configuration Kind = "configuration"
	Fixture       Kind = "fixture"
	Scenario      Kind = "scenario"
	Setup         Kind = "setup"
	// Manifest is an OCB manifest a runtime is built from.
	Manifest Kind = "manifest"
)

// Valid reports whether k is one of the kinds above.
func (k Kind) Valid() bool {
	return k == Configuration || k == Fixture || k == Scenario || k == Setup || k == Manifest
}

var (
	ErrInvalidRequest    = errors.New("invalid artifact request")
	ErrNotFound          = errors.New("artifact or revision not found")
	ErrConflict          = errors.New("artifact conflict")
	ErrLocked            = errors.New("workspace already has an owner")
	ErrClosed            = errors.New("artifact repository is closed")
	ErrCorrupt           = errors.New("workspace integrity check failed")
	ErrUnsupportedSchema = errors.New("unsupported workspace schema")
	ErrLimit             = errors.New("workspace storage limit exceeded")
	ErrStorage           = errors.New("workspace storage operation failed")
)

type Info struct {
	ID         string `json:"id"`
	Schema     int    `json:"schema"`
	Generation uint64 `json:"generation"`
}

type Ref struct {
	WorkspaceID string `json:"workspace_id"`
	ArtifactID  string `json:"artifact_id"`
	RevisionID  string `json:"revision_id"`
}

type Metadata struct {
	WorkspaceID string    `json:"workspace_id"`
	ID          string    `json:"id"`
	Project     string    `json:"project_id"`
	Kind        Kind      `json:"kind"`
	Label       string    `json:"label"`
	Generation  uint64    `json:"generation"`
	Head        string    `json:"head_revision_id"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
}

type Revision struct {
	Ref        Ref       `json:"ref"`
	Generation uint64    `json:"generation"`
	Parent     string    `json:"parent_revision_id,omitempty"`
	Digest     string    `json:"content_digest"`
	Bytes      int64     `json:"content_bytes"`
	Created    time.Time `json:"created"`
	Actor      string    `json:"actor"`
	Origin     *Origin   `json:"origin,omitempty"`
}

// Origin records the provenance supplied by the platform, never by a client.
type Origin struct {
	Kind         string          `json:"kind"`
	BaseDigest   string          `json:"base_digest"`
	BaseRevision string          `json:"base_revision_id,omitempty"`
	RunID        string          `json:"run_id,omitempty"`
	Edit         json.RawMessage `json:"edit,omitempty"`
}

// SaveRequest creates a document when ArtifactID is empty. Updates require
// both the current artifact generation and head; Kind cannot change. Content
// is arbitrary original bytes (base64 in JSON), including invalid drafts.
type SaveRequest struct {
	WorkspaceID        string  `json:"workspace_id"`
	ArtifactID         string  `json:"artifact_id,omitempty"`
	ExpectedGeneration uint64  `json:"expected_generation"`
	ExpectedHead       string  `json:"expected_head_revision_id,omitempty"`
	Kind               Kind    `json:"kind"`
	Label              string  `json:"label"`
	Content            []byte  `json:"content_base64"`
	Actor              string  `json:"-"`
	Origin             *Origin `json:"-"`
	// Project is required for a new artifact; an update may omit it but
	// cannot change it.
	Project string `json:"-"`
}

type Saved struct {
	Artifact Metadata `json:"artifact"`
	Revision Revision `json:"revision"`
}

type RevisionData struct {
	Revision Revision `json:"revision"`
	Content  []byte   `json:"content_base64"`
}

type Conflict struct {
	Current *Metadata
}

func (e *Conflict) Error() string { return ErrConflict.Error() }
func (e *Conflict) Unwrap() error { return ErrConflict }

type ListRequest struct {
	Kind Kind
	// Projects limits the page to these projects; nil lists every project.
	Projects []string
	Limit    int
	Cursor   string
}

type Page struct {
	Workspace  Info       `json:"workspace"`
	Items      []Metadata `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// Limits bound source blobs, metadata, run archives and physical storage,
// including orphans. Nonpositive values use defaults. Collector capture memory
// and query response budgets are separate application policies.
type Limits struct {
	BlobBytes    int64
	CatalogBytes int64
	StorageBytes int64
	Revisions    int
	// RunBytes bounds one terminal evidence archive.
	RunBytes int64
	// BundleBytes bounds one encoded private execution bundle.
	BundleBytes int64
	// BuildBytes is the storage each queued or running build reserves for
	// its binary until it stores one or ends, so admitted builds do not find
	// the space taken when they finish; a larger binary is still stored when
	// the space is free. Zero reserves none. Only a store that holds build
	// binaries (the PostgreSQL store) reserves it.
	BuildBytes int64
	// Runs bounds the runs the in-memory store keeps: admission beyond it is
	// refused. The PostgreSQL store has no such bound; retention bounds its
	// history (Retention).
	Runs int
	// EvaluationsPerRun bounds immutable assertion evaluations for one run.
	EvaluationsPerRun int
	// Suites bounds durable suite admission records; they are never evicted.
	Suites int
	// AuditEvents and AuditAge bound the audit log's retention.
	AuditEvents int
	AuditAge    time.Duration
}

// Defaults returns l with every nonpositive field replaced by its default.
func (l Limits) Defaults() Limits {
	if l.BlobBytes <= 0 {
		l.BlobBytes = 16 << 20
	}
	if l.CatalogBytes <= 0 {
		l.CatalogBytes = 8 << 20
	}
	if l.StorageBytes <= 0 {
		l.StorageBytes = 256 << 20
	}
	if l.Revisions <= 0 {
		l.Revisions = 10000
	}
	if l.RunBytes <= 0 {
		l.RunBytes = 16 << 20
	}
	if l.BundleBytes <= 0 {
		l.BundleBytes = 64 << 20
	}
	if l.Runs <= 0 {
		l.Runs = 100
	}
	if l.EvaluationsPerRun <= 0 {
		l.EvaluationsPerRun = 100
	}
	if l.Suites <= 0 {
		l.Suites = 1000
	}
	if l.AuditEvents <= 0 {
		l.AuditEvents = 100000
	}
	if l.AuditAge <= 0 {
		l.AuditAge = 90 * 24 * time.Hour
	}
	return l
}
