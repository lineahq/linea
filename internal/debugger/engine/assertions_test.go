package engine

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/assertion"
)

const quickAssertions = "assertions: [{name: span count, target: {exporter: otlp, signal: traces}, count: {equals: 3}}]\n"

func TestExecuteQuickAssertions(t *testing.T) {
	for _, tt := range []struct {
		name, config, yaml, message string
		outcome                     assertion.Outcome
	}{
		{"pass", captureConfig, quickAssertions, "", assertion.Pass},
		{"fail", captureConfig, strings.Replace(quickAssertions, "equals: 3", "equals: 4", 1), "3 span(s) reached exporter otlp (traces), expected == 4", assertion.Fail},
		{"inconclusive", captureConfig, "assertions: [{target: {exporter: otlp, signal: logs}, attributes: {absent: [secret]}}]\n", "no records reached the target", assertion.Inconclusive},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cfg := tt.config
			if tt.outcome == assertion.Inconclusive {
				cfg = strings.Replace(cfg, "pipelines: {traces:", "pipelines: {logs: {receivers: [otlp], exporters: [otlp]}, traces:", 1)
			}
			rep, err := Execute(ctx, Request{ConfigYAML: cfg, Fixture: tracesFixture, AssertionsYAML: tt.yaml})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Assertions == nil || rep.Assertions.Outcome != tt.outcome || len(rep.Assertions.Results) != 1 || len(rep.Assertions.Results[0].Checks) != 1 {
				t.Fatalf("assertions = %+v, want %s", rep.Assertions, tt.outcome)
			}
			if got := rep.Assertions.Results[0].Checks[0].Message; !strings.Contains(got, tt.message) {
				t.Fatalf("message = %q, want mention of %q", got, tt.message)
			}
			want := sourceFromBytes("", []byte(tt.yaml))
			if rep.Provenance.Assertions == nil || *rep.Provenance.Assertions != want {
				t.Fatalf("assertion provenance = %+v, want %+v", rep.Provenance.Assertions, want)
			}
		})
	}
}

func TestQuickAssertionsRejectInvalidRequests(t *testing.T) {
	for _, req := range []Request{
		{ConfigYAML: captureConfig, Scenario: "scenario.yaml", AssertionsYAML: quickAssertions},
		{ConfigYAML: captureConfig, ScenarioYAML: "version: 1", AssertionsYAML: quickAssertions},
	} {
		err := req.Validate()
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "assertions_yaml applies to fixtures only; a scenario declares its own") {
			t.Fatalf("scenario assertions source = %v", err)
		}
	}
	for _, raw := range []string{"assertions: [", "assertions: []", "inputs: []", "assertions: [{count: {equals: 1}}]"} {
		_, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, Fixture: tracesFixture, AssertionsYAML: raw})
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "assertions_yaml:") {
			t.Fatalf("%q: err = %v", raw, err)
		}
	}
}

func TestPreparedQuickAssertionsRejectSourceMutation(t *testing.T) {
	req, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, Fixture: tracesFixture, AssertionsYAML: quickAssertions})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(req.prepared.assertions, []byte(quickAssertions)) {
		t.Fatal("assertion source bytes changed")
	}
	req.AssertionsYAML = strings.Replace(quickAssertions, "equals: 3", "equals: 4", 1)
	if _, err := Prepare(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("mutated assertion source = %v", err)
	}
	if _, _, err := EncodeBundle(req); !errors.Is(err, ErrBundle) {
		t.Fatalf("mutated assertion bundle = %v", err)
	}
}

// A quick run without assertions has neither an assertion report (so no
// outcome) nor an assertions provenance entry, and still matches the reference.
func TestQuickWithoutAssertionsHasNoOutcome(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	multisignalDir := filepath.Join("..", "..", "..", "testdata", "multisignal")
	req := Request{Config: filepath.Join(multisignalDir, "fanin.yaml"), Fixture: filepath.Join(multisignalDir, "traces.json"), Compare: true, Normalize: true}
	rep, err := Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Assertions != nil || rep.Provenance.Assertions != nil || !rep.Comparison.Equal {
		t.Fatalf("unexpected quick outcome/provenance/comparison: %+v", rep)
	}
	var buf bytes.Buffer
	if err := Encode(&buf, rep); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte(`"assertions"`)) {
		t.Fatal("a quick run without assertions encodes an assertions field")
	}
}
