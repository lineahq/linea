package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// fleetPlatform composes a platform connected to a fake fleet, served over
// HTTP for the CLI.
func fleetPlatform(t *testing.T) (*platform.Service, *fleetcapturetest.Server, *httptest.Server) {
	t.Helper()
	fleet := fleetcapturetest.NewServer(t)
	service := platform.New(platform.Options{Capture: platform.CaptureOptions{Fleet: platform.FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})
	t.Cleanup(service.Close)
	server := httptest.NewServer(api.New(api.Options{Platform: service}))
	t.Cleanup(server.Close)
	return service, fleet, server
}

func TestCaptureDiscoveryCLIMapsSharedPlatform(t *testing.T) {
	_, _, server := fleetPlatform(t)
	for _, command := range []string{"collectors", "collector", "points"} {
		t.Run(command, func(t *testing.T) {
			args := []string{command, "--server", server.URL, "--format", "json"}
			if command != "collectors" {
				args = append(args, "--collector", fleetcapturetest.Peer.ID)
			}
			var out, stderr bytes.Buffer
			if code := captureCmd(context.Background(), args, &out, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			var parsed any
			if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
				t.Fatal(err)
			}
			if command == "points" && !strings.Contains(out.String(), "egress") {
				t.Fatal(out.String())
			}
			if command == "collectors" && !strings.Contains(out.String(), `"pod"`) {
				t.Fatal(out.String())
			}
			if strings.Contains(out.String(), fleetcapturetest.Token) {
				t.Fatal("discovery exposed the fleet token")
			}
		})
	}
	var out, stderr bytes.Buffer
	if captureCmd(context.Background(), []string{"collectors"}, &out, &stderr) != exitError || !strings.Contains(stderr.String(), "--server") {
		t.Fatal("discovery started a disposable local service")
	}
}
