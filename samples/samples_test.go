package samples_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/samples"
)

// TestSamplesRunAsDocumented runs every sample the way the web client does:
// the inlined scenario must produce the outcome the index states, and the
// input alone must run without a scenario and reach an exporter.
func TestSamplesRunAsDocumented(t *testing.T) {
	list, err := samples.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("no samples")
	}
	seen := map[string]bool{}
	for _, s := range list {
		t.Run(s.ID, func(t *testing.T) {
			if seen[s.ID] || s.Title == "" || s.Description == "" || s.Receiver == "" {
				t.Fatalf("index entry = %+v", s)
			}
			seen[s.ID] = true
			sc, err := scenario.Parse([]byte(s.ScenarioYAML), "")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(s.ScenarioYAML, "file:") || (len(sc.Inputs) > 0 && !strings.Contains(s.ScenarioYAML, "data: |")) ||
				!strings.HasPrefix(s.ScenarioYAML, "#") {
				t.Fatalf("scenario inputs are not inlined with comments kept:\n%s", s.ScenarioYAML)
			}
			if _, err := sc.Fixtures(); err != nil {
				t.Fatal(err)
			}
			// Mock fixtures are inline too: a sample has no directory.
			if _, err := sc.MockSpec(); err != nil {
				t.Fatal(err)
			}
			if len(sc.Inputs) == 0 && len(sc.Mocks.Files) == 0 {
				t.Fatal("the scenario feeds nothing")
			}
			if _, err := telemetry.Decode([]byte(s.InputJSON)); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rep, err := engine.Execute(ctx, engine.Request{ConfigYAML: s.ConfigYAML, ScenarioYAML: s.ScenarioYAML})
			if err != nil {
				t.Fatal(err)
			}
			if got := rep.Assertions.Outcome; got != assertion.Outcome(s.Expect) {
				t.Fatalf("assertions = %s, want %s: %+v", got, s.Expect, rep.Assertions.Results)
			}
			for _, o := range rep.OTTL {
				if o.Verification != "replay_verified" {
					t.Errorf("OTTL replay of %s is %s", o.Processor, o.Verification)
				}
			}

			quick, err := engine.Execute(ctx, engine.Request{ConfigYAML: s.ConfigYAML, FixtureJSON: s.InputJSON, Receiver: s.Receiver})
			if err != nil {
				t.Fatal(err)
			}
			if len(quick.Result.Outputs) == 0 {
				t.Fatal("the input alone reached no exporter")
			}
		})
	}
}
