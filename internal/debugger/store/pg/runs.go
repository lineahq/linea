package pg

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

const maxRunMetadata = 8192

// EnableRuns is a no-op: the run journal's tables belong to the schema.
func (s *Store) EnableRuns(ctx context.Context) error { return s.ready(ctx) }

// SaveRun publishes metadata and optional immutable evidence together. The
// expected generation is zero for a run's first publication. A run's project
// is set at its first publication and never changes; a terminal run is
// immutable.
func (s *Store) SaveRun(ctx context.Context, id, project string, expected uint64, terminal bool, index artifact.RunIndex, metadata, evidence []byte) (*artifact.RunRecord, error) {
	return s.saveRun(ctx, id, project, expected, terminal, index, metadata, evidence, nil, nil)
}

// AdmitRun stores a run's immutable input bundle and its first record and
// queues it, in one transaction.
func (s *Store) AdmitRun(ctx context.Context, a artifact.Admission) (*artifact.RunRecord, error) {
	return s.saveRun(ctx, a.ID, a.Project, 0, false, a.Index, a.Metadata, nil, a.Bundle, &a)
}

type runRow struct {
	project    string
	generation int64
	ordinal    int64
	terminal   bool
	evidence   blob
	bundle     blob
	kept       bool
}

func (s *Store) saveRun(ctx context.Context, id, project string, expected uint64, terminal bool, index artifact.RunIndex, metadata, evidence, bundle []byte, admission *artifact.Admission) (*artifact.RunRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if !validRunID(id) || !artifact.ValidProject(project) || !json.Valid(metadata) || len(metadata) > maxRunMetadata || (!terminal && len(evidence) != 0) || !index.Valid() {
		return nil, invalid("invalid run publication")
	}
	var pin artifact.RunBinary
	if admission != nil && admission.Binary != nil {
		if !admission.Binary.Valid() {
			return nil, invalid("invalid run binary")
		}
		pin = *admission.Binary
	}
	var rec *artifact.RunRecord
	err := s.inTx(ctx, "run", func(tx pgx.Tx) error {
		// Creating a run and adding bytes check counts and storage, so they
		// serialize on the installation; a status update touches its row only.
		var inst installationRow
		var err error
		locked := expected == 0 || len(evidence) != 0 || len(bundle) != 0
		if locked {
			if inst, err = lockInstallation(ctx, tx); err != nil {
				return err
			}
		}
		var deleted bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tombstones WHERE kind = 'run' AND id = $1)", id).Scan(&deleted); err != nil {
			return err
		}
		if deleted {
			return artifact.ErrConflict
		}
		var old runRow
		exists := true
		err = tx.QueryRow(ctx, `SELECT project_id, generation, ordinal, terminal, evidence_digest, evidence_bytes, bundle_digest, bundle_bytes, kept
			FROM runs WHERE id = $1 FOR UPDATE`, id).Scan(&old.project, &old.generation, &old.ordinal, &old.terminal,
			&old.evidence.digest, &old.evidence.size, &old.bundle.digest, &old.bundle.size, &old.kept)
		if errors.Is(err, pgx.ErrNoRows) {
			exists = false
		} else if err != nil {
			return err
		}
		if exists && (len(bundle) != 0 || admission != nil) {
			return artifact.ErrConflict
		}
		if exists && (old.terminal || uint64(old.generation) != expected) || !exists && expected != 0 {
			return artifact.ErrConflict
		}
		if exists && old.project != project {
			return invalid("a run's project cannot change")
		}
		if int64(len(evidence)) > s.limits.RunBytes || int64(len(bundle)) > s.limits.BundleBytes {
			return artifact.ErrLimit
		}
		if pin.Digest != "" {
			// The installation lock keeps Sweep from deleting the object
			// until this run, which then references it, commits.
			var size int64
			err := tx.QueryRow(ctx, "SELECT binary_bytes FROM runtimes WHERE binary_digest = $1 LIMIT 1", pin.Digest).Scan(&size)
			if errors.Is(err, pgx.ErrNoRows) {
				return artifact.ErrNotFound
			}
			if err != nil {
				return err
			}
			if size != pin.Bytes {
				return invalid("a run's binary size differs from the stored binary")
			}
		}
		r := artifact.RunRecord{ID: id, Project: project, Generation: expected + 1, Terminal: terminal,
			Metadata: append(json.RawMessage(nil), metadata...), Bytes: int64(len(evidence)),
			BundleDigest: old.bundle.digest, BundleBytes: old.bundle.size, Kept: old.kept, Index: &index}
		if len(bundle) != 0 {
			r.BundleDigest, r.BundleBytes = digestOf(bundle), int64(len(bundle))
		}
		if len(evidence) != 0 {
			r.Digest = digestOf(evidence)
		}
		if locked {
			additional, err := newBytes(ctx, tx, blob{r.Digest, r.Bytes}, blob{r.BundleDigest, r.BundleBytes})
			if err != nil {
				return err
			}
			extra := int64(1)
			if terminal {
				extra = 0
			}
			if err := s.checkSpace(ctx, tx, additional, reserve{excludeRun: id, extraRuns: extra}); err != nil {
				return err
			}
		}
		if len(evidence) != 0 {
			if err := s.putBlob(ctx, r.Digest, evidence); err != nil {
				return err
			}
		}
		if len(bundle) != 0 {
			if err := s.putBlob(ctx, r.BundleDigest, bundle); err != nil {
				return err
			}
		}
		if exists {
			r.Ordinal = uint64(old.ordinal)
			// A terminal publication takes the run off the queue.
			_, err = tx.Exec(ctx, `UPDATE runs SET generation = $2, terminal = $3, metadata = $4, evidence_digest = $5, evidence_bytes = $6, updated_at = now(),
					queue_state = CASE WHEN $3 THEN NULL ELSE queue_state END,
					worker = CASE WHEN $3 THEN '' ELSE worker END,
					lease_expires_at = CASE WHEN $3 THEN NULL ELSE lease_expires_at END,
					scenario = $7, runtime_name = $8, result = $9, indexed = true
				WHERE id = $1`, id, int64(r.Generation), terminal, []byte(r.Metadata), r.Digest, r.Bytes, nullable(index.Scenario), nullable(index.Runtime), nullable(index.Result))
			if err == nil && terminal {
				_, err = tx.Exec(ctx, "DELETE FROM run_events WHERE run_id = $1", id)
			}
		} else {
			r.Ordinal = uint64(inst.runOrdinal + 1)
			if _, err = tx.Exec(ctx, "UPDATE installation SET run_ordinal = $1", int64(r.Ordinal)); err != nil {
				return err
			}
			var queue *string
			var runtime string
			var priority int
			if admission != nil {
				queued := string(artifact.QueueQueued)
				queue, runtime, priority = &queued, admission.Runtime, admission.Priority
			}
			_, err = tx.Exec(ctx, `INSERT INTO runs (id, project_id, generation, ordinal, terminal, metadata, evidence_digest, evidence_bytes, bundle_digest, bundle_bytes, queue_state, runtime, priority,
					scenario, runtime_name, result, indexed, binary_digest, binary_bytes, binary_platform)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, true, $17, $18, $19)`, id, project, int64(r.Generation), int64(r.Ordinal), terminal, []byte(r.Metadata),
				r.Digest, r.Bytes, r.BundleDigest, r.BundleBytes, queue, runtime, priority, nullable(index.Scenario), nullable(index.Runtime), nullable(index.Result),
				pin.Digest, pin.Bytes, pin.Platform)
		}
		rec = &r
		return err
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// Runs returns every run in admission order.
func (s *Store) Runs(ctx context.Context) ([]artifact.RunRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+runColumns+" FROM runs ORDER BY ordinal")
	if err != nil {
		return nil, storageErr("runs", err)
	}
	defer rows.Close()
	out := []artifact.RunRecord{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, storageErr("runs", err)
		}
		out = append(out, r)
	}
	return out, storageErr("runs", rows.Err())
}

// nullable stores an empty index field as NULL, which no filter matches and
// the partial filter indexes leave out.
func nullable(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// ListRuns returns a page of runs, newest first, keyed on the admission
// ordinal. One row beyond the limit tells whether another page follows.
func (s *Store) ListRuns(ctx context.Context, q artifact.RunQuery) (*artifact.RunPage, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	q, before, err := q.Normalize()
	if err != nil {
		return nil, err
	}
	if q.Projects != nil && len(q.Projects) == 0 {
		return &artifact.RunPage{Runs: []artifact.RunRecord{}}, nil
	}
	rows, err := s.pool.Query(ctx, "SELECT "+runColumns+` FROM runs
		WHERE ($1::text[] IS NULL OR project_id = ANY($1))
			AND ($2 = '' OR scenario = $2) AND ($3 = '' OR runtime_name = $3) AND ($4 = '' OR result = $4)
			AND ($5 = 0 OR ordinal < $5) AND (NOT $7 OR NOT terminal) AND (NOT $8 OR NOT indexed)
		ORDER BY ordinal DESC LIMIT $6`, q.Projects, q.Scenario, q.Runtime, q.Result, int64(before), q.Limit+1, q.Unfinished, q.Unindexed)
	if err != nil {
		return nil, storageErr("run list", err)
	}
	defer rows.Close()
	p := &artifact.RunPage{Runs: []artifact.RunRecord{}}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, storageErr("run list", err)
		}
		p.Runs = append(p.Runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, storageErr("run list", err)
	}
	if len(p.Runs) > q.Limit {
		p.Runs = p.Runs[:q.Limit]
		p.NextCursor = q.NextCursor(p.Runs[q.Limit-1].Ordinal)
	}
	return p, nil
}

// IndexRun sets the index of a run stored before migration 005. An indexed
// run keeps its index: its own publications maintain it.
func (s *Store) IndexRun(ctx context.Context, id string, index artifact.RunIndex) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if !index.Valid() {
		return invalid("invalid run index")
	}
	var found bool
	err := s.pool.QueryRow(ctx, `WITH updated AS (
			UPDATE runs SET scenario = $2, runtime_name = $3, result = $4, indexed = true WHERE id = $1 AND NOT indexed RETURNING id)
		SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1)`, id, nullable(index.Scenario), nullable(index.Runtime), nullable(index.Result)).Scan(&found)
	if err != nil {
		return storageErr("run index", err)
	}
	if !found {
		return artifact.ErrNotFound
	}
	return nil
}

// runBlob reads one of a run's objects; column is evidence or bundle.
func (s *Store) runBlob(ctx context.Context, id, column string) ([]byte, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var b blob
	err := s.pool.QueryRow(ctx, "SELECT "+column+"_digest, "+column+"_bytes FROM runs WHERE id = $1", id).Scan(&b.digest, &b.size)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && b.digest == "" {
		return nil, artifact.ErrNotFound
	}
	if err != nil {
		return nil, storageErr("run "+column, err)
	}
	return s.readBlob(ctx, b.digest, b.size)
}

// RunBundle returns a run's private execution bundle.
func (s *Store) RunBundle(ctx context.Context, id string) ([]byte, error) {
	return s.runBlob(ctx, id, "bundle")
}

// RunEvidence returns a terminal run's evidence.
func (s *Store) RunEvidence(ctx context.Context, id string) ([]byte, error) {
	return s.runBlob(ctx, id, "evidence")
}

// SaveEvaluation records an immutable evaluation of a terminal run. The
// result and source are stored as objects; recording never changes the run.
func (s *Store) SaveEvaluation(ctx context.Context, id, runID string, metadata, result, source []byte) (*artifact.EvaluationRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var rec *artifact.EvaluationRecord
	err := s.inTx(ctx, "evaluation", func(tx pgx.Tx) error {
		if _, err := lockInstallation(ctx, tx); err != nil {
			return err
		}
		var terminal bool
		err := tx.QueryRow(ctx, "SELECT terminal FROM runs WHERE id = $1 FOR UPDATE", runID).Scan(&terminal)
		if errors.Is(err, pgx.ErrNoRows) {
			return artifact.ErrNotFound
		}
		if err != nil {
			return err
		}
		if !terminal {
			return artifact.ErrConflict
		}
		if !validEvaluationID(id) || !json.Valid(metadata) || len(metadata) > maxRunMetadata || len(result) == 0 || len(source) == 0 {
			return invalid("invalid evaluation publication")
		}
		var exists bool
		var count int64
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM evaluations WHERE id = $1)", id).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return artifact.ErrConflict
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM evaluations WHERE run_id = $1", runID).Scan(&count); err != nil {
			return err
		}
		if count >= int64(s.limits.EvaluationsPerRun) || int64(len(result)) > s.limits.RunBytes || int64(len(source)) > s.limits.BlobBytes {
			return artifact.ErrLimit
		}
		r := artifact.EvaluationRecord{ID: id, RunID: runID, Ordinal: uint64(count + 1), Metadata: append(json.RawMessage(nil), metadata...),
			ResultDigest: digestOf(result), ResultBytes: int64(len(result)), SourceDigest: digestOf(source), SourceBytes: int64(len(source))}
		additional, err := newBytes(ctx, tx, blob{r.ResultDigest, r.ResultBytes}, blob{r.SourceDigest, r.SourceBytes})
		if err != nil {
			return err
		}
		if err := s.checkSpace(ctx, tx, additional, reserve{}); err != nil {
			return err
		}
		if err := s.putBlob(ctx, r.ResultDigest, result); err != nil {
			return err
		}
		if err := s.putBlob(ctx, r.SourceDigest, source); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO evaluations (id, run_id, ordinal, metadata, result_digest, result_bytes, source_digest, source_bytes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, runID, int64(r.Ordinal), []byte(r.Metadata), r.ResultDigest, r.ResultBytes, r.SourceDigest, r.SourceBytes); err != nil {
			return err
		}
		rec = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

const evaluationColumns = "id, run_id, ordinal, metadata, result_digest, result_bytes, source_digest, source_bytes"

func scanEvaluation(row pgx.Row) (artifact.EvaluationRecord, error) {
	var r artifact.EvaluationRecord
	var ordinal int64
	var metadata []byte
	err := row.Scan(&r.ID, &r.RunID, &ordinal, &metadata, &r.ResultDigest, &r.ResultBytes, &r.SourceDigest, &r.SourceBytes)
	r.Ordinal, r.Metadata = uint64(ordinal), metadata
	return r, err
}

// Evaluations returns a run's evaluation envelopes in recording order, never
// their source.
func (s *Store) Evaluations(ctx context.Context, runID string) ([]artifact.EvaluationRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1)", runID).Scan(&exists); err != nil {
		return nil, storageErr("evaluations", err)
	}
	if !exists {
		return nil, artifact.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, "SELECT "+evaluationColumns+" FROM evaluations WHERE run_id = $1 ORDER BY ordinal", runID)
	if err != nil {
		return nil, storageErr("evaluations", err)
	}
	defer rows.Close()
	out := []artifact.EvaluationRecord{}
	for rows.Next() {
		r, err := scanEvaluation(rows)
		if err != nil {
			return nil, storageErr("evaluations", err)
		}
		out = append(out, r)
	}
	return out, storageErr("evaluations", rows.Err())
}

// EvaluationResult returns an evaluation's envelope and its digest-checked
// result.
func (s *Store) EvaluationResult(ctx context.Context, id string) (*artifact.EvaluationRecord, []byte, error) {
	if err := s.ready(ctx); err != nil {
		return nil, nil, err
	}
	r, err := scanEvaluation(s.pool.QueryRow(ctx, "SELECT "+evaluationColumns+" FROM evaluations WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, artifact.ErrNotFound
	}
	if err != nil {
		return nil, nil, storageErr("evaluation", err)
	}
	raw, err := s.readBlob(ctx, r.ResultDigest, r.ResultBytes)
	if err != nil {
		return nil, nil, err
	}
	return &r, raw, nil
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

// suiteMembers checks a suite's metadata and returns its member run IDs: a
// caller, a creation time, a config and one to 32 members, each a run of the
// suite's project or an error.
func suiteMembers(ctx context.Context, tx pgx.Tx, project string, raw []byte) ([]string, bool, error) {
	var m suiteMetadata
	if decodeStrict(raw, &m) != nil || m.Created.IsZero() || m.Config == "" || len(m.Caller) == 0 || string(m.Caller) == "null" || len(m.Members) == 0 || len(m.Members) > 32 {
		return nil, false, nil
	}
	ids := []string{}
	for _, member := range m.Members {
		if member.Label == "" || (member.RunID == "") == (member.Error == "") {
			return nil, false, nil
		}
		if member.RunID != "" && !slices.Contains(ids, member.RunID) {
			ids = append(ids, member.RunID)
		}
	}
	if len(ids) == 0 {
		return ids, true, nil
	}
	rows, err := tx.Query(ctx, "SELECT project_id FROM runs WHERE id = ANY($1)", ids)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	found := 0
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, false, err
		}
		if p != project {
			return nil, false, nil
		}
		found++
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return ids, found == len(ids), nil
}

// CheckSuiteCapacity refuses when pending more suites would not fit under
// Limits.Suites. It writes nothing; SaveSuite enforces the cap again.
func (s *Store) CheckSuiteCapacity(ctx context.Context, pending int) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if pending < 0 {
		return invalid("invalid suite reservation")
	}
	var count int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM suites").Scan(&count); err != nil {
		return storageErr("suite capacity", err)
	}
	if int64(pending) >= int64(s.limits.Suites)-count {
		return artifact.ErrLimit
	}
	return nil
}

// SaveSuite records a suite once after all its member admissions.
func (s *Store) SaveSuite(ctx context.Context, id, project string, metadata []byte) (*artifact.SuiteRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if !validSuiteID(id) || !artifact.ValidProject(project) {
		return nil, invalid("invalid suite publication")
	}
	var rec *artifact.SuiteRecord
	err := s.inTx(ctx, "suite", func(tx pgx.Tx) error {
		inst, err := lockInstallation(ctx, tx)
		if err != nil {
			return err
		}
		members, ok, err := suiteMembers(ctx, tx, project, metadata)
		if err != nil {
			return err
		}
		if !ok {
			return invalid("invalid suite publication")
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM suites WHERE id = $1) OR EXISTS (SELECT 1 FROM tombstones WHERE kind = 'suite' AND id = $1)`, id).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return artifact.ErrConflict
		}
		var count int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM suites").Scan(&count); err != nil {
			return err
		}
		if count >= int64(s.limits.Suites) {
			return artifact.ErrLimit
		}
		r := artifact.SuiteRecord{ID: id, Project: project, Ordinal: uint64(inst.suiteOrdinal + 1), Metadata: append(json.RawMessage(nil), metadata...)}
		if _, err := tx.Exec(ctx, "UPDATE installation SET suite_ordinal = $1", int64(r.Ordinal)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO suites (id, project_id, ordinal, metadata) VALUES ($1, $2, $3, $4)", id, project, int64(r.Ordinal), []byte(r.Metadata)); err != nil {
			return err
		}
		for _, run := range members {
			if _, err := tx.Exec(ctx, "INSERT INTO suite_members (suite_id, run_id) VALUES ($1, $2)", id, run); err != nil {
				return err
			}
		}
		rec = &r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// Suites returns the suites, newest first.
func (s *Store) Suites(ctx context.Context) ([]artifact.SuiteRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT id, project_id, ordinal, metadata FROM suites ORDER BY ordinal DESC")
	if err != nil {
		return nil, storageErr("suites", err)
	}
	defer rows.Close()
	out := []artifact.SuiteRecord{}
	for rows.Next() {
		var r artifact.SuiteRecord
		var ordinal int64
		var metadata []byte
		if err := rows.Scan(&r.ID, &r.Project, &ordinal, &metadata); err != nil {
			return nil, storageErr("suites", err)
		}
		r.Ordinal, r.Metadata = uint64(ordinal), metadata
		out = append(out, r)
	}
	return out, storageErr("suites", rows.Err())
}

// DeleteRun removes a terminal run that belongs to no suite, with its
// evaluations, keeping a tombstone. A run still in flight, or in a suite, is a
// conflict. Its objects stay until Sweep finds them unreferenced.
func (s *Store) DeleteRun(ctx context.Context, id string, at time.Time) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	return s.inTx(ctx, "delete run", func(tx pgx.Tx) error {
		var terminal bool
		err := tx.QueryRow(ctx, "SELECT terminal FROM runs WHERE id = $1 FOR UPDATE", id).Scan(&terminal)
		if errors.Is(err, pgx.ErrNoRows) {
			return artifact.ErrNotFound
		}
		if err != nil {
			return err
		}
		var member bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM suite_members WHERE run_id = $1)", id).Scan(&member); err != nil {
			return err
		}
		if !terminal || member {
			return artifact.ErrConflict
		}
		return removeRuns(ctx, tx, []string{id}, at)
	})
}

// DeleteSuite removes a suite and its member runs, with their evaluations,
// keeping tombstones. A member that is not terminal is a conflict.
func (s *Store) DeleteSuite(ctx context.Context, id string, at time.Time) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	return s.inTx(ctx, "delete suite", func(tx pgx.Tx) error {
		var found string
		err := tx.QueryRow(ctx, "SELECT id FROM suites WHERE id = $1 FOR UPDATE", id).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			return artifact.ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT r.id, r.terminal FROM runs r JOIN suite_members m ON m.run_id = r.id WHERE m.suite_id = $1 ORDER BY r.id FOR UPDATE OF r`, id)
		if err != nil {
			return err
		}
		var members []string
		for rows.Next() {
			var run string
			var terminal bool
			if err := rows.Scan(&run, &terminal); err != nil {
				rows.Close()
				return err
			}
			if !terminal {
				rows.Close()
				return artifact.ErrConflict
			}
			members = append(members, run)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM suites WHERE id = $1", id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO tombstones (kind, id, deleted_at) VALUES ('suite', $1, $2)", id, at.UTC()); err != nil {
			return err
		}
		return removeRuns(ctx, tx, members, at)
	})
}

// removeRuns deletes the runs (their evaluations cascade) and records a
// tombstone for each. A run another live suite still lists makes the delete
// fail with a foreign-key conflict.
func removeRuns(ctx context.Context, tx pgx.Tx, ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, "DELETE FROM runs WHERE id = ANY($1)", ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "INSERT INTO tombstones (kind, id, deleted_at) SELECT 'run', unnest($1::text[]), $2", ids, at.UTC())
	return err
}

// Tombstones returns the deleted runs and suites, ordered by ID.
func (s *Store) Tombstones(ctx context.Context) (runs, suites []artifact.Tombstone, err error) {
	if err := s.ready(ctx); err != nil {
		return nil, nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT kind, id, deleted_at FROM tombstones ORDER BY id")
	if err != nil {
		return nil, nil, storageErr("tombstones", err)
	}
	defer rows.Close()
	runs, suites = []artifact.Tombstone{}, []artifact.Tombstone{}
	for rows.Next() {
		var kind string
		var t artifact.Tombstone
		if err := rows.Scan(&kind, &t.ID, &t.Deleted); err != nil {
			return nil, nil, storageErr("tombstones", err)
		}
		t.Deleted = t.Deleted.UTC()
		if kind == "run" {
			runs = append(runs, t)
		} else {
			suites = append(suites, t)
		}
	}
	return runs, suites, storageErr("tombstones", rows.Err())
}
