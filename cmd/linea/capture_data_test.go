package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestCaptureDataCLIMapsRecordsDerivationExportAndDeletion(t *testing.T) {
	ctx := context.Background()
	fleet := fleetcapturetest.NewServer(t)
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{Capture: platform.CaptureOptions{Fleet: platform.FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	owner := platform.Local("cli")
	capture, err := service.CreateCaptureSession(ctx, owner, platform.CaptureSelection{CollectorIDs: []string{fleetcapturetest.Peer.ID}, CapturePoints: []platform.CapturePointSelection{{Pipeline: "logs", Point: "egress"}}, Signals: []string{"logs"}})
	if err != nil {
		t.Fatal(err)
	}
	logs := plog.NewLogs()
	r := logs.ResourceLogs().AppendEmpty()
	for key, value := range map[string]string{"capture.session.id": "wire-session", "capture.point": "egress", "capture.pipeline": "logs"} {
		r.Resource().Attributes().PutStr(key, value)
	}
	r.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("captured CLI data")
	raw, _ := plogotlp.NewExportRequestFromLogs(logs).MarshalProto()
	batch := fleet.Deliver("logs", raw, 1)
	eventually(t, func() bool {
		accepted, ok := fleet.Acked(batch)
		if ok && accepted != 1 {
			t.Fatalf("accepted %d records", accepted)
		}
		return ok
	})
	server := httptest.NewServer(api.New(api.Options{Platform: service}))
	t.Cleanup(server.Close)
	run := func(command string, args ...string) []byte {
		t.Helper()
		var out, stderr bytes.Buffer
		flags := append([]string{command, "--server", server.URL, "--session", capture.ID, "--format", "json"}, args...)
		if code := captureCmd(ctx, flags, &out, &stderr); code != exitOK {
			t.Fatalf("%s exit %d: %s", command, code, stderr.String())
		}
		return out.Bytes()
	}
	var page platform.CaptureRecordsPage
	if json.Unmarshal(run("records"), &page) != nil || len(page.Items) != 1 {
		t.Fatal("CLI records")
	}
	config := "receivers:\n  otlp:\n    protocols:\n      grpc: {}\nexporters:\n  debug: {}\nservice:\n  pipelines:\n    logs:\n      receivers: [otlp]\n      exporters: [debug]\n"
	selection := platform.CaptureInputSelection{Stream: page.Items[0].Stream, Watermark: page.Watermark, Receiver: "otlp"}
	file := filepath.Join(t.TempDir(), "request.json")
	write := func(v any) {
		t.Helper()
		raw, _ := json.Marshal(v)
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(platform.CaptureDeriveRequest{ConfigYAML: config, Selection: selection, Save: true})
	var fixture platform.CaptureFixture
	_ = json.Unmarshal(run("fixture", "--request", file), &fixture)
	if fixture.Saved == nil || !strings.Contains(string(fixture.Payload), "captured CLI data") {
		t.Fatal("CLI fixture")
	}
	write(platform.CaptureScenarioRequest{ConfigYAML: config, Inputs: []platform.CaptureInputSelection{selection}})
	var scenario platform.CaptureScenario
	_ = json.Unmarshal(run("scenario", "--request", file), &scenario)
	if scenario.YAML == "" {
		t.Fatal("CLI scenario")
	}
	write(platform.CaptureExportRequest{Fixture: &platform.CaptureDeriveRequest{ConfigYAML: config, Selection: selection}})
	var exported platform.CaptureExport
	_ = json.Unmarshal(run("export", "--request", file), &exported)
	if exported.Content != fixture.FixtureJSON || exported.Digest != fixture.Derivation.Digest {
		t.Fatal("CLI export digest parity")
	}
	if _, err := service.StopCaptureSession(ctx, owner, capture.ID); err != nil {
		t.Fatal(err)
	}
	// Deletion waits until the session has finished and drained.
	eventually(t, func() bool {
		var out, stderr bytes.Buffer
		return captureCmd(ctx, []string{"delete", "--server", server.URL, "--session", capture.ID}, &out, &stderr) == exitOK
	})
	if _, err := service.GetCaptureSession(ctx, owner, capture.ID); platform.ErrorCode(err) != platform.CodeNotFound {
		t.Fatal("capture was not deleted")
	}
}

// eventually polls a condition for up to ten seconds.
func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
