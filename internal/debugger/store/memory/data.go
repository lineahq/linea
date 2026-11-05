// Package memory is an in-memory artifact.Repository. It follows the shared
// repository contract (errors, limits, ID formats, ordering, tombstones, audit
// sequence and retention) without a database or object storage, and is safe
// for concurrent use.
//
// It is for tests and development only: nothing survives the process, and a
// Data is not shared between processes. Production uses store/pg.
package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

const (
	runSchema     = 5
	captureSchema = 4
)

// catalog is the source catalog. Its encoded size counts toward the storage
// limit and is bounded by Limits.CatalogBytes. The PostgreSQL store has no
// catalog and no such bound.
type catalog struct {
	Workspace     artifact.Info                `json:"workspace"`
	Artifacts     map[string]artifact.Metadata `json:"artifacts"`
	Revisions     map[string]artifact.Revision `json:"revisions"`
	RunSchema     int                          `json:"run_schema,omitempty"`
	CaptureSchema int                          `json:"capture_schema,omitempty"`
}

// Data is the storage behind one or more Stores. Opening it again with Open
// sees everything an earlier Store wrote, which stands in for reopening a
// persistent store.
type Data struct {
	mu       sync.Mutex
	cat      catalog
	catSize  int64
	blobs    map[string][]byte // by digest
	runs     *runCatalog
	runsSize int64
	admitted map[string]time.Time          // when each run was admitted, for retention
	queue    entries                       // unfinished admitted runs by ID
	pins     map[string]artifact.RunBinary // binary of each unfinished run admitted with one, by run ID
	runtimes runtimeCatalog
	// defaultRuntime is nil until one is set.
	defaultRuntime *artifact.DefaultRuntime
	captures       *captureCatalog
	capSize        int64
	audit          []artifact.AuditEvent
	auditEnd       uint64
	tokens         []artifact.TokenRecord
	sessions       []artifact.SessionRecord
	logins         map[string]artifact.LoginRecord
	roleBindings   artifact.RoleBindingSet
	projects       artifact.ProjectSet
	feed           feed
	jobs           jobQueue
	builds         buildQueue
	presence       map[string]presence // by instance
}

// NewData returns empty storage with a new workspace ID.
func NewData() *Data {
	d := &Data{blobs: map[string][]byte{}, admitted: map[string]time.Time{}, queue: entries{}, pins: map[string]artifact.RunBinary{}, runtimes: runtimeCatalog{}, logins: map[string]artifact.LoginRecord{}}
	d.cat = catalog{Workspace: artifact.Info{ID: newID(), Schema: artifact.SchemaVersion},
		Artifacts: map[string]artifact.Metadata{}, Revisions: map[string]artifact.Revision{}}
	d.catSize = encodedSize(d.cat)
	return d
}

// Store is one open handle on a Data. It implements artifact.Repository.
type Store struct {
	d      *Data
	limits artifact.Limits
	closed bool          // guarded by d.mu
	hints  chan struct{} // guarded by d.mu
}

var _ artifact.Repository = (*Store)(nil)

// New returns a Store on new, empty storage.
func New(limits artifact.Limits) *Store { return Open(NewData(), limits) }

// Open returns a Store on d with its own limits (nonpositive values use the
// defaults). Several Stores may be open on one Data at once.
func Open(d *Data, limits artifact.Limits) *Store {
	return &Store{d: d, limits: limits.Defaults()}
}

// Reopen returns another Store on the same data, as reconnecting to a
// persistent store would.
func (s *Store) Reopen(limits artifact.Limits) *Store { return Open(s.d, limits) }

// Data returns the storage the Store was opened on.
func (s *Store) Data() *Data { return s.d }

func invalid(message string) error {
	return errors.Join(artifact.ErrInvalidRequest, errors.New(message))
}

func newID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

func validID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16 && id == strings.ToLower(id)
}

func validRunID(id string) bool {
	h := strings.TrimPrefix(id, "run-")
	b, err := hex.DecodeString(h)
	return strings.HasPrefix(id, "run-") && err == nil && len(b) == 8 && h == strings.ToLower(h)
}

func validSuiteID(id string) bool {
	return strings.HasPrefix(id, "suite-") && validRunID("run-"+strings.TrimPrefix(id, "suite-"))
}

func validEvaluationID(id string) bool {
	return strings.HasPrefix(id, "eval-") && validRunID("run-"+strings.TrimPrefix(id, "eval-"))
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func encodedSize(v any) int64 {
	raw, _ := json.Marshal(v)
	return int64(len(raw))
}

func decodeStrict(raw []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func cloneBytes(b []byte) []byte { return append([]byte(nil), b...) }

// ready reports why the handle cannot be used; the caller holds d.mu.
func (s *Store) ready(ctxErr error) error {
	if ctxErr != nil {
		return ctxErr
	}
	if s.closed {
		return artifact.ErrClosed
	}
	return nil
}

// Close ends this handle. The data stays for Open to find again.
func (s *Store) Close() {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	s.closed = true
}

// Limits returns the effective limits.
func (s *Store) Limits() artifact.Limits { return s.limits }

// Info returns the workspace identity.
func (s *Store) Info() artifact.Info {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	return s.d.cat.Workspace
}

// storeBlob keeps content under its digest.
func (d *Data) storeBlob(dg string, content []byte) {
	if _, ok := d.blobs[dg]; !ok {
		d.blobs[dg] = cloneBytes(content)
	}
}

// measure is the bytes stored: blobs and the encoded catalogs. The audit log,
// tokens and sessions have their own bounds.
func (d *Data) measure(limit int64) (int64, error) {
	used := d.catSize + d.runsSize + d.capSize
	for _, b := range d.blobs {
		used += int64(len(b))
	}
	if used > limit {
		return 0, artifact.ErrLimit
	}
	return used, nil
}

func (s *Store) usage() (int64, error) { return s.d.measure(s.limits.StorageBytes) }

func (s *Store) checkSpace(used, additional, reserved int64) error {
	if used > s.limits.StorageBytes || additional > s.limits.StorageBytes-used || reserved > s.limits.StorageBytes-used-additional {
		return artifact.ErrLimit
	}
	return nil
}

func (s *Store) reserved(c *runCatalog) int64 {
	return s.runReserved(c) + captureReserved(s.d.captures)
}

// sweep removes blobs no revision, run, evaluation, capture, build,
// catalogued runtime or unfinished run's binary references.
func (d *Data) sweep() {
	live := d.live(nil)
	for dg := range d.blobs {
		if !live[dg] {
			delete(d.blobs, dg)
		}
	}
}

// live is the set of digests a record references, leaving out the runs in
// skip and their evaluations.
func (d *Data) live(skip map[string]bool) map[string]bool {
	live := map[string]bool{}
	for _, r := range d.cat.Revisions {
		live[r.Digest] = true
	}
	if d.captures != nil {
		for _, r := range d.captures.Records {
			for _, seg := range r.Segments {
				live[seg.Digest] = true
			}
		}
	}
	for _, b := range d.builds.byID {
		for _, in := range b.Inputs {
			live[in.Digest] = true
		}
		if b.BinaryDigest != "" {
			live[b.BinaryDigest] = true
		}
	}
	for _, e := range d.runtimes {
		if e.Binary != nil {
			live[e.Binary.Digest] = true
		}
	}
	// An unfinished run keeps the binary it was admitted with.
	for id, b := range d.pins {
		if !skip[id] {
			live[b.Digest] = true
		}
	}
	if d.runs != nil {
		for id, r := range d.runs.Records {
			if !skip[id] {
				live[r.Digest], live[r.BundleDigest] = true, true
			}
		}
		for _, r := range d.runs.Evaluations {
			if !skip[r.RunID] {
				live[r.ResultDigest], live[r.SourceDigest] = true, true
			}
		}
	}
	return live
}

// Usage is the bytes stored, which may exceed the storage limit when the
// limit was lowered after they were written.
func (s *Store) Usage(ctx context.Context) (int64, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return 0, err
	}
	return s.d.measure(1<<63 - 1)
}
