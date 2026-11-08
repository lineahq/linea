package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
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

// squeezed collapses the padding between aligned columns to one space and
// keeps the indentation, so a table is compared by its cells.
func squeezed(s string) string {
	return regexp.MustCompile(`(\S) {2,}`).ReplaceAllString(s, "$1 ")
}

// TestCaptureDiscoveryTextIsATableOrFields prints Collectors and capture
// points as tables, one Collector as fields, and says when capture is off.
func TestCaptureDiscoveryTextIsATableOrFields(t *testing.T) {
	var out bytes.Buffer
	if err := writeCollectors(&out, &platform.CollectorPage{}); err != nil {
		t.Fatal(err)
	}
	if want := "live capture is off; start linea-server with --fleet-url and --fleet-token-file\n"; out.String() != want {
		t.Fatalf("capture off:\n%s", out.String())
	}
	collector := platform.Collector{ID: "c1", Generation: 4, AgentID: "agent-1", Cluster: "prod", Namespace: "obs", Pod: "gw-0", CollectorGroup: "gateways",
		CollectorVersion: "0.162.0", DistributionVersion: "1.2.3", Enabled: true, PointCount: 3, RawPointCount: 1}
	out.Reset()
	if err := writeCollectors(&out, &platform.CollectorPage{Enabled: true, Items: []platform.Collector{collector}, NextCursor: "abc"}); err != nil {
		t.Fatal(err)
	}
	want := "ID CLUSTER/NAMESPACE/POD GROUP ENABLED POINTS RAW POINTS\nc1 prod/obs/gw-0 gateways true 3 1\n\nnext page: --cursor abc\n"
	if got := squeezed(out.String()); got != want {
		t.Fatalf("collectors:\n%s\nwant:\n%s", got, want)
	}
	out.Reset()
	if err := writeCollectors(&out, &platform.CollectorPage{Enabled: true}); err != nil || out.String() != "no collectors\n" {
		t.Fatalf("no collectors = %q, %v", out.String(), err)
	}
	out.Reset()
	if err := writeCollector(&out, &collector); err != nil {
		t.Fatal(err)
	}
	want = "collector c1\n  cluster: prod\n  namespace: obs\n  pod: gw-0\n  group: gateways\n  fleet agent: agent-1\n  generation: 4\n" +
		"  collector version: 0.162.0\n  distribution version: 1.2.3\n  capture enabled: true\n  points: 3 (1 raw)\n"
	if out.String() != want {
		t.Fatalf("collector:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	points := &platform.CapturePointPage{Items: []platform.CapturePoint{{Pipeline: "logs", Point: "egress", Signal: "logs", Sensitivity: "raw"}}, NextCursor: "p2"}
	if err := writeCapturePoints(&out, points); err != nil {
		t.Fatal(err)
	}
	want = "PIPELINE POINT SIGNAL SENSITIVITY\nlogs egress logs raw\n\nnext page: --cursor p2\n"
	if got := squeezed(out.String()); got != want {
		t.Fatalf("points:\n%s\nwant:\n%s", got, want)
	}
	out.Reset()
	if err := writeCapturePoints(&out, &platform.CapturePointPage{}); err != nil || out.String() != "no capture points\n" {
		t.Fatalf("no points = %q, %v", out.String(), err)
	}
}
