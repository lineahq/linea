package mcp

import (
	"regexp"
	"testing"

	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

var fleetToolText = regexp.MustCompile(`^(Fleet: (not connected|config management (true|false); [0-9]+ agents, [0-9]+ connected)|Fleet agents: [0-9]+; capture linked (true|false); more (true|false)|Fleet agent [0-9a-f]+: connected (true|false); [0-9]+ linked Collectors; config (in sync (true|false)|state unknown)|Agent config retried: config (in sync (true|false)|state unknown)|Fleet events: [0-9]+ after the cursor|Collector group [0-9a-f]+: (static|selector), revision [0-9]+|Fleet (group|config) deleted|(Fleet configs|Config versions|Config assignments|Rollouts|Rollout targets): [0-9]+; more (true|false)|Fleet config [0-9a-f]+: revision [0-9]+; latest version [0-9]+|Config version [0-9]+: [0-9]+ bytes|Config assignment [0-9a-f]+: version [0-9]+ at layer [0-9]+; rollout ([0-9a-f]+|none)|Assignment preview: [0-9]+ agents; more (true|false)|Rollout [0-9a-f]+: state [a-z_]+; [0-9]+ of [0-9]+ waves released; [0-9]+ targets)\.$`)

// TestFleetToolsManageFleet reads the inventory, manages a group, and moves
// a config from creation to a cancelled rollout; the texts carry no names.
func TestFleetToolsManageFleet(t *testing.T) {
	fleet := fleetcapturetest.NewServer(t)
	ctx, session, _ := newTestClient(t, platform.Options{Capture: platform.CaptureOptions{Fleet: platform.FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		r, out := callTool(t, ctx, session, name, args)
		if r.IsError {
			t.Fatalf("%s = %+v", name, out)
		}
		return out
	}

	if out := call("linea_get_fleet_overview", map[string]any{"group_by": "service.name"}); out["enabled"] != true {
		t.Fatalf("overview = %+v", out)
	}
	if out := call("linea_list_fleet_agents", map[string]any{"connected": true}); len(out["items"].([]any)) != 1 {
		t.Fatalf("agents = %+v", out)
	}
	call("linea_get_fleet_agent", map[string]any{"id": fleetcapturetest.Peer.AgentID})
	call("linea_retry_fleet_agent_config", map[string]any{"id": fleetcapturetest.Peer.AgentID})
	g := call("linea_save_collector_group", map[string]any{"name": "Gateways", "selector": "service.name=gateway", "revision": 0})
	call("linea_delete_collector_group", map[string]any{"id": g["id"], "revision": g["revision"]})
	call("linea_list_fleet_events", map[string]any{})

	c := call("linea_create_fleet_config", map[string]any{"name": "edge"})
	call("linea_update_fleet_config", map[string]any{"id": c["id"], "request": map[string]any{"revision": 1, "description": "Edge"}})
	call("linea_create_fleet_config_version", map[string]any{"id": c["id"], "body": "processors:\n  batch: {}\n"})
	if v := call("linea_get_fleet_config_version", map[string]any{"id": c["id"], "version": 1}); v["body"] == "" {
		t.Fatalf("version = %+v", v)
	}
	call("linea_list_fleet_configs", map[string]any{})
	call("linea_list_fleet_config_versions", map[string]any{"id": c["id"]})
	a := call("linea_create_fleet_assignment", map[string]any{"name": "edge", "config_id": c["id"], "version": 1, "layer": 10, "selector": "service.name=gateway"})
	call("linea_get_fleet_assignment", map[string]any{"id": a["id"]})
	call("linea_list_fleet_assignments", map[string]any{})
	call("linea_preview_fleet_assignment", map[string]any{"id": a["id"]})
	call("linea_get_fleet_rollout", map[string]any{"id": a["rollout_id"]})
	if ro := call("linea_control_fleet_rollout", map[string]any{"id": a["rollout_id"], "action": "cancel"}); ro["state"] != "cancelled" {
		t.Fatalf("rollout = %+v", ro)
	}
	call("linea_list_fleet_rollouts", map[string]any{})
	call("linea_list_fleet_rollout_targets", map[string]any{"id": a["rollout_id"], "outcome": "pending"})
	call("linea_start_fleet_rollout", map[string]any{"assignment_id": a["id"], "delete": true, "policy": map[string]any{}})

	r, out := callTool(t, ctx, session, "linea_delete_fleet_config", map[string]any{"id": c["id"], "revision": 2})
	if !r.IsError || out["code"] != string(platform.CodeConflict) {
		t.Fatalf("delete of an assigned config = %+v", out)
	}
}
