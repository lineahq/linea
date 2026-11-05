package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
)

const maxEvaluationsPerRun = 100

// Fresh errors per call: adapters may annotate the *Error they receive.
func errEvaluationCap() error {
	return &Error{Code: CodeResourceExhausted, Message: fmt.Sprintf("a run keeps at most %d recorded evaluations", maxEvaluationsPerRun)}
}

func errUnknownEvaluation() error { return &Error{Code: CodeNotFound, Message: "unknown evaluation"} }

type retainedEvaluation struct {
	info   EvaluationInfo
	result []byte
	source []byte
}

// Stored metadata intentionally contains neither source nor result bytes.
type evaluationMetadata struct {
	Caller  Actor             `json:"caller"`
	Created time.Time         `json:"created"`
	Outcome assertion.Outcome `json:"outcome"`
}

// EvaluateAssertions uses recorded observations, without executing again or
// changing the run. Recording keeps independent evidence of this evaluation.
func (s *Service) EvaluateAssertions(ctx context.Context, caller Caller, id string, req AssertionsRequest) (_ *Evaluation, err error) {
	defer s.audit(&caller, "EvaluateAssertions", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if err := s.runIn(ctx, caller, authz.RunSubmit, id); err != nil {
		return nil, err
	}
	v, err := s.finished(ctx, caller, authz.RunRead, id, true) // Attribute checks read snapshots.
	if err != nil {
		return nil, err
	}
	rep, err := engine.EvaluateAssertions(v.Report, engine.AssertionsRequest{ScenarioYAML: req.ScenarioYAML})
	if err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
	e := &Evaluation{EvaluationInfo: EvaluationInfo{RunID: id, Caller: caller.Actor(), Created: time.Now().UTC(), Outcome: rep.Outcome}, Result: rep}
	if !req.Record {
		return e, nil
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		return nil, classify(err, CodeInternal)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, classify(err, CodeInternal)
	}
	e.ID = "eval-" + hex.EncodeToString(random[:])

	// Serialize recording with eviction and Close; an evicted handle cannot
	// acquire new evaluations. Keep the same service -> handle -> store order.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, classify(ErrClosed, CodeInternal)
	}
	r, exists := s.runs[id]
	if !exists {
		return nil, classify(s.unknownRunLocked(id), CodeInternal)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, classify(err, CodeCancelled)
	}
	count := len(r.evaluations)
	if s.repository != nil {
		records, err := s.repository.Evaluations(ctx, id)
		if err != nil {
			return nil, classify(err, CodeStorageError)
		}
		count = len(records)
	}
	if count >= maxEvaluationsPerRun {
		return nil, errEvaluationCap()
	}
	if s.repository != nil {
		metadata, err := json.Marshal(evaluationMetadata{Caller: caller.Actor(), Created: e.Created, Outcome: e.Outcome})
		if err != nil {
			return nil, classify(err, CodeInternal)
		}
		if _, err := s.repository.SaveEvaluation(ctx, e.ID, id, metadata, raw, []byte(req.ScenarioYAML)); err != nil {
			return nil, classify(err, CodeStorageError)
		}
	} else {
		r.evaluations = append(r.evaluations, retainedEvaluation{info: e.EvaluationInfo, result: raw, source: []byte(req.ScenarioYAML)})
	}
	e.Recorded = true
	return e, nil
}

// GetEvaluation returns recorded results only, never the private source.
func (s *Service) GetEvaluation(ctx context.Context, caller Caller, id string) (_ *Evaluation, err error) {
	defer s.audit(&caller, "GetEvaluation", &err)()
	if err := s.begin(ctx, caller, authz.RunRead); err != nil {
		return nil, err
	}
	if s.repository != nil {
		record, raw, err := s.repository.EvaluationResult(ctx, id)
		if errors.Is(err, artifact.ErrNotFound) {
			return nil, errUnknownEvaluation()
		}
		if err != nil {
			return nil, classify(err, CodeStorageError)
		}
		if err := s.runIn(ctx, caller, authz.RunRead, record.RunID); err != nil {
			return nil, err
		}
		e, err := decodeEvaluation(*record, raw)
		return e, classify(err, CodeStorageError)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		r.mu.Lock()
		for _, stored := range r.evaluations {
			if stored.info.ID == id {
				r.mu.Unlock()
				if err := s.authorizeIn(caller, authz.RunRead, r.project); err != nil {
					return nil, err
				}
				return evaluationResult(stored.info, stored.result)
			}
		}
		r.mu.Unlock()
	}
	return nil, errUnknownEvaluation()
}

// ListEvaluations returns recording order, independent of wall-clock changes.
func (s *Service) ListEvaluations(ctx context.Context, caller Caller, runID string) (_ []EvaluationInfo, err error) {
	defer s.audit(&caller, "ListEvaluations", &err)()
	if err := s.begin(ctx, caller, authz.RunRead); err != nil {
		return nil, err
	}
	if err := s.load(ctx, runID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, exists := s.runs[runID]
	if !exists {
		return nil, classify(s.unknownRunLocked(runID), CodeInternal)
	}
	if err := s.authorizeIn(caller, authz.RunRead, r.project); err != nil {
		return nil, err
	}
	result := []EvaluationInfo{}
	if s.repository != nil {
		records, err := s.repository.Evaluations(ctx, runID)
		if err != nil {
			return nil, classify(err, CodeStorageError)
		}
		for _, record := range records {
			info, err := evaluationInfo(record)
			if err != nil {
				return nil, classify(err, CodeStorageError)
			}
			result = append(result, info)
		}
	} else {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, stored := range r.evaluations {
			result = append(result, stored.info)
		}
	}
	return result, nil
}

func evaluationInfo(r artifact.EvaluationRecord) (EvaluationInfo, error) {
	var m evaluationMetadata
	if err := decodeStored(r.Metadata, &m); err != nil || m.Caller.validate() != nil || m.Created.IsZero() || !validEvaluationOutcome(m.Outcome) {
		return EvaluationInfo{}, fmt.Errorf("%w: evaluation metadata", artifact.ErrCorrupt)
	}
	return EvaluationInfo{ID: r.ID, RunID: r.RunID, Caller: m.Caller, Created: m.Created, Outcome: m.Outcome}, nil
}

func validEvaluationOutcome(outcome assertion.Outcome) bool {
	return outcome == assertion.Pass || outcome == assertion.Fail || outcome == assertion.Inconclusive || outcome == assertion.Error
}

func evaluationResult(info EvaluationInfo, raw []byte) (*Evaluation, error) {
	var result *AssertionReport
	if err := decodeStored(raw, &result); err != nil || result == nil || result.Outcome != info.Outcome {
		return nil, classify(fmt.Errorf("%w: evaluation result", artifact.ErrCorrupt), CodeStorageError)
	}
	return &Evaluation{EvaluationInfo: info, Recorded: true, Result: result}, nil
}

func decodeEvaluation(r artifact.EvaluationRecord, raw []byte) (*Evaluation, error) {
	info, err := evaluationInfo(r)
	if err != nil {
		return nil, err
	}
	return evaluationResult(info, raw)
}

// Recover application metadata/results as well as the store's envelope/blob
// checks. Workspace reads remain authoritative; no private sources are cached.
func (s *Service) recoverEvaluations() error {
	ctx := context.Background()
	for runID := range s.runs {
		records, err := s.repository.Evaluations(ctx, runID)
		if err != nil {
			return err
		}
		if len(records) > maxEvaluationsPerRun {
			return artifact.ErrLimit
		}
		for _, record := range records {
			_, raw, err := s.repository.EvaluationResult(ctx, record.ID)
			if err != nil {
				return err
			}
			if _, err := decodeEvaluation(record, raw); err != nil {
				return err
			}
		}
	}
	return nil
}
