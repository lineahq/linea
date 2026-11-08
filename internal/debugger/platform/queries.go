package platform

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/evidence"
)

// Evidence projections under platform names; their definitions
// live in internal/debugger/evidence.
type (
	RunSummary       = evidence.Summary
	AssertionSummary = evidence.AssertionSummary
	SummaryError     = evidence.SummaryError
	SummaryValue     = evidence.SummaryValue
	EvidenceQuery    = evidence.Query
	EvidenceFamily   = evidence.Family
	EvidenceResult   = evidence.Result
	EvidenceItem     = evidence.Item
	EvidenceOversize = evidence.Oversize
)

// Evidence query families.
const (
	EvidenceStageChanges = evidence.FamilyStageChanges
	EvidenceRecords      = evidence.FamilyRecords
	EvidenceJourney      = evidence.FamilyJourney
	EvidenceStatements   = evidence.FamilyStatements
	EvidenceEvents       = evidence.FamilyEvents
	// EvidenceRecommendations are the run's recommendations.
	EvidenceRecommendations = evidence.FamilyRecommendations
)

// EvidencePage bounds one evidence answer. Zero sizes use the service's
// budgets; larger sizes are clamped to them and the clamp is recorded.
// Cursor continues a previous answer for the same run, family and parameters.
type EvidencePage struct {
	Cursor string
	Items  int
	Bytes  int
}

// Summary is the run's compact summary, the definition GetRunSummary
// returns; nil while the run is queued or running.
func (r Run) Summary() *RunSummary { return evidence.Summarize(r.evidence()) }

// SummarizeAssertions counts an assertion report's results by outcome, as
// the run summary does; nil without a report.
func SummarizeAssertions(report *AssertionReport) *AssertionSummary {
	return evidence.SummarizeAssertions(report)
}

func (r Run) evidence() evidence.Run {
	var end evidence.End
	switch r.State {
	case StateDone:
		end = evidence.EndDone
	case StateCancelled:
		end = evidence.EndCancelled
	case StateError:
		end = evidence.EndError
		if r.Interrupted {
			end = evidence.EndInterrupted
		}
	}
	return evidence.Run{ID: r.ID, End: end, Error: r.Error, Report: r.Report}
}

// GetRunSummary returns a terminal run's compact summary: grouped and capped,
// with incomplete runs stated. A queued or running run is a conflict.
func (s *Service) GetRunSummary(ctx context.Context, caller Caller, id string) (_ *RunSummary, err error) {
	defer s.audit(&caller, "GetRunSummary", &err)()
	r, err := s.lookup(ctx, caller, authz.RunRead, id)
	if err != nil {
		return nil, err
	}
	v := r.View(false)
	summary := v.Summary()
	if summary == nil {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("run is %s", v.State)}
	}
	return summary, nil
}

// QueryEvidence answers one evidence query over a terminal run's report, one
// bounded page at a time. It never executes or replays anything. A
// queued or running run, or one without a report, is a conflict; an unknown
// stage, record or processor is not_found; a bad query, page or cursor
// (including one issued for another run, family or parameters) is
// invalid_request.
func (s *Service) QueryEvidence(ctx context.Context, caller Caller, id string, q EvidenceQuery, page EvidencePage) (_ *EvidenceResult, err error) {
	defer s.audit(&caller, "QueryEvidence", &err)()
	r, err := s.lookup(ctx, caller, authz.RunRead, id)
	if err != nil {
		return nil, err
	}
	v := r.View(true)
	if v.State != StateDone && v.State != StateCancelled && v.State != StateError {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("run is %s", v.State)}
	}
	if v.Report == nil || v.Report.Result == nil {
		return nil, &Error{Code: CodeConflict, Message: "run has no report"}
	}
	if page.Items < 0 || page.Bytes < 0 {
		return nil, &Error{Code: CodeInvalidRequest, Message: "page sizes must not be negative"}
	}
	items, bytes, clamped := s.budgets.page(page.Items, page.Bytes)
	result, err := evidence.QueryRun(v.evidence(), q, evidence.Page{Cursor: page.Cursor, Items: items, Bytes: bytes})
	switch {
	case errors.Is(err, evidence.ErrInvalidQuery):
		return nil, newError(CodeInvalidRequest, err)
	case errors.Is(err, evidence.ErrNotFound):
		return nil, newError(CodeNotFound, err)
	case err != nil:
		return nil, classify(err, CodeInternal)
	}
	result.Page.Clamped = clamped
	return result, nil
}

// page applies the query page budgets to requested sizes (zero is the
// default), recording any size above the policy.
func (b Budgets) page(items, bytes int) (int, int, []engine.Clamp) {
	var clamped []engine.Clamp
	if items == 0 {
		items = b.PageItems
	} else if items > b.PageItems {
		clamped = append(clamped, engine.Clamp{Limit: "page_items", Requested: strconv.Itoa(items), Effective: strconv.Itoa(b.PageItems)})
		items = b.PageItems
	}
	if bytes == 0 {
		bytes = b.PageBytes
	} else if bytes > b.PageBytes {
		clamped = append(clamped, engine.Clamp{Limit: "page_bytes", Requested: strconv.Itoa(bytes), Effective: strconv.Itoa(b.PageBytes)})
		bytes = b.PageBytes
	}
	return items, bytes, clamped
}
