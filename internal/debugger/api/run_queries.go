package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Run reads that only MCP had: the compact summary,
// bounded evidence pages and recorded assertion evaluations. Failures carry
// the platform code (OperationError).

// EvaluationInfo is one recorded evaluation of a run, without its result.
type EvaluationInfo struct {
	ID      string            `json:"id"`
	RunID   string            `json:"run_id"`
	Client  string            `json:"client"`
	Created time.Time         `json:"created"`
	Outcome assertion.Outcome `json:"outcome"`
}

// Evaluation is one recorded evaluation with its assertion report. The
// scenario text it evaluated is never returned.
type Evaluation struct {
	EvaluationInfo
	Result *platform.AssertionReport `json:"result"`
}

func evaluationInfoOf(e platform.EvaluationInfo) EvaluationInfo {
	return EvaluationInfo{ID: e.ID, RunID: e.RunID, Client: e.Caller.Client, Created: e.Created, Outcome: e.Outcome}
}

func (s *Server) getRunSummary(w http.ResponseWriter, r *http.Request) {
	summary, err := s.runs.GetRunSummary(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// queryEvidence answers one evidence query, the query parameters naming the
// family, its parameters and the page.
func (s *Server) queryEvidence(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ints := map[string]int{}
	for _, name := range []string{"from_seq", "to_seq", "page_items", "page_bytes"} {
		if value := q.Get(name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil {
				writeOperationError(w, &platform.Error{Code: platform.CodeInvalidRequest, Message: name + " must be an integer"})
				return
			}
			ints[name] = n
		}
	}
	query := platform.EvidenceQuery{Family: platform.EvidenceFamily(q.Get("family")), Pipeline: q.Get("pipeline"), Component: q.Get("component"),
		Signal: q.Get("signal"), Side: q.Get("side"), Entity: q.Get("entity"), FromSeq: ints["from_seq"], ToSeq: ints["to_seq"]}
	page := platform.EvidencePage{Cursor: q.Get("cursor"), Items: ints["page_items"], Bytes: ints["page_bytes"]}
	result, err := s.runs.QueryEvidence(r.Context(), callerOf(r), r.PathValue("id"), query, page)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listEvaluations(w http.ResponseWriter, r *http.Request) {
	list, err := s.runs.ListEvaluations(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	out := make([]EvaluationInfo, 0, len(list))
	for _, e := range list {
		out = append(out, evaluationInfoOf(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// getEvaluation returns one recorded evaluation of the run the path names;
// another run's evaluation is unknown here.
func (s *Server) getEvaluation(w http.ResponseWriter, r *http.Request) {
	e, err := s.runs.GetEvaluation(r.Context(), callerOf(r), r.PathValue("evaluation"))
	if err == nil && e.RunID != r.PathValue("id") {
		err = &platform.Error{Code: platform.CodeNotFound, Message: "unknown evaluation"}
	}
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Evaluation{EvaluationInfo: evaluationInfoOf(e.EvaluationInfo), Result: e.Result})
}
