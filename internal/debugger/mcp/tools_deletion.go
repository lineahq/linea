package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Deleting finished history with the rules HTTP applies: only terminal runs, and a suite
// member only with its suite.

type deleteSuiteInput struct {
	SuiteID string `json:"suite_id" jsonschema:"A suite whose member runs have all finished."`
}

type runDeletedResult struct {
	Deleted bool `json:"deleted"`
}

func (runDeletedResult) summary() string { return "Run deleted." }

type suiteDeletedResult struct {
	Deleted bool `json:"deleted"`
}

func (suiteDeletedResult) summary() string { return "Suite and its member runs deleted." }

func addDeletionTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_delete_run", "Delete a finished run that belongs to no suite: its report, evidence, inputs and recorded evaluations. A queued or running run, or a suite member, is a conflict. Irreversible: confirm with the user before calling.",
		func(ctx context.Context, in runInput) (summarized, error) {
			if err := service.DeleteRun(ctx, callerOf(ctx), in.RunID); err != nil {
				return nil, err
			}
			return runDeletedResult{Deleted: true}, nil
		})
	addTool(server, "linea_delete_suite", "Delete a suite and every member run. A member that is queued or running is a conflict and nothing is deleted. Irreversible: confirm with the user before calling.",
		func(ctx context.Context, in deleteSuiteInput) (summarized, error) {
			if err := service.DeleteSuite(ctx, callerOf(ctx), in.SuiteID); err != nil {
				return nil, err
			}
			return suiteDeletedResult{Deleted: true}, nil
		})
}
