package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// TestBisectRouteMapsThePlatformOperation bisects a config whose third of
// four versions stops dropping the health check.
func TestBisectRouteMapsThePlatformOperation(t *testing.T) {
	sample := filepath.Join("..", "..", "..", "samples", "logs-cleanup")
	raw, err := os.ReadFile(filepath.Join(sample, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	good := string(raw)
	bad := strings.Replace(good, "filter/health, ", "", 1)
	fleet := fleetcapturetest.NewServer(t)
	fleet.ManageConfig(good, good, bad, bad)
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true},
		Capture: platform.CaptureOptions{Fleet: platform.FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})
	t.Cleanup(service.Close)
	_, server := newServerOptions(t, Options{Platform: service})
	api := server.URL + "/api/v1/config-bisects"

	req := ConfigBisectRequest{Agent: fleetcapturetest.Peer.AgentID, Scenarios: []SuiteScenario{{ScenarioYAML: scenarioText(t, filepath.Join(sample, "scenario.yaml"))}}}
	b := decodeBody[platform.Bisection](t, post(t, api, req), http.StatusOK)
	if b.Verdict != platform.BisectFound || b.FirstBad != 3 || b.LastGood != 2 || b.Target.ConfigName != "Pipeline" || b.Culprit == nil {
		t.Fatalf("bisection = %+v", b)
	}
	resp := post(t, api+"?format=markdown", req)
	md, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(md), "# Config bisect: found") ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("markdown %d %q: %s", resp.StatusCode, resp.Header.Get("Content-Type"), md)
	}
	decodeBody[OperationError](t, post(t, api+"?format=yaml", req), http.StatusBadRequest)
	decodeBody[OperationError](t, post(t, api, ConfigBisectRequest{Agent: fleetcapturetest.Peer.AgentID}), http.StatusBadRequest)
	req.Agent = strings.Repeat("e", 32)
	decodeBody[OperationError](t, post(t, api, req), http.StatusNotFound)

	_, unconnected := newServerOptions(t, Options{})
	decodeBody[OperationError](t, post(t, unconnected.URL+"/api/v1/config-bisects", req), http.StatusServiceUnavailable)
}
