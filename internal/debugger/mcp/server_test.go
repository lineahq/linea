package mcp

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestServerListsInspectionTools(t *testing.T) {
	service := platform.New(platform.Options{})
	t.Cleanup(service.Close)
	server := httptest.NewServer(New(Options{Platform: service}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := session.InitializeResult().ServerInfo
	t.Cleanup(func() { _ = session.Close() })
	if info.Name != "linea" || info.Version != platform.CurrentVersions().Product {
		t.Fatalf("server info = %+v", info)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"linea_bisect_config", "linea_build_runtime", "linea_cancel_run", "linea_cancel_runtime_build", "linea_compare_configs", "linea_compare_runs", "linea_compare_versions", "linea_compose_manifest", "linea_control_fleet_rollout", "linea_copy_artifact", "linea_create_candidate", "linea_create_capture_session", "linea_create_fixture_from_capture", "linea_create_fleet_assignment", "linea_create_fleet_config", "linea_create_fleet_config_version", "linea_create_scenario", "linea_create_scenario_from_capture", "linea_delete_capture_session", "linea_delete_collector_group", "linea_delete_fleet_config", "linea_delete_run", "linea_delete_runtime", "linea_delete_suite", "linea_evaluate_assertions", "linea_export_artifacts_for_ci", "linea_export_capture_selection", "linea_get_artifact", "linea_get_capture_records", "linea_get_capture_session", "linea_get_collector", "linea_get_collector_catalog", "linea_get_collector_group", "linea_get_collector_support", "linea_get_default_runtime", "linea_get_evaluation", "linea_get_execution_trace", "linea_get_fleet_agent", "linea_get_fleet_assignment", "linea_get_fleet_config", "linea_get_fleet_config_version", "linea_get_fleet_overview", "linea_get_fleet_rollout", "linea_get_pipeline", "linea_get_run", "linea_get_run_inputs", "linea_get_run_log", "linea_get_runtime", "linea_get_runtime_build", "linea_get_runtime_storage", "linea_get_snapshot", "linea_get_storage_usage", "linea_get_suite_junit", "linea_get_test_results", "linea_import_artifacts", "linea_import_runtime", "linea_keep_run", "linea_list_artifact_revisions", "linea_list_artifacts", "linea_list_audit_events", "linea_list_capture_points", "linea_list_capture_sessions", "linea_list_collector_group_members", "linea_list_collector_groups", "linea_list_collectors", "linea_list_evaluations", "linea_list_fleet_agents", "linea_list_fleet_assignments", "linea_list_fleet_config_versions", "linea_list_fleet_configs", "linea_list_fleet_events", "linea_list_fleet_rollout_targets", "linea_list_fleet_rollouts", "linea_list_library", "linea_list_pipelines", "linea_list_projects", "linea_list_runs", "linea_list_runtime_builds", "linea_list_runtimes", "linea_list_samples", "linea_list_suites", "linea_preview_fleet_assignment", "linea_preview_ottl", "linea_prune_runs", "linea_prune_runtimes", "linea_push_runtime", "linea_rerun_suite", "linea_retarget_manifest", "linea_retry_fleet_agent_config", "linea_run", "linea_run_scenarios", "linea_save_artifact", "linea_save_collector_group", "linea_set_default_runtime", "linea_start_fleet_rollout", "linea_stop_capture_session", "linea_test_statement", "linea_unkeep_run", "linea_update_capture_metadata", "linea_update_fleet_config", "linea_validate_capture", "linea_validate_config", "linea_verify_recommendation", "linea_wait_capture_session"}) {
		t.Fatalf("tools = %v", names)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := allRuns(ctx, service, caller, platform.RunFilter{}); err != nil {
		t.Fatalf("closing MCP must not close the platform: %v", err)
	}
}

// allRuns lists every page of the runs f matches, newest first.
func allRuns(ctx context.Context, s *platform.Service, caller platform.Caller, f platform.RunFilter) ([]platform.Run, error) {
	q := platform.RunQuery{RunFilter: f, Limit: platform.MaxRunPage}
	runs := []platform.Run{}
	for {
		page, err := s.ListRuns(ctx, caller, q)
		if err != nil {
			return nil, err
		}
		runs = append(runs, page.Runs...)
		if page.NextCursor == "" {
			return runs, nil
		}
		q.Cursor = page.NextCursor
	}
}
