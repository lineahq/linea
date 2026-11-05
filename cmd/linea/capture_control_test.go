package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
