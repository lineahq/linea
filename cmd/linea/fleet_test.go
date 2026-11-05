package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// TestFleetCLIManagesFleet drives a config from creation to a cancelled
// rollout through the fleet command, and refuses flags a command does not
// take.
func TestFleetCLIManagesFleet(t *testing.T) {
	ctx := context.Background()
	_, _, server := fleetPlatform(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fleet := func(out any, args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := fleetCmd(ctx, append(args, "--server", server.URL), &stdout, &stderr); code != exitOK {
			t.Fatalf("fleet %v: exit %d: %s", args, code, stderr.String())
		}
		if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
			t.Fatalf("fleet %v: %v: %s", args, err, stdout.String())
		}
	}

	var o platform.FleetOverview
	fleet(&o, "overview", "--group-by", "service.name")
	if !o.Enabled || len(o.Groups) != 1 {
		t.Fatalf("overview %+v", o)
	}
	var agents platform.FleetAgentPage
	fleet(&agents, "agents", "--connected", "true")
	if len(agents.Items) != 1 || agents.Items[0].ID != fleetcapturetest.Peer.AgentID {
		t.Fatalf("agents %+v", agents)
	}
	var g platform.CollectorGroup
	fleet(&g, "group-save", "--request", write("group.json", `{"name":"Gateways","selector":"service.name=gateway"}`))
	var deleted map[string]bool
	fleet(&deleted, "group-delete", "--id", g.ID, "--revision", strconv.FormatInt(g.Revision, 10))
	if !deleted["deleted"] {
		t.Fatal(deleted)
	}

	var c platform.FleetConfig
	fleet(&c, "config-create", "--request", write("config.json", `{"name":"edge"}`))
	var v platform.FleetConfigVersion
	fleet(&v, "version-create", "--id", c.ID, "--body", write("v1.yaml", "processors:\n  batch: {}\n"), "--message", "batch")
	fleet(&v, "version", "--id", c.ID, "--version", "1")
	if !strings.Contains(v.Body, "batch") {
		t.Fatalf("version %+v", v)
	}
	var a platform.FleetAssignment
	fleet(&a, "assignment-create", "--request", write("assign.json", `{"name":"edge","config_id":"`+c.ID+`","version":1,"layer":10,"selector":"service.name=gateway"}`))
	var ro platform.FleetRollout
	fleet(&ro, "rollout-action", "--id", a.RolloutID, "--action", "cancel")
	if ro.State != "cancelled" {
		t.Fatalf("rollout %+v", ro)
	}
	var targets platform.FleetRolloutTargetPage
	fleet(&targets, "targets", "--id", ro.ID, "--outcome", "pending")
	if len(targets.Items) != 1 {
		t.Fatalf("targets %+v", targets)
	}

	for _, args := range [][]string{
		{"agent"},
		{"configs", "--id", c.ID},
		{"agents", "--healthy", "maybe"},
		{"config-create", "--request", write("bad.json", `{"name":"edge","extra":1}`)},
		{"explode"},
	} {
		var stdout, stderr bytes.Buffer
		if fleetCmd(ctx, append(args, "--server", server.URL), &stdout, &stderr) != exitError {
			t.Errorf("fleet %v was accepted", args)
		}
	}
}
