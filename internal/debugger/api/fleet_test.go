package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// TestFleetRoutesManageFleet walks the fleet node's routes: inventory, a
// group's lifecycle and a config's assignment and rollout.
func TestFleetRoutesManageFleet(t *testing.T) {
	fleet := fleetcapturetest.NewServer(t)
	service := platform.New(platform.Options{Capture: platform.CaptureOptions{Fleet: platform.FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})
	t.Cleanup(service.Close)
	_, server := newServerOptions(t, Options{Platform: service})
	send := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	o := decodeBody[platform.FleetOverview](t, get(t, server.URL+"/api/v1/fleet?group_by=service.name"), http.StatusOK)
	if !o.Enabled || !o.ConfigManagement || len(o.Groups) != 1 {
		t.Fatalf("overview %+v", o)
	}
	agents := decodeBody[platform.FleetAgentPage](t, get(t, server.URL+"/api/v1/fleet/agents?connected=true&selector=service.name%3Dgateway"), http.StatusOK)
	if len(agents.Items) != 1 || len(agents.Items[0].CollectorIDs) != 1 {
		t.Fatalf("agents %+v", agents)
	}
	if e := decodeBody[OperationError](t, get(t, server.URL+"/api/v1/fleet/agents?healthy=maybe"), http.StatusBadRequest); e.Code != "invalid_request" {
		t.Fatal(e)
	}
	id := agents.Items[0].ID
	if d := decodeBody[platform.FleetAgentDetail](t, get(t, server.URL+"/api/v1/fleet/agents/"+id), http.StatusOK); d.Config == nil {
		t.Fatalf("detail %+v", d)
	}
	decodeBody[platform.FleetAgentConfig](t, send("POST", "/api/v1/fleet/agents/"+id+"/config-retry", ""), http.StatusOK)

	g := decodeBody[platform.CollectorGroup](t, send("POST", "/api/v1/collector-groups", `{"name":"Gateways","selector":"service.name=gateway"}`), http.StatusOK)
	if g.Revision != 1 {
		t.Fatalf("group %+v", g)
	}
	if e := decodeBody[OperationError](t, send("PUT", "/api/v1/collector-groups/"+g.ID, `{"revision":7,"name":"Gateways","selector":"host.name"}`), http.StatusConflict); e.Code != "conflict" {
		t.Fatal(e)
	}
	g = decodeBody[platform.CollectorGroup](t, send("PUT", "/api/v1/collector-groups/"+g.ID, `{"revision":1,"name":"Gateways","selector":"host.name"}`), http.StatusOK)
	if e := decodeBody[OperationError](t, send("DELETE", "/api/v1/collector-groups/"+g.ID, ""), http.StatusBadRequest); e.Code != "invalid_request" {
		t.Fatal(e)
	}
	if d := decodeBody[FleetDeleted](t, send("DELETE", "/api/v1/collector-groups/"+g.ID+"?revision="+strconv.FormatInt(g.Revision, 10), ""), http.StatusOK); !d.Deleted {
		t.Fatal(d)
	}
	events := decodeBody[platform.FleetEventPage](t, get(t, server.URL+"/api/v1/fleet/events?limit=2"), http.StatusOK)
	if len(events.Items) == 0 || events.NextCursor == "" {
		t.Fatalf("events %+v", events)
	}

	c := decodeBody[platform.FleetConfig](t, send("POST", "/api/v1/fleet/configs", `{"name":"edge"}`), http.StatusOK)
	decodeBody[platform.FleetConfig](t, send("PATCH", "/api/v1/fleet/configs/"+c.ID, `{"revision":1,"description":"Edge"}`), http.StatusOK)
	v := decodeBody[platform.FleetConfigVersion](t, send("POST", "/api/v1/fleet/configs/"+c.ID+"/versions", `{"body":"processors:\n  batch: {}\n","message":"batch"}`), http.StatusOK)
	if got := decodeBody[platform.FleetConfigVersion](t, get(t, server.URL+"/api/v1/fleet/configs/"+c.ID+"/versions/1"), http.StatusOK); got.Body == "" || got.Version != v.Version {
		t.Fatalf("version %+v", got)
	}
	if page := decodeBody[platform.FleetConfigVersionPage](t, get(t, server.URL+"/api/v1/fleet/configs/"+c.ID+"/versions"), http.StatusOK); len(page.Items) != 1 {
		t.Fatalf("versions %+v", page)
	}
	a := decodeBody[platform.FleetAssignment](t, send("POST", "/api/v1/fleet/assignments", `{"name":"edge","config_id":"`+c.ID+`","version":1,"layer":10,"selector":"service.name=gateway"}`), http.StatusOK)
	if p := decodeBody[platform.FleetAssignmentPreviewPage](t, get(t, server.URL+"/api/v1/fleet/assignments/"+a.ID+"/preview"), http.StatusOK); len(p.Items) != 1 {
		t.Fatalf("preview %+v", p)
	}
	ro := decodeBody[platform.FleetRollout](t, get(t, server.URL+"/api/v1/fleet/rollouts/"+a.RolloutID), http.StatusOK)
	if e := decodeBody[OperationError](t, send("POST", "/api/v1/fleet/rollouts", `{"assignment_id":"`+a.ID+`","to_version":1}`), http.StatusConflict); e.Code != "conflict" {
		t.Fatal(e)
	}
	if ro = decodeBody[platform.FleetRollout](t, send("POST", "/api/v1/fleet/rollouts/"+ro.ID+"/actions", `{"action":"cancel"}`), http.StatusOK); ro.State != "cancelled" {
		t.Fatalf("rollout %+v", ro)
	}
	if page := decodeBody[platform.FleetRolloutTargetPage](t, get(t, server.URL+"/api/v1/fleet/rollouts/"+ro.ID+"/targets?outcome=pending"), http.StatusOK); len(page.Items) != 1 {
		t.Fatalf("targets %+v", page)
	}
	if e := decodeBody[OperationError](t, send("DELETE", "/api/v1/fleet/configs/"+c.ID+"?revision=2", ""), http.StatusConflict); e.Code != "conflict" {
		t.Fatal(e)
	}
	if e := decodeBody[OperationError](t, send("POST", "/api/v1/fleet/configs", `{"name":"edge","extra":1}`), http.StatusBadRequest); e.Code != "invalid_request" {
		t.Fatal(e)
	}
}
