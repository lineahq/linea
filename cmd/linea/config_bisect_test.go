package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// bisectFleet serves a fleet config whose third of four versions stops
// dropping the health check.
func bisectFleet(t *testing.T) (*fleetcapturetest.Server, string) {
	t.Helper()
	sample := filepath.Join("..", "..", "samples", "logs-cleanup")
	raw, err := os.ReadFile(filepath.Join(sample, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	good := string(raw)
	bad := strings.Replace(good, "filter/health, ", "", 1)
	fleet := fleetcapturetest.NewServer(t)
	fleet.ManageConfig(good, good, bad, bad)
	return fleet, filepath.Join(sample, "scenario.yaml")
}

func TestConfigBisectMapsTheServer(t *testing.T) {
	fleet, scenario := bisectFleet(t)
	good, err := os.ReadFile(filepath.Join("..", "..", "samples", "logs-cleanup", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fleet.ManageConfig(string(good), string(good))
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true},
		Capture: platform.CaptureOptions{Fleet: platform.FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})
	t.Cleanup(service.Close)
	server := httptest.NewServer(api.New(api.Options{Platform: service}))
	t.Cleanup(server.Close)
	var out, stderr bytes.Buffer
	code := configBisectCmd(context.Background(), []string{"--agent", fleetcapturetest.Peer.AgentID, "--scenario", scenario,
		"--server", server.URL, "--format", "markdown"}, &out, &stderr)
	if code != exitMismatch || !strings.HasPrefix(out.String(), "# Config bisect: no_regression") {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), stderr.String())
	}
	for _, args := range [][]string{
		{"--agent", "a", "--scenario", scenario},
		{"--agent", "a", "--scenario", scenario, "--fleet-url", fleet.URL, "--fleet-token-file", "token"},
	} {
		if code := configBisectCmd(context.Background(), args, &out, &stderr); code != exitError {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
