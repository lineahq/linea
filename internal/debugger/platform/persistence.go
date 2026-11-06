package platform

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
)

var ErrPersistence = errors.New("run persistence failed")
var ErrInterrupted = errors.New("run interrupted: the server stopped before the run ended, and the run has no input bundle to execute it again")

func persistenceError(err error) error { return fmt.Errorf("%w: %w", ErrPersistence, err) }

// Independent schema: the source catalog and engine report keep their versions.
// Schema 2 adds the caller; schema-1 runs stay readable without one.
// Attempt counts the run's assignments to a worker.
type runMetadata struct {
	Schema      int                `json:"schema"`
	ID          string             `json:"id"`
	Mode        InputMode          `json:"mode"`
	Caller      *Actor             `json:"caller,omitempty"`
	State       State              `json:"state"`
	Error       string             `json:"error,omitempty"`
	Failure     string             `json:"failure,omitempty"`
	Created     time.Time          `json:"created"`
	Started     time.Time          `json:"started"`
	Finished    time.Time          `json:"finished"`
	Interrupted bool               `json:"interrupted,omitempty"`
	Attempt     int                `json:"attempt,omitempty"`
	Bundle      *engine.BundleInfo `json:"bundle,omitempty"`
}

type evidenceArchive struct {
	Schema             int                `json:"schema"`
	Report             json.RawMessage    `json:"report,omitempty"`
	Events             []instrument.Event `json:"events,omitempty"`
	Snapshots          map[string][]byte  `json:"snapshots,omitempty"`
	ReferenceSnapshots map[string][]byte  `json:"reference_snapshots,omitempty"`
	// Log is a failed or partial run's redacted Collector log tail.
	Log          string `json:"log,omitempty"`
	LogTruncated bool   `json:"log_truncated,omitempty"`
}

func (s *Service) persistRun(r *Handle, includeEvidence bool) error {
	m := runMetadata{Schema: 2, ID: r.id, Mode: r.mode, State: r.state, Created: r.created, Started: r.started, Finished: r.finished, Interrupted: r.interrupted, Attempt: r.attempt}
	if r.caller != (Actor{}) {
		caller := r.caller
		m.Caller = &caller
	}
	m.Bundle = r.bundleInfo
	if r.err != nil {
		m.Error = r.err.Error()
		if errors.Is(r.err, ErrPersistence) {
			m.Failure = "persistence"
		} else if errors.Is(r.err, ErrInterrupted) {
			m.Failure = "interrupted"
		} else if errors.Is(r.err, ErrLost) {
			m.Failure = "lost"
		}
		if len(m.Error) > 4096 {
			m.Error = m.Error[:4096] + " (truncated)"
		}
	}
	metadata, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// Escaped control characters can use six JSON bytes per input byte.
	for len(metadata) > 8192 && len(m.Error) > 128 {
		m.Error = m.Error[:len(m.Error)/2] + " (truncated)"
		metadata, err = json.Marshal(m)
		if err != nil {
			return err
		}
	}
	var evidence []byte
	if includeEvidence && (r.report != nil || len(r.events) != 0 || r.log != "") {
		limit := s.repository.Limits().RunBytes
		var decoded int64
		if evidence, decoded, err = encodeEvidence(r.events, r.report, r.log, r.logTruncated); err != nil {
			return err
		}
		// The evidence budget leaves headroom for the report's analysis; when
		// the analysis outgrows it, keep the run without its derived sections
		// rather than losing it. The handle keeps the same reduced report, so
		// answers agree before and after a restart.
		if !archiveFits(int64(len(evidence)), decoded, limit) && r.report != nil && r.report.Result != nil {
			reduced := withoutAnalysis(r.report)
			if smaller, size, err := encodeEvidence(r.events, reduced, r.log, r.logTruncated); err == nil && archiveFits(int64(len(smaller)), size, limit) {
				r.report, evidence = reduced, smaller
			}
		}
	}
	var record *artifact.RunRecord
	if r.generation == 0 {
		record, err = s.repository.AdmitRun(context.Background(), artifact.Admission{
			ID: r.id, Project: r.project, Metadata: metadata, Bundle: r.bundle, Runtime: r.runtimeKey, Binary: r.binary, Index: runIndex(r.state, r.report)})
	} else {
		record, err = s.repository.SaveRun(context.Background(), r.id, r.project, r.generation, r.terminalLocked(), runIndex(r.state, r.report), metadata, evidence)
	}
	if err == nil {
		if r.generation == 0 {
			s.nudgeRetention()
		}
		r.generation, r.ordinal = record.Generation, record.Ordinal
		r.stored = r.terminalLocked()
	}
	return err
}

// Evidence archives are schema 2: the archive JSON, gzip-compressed, with its
// decoded length declared in the gzip header. Schema-1 archives are the
// same JSON uncompressed and stay readable. The repository stores, digests and
// limits the compressed bytes.
const (
	archiveSchema = 2
	// decodedArchiveFactor bounds an archive's decoded size, as a multiple of
	// the run allowance, beyond which its analysis is omitted.
	decodedArchiveFactor = 4
)

// maxDeflateRatio is deflate's largest expansion (about 1032:1), so a declared
// length beyond it marks a corrupt archive before anything is decoded.
const maxDeflateRatio = 1032

// archiveSizeField is the gzip extra subfield declaring the decoded length.
var archiveSizeField = [2]byte{'O', 'D'}

// archiveFits reports whether an archive of stored compressed bytes and
// decoded bytes may be kept whole under a run allowance of limit bytes.
func archiveFits(stored, decoded, limit int64) bool {
	return stored <= limit && decoded <= decodedArchiveFactor*limit
}

// encodeEvidence returns the stored archive and its decoded length.
func encodeEvidence(events []instrument.Event, report *engine.Report, log string, logTruncated bool) ([]byte, int64, error) {
	a := evidenceArchive{Schema: archiveSchema, Events: events, Log: log, LogTruncated: logTruncated}
	if report != nil {
		var b bytes.Buffer
		if err := engine.Encode(&b, withoutSnapshots(report)); err != nil {
			return nil, 0, err
		}
		a.Report = b.Bytes()
		if report.Result != nil {
			a.Snapshots = snapshotBytes(report.Result.Snapshots)
		}
		if report.Reference != nil {
			a.ReferenceSnapshots = snapshotBytes(report.Reference.Snapshots)
		}
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, 0, err
	}
	extra := make([]byte, 12)
	copy(extra, archiveSizeField[:])
	binary.LittleEndian.PutUint16(extra[2:], 8)
	binary.LittleEndian.PutUint64(extra[4:], uint64(len(raw)))
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Extra = extra
	if _, err := w.Write(raw); err != nil {
		return nil, 0, err
	}
	if err := w.Close(); err != nil {
		return nil, 0, err
	}
	return b.Bytes(), int64(len(raw)), nil
}

// decodeEvidence reads a stored archive of either schema. A compressed
// archive must decode to exactly its declared length, in one gzip member, so
// it can never expand without bound.
func decodeEvidence(stored []byte) (*evidenceArchive, error) {
	raw, schema := stored, 1
	if len(stored) >= 2 && stored[0] == 0x1f && stored[1] == 0x8b {
		in := bytes.NewReader(stored)
		z, err := gzip.NewReader(in)
		if err != nil {
			return nil, err
		}
		z.Multistream(false)
		size, ok := declaredArchiveSize(z.Header.Extra)
		if !ok {
			return nil, errors.New("missing decoded length")
		}
		if size > int64(len(stored))*maxDeflateRatio {
			return nil, errors.New("declared length exceeds what the archive can hold")
		}
		if raw, err = io.ReadAll(io.LimitReader(z, size+1)); err != nil {
			return nil, err
		}
		if int64(len(raw)) != size {
			return nil, errors.New("decoded length differs from the declared length")
		}
		if in.Len() != 0 {
			return nil, errors.New("trailing data after the archive")
		}
		schema = archiveSchema
	}
	var a evidenceArchive
	if err := decodeStored(raw, &a); err != nil {
		return nil, err
	}
	if a.Schema != schema {
		return nil, artifact.ErrUnsupportedSchema
	}
	return &a, nil
}

// declaredArchiveSize finds the decoded length among gzip extra subfields.
func declaredArchiveSize(extra []byte) (int64, bool) {
	for len(extra) >= 4 {
		n := int(binary.LittleEndian.Uint16(extra[2:]))
		if len(extra) < 4+n {
			return 0, false
		}
		if extra[0] == archiveSizeField[0] && extra[1] == archiveSizeField[1] && n == 8 {
			size := binary.LittleEndian.Uint64(extra[4:])
			if size > 1<<62 {
				return 0, false
			}
			return int64(size), true
		}
		extra = extra[4+n:]
	}
	return 0, false
}

// omittedSections are the report sections derived from recorded evidence,
// left out when an archive would exceed its storage limit.
var omittedSections = []string{"stages", "lineage", "aggregations", "routing", "sampling", "batching", "grouping", "filtering", "ottl"}

// withoutAnalysis returns a copy of rep without its derived analysis. Its
// result names the omitted sections; status, assertions, outputs, events and
// snapshots are unchanged.
func withoutAnalysis(rep *engine.Report) *engine.Report {
	out := *rep
	out.Stages, out.Lineage, out.Aggregations = []analysis.Stage{}, []analysis.RecordLineage{}, []*analysis.Aggregation{}
	out.Routing, out.Sampling, out.OTTL = []*analysis.Routing{}, []*analysis.Sampling{}, []*ottldebug.Report{}
	out.Batching, out.Grouping, out.Filtering = nil, nil, nil
	result := *rep.Result
	cut := runresult.Truncation{Reason: runresult.TruncationEvidenceBudget}
	if result.Truncation != nil {
		cut = *result.Truncation
	}
	cut.Omitted = slices.Clone(omittedSections)
	result.Truncation = &cut
	out.Result = &result
	return &out
}

func decodeStored(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber() // Preserve int64 attribute values in analysis.Change.Before/After.
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

// recoverRuns reads the unfinished runs, which the scheduler needs; every
// other run is read when named.
func (s *Service) recoverRuns() error {
	ctx := context.Background()
	records, err := s.storedRuns(ctx, artifact.RunQuery{Unfinished: true})
	if err != nil {
		return err
	}
	for _, record := range records {
		r, err := s.handleFromRecord(ctx, record)
		if err != nil {
			return err
		}
		if err := s.recoverUnfinished(r); err != nil {
			return err
		}
		s.runs[r.id] = r
		s.order = append(s.order, r.id)
	}
	if err := s.indexRuns(ctx); err != nil {
		return err
	}
	return s.recoverEvaluations()
}

// handleFromRecord decodes and checks a stored run: its metadata, bundle
// identity and, once it ended, its evidence. A terminal run's handle is done;
// an unfinished one is not yet scheduled (recoverUnfinished, adoptLocked).
func (s *Service) handleFromRecord(ctx context.Context, record artifact.RunRecord) (*Handle, error) {
	var m runMetadata
	if err := decodeStored(record.Metadata, &m); err != nil {
		return nil, fmt.Errorf("%w: run metadata: %w", artifact.ErrCorrupt, err)
	}
	if m.Schema != 1 && m.Schema != 2 {
		return nil, artifact.ErrUnsupportedSchema
	}
	var caller Actor
	if m.Caller != nil {
		if m.Schema == 1 || m.Caller.validate() != nil {
			return nil, fmt.Errorf("%w: run caller", artifact.ErrCorrupt)
		}
		caller = *m.Caller
	}
	r := &Handle{id: m.ID, project: record.Project, mode: m.Mode, caller: caller, state: m.State, created: m.Created, started: m.Started, finished: m.Finished, interrupted: m.Interrupted, attempt: m.Attempt, kept: record.Kept, generation: record.Generation, ordinal: record.Ordinal, stored: record.Terminal, changed: make(chan struct{}), done: make(chan struct{}), cancel: func(error) {}}
	if (m.Bundle == nil) != (record.BundleDigest == "") {
		return nil, fmt.Errorf("%w: bundle binding", artifact.ErrCorrupt)
	}
	if m.Bundle != nil {
		raw, err := s.repository.RunBundle(ctx, record.ID)
		if err != nil {
			return nil, err
		}
		info, err := engine.InspectBundle(raw)
		if err != nil {
			if errors.Is(err, engine.ErrBundleSchema) {
				return nil, artifact.ErrUnsupportedSchema
			}
			return nil, fmt.Errorf("%w: %w", artifact.ErrCorrupt, err)
		}
		if !reflect.DeepEqual(info, m.Bundle) || info.Digest != record.BundleDigest || info.Bytes != record.BundleBytes || info.Mode != string(m.Mode) {
			return nil, fmt.Errorf("%w: bundle identity", artifact.ErrCorrupt)
		}
		r.bundleInfo = info
	}
	if m.ID != record.ID || m.Created.IsZero() || (m.Mode != ModeQuick && m.Mode != ModeScenario) || r.terminalLocked() != record.Terminal || record.Terminal != !m.Finished.IsZero() || (m.State != StateQueued && m.State != StateAssigned && m.State != StateRunning && !r.terminalLocked()) || (m.State != StateRunning && !r.terminalLocked() && !m.Started.IsZero()) || (m.State == StateRunning && m.Started.IsZero()) || (m.Interrupted && m.State != StateError) || m.Attempt < 0 {
		return nil, fmt.Errorf("%w: invalid run lifecycle", artifact.ErrCorrupt)
	}
	if m.Error != "" {
		r.err = errors.New(m.Error)
		if m.Failure == "persistence" {
			r.err = &storedError{m.Error, ErrPersistence}
		}
		if m.Failure == "interrupted" {
			r.err = &storedError{m.Error, ErrInterrupted}
		}
		if m.Failure == "lost" {
			r.err = &storedError{m.Error, ErrLost}
		}
	}
	if !slices.Contains([]string{"", "persistence", "interrupted", "lost"}, m.Failure) || (m.Failure != "" && m.Error == "") || (m.Failure == "lost") != (m.State == StateLost) || (m.Failure != "" && m.Failure != "lost" && m.State != StateError) || m.Interrupted != (m.Failure == "interrupted") {
		return nil, fmt.Errorf("%w: invalid run failure", artifact.ErrCorrupt)
	}
	if record.Digest != "" {
		raw, err := s.repository.RunEvidence(ctx, record.ID)
		if err != nil {
			return nil, err
		}
		a, err := decodeEvidence(raw)
		if errors.Is(err, artifact.ErrUnsupportedSchema) {
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("%w: evidence archive: %w", artifact.ErrCorrupt, err)
		}
		r.events, r.recording = a.Events, r.attempt
		r.log, r.logTruncated = a.Log, a.LogTruncated
		if len(a.Report) != 0 {
			if err := decodeStored(a.Report, &r.report); err != nil {
				return nil, fmt.Errorf("%w: report: %w", artifact.ErrCorrupt, err)
			}
			if r.report == nil || !engine.ReadableReportSchema(r.report.Schema) {
				return nil, artifact.ErrUnsupportedSchema
			}
			if r.report.Result != nil {
				r.report.Result.Snapshots = restoredSnapshots(a.Snapshots)
			} else if len(a.Snapshots) != 0 {
				return nil, artifact.ErrCorrupt
			}
			if r.report.Reference != nil {
				r.report.Reference.Snapshots = restoredSnapshots(a.ReferenceSnapshots)
			} else if len(a.ReferenceSnapshots) != 0 {
				return nil, artifact.ErrCorrupt
			}
		}
	}
	if (m.State == StateDone) != (r.report != nil) {
		return nil, fmt.Errorf("%w: report/lifecycle mismatch", artifact.ErrCorrupt)
	}
	for i, e := range r.events {
		if e.RunID != r.id || e.Seq != i+1 || e.ID != fmt.Sprintf("%s:%d", r.id, e.Seq) {
			return nil, fmt.Errorf("%w: event identity", artifact.ErrCorrupt)
		}
	}
	r.persist = func(evidence bool) error { return s.persistRun(r, evidence) }
	if record.Terminal {
		close(r.done)
	}
	return r, nil
}

// recoverUnfinished resumes scheduling a run that had not ended. A run with
// a bundle stays queued, or held until its lease ends, and executes from its
// bundle; one without a bundle, or outside the queue, is interrupted.
func (s *Service) recoverUnfinished(r *Handle) error {
	s.queueing(r)
	a, err := s.runQueue.Assignment(context.Background(), r.id)
	if err != nil && !errors.Is(err, artifact.ErrNotFound) {
		return err
	}
	if err != nil || r.bundleInfo == nil {
		r.interrupted = true
		return r.finishLocked(StateError, nil, ErrInterrupted)
	}
	r.runtimeKey = a.Runtime
	if a.State == artifact.QueueQueued {
		r.state, r.started = StateQueued, time.Time{}
		s.jobs[r.id] = s.recoveredJobLocked(r)
	}
	return nil
}

type storedError struct {
	message string
	cause   error
}

func (e *storedError) Error() string { return e.message }
func (e *storedError) Unwrap() error { return e.cause }

// Snapshot endpoints return original bytes, while engine.Encode formats their
// JSON for reports. Base64 in the archive preserves that distinction exactly.
func snapshotBytes(snaps instrument.Snapshots) map[string][]byte {
	if snaps == nil {
		return nil
	}
	out := make(map[string][]byte, len(snaps))
	for ref, raw := range snaps {
		out[ref] = raw
	}
	return out
}
func restoredSnapshots(snaps map[string][]byte) instrument.Snapshots {
	if snaps == nil {
		return nil
	}
	out := make(instrument.Snapshots, len(snaps))
	for ref, raw := range snaps {
		out[ref] = raw
	}
	return out
}
