package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

const maxRunMetadata = 8192

type runCatalog struct {
	Schema        int                                  `json:"schema"`
	Workspace     string                               `json:"workspace_id"`
	Records       map[string]artifact.RunRecord        `json:"records"`
	Evaluations   map[string]artifact.EvaluationRecord `json:"evaluations,omitempty"`
	Suites        map[string]artifact.SuiteRecord      `json:"suites,omitempty"`
	DeletedRuns   map[string]artifact.Tombstone        `json:"deleted_runs,omitempty"`
	DeletedSuites map[string]artifact.Tombstone        `json:"deleted_suites,omitempty"`
}

// EnableRuns creates the run journal; it is idempotent.
func (s *Store) EnableRuns(ctx context.Context) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if d.runs != nil {
		return nil
	}
	c := runCatalog{Schema: runSchema, Workspace: d.cat.Workspace.ID, Records: map[string]artifact.RunRecord{}}
	size := encodedSize(c)
	used, err := s.usage()
	if err != nil {
		return err
	}
	if err := s.checkSpace(used, size+s.limits.CatalogBytes, 0); err != nil {
		return err
	}
	next := d.cat
	next.RunSchema = runSchema
	next.Workspace.Schema = artifact.SchemaVersion
	d.runs, d.runsSize = &c, size
	d.cat, d.catSize = next, encodedSize(next)
	d.sweep()
	return nil
}

func (s *Store) runReserved(c *runCatalog) int64 {
	if c == nil {
		return 0
	}
	var n int64
	for _, r := range c.Records {
		if !r.Terminal {
			n++
		}
	}
	if n == 0 {
		return 0
	}
	// Saturate rather than overflow caller-configured limits.
	if s.limits.CatalogBytes > s.limits.StorageBytes/2 || n > (s.limits.StorageBytes-2*s.limits.CatalogBytes)/s.limits.RunBytes {
		return s.limits.StorageBytes
	}
	return n*s.limits.RunBytes + 2*s.limits.CatalogBytes
}

// recordDelta is how much setting records[id] to r changes the encoded
// journal: exactly what encoding it again would measure, without encoding
// every run on every write.
func recordDelta(records map[string]artifact.RunRecord, id string, r artifact.RunRecord) int64 {
	entry := func(r artifact.RunRecord) int64 { return encodedSize(id) + 1 + encodedSize(r) }
	if old, ok := records[id]; ok {
		return entry(r) - entry(old)
	}
	if len(records) == 0 {
		return entry(r)
	}
	return entry(r) + 1 // the comma before it
}

// checkRunCatalog preserves the admission reserve for unfinished runs.
func (s *Store) checkRunCatalog(c *runCatalog, size int64) error {
	future := size
	for _, record := range c.Records {
		if !record.Terminal {
			// Full metadata plus envelope growth (digest, bytes, generation).
			future += maxRunMetadata + 512 - encodedSize(record)
		}
	}
	if size > s.limits.CatalogBytes || future > s.limits.CatalogBytes {
		return artifact.ErrLimit
	}
	return nil
}

// SaveRun publishes metadata and optional immutable evidence together. The
// expected generation is zero for admission. A run's project is set at
// admission and never changes.
func (s *Store) SaveRun(ctx context.Context, id, project string, expected uint64, terminal bool, index artifact.RunIndex, metadata, evidence []byte) (*artifact.RunRecord, error) {
	return s.saveRun(ctx, id, project, expected, terminal, index, metadata, evidence, nil, nil)
}

// AdmitRun stores the immutable input bundle together with the queued record
// referencing it.
func (s *Store) AdmitRun(ctx context.Context, a artifact.Admission) (*artifact.RunRecord, error) {
	return s.saveRun(ctx, a.ID, a.Project, 0, false, a.Index, a.Metadata, nil, a.Bundle, &a)
}

func (s *Store) saveRun(ctx context.Context, id, project string, expected uint64, terminal bool, index artifact.RunIndex, metadata, evidence, bundle []byte, admission *artifact.Admission) (*artifact.RunRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.runs == nil || !validRunID(id) || !artifact.ValidProject(project) || !json.Valid(metadata) || len(metadata) > maxRunMetadata || (!terminal && len(evidence) != 0) || !index.Valid() {
		return nil, invalid("invalid run publication")
	}
	var pin *artifact.RunBinary
	if admission != nil && admission.Binary != nil {
		if !admission.Binary.Valid() {
			return nil, invalid("invalid run binary")
		}
		b := *admission.Binary
		pin = &b
	}
	old, exists := d.runs.Records[id]
	if _, deleted := d.runs.DeletedRuns[id]; deleted || exists && (len(bundle) != 0 || admission != nil) {
		return nil, artifact.ErrConflict
	}
	if exists && (old.Terminal || old.Generation != expected) || !exists && expected != 0 {
		return nil, artifact.ErrConflict
	}
	if exists && old.Project != project {
		return nil, invalid("a run's project cannot change")
	}
	if pin != nil {
		stored := false
		for _, e := range d.runtimes {
			if e.Binary != nil && e.Binary.Digest == pin.Digest {
				if e.Binary.Bytes != pin.Bytes {
					return nil, invalid("a run's binary size differs from the stored binary")
				}
				stored = true
			}
		}
		if !stored {
			return nil, artifact.ErrNotFound
		}
	}
	if !exists && len(d.runs.Records) >= s.limits.Runs || int64(len(evidence)) > s.limits.RunBytes || int64(len(bundle)) > s.limits.BundleBytes {
		return nil, artifact.ErrLimit
	}
	ordinal := old.Ordinal
	if !exists {
		ordinal = uint64(len(d.runs.Records) + len(d.runs.DeletedRuns) + 1)
	}
	r := artifact.RunRecord{ID: id, Project: project, Generation: expected + 1, Ordinal: ordinal, Terminal: terminal,
		Metadata: append(json.RawMessage(nil), metadata...), Bytes: int64(len(evidence)), Index: &index}
	r.BundleDigest, r.BundleBytes, r.Kept = old.BundleDigest, old.BundleBytes, old.Kept
	if len(bundle) != 0 {
		r.BundleDigest, r.BundleBytes = digest(bundle), int64(len(bundle))
	}
	if len(evidence) != 0 {
		r.Digest = digest(evidence)
	}
	next := *d.runs
	next.Records = maps.Clone(d.runs.Records)
	next.Records[id] = r
	var size int64
	if d.runs.Records != nil {
		size = d.runsSize + recordDelta(d.runs.Records, id, r)
	} else {
		size = encodedSize(next)
	}
	if s.checkRunCatalog(&next, size) != nil {
		return nil, artifact.ErrLimit
	}
	additional := size
	newBlobs := map[string][]byte{}
	for _, blob := range []struct {
		digest  string
		content []byte
	}{{r.Digest, evidence}, {r.BundleDigest, bundle}} {
		if blob.digest == "" || len(blob.content) == 0 {
			continue
		}
		if _, pending := newBlobs[blob.digest]; pending {
			continue
		}
		if _, stored := d.blobs[blob.digest]; !stored {
			additional += int64(len(blob.content))
			newBlobs[blob.digest] = blob.content
		}
	}
	used, err := s.usage()
	if err != nil {
		return nil, err
	}
	if err := s.checkSpace(used, additional, s.reserved(&next)); err != nil {
		return nil, err
	}
	for dg, content := range newBlobs {
		d.storeBlob(dg, content)
	}
	d.runs, d.runsSize = &next, size
	if !exists {
		d.admitted[id] = time.Now()
	}
	if admission != nil {
		d.queue[id] = &queueEntry{runtime: admission.Runtime, priority: admission.Priority, ordinal: ordinal, state: artifact.QueueQueued, binary: pin}
		if pin != nil {
			d.pins[id] = *pin
		}
	}
	if e := d.queue[id]; e != nil {
		e.generation = r.Generation
	}
	if terminal {
		delete(d.queue, id)
		delete(d.pins, id)
		delete(d.feed.events, id)
	}
	d.change(artifact.ChangeRun, id)
	r = cloneRun(r)
	return &r, nil
}

// cloneRun copies a record so callers never share the journal's metadata
// or index.
func cloneRun(r artifact.RunRecord) artifact.RunRecord {
	r.Metadata = append(json.RawMessage(nil), r.Metadata...)
	if r.Index != nil {
		x := *r.Index
		r.Index = &x
	}
	return r
}

// Runs returns every run record in admission order.
func (s *Store) Runs(ctx context.Context) ([]artifact.RunRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.runs == nil {
		return nil, invalid("run journal is not enabled")
	}
	result := make([]artifact.RunRecord, 0, len(d.runs.Records))
	for _, r := range d.runs.Records {
		result = append(result, cloneRun(r))
	}
	slices.SortFunc(result, func(a, b artifact.RunRecord) int { return cmp.Compare(a.Ordinal, b.Ordinal) })
	return result, nil
}

// ListRuns returns a page of runs, newest first, keyed on the admission
// ordinal as the PostgreSQL store does.
func (s *Store) ListRuns(ctx context.Context, q artifact.RunQuery) (*artifact.RunPage, error) {
	q, before, err := q.Normalize()
	if err != nil {
		return nil, err
	}
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.runs == nil {
		return nil, invalid("run journal is not enabled")
	}
	matched := []artifact.RunRecord{}
	for _, r := range d.runs.Records {
		if (before == 0 || r.Ordinal < before) && q.Matches(r) {
			matched = append(matched, r)
		}
	}
	slices.SortFunc(matched, func(a, b artifact.RunRecord) int { return cmp.Compare(b.Ordinal, a.Ordinal) })
	p := &artifact.RunPage{Runs: []artifact.RunRecord{}}
	for _, r := range matched[:min(len(matched), q.Limit)] {
		p.Runs = append(p.Runs, cloneRun(r))
	}
	if len(matched) > q.Limit {
		p.NextCursor = q.NextCursor(p.Runs[q.Limit-1].Ordinal)
	}
	return p, nil
}

// IndexRun sets the index of a run recorded without one; an indexed run
// keeps its index.
func (s *Store) IndexRun(ctx context.Context, id string, index artifact.RunIndex) error {
	if !index.Valid() {
		return invalid("invalid run index")
	}
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if d.runs == nil {
		return invalid("run journal is not enabled")
	}
	r, ok := d.runs.Records[id]
	if !ok {
		return artifact.ErrNotFound
	}
	if r.Index != nil {
		return nil
	}
	next := *d.runs
	next.Records = maps.Clone(d.runs.Records)
	r.Index = &index
	next.Records[id] = r
	d.runs, d.runsSize = &next, encodedSize(next)
	d.change(artifact.ChangeRun, id)
	return nil
}

// RunBundle returns a run's private execution bundle.
func (s *Store) RunBundle(ctx context.Context, id string) ([]byte, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.runs == nil {
		return nil, artifact.ErrNotFound
	}
	r, ok := d.runs.Records[id]
	if !ok || r.BundleDigest == "" {
		return nil, artifact.ErrNotFound
	}
	return cloneBytes(d.blobs[r.BundleDigest]), nil
}

// RunEvidence returns a terminal run's evidence archive.
func (s *Store) RunEvidence(ctx context.Context, id string) ([]byte, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.runs == nil {
		return nil, artifact.ErrNotFound
	}
	r, ok := d.runs.Records[id]
	if !ok || r.Digest == "" {
		return nil, artifact.ErrNotFound
	}
	return cloneBytes(d.blobs[r.Digest]), nil
}

// SaveEvaluation records an immutable evaluation of a terminal run. It never
// changes the run's generation or evidence.
func (s *Store) SaveEvaluation(ctx context.Context, id, runID string, metadata, result, source []byte) (*artifact.EvaluationRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.runs == nil {
		return nil, artifact.ErrNotFound
	}
	run, exists := d.runs.Records[runID]
	if !exists {
		return nil, artifact.ErrNotFound
	}
	if !run.Terminal {
		return nil, artifact.ErrConflict
	}
	if !validEvaluationID(id) || !json.Valid(metadata) || len(metadata) > maxRunMetadata || len(result) == 0 || len(source) == 0 {
		return nil, invalid("invalid evaluation publication")
	}
	if _, exists := d.runs.Evaluations[id]; exists {
		return nil, artifact.ErrConflict
	}
	count := 0
	for _, r := range d.runs.Evaluations {
		if r.RunID == runID {
			count++
		}
	}
	if count >= s.limits.EvaluationsPerRun || int64(len(result)) > s.limits.RunBytes || int64(len(source)) > s.limits.BlobBytes {
		return nil, artifact.ErrLimit
	}
	r := artifact.EvaluationRecord{ID: id, RunID: runID, Ordinal: uint64(count + 1), Metadata: append(json.RawMessage(nil), metadata...),
		ResultDigest: digest(result), ResultBytes: int64(len(result)), SourceDigest: digest(source), SourceBytes: int64(len(source))}
	next := *d.runs
	next.Schema = runSchema
	next.Evaluations = make(map[string]artifact.EvaluationRecord, len(d.runs.Evaluations)+1)
	for key, record := range d.runs.Evaluations {
		next.Evaluations[key] = record
	}
	next.Evaluations[id] = r
	size := encodedSize(next)
	if err := s.checkRunCatalog(&next, size); err != nil {
		return nil, err
	}
	additional := size
	newBlobs := map[string][]byte{}
	for _, blob := range []struct {
		digest  string
		content []byte
	}{{r.ResultDigest, result}, {r.SourceDigest, source}} {
		if _, pending := newBlobs[blob.digest]; pending {
			continue
		}
		if _, stored := d.blobs[blob.digest]; !stored {
			additional += int64(len(blob.content))
			newBlobs[blob.digest] = blob.content
		}
	}
	used, err := s.usage()
	if err != nil {
		return nil, err
	}
	if err := s.checkSpace(used, additional, s.reserved(&next)); err != nil {
		return nil, err
	}
	for dg, content := range newBlobs {
		d.storeBlob(dg, content)
	}
	d.runs, d.runsSize = &next, size
	d.change(artifact.ChangeEvaluation, runID)
	r.Metadata = append(json.RawMessage(nil), r.Metadata...)
	return &r, nil
}

// Evaluations returns listing envelopes in recording order, never source
// bytes.
func (s *Store) Evaluations(ctx context.Context, runID string) ([]artifact.EvaluationRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.runs == nil {
		return nil, artifact.ErrNotFound
	}
	if _, exists := d.runs.Records[runID]; !exists {
		return nil, artifact.ErrNotFound
	}
	result := []artifact.EvaluationRecord{}
	for _, r := range d.runs.Evaluations {
		if r.RunID == runID {
			r.Metadata = append(json.RawMessage(nil), r.Metadata...)
			result = append(result, r)
		}
	}
	slices.SortFunc(result, func(a, b artifact.EvaluationRecord) int { return cmp.Compare(a.Ordinal, b.Ordinal) })
	return result, nil
}

// EvaluationResult returns an evaluation's envelope and result bytes.
func (s *Store) EvaluationResult(ctx context.Context, id string) (*artifact.EvaluationRecord, []byte, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, nil, err
	}
	if d.runs == nil {
		return nil, nil, artifact.ErrNotFound
	}
	r, exists := d.runs.Evaluations[id]
	if !exists {
		return nil, nil, artifact.ErrNotFound
	}
	r.Metadata = append(json.RawMessage(nil), r.Metadata...)
	return &r, cloneBytes(d.blobs[r.ResultDigest]), nil
}

type suiteMetadata struct {
	Caller  json.RawMessage `json:"caller"`
	Created time.Time       `json:"created"`
	Config  string          `json:"config"`
	Members []struct {
		Label string `json:"label"`
		RunID string `json:"run_id,omitempty"`
		Error string `json:"error,omitempty"`
	} `json:"members"`
}

// validSuiteMetadata checks a suite's members, which are runs of its project.
func validSuiteMetadata(c *runCatalog, project string, raw []byte) bool {
	var m suiteMetadata
	if decodeStrict(raw, &m) != nil || m.Created.IsZero() || m.Config == "" || len(m.Caller) == 0 || string(m.Caller) == "null" || len(m.Members) == 0 || len(m.Members) > 32 {
		return false
	}
	for _, member := range m.Members {
		if member.Label == "" || (member.RunID == "") == (member.Error == "") {
			return false
		}
		if member.RunID != "" {
			if r, exists := c.Records[member.RunID]; !exists || r.Project != project {
				return false
			}
		}
	}
	return true
}

func suiteMembers(r artifact.SuiteRecord) []string {
	var m suiteMetadata
	if decodeStrict(r.Metadata, &m) != nil {
		return nil // Validated on publication.
	}
	ids := []string{}
	for _, member := range m.Members {
		if member.RunID != "" {
			ids = append(ids, member.RunID)
		}
	}
	return ids
}

// CheckSuiteCapacity checks the record cap including application
// reservations. It writes nothing.
func (s *Store) CheckSuiteCapacity(ctx context.Context, pending int) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if d.runs == nil || pending < 0 {
		return invalid("invalid suite reservation")
	}
	if pending >= s.limits.Suites-len(d.runs.Suites) {
		return artifact.ErrLimit
	}
	return nil
}

// SaveSuite records a suite after all its member admissions.
func (s *Store) SaveSuite(ctx context.Context, id, project string, metadata []byte) (*artifact.SuiteRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.runs == nil || !validSuiteID(id) || !artifact.ValidProject(project) || !validSuiteMetadata(d.runs, project, metadata) {
		return nil, invalid("invalid suite publication")
	}
	_, exists := d.runs.Suites[id]
	if _, deleted := d.runs.DeletedSuites[id]; exists || deleted {
		return nil, artifact.ErrConflict
	}
	if len(d.runs.Suites) >= s.limits.Suites {
		return nil, artifact.ErrLimit
	}
	r := artifact.SuiteRecord{ID: id, Project: project, Ordinal: uint64(len(d.runs.Suites) + len(d.runs.DeletedSuites) + 1),
		Metadata: append(json.RawMessage(nil), metadata...)}
	next := *d.runs
	next.Suites = make(map[string]artifact.SuiteRecord, len(d.runs.Suites)+1)
	for key, record := range d.runs.Suites {
		next.Suites[key] = record
	}
	next.Suites[id] = r
	size := encodedSize(next)
	if err := s.checkRunCatalog(&next, size); err != nil {
		return nil, err
	}
	used, err := s.usage()
	if err != nil {
		return nil, err
	}
	if err := s.checkSpace(used, size, s.reserved(&next)); err != nil {
		return nil, err
	}
	d.runs, d.runsSize = &next, size
	d.change(artifact.ChangeSuite, id)
	r.Metadata = append(json.RawMessage(nil), r.Metadata...)
	return &r, nil
}

// Suites returns suite envelopes, newest first.
func (s *Store) Suites(ctx context.Context) ([]artifact.SuiteRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.runs == nil {
		return nil, artifact.ErrNotFound
	}
	out := make([]artifact.SuiteRecord, 0, len(d.runs.Suites))
	for _, r := range d.runs.Suites {
		r.Metadata = append(json.RawMessage(nil), r.Metadata...)
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b artifact.SuiteRecord) int { return cmp.Compare(b.Ordinal, a.Ordinal) })
	return out, nil
}

// suiteOf returns the live suite listing run id, if any.
func (d *Data) suiteOf(id string) string {
	for sid, suite := range d.runs.Suites {
		if slices.Contains(suiteMembers(suite), id) {
			return sid
		}
	}
	return ""
}

func cloneTombstones(m map[string]artifact.Tombstone) map[string]artifact.Tombstone {
	out := make(map[string]artifact.Tombstone, len(m)+1)
	for id, t := range m {
		out[id] = t
	}
	return out
}

// withoutRuns is the journal with the runs and their evaluations replaced by
// tombstones.
func (d *Data) withoutRuns(ids []string, at time.Time) runCatalog {
	next := *d.runs
	next.Schema = runSchema
	next.Records = make(map[string]artifact.RunRecord, len(d.runs.Records))
	for id, r := range d.runs.Records {
		if !slices.Contains(ids, id) {
			next.Records[id] = r
		}
	}
	next.Evaluations = make(map[string]artifact.EvaluationRecord, len(d.runs.Evaluations))
	for id, r := range d.runs.Evaluations {
		if !slices.Contains(ids, r.RunID) {
			next.Evaluations[id] = r
		}
	}
	next.DeletedRuns = cloneTombstones(d.runs.DeletedRuns)
	for _, id := range ids {
		next.DeletedRuns[id] = artifact.Tombstone{ID: id, Deleted: at.UTC()}
	}
	return next
}

// publishDeletion installs the journal without the deleted records and drops
// blobs nothing references. Deletion needs no storage reservation, so storage
// over its limit can always be reduced.
func (s *Store) publishDeletion(next runCatalog) error {
	size := encodedSize(next)
	if size > s.limits.CatalogBytes {
		return artifact.ErrLimit
	}
	s.d.runs, s.d.runsSize = &next, size
	s.d.sweep()
	return nil
}

// DeleteRun removes a terminal run that belongs to no suite, with its
// evaluations, keeping a tombstone. A queued or running run, or a suite
// member, is ErrConflict.
func (s *Store) DeleteRun(ctx context.Context, id string, at time.Time) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if d.runs == nil {
		return artifact.ErrNotFound
	}
	r, exists := d.runs.Records[id]
	if !exists {
		return artifact.ErrNotFound
	}
	if !r.Terminal || d.suiteOf(id) != "" {
		return artifact.ErrConflict
	}
	if err := s.publishDeletion(d.withoutRuns([]string{id}, at)); err != nil {
		return err
	}
	d.change(artifact.ChangeDeletion, id)
	return nil
}

// DeleteSuite removes a suite and its member runs, with their evaluations,
// keeping tombstones. A member that is not terminal is ErrConflict.
func (s *Store) DeleteSuite(ctx context.Context, id string, at time.Time) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if d.runs == nil {
		return artifact.ErrNotFound
	}
	suite, exists := d.runs.Suites[id]
	if !exists {
		return artifact.ErrNotFound
	}
	members := suiteMembers(suite)
	for _, member := range members {
		if !d.runs.Records[member].Terminal {
			return artifact.ErrConflict
		}
	}
	next := d.withoutRuns(members, at)
	next.Suites = make(map[string]artifact.SuiteRecord, len(d.runs.Suites))
	for sid, record := range d.runs.Suites {
		if sid != id {
			next.Suites[sid] = record
		}
	}
	next.DeletedSuites = cloneTombstones(d.runs.DeletedSuites)
	next.DeletedSuites[id] = artifact.Tombstone{ID: id, Deleted: at.UTC()}
	if err := s.publishDeletion(next); err != nil {
		return err
	}
	for _, member := range members {
		d.change(artifact.ChangeDeletion, member)
	}
	d.change(artifact.ChangeDeletion, id)
	return nil
}

// Tombstones returns the deleted runs and suites, ordered by ID.
func (s *Store) Tombstones(ctx context.Context) (runs, suites []artifact.Tombstone, err error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, nil, err
	}
	if d.runs == nil {
		return nil, nil, artifact.ErrNotFound
	}
	byID := func(a, b artifact.Tombstone) int { return cmp.Compare(a.ID, b.ID) }
	for _, t := range d.runs.DeletedRuns {
		runs = append(runs, t)
	}
	for _, t := range d.runs.DeletedSuites {
		suites = append(suites, t)
	}
	slices.SortFunc(runs, byID)
	slices.SortFunc(suites, byID)
	return runs, suites, nil
}
