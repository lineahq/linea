package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Run retention with the permissions HTTP applies: a run's
// owner or an admin keeps and unkeeps it, only an admin prunes.

type runKeptResult struct {
	RunID string `json:"run_id"`
	Kept  bool   `json:"kept"`
}

func (r runKeptResult) summary() string {
	if r.Kept {
		return "Run kept."
	}
	return "Run no longer kept."
}

type pruneRunsInput struct {
	Before   string `json:"before,omitempty" jsonschema:"Only runs admitted before this RFC 3339 time; default any time."`
	Project  string `json:"project,omitempty" jsonschema:"Only runs of this project; default every project the caller administers."`
	Scenario string `json:"scenario,omitempty" jsonschema:"Only runs of this scenario name."`
	Runtime  string `json:"runtime,omitempty" jsonschema:"Only runs on this runtime name; built-in is the debugger's own."`
	Result   string `json:"result,omitempty" jsonschema:"Only runs that ended this way: completed, partial, failed or cancelled for a run with a report, otherwise the state error."`
	DryRun   bool   `json:"dry_run,omitempty" jsonschema:"Delete nothing and report what the same call would delete."`
}

type pruneRunsResult struct {
	Runs   int   `json:"runs"`
	Bytes  int64 `json:"bytes"`
	DryRun bool  `json:"dry_run"`
}

func (r pruneRunsResult) summary() string {
	if r.DryRun {
		return fmt.Sprintf("Run prune: %d runs would be removed, %d bytes would be freed.", r.Runs, r.Bytes)
	}
	return fmt.Sprintf("Run prune: %d runs removed, %d bytes freed.", r.Runs, r.Bytes)
}

type retentionPolicyView struct {
	Runs       int   `json:"runs"`
	AgeSeconds int64 `json:"age_seconds"`
}

type storageUsageResult struct {
	Limit     int64               `json:"limit"`
	RunLimit  int64               `json:"run_limit"`
	Used      int64               `json:"used"`
	Runs      int64               `json:"runs"`
	KeptRuns  int64               `json:"kept_runs"`
	OldestRun *time.Time          `json:"oldest_run,omitempty"`
	Retention retentionPolicyView `json:"retention"`
}

func (r storageUsageResult) summary() string {
	return fmt.Sprintf("Run storage: %d runs, %d kept; %d bytes used of %d.", r.Runs, r.KeptRuns, r.Used, r.Limit)
}

func addRetentionTools(server *sdk.Server, service *platform.Service) {
	keep := func(kept bool) func(context.Context, runInput) (summarized, error) {
		return func(ctx context.Context, in runInput) (summarized, error) {
			op := service.UnkeepRun
			if kept {
				op = service.KeepRun
			}
			run, err := op(ctx, callerOf(ctx), in.RunID)
			if err != nil {
				return nil, err
			}
			return runKeptResult{RunID: run.ID, Kept: run.Kept}, nil
		}
	}
	addTool(server, "linea_keep_run", "Keep a run: retention and prune skip it until linea_unkeep_run. The run may be unfinished. Allowed for the run's owner and for an admin.", keep(true))
	addTool(server, "linea_unkeep_run", "Make a kept run subject to retention and prune again. Allowed for the run's owner and for an admin.", keep(false))
	addTool(server, "linea_prune_runs", "Delete the finished runs the filters select, as linea_delete_run does, except those retention keeps: a kept run, a run of a suite and the origin of a saved source's current revision. Use dry_run first to see the count and bytes. Only an admin may prune. Irreversible unless dry_run: confirm with the user before calling.",
		func(ctx context.Context, in pruneRunsInput) (summarized, error) {
			req := platform.RunPruneRequest{RunFilter: platform.RunFilter{Scenario: in.Scenario, Runtime: in.Runtime, Result: in.Result}, Project: in.Project, DryRun: in.DryRun}
			if in.Before != "" {
				before, err := time.Parse(time.RFC3339, in.Before)
				if err != nil {
					return nil, invalidRequest("before must be an RFC 3339 time")
				}
				req.Before = before
			}
			res, err := service.PruneRuns(ctx, callerOf(ctx), req)
			if err != nil {
				return nil, err
			}
			return pruneRunsResult{Runs: res.Runs, Bytes: res.Bytes, DryRun: res.DryRun}, nil
		})
	addTool(server, "linea_get_storage_usage", "Read the workspace's stored bytes against its limits, its run count, kept runs and oldest run, and the retention policy a team server applies.",
		func(ctx context.Context, in struct{}) (summarized, error) {
			u, err := service.GetStorageUsage(ctx, callerOf(ctx))
			if err != nil {
				return nil, err
			}
			return storageUsageResult{Limit: u.Limit, RunLimit: u.RunLimit, Used: u.Used, Runs: u.Runs, KeptRuns: u.KeptRuns, OldestRun: u.OldestRun,
				Retention: retentionPolicyView{Runs: u.Retention.Runs, AgeSeconds: int64(u.Retention.Age / time.Second)}}, nil
		})
}
