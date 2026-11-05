package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/assertion"
)

// The capture processor and livecapture extension are not in the built-in
// registry: a configuration using them runs unmodified with stand-ins, and
// its assertions cannot pass through them.
func TestLivecaptureComponentsRunAsStandIns(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "livecapture", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{})
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := Source{Text: string(raw), Label: "livecapture.yaml"}
	inspection, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: config})
	if err != nil || !inspection.Valid || len(inspection.Diagnostics) != 3 {
		t.Fatalf("inspection %+v, %v", inspection, err)
	}
	for _, d := range inspection.Diagnostics {
		if d.Code != "unsupported_component" {
			t.Fatalf("diagnostic %+v, want unsupported_component", d)
		}
	}
	const input = `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"unchanged"}}]}]}]}`
	const assertions = "assertions: [{target: {exporter: otlp, signal: logs}, count: {min: 1, max: 1}}]\n"
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			req := RunRequest{Mode: mode, Config: config, Compare: true}
			if mode == ModeQuick {
				req.Quick = &QuickInput{Fixture: Source{Text: input}, Receiver: "otlp", Assertions: assertions}
			} else {
				req.Scenario = &ScenarioInput{Scenario: Source{Text: "version: 1\ninputs:\n  - receiver: otlp\n    data: " + input + "\n" + assertions}}
			}
			run, err := s.SubmitRun(ctx, testCaller, req)
			if err != nil {
				t.Fatal(err)
			}
			report, err := s.GetRunReport(ctx, testCaller, run.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			if report.Comparison == nil || !report.Comparison.Equal {
				t.Fatalf("reference comparison: %+v", report.Comparison)
			}
			if report.Assertions == nil || report.Assertions.Outcome != assertion.Inconclusive {
				t.Fatalf("assertions on data through a stand-in must be inconclusive: %+v", report.Assertions)
			}
			inputs, err := s.GetRunInputs(ctx, testCaller, run.ID)
			if err != nil || inputs.Config.Text != config.Text || inputs.Config.Label != config.Label {
				t.Fatalf("original config was not preserved: %+v, %v", inputs, err)
			}
			if !strings.Contains(inputs.Config.Text, "enabled: true") {
				t.Fatal("stand-ins rewrote admitted YAML")
			}
		})
	}
}
