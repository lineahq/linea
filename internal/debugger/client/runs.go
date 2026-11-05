package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/lineahq/linea/internal/debugger/api"
)

// Reads of one retained run, suite reads, OTTL previews and candidates.
// Each returns the server's JSON as sent, so the CLI
// prints exactly what HTTP returns.

func runPath(id string) string { return "/api/v1/runs/" + url.PathEscape(id) }

// GetRun returns a run's state and, once finished, its report without
// snapshots.
func (c *Client) GetRun(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, runPath(id), nil, http.StatusOK)
}

// RunReport returns a run's complete report; with wait it blocks until the
// run finishes, otherwise an unfinished run is a conflict.
func (c *Client) RunReport(ctx context.Context, id string, wait bool) ([]byte, error) {
	path := runPath(id) + "/report"
	if wait {
		return c.awaited(ctx, path+"?wait=true")
	}
	return c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
}

// CancelRun cancels a queued or running run and returns its state.
func (c *Client) CancelRun(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodPost, runPath(id)+"/cancel", nil, http.StatusAccepted)
}

// EvaluateAssertions evaluates a scenario's assertions against a finished
// run without executing it again.
func (c *Client) EvaluateAssertions(ctx context.Context, id, scenarioYAML string) ([]byte, error) {
	return c.do(ctx, http.MethodPost, runPath(id)+"/assertions", api.AssertionsRequest{ScenarioYAML: scenarioYAML}, http.StatusOK)
}

// RunDiffs returns a finished run's stage changes and OTTL reports.
func (c *Client) RunDiffs(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, runPath(id)+"/diffs", nil, http.StatusOK)
}

// Snapshot returns one recorded snapshot's bytes.
func (c *Client) Snapshot(ctx context.Context, id, ref string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, runPath(id)+"/snapshots/"+url.PathEscape(ref), nil, http.StatusOK)
}

// RunInputs returns the root sources a run was admitted with.
func (c *Client) RunInputs(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, runPath(id)+"/inputs", nil, http.StatusOK)
}

// PreviewOTTL evaluates one transform processor's statements on an input.
func (c *Client) PreviewOTTL(ctx context.Context, req api.OTTLRequest) ([]byte, error) {
	return c.do(ctx, http.MethodPost, "/api/v1/ottl/evaluate", req, http.StatusOK)
}

// CreateCandidate validates a proposed configuration or statement edit; a
// saved one answers 201.
func (c *Client) CreateCandidate(ctx context.Context, req api.CandidateRequest) ([]byte, error) {
	want := http.StatusOK
	if req.Save {
		want = http.StatusCreated
	}
	return c.do(ctx, http.MethodPost, "/api/v1/candidates", req, want)
}

// ListSuites returns the server's retained suites.
func (c *Client) ListSuites(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/suites", nil, http.StatusOK)
}

// GetSuite returns a suite's members with their states and outcomes.
func (c *Client) GetSuite(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/suites/"+url.PathEscape(id), nil, http.StatusOK)
}

// SuiteReport returns a suite's report; with wait it blocks until every
// member finishes.
func (c *Client) SuiteReport(ctx context.Context, id string, wait bool) ([]byte, error) {
	path := "/api/v1/suites/" + url.PathEscape(id) + "/report"
	if wait {
		return c.awaited(ctx, path+"?wait=true")
	}
	return c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
}

// RunSummary returns a finished run's compact summary.
func (c *Client) RunSummary(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, runPath(id)+"/summary", nil, http.StatusOK)
}

// RunEvidence returns one page of a finished run's evidence for the query
// parameters q (family, its parameters, cursor and page sizes).
func (c *Client) RunEvidence(ctx context.Context, id string, q url.Values) ([]byte, error) {
	return c.do(ctx, http.MethodGet, runPath(id)+"/evidence?"+q.Encode(), nil, http.StatusOK)
}

// RunEvaluations lists the evaluations recorded beside a run.
func (c *Client) RunEvaluations(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, runPath(id)+"/evaluations", nil, http.StatusOK)
}

// RunEvaluation returns one recorded evaluation of a run with its report.
func (c *Client) RunEvaluation(ctx context.Context, id, evaluation string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, runPath(id)+"/evaluations/"+url.PathEscape(evaluation), nil, http.StatusOK)
}

// DeleteRun deletes a finished run that belongs to no suite.
func (c *Client) DeleteRun(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, runPath(id), nil, http.StatusNoContent)
	return err
}

// DeleteSuite deletes a suite and its member runs.
func (c *Client) DeleteSuite(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/v1/suites/"+url.PathEscape(id), nil, http.StatusNoContent)
	return err
}

// TestStatementOnRun tests a candidate on a run's recorded configuration and
// inputs: 202 with the queued run, or 200 for an invalid candidate.
func (c *Client) TestStatementOnRun(ctx context.Context, id string, req api.StatementTestRequest) ([]byte, error) {
	return c.do(ctx, http.MethodPost, runPath(id)+"/statements/test", req, http.StatusAccepted, http.StatusOK)
}

// KeepRun protects a run from retention and prune and returns its state.
func (c *Client) KeepRun(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodPut, runPath(id)+"/keep", nil, http.StatusOK)
}

// UnkeepRun makes a kept run subject to retention again and returns its
// state.
func (c *Client) UnkeepRun(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodDelete, runPath(id)+"/keep", nil, http.StatusOK)
}

// PruneRuns deletes the runs req selects, or with req.DryRun only counts
// them: {"runs", "bytes", "dry_run"}.
func (c *Client) PruneRuns(ctx context.Context, req api.RunPruneRequest) ([]byte, error) {
	return c.do(ctx, http.MethodPost, "/api/v1/runs/prune", req, http.StatusOK)
}

// StorageUsage returns the workspace's storage use against its limits, its
// run counts and the retention policy.
func (c *Client) StorageUsage(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/storage", nil, http.StatusOK)
}
