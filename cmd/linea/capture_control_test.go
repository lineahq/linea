package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestCaptureControlCLIUsesRunningPlatform(t *testing.T) {
	ctx := context.Background()
	_, _, server := fleetPlatform(t)
	req := platform.CaptureSelection{CollectorIDs: []string{fleetcapturetest.Peer.ID}, CapturePoints: []platform.CapturePointSelection{{Pipeline: "logs", Point: "egress"}}, Signals: []string{"logs"}, DurationSeconds: 5}
	file := filepath.Join(t.TempDir(), "capture.json")
	raw, _ := json.Marshal(req)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	run := func(command string, args ...string) []byte {
		t.Helper()
		var out, stderr bytes.Buffer
		flags := append([]string{command, "--server", server.URL, "--format", "json"}, args...)
		if code := captureCmd(ctx, flags, &out, &stderr); code != exitOK {
			t.Fatalf("%s exit %d: %s", command, code, stderr.String())
		}
		return out.Bytes()
	}
	run("validate", "--request", file)
	var session platform.CaptureSession
	_ = json.Unmarshal(run("start", "--request", file), &session)
	if session.ID == "" {
		t.Fatal("missing session ID")
	}
	var status platform.CaptureSession
	_ = json.Unmarshal(run("status", "--session", session.ID), &status)
	if status.State != "active" {
		t.Fatal(status)
	}
	run("list")
	run("stop", "--session", session.ID)
	_ = json.Unmarshal(run("wait", "--session", session.ID), &status)
	if !status.Terminal || status.Targets[0].State != "completed" {
		t.Fatal(status)
	}
	var out, stderr bytes.Buffer
	if code := captureCmd(ctx, []string{"status", "--server", server.URL, "--session", session.ID}, &out, &stderr); code != exitOK || !bytes.Contains(out.Bytes(), []byte("completed")) {
		t.Fatalf("text status %d: %s%s", code, out.String(), stderr.String())
	}
	unknown := filepath.Join(t.TempDir(), "unknown.json")
	req.CollectorIDs = []string{"no-such-collector"}
	raw, _ = json.Marshal(req)
	if err := os.WriteFile(unknown, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"start", "validate"} {
		for _, format := range []string{"json", "text"} {
			var out, stderr bytes.Buffer
			if code := captureCmd(ctx, []string{command, "--server", server.URL, "--request", unknown, "--format", format}, &out, &stderr); code != exitError || out.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("%s %s refused by the server: exit %d, out %q, stderr %q", command, format, code, out.String(), stderr.String())
			}
		}
	}
	for _, args := range [][]string{{"start", "--server", server.URL}, {"status", "--server", server.URL}, {"policy", "--server", server.URL}, {"disable", "--server", server.URL}, {"start", "--request", file}} {
		var out, stderr bytes.Buffer
		if captureCmd(ctx, args, &out, &stderr) != exitError {
			t.Fatalf("accepted %v", args)
		}
	}
}

// TestCaptureControlTextLeadsWithTheState prints a capture's state first,
// its targets in a table, the list as a table and the preflight verdict.
func TestCaptureControlTextLeadsWithTheState(t *testing.T) {
	expires := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var out bytes.Buffer
	session := platform.CaptureSession{ID: "s1", State: "completed", Terminal: true, ExpiresAt: expires, StopReason: "operator stop",
		Targets: []platform.CaptureTargetStatus{{CollectorID: "c1", State: "completed", Stats: &platform.CaptureTargetStats{Selected: 5, EnqueuedRecords: 4, EnqueuedBytes: 300}}, {CollectorID: "c2", State: "unknown"}}}
	if err := writeCaptureSession(&out, &session); err != nil {
		t.Fatal(err)
	}
	want := "COMPLETED capture s1\n  terminal: true\n  expires: 2026-01-02T03:04:05Z\n  stop reason: operator stop\n\nTARGETS\n" +
		"  COLLECTOR STATE SELECTED ENQUEUED RECORDS ENQUEUED BYTES FILTER ERRORS\n  c1 completed 5 4 300 0\n  c2 unknown 0 0 0 0\n"
	if got := squeezed(out.String()); got != want {
		t.Fatalf("session:\n%s\nwant:\n%s", got, want)
	}
	out.Reset()
	page := &platform.CaptureSessionPage{Items: []platform.CaptureSessionSummary{{ID: "s1", State: "active", TargetCount: 2, ActiveCount: 2}}, NextCursor: "n"}
	if err := writeCaptureSessions(&out, page); err != nil {
		t.Fatal(err)
	}
	want = "ID STATE TERMINAL TARGETS ACTIVE UNCERTAIN FAILED\ns1 active false 2 2 0 0\n\nnext page: --cursor n\n"
	if got := squeezed(out.String()); got != want {
		t.Fatalf("list:\n%s\nwant:\n%s", got, want)
	}
	out.Reset()
	if err := writeCaptureSessions(&out, &platform.CaptureSessionPage{}); err != nil || out.String() != "no captures\n" {
		t.Fatalf("no captures = %q, %v", out.String(), err)
	}
	out.Reset()
	validation := &platform.CaptureValidation{Collectors: make([]platform.Collector, 2), Selection: platform.CaptureSelection{DurationSeconds: 5}}
	if err := writeCaptureValidation(&out, validation); err != nil {
		t.Fatal(err)
	}
	want = "OK  preflight accepted: 2 Collectors, 5s duration\n  Fleet admits the capture on start; budgets apply per Collector.\n"
	if out.String() != want {
		t.Fatalf("validation:\n%s\nwant:\n%s", out.String(), want)
	}
}
