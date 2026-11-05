package api

import (
	"net/http"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Run retention: keeping a run, pruning runs and the
// workspace's storage usage. The platform decides who may do each.

// RunPruneRequest selects the runs to prune; every field is optional. Only
// runs retention could evict match: finished, not kept, in no suite and
// the origin of no saved source's current revision.
type RunPruneRequest struct {
	// Before limits the prune to runs admitted before this time (RFC 3339).
	Before *time.Time `json:"before,omitempty"`
	// Project limits it to one project; empty means every project the
	// caller administers.
	Project  string `json:"project,omitempty"`
	Scenario string `json:"scenario,omitempty"`
	Runtime  string `json:"runtime,omitempty"`
	// Result is one of the values of the run list's result filter.
	Result string `json:"result,omitempty"`
	// DryRun removes nothing and reports what the same request would.
	DryRun bool `json:"dry_run,omitempty"`
}

func (r RunPruneRequest) prune() platform.RunPruneRequest {
	req := platform.RunPruneRequest{RunFilter: platform.RunFilter{Scenario: r.Scenario, Runtime: r.Runtime, Result: r.Result}, Project: r.Project, DryRun: r.DryRun}
	if r.Before != nil {
		req.Before = *r.Before
	}
	return req
}

// RunPruneResult counts the runs a prune removed, or would remove in a dry
// run, and the bytes of stored objects only they referenced.
type RunPruneResult struct {
	Runs   int   `json:"runs"`
	Bytes  int64 `json:"bytes"`
	DryRun bool  `json:"dry_run"`
}

// RetentionPolicy is the bound on retained runs; zero fields bound nothing.
type RetentionPolicy struct {
	// Runs is the most runs kept; protected runs count toward it.
	Runs int `json:"runs"`
	// AgeSeconds is how long a run is kept after its admission.
	AgeSeconds int64 `json:"age_seconds"`
}

// StorageUsage is the workspace's storage limits and use (GET /storage).
type StorageUsage struct {
	// Limit is the bytes of every stored object the workspace may hold, and
	// RunLimit the bytes one run may reserve.
	Limit    int64 `json:"limit"`
	RunLimit int64 `json:"run_limit"`
	// Used may exceed Limit when the limit was lowered; new work is then
	// refused.
	Used int64 `json:"used"`
	// Runs counts the stored runs and KeptRuns the kept ones.
	Runs     int64 `json:"runs"`
	KeptRuns int64 `json:"kept_runs"`
	// OldestRun is when the oldest run was admitted, absent without runs.
	OldestRun *time.Time      `json:"oldest_run,omitempty"`
	Retention RetentionPolicy `json:"retention"`
}

func storageUsageOf(u *platform.StorageUsage) StorageUsage {
	return StorageUsage{Limit: u.Limit, RunLimit: u.RunLimit, Used: u.Used, Runs: u.Runs, KeptRuns: u.KeptRuns, OldestRun: u.OldestRun,
		Retention: RetentionPolicy{Runs: u.Retention.Runs, AgeSeconds: int64(u.Retention.Age / time.Second)}}
}

func (s *Server) keepRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.KeepRun(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runOf(run))
}

func (s *Server) unkeepRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.UnkeepRun(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runOf(run))
}

func (s *Server) pruneRuns(w http.ResponseWriter, r *http.Request) {
	var req RunPruneRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.runs.PruneRuns(r.Context(), callerOf(r), req.prune())
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, RunPruneResult{Runs: res.Runs, Bytes: res.Bytes, DryRun: res.DryRun})
}

func (s *Server) getStorage(w http.ResponseWriter, r *http.Request) {
	usage, err := s.runs.GetStorageUsage(r.Context(), callerOf(r))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, storageUsageOf(usage))
}

func (s *Server) routeRunRetention() {
	s.mux.HandleFunc("PUT /api/v1/runs/{id}/keep", s.keepRun)
	s.mux.HandleFunc("DELETE /api/v1/runs/{id}/keep", s.unkeepRun)
	s.mux.HandleFunc("POST /api/v1/runs/prune", s.pruneRuns)
	s.mux.HandleFunc("GET /api/v1/storage", s.getStorage)
}

func runRetentionPaths() map[string]any {
	const owner = "Allowed for the run's owner and for an admin."
	return map[string]any{
		"/api/v1/runs/{id}/keep": map[string]any{
			"put": op("keepRun",
				"Keep a run: retention and prune skip it until it is unkept, and it may still be unfinished. "+owner+" Like every PUT, the request needs Content-Type: application/json, though it has no body.",
				[]any{idParam}, "", map[string]any{
					"200": reply("The run, with kept true.", ref("Run")),
					"403": operationFailure("The caller is neither the run's owner nor an admin."),
					"404": operationFailure("Unknown run, or a deleted run."),
					"503": operationFailure(noRetention),
				}),
			"delete": op("unkeepRun",
				"Make a kept run subject to retention and prune again. "+owner,
				[]any{idParam}, "", map[string]any{
					"200": reply("The run, with kept absent.", ref("Run")),
					"403": operationFailure("The caller is neither the run's owner nor an admin."),
					"404": operationFailure("Unknown run, or a deleted run."),
					"503": operationFailure(noRetention),
				}),
		},
		"/api/v1/runs/prune": map[string]any{"post": op("pruneRuns",
			"Delete the finished runs the filters select, as deleteRun does, except those retention keeps: a kept run, a run of a suite, and the origin of a saved source's current revision. With dry_run nothing is deleted and the response counts what the same request would remove now. Only an admin may prune; without project, every project the caller administers is pruned. The audit records counts, never run IDs.",
			nil, "RunPruneRequest", map[string]any{
				"200": reply("The runs and stored bytes removed, or that would be.", ref("RunPruneResult")),
				"400": operationFailure("Malformed request, a before that is not an RFC 3339 time, or an unknown result."),
				"403": operationFailure("The caller is not an admin of the project, or of any project."),
				"503": operationFailure(noRetention),
			})},
		"/api/v1/storage": map[string]any{"get": op("getStorage",
			"The workspace's stored bytes against its limits, its run count, kept runs and oldest run, and the retention policy a team server applies.",
			nil, "", map[string]any{
				"200": reply("The storage usage.", ref("StorageUsage")),
				"403": operationFailure("The caller may not read saved work."),
				"503": operationFailure(noRetention),
			})},
	}
}

// noRetention describes a server that keeps no run history to retain.
const noRetention = "The server keeps no saved work, so no run history."
