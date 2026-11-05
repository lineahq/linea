package platform

import (
	"context"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// RunLog is the tail of a run's Collector log: at most
// engine.RunLogLimit bytes of whole lines at info level and above, with every
// environment value the configuration read redacted. Only a failed run, or
// one whose execution ended partial or failed, keeps it; it is stored with
// the run's evidence and deleted with the run.
type RunLog struct {
	RunID string `json:"run_id"`
	// Kept says the run kept its log; Log is empty when it did not, or when
	// the Collector logged nothing.
	Kept bool   `json:"kept"`
	Log  string `json:"log"`
	// Truncated says earlier lines were dropped to stay within the limit.
	Truncated bool `json:"truncated"`
}

// keepsLog reports whether a run that ends in state with report keeps its
// log: it failed, or its execution was partial or failed.
func keepsLog(state State, report *engine.Report) bool {
	if state == StateError {
		return true
	}
	if state != StateDone || report == nil || report.Result == nil {
		return false
	}
	return report.Result.Status == runresult.StatusPartial || report.Result.Status == runresult.StatusFailed
}

// GetRunLog returns a run's kept Collector log tail. A run that has not
// ended, or ended without keeping its log, answers with Kept false.
func (s *Service) GetRunLog(ctx context.Context, caller Caller, id string) (_ *RunLog, err error) {
	defer s.audit(&caller, "GetRunLog", &err)()
	r, err := s.lookup(ctx, caller, authz.RunRead, id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.terminalLocked() && keepsLog(r.state, r.report)
	out := &RunLog{RunID: r.id, Kept: kept}
	if kept {
		out.Log, out.Truncated = r.log, r.logTruncated
	}
	return out, nil
}
