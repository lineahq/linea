package telemetry

import (
	"testing"

	"go.opentelemetry.io/collector/pipeline"
)

func TestDecodeDetectsSignal(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    pipeline.Signal
		wantErr bool
	}{
		{name: "traces", doc: `{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"a"}]}]}]}`, want: pipeline.SignalTraces},
		{name: "metrics", doc: `{"resourceMetrics":[]}`, want: pipeline.SignalMetrics},
		{name: "logs", doc: `{"resourceLogs":[]}`, want: pipeline.SignalLogs},
		{name: "none", doc: `{}`, wantErr: true},
		{name: "mixed", doc: `{"resourceSpans":[],"resourceLogs":[]}`, wantErr: true},
		{name: "invalid", doc: `[`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Decode([]byte(tt.doc))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Signal != tt.want {
				t.Fatalf("signal = %v, want %v", p.Signal, tt.want)
			}
		})
	}
}

func TestEmptyPayloadJSONRetainsItsSignal(t *testing.T) {
	for _, key := range []string{"resourceSpans", "resourceMetrics", "resourceLogs"} {
		t.Run(key, func(t *testing.T) {
			source := []byte(`{"` + key + `":[]}`)
			p, err := Decode(source)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := p.JSON()
			if err != nil {
				t.Fatal(err)
			}
			restored, err := Decode(raw)
			if err != nil || restored.Signal != p.Signal || restored.Counts() != (Counts{}) {
				t.Fatal("empty payload lost its signal", err)
			}
		})
	}
}

func TestConcatAppendsResourcesInOrder(t *testing.T) {
	a, _ := Decode([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"a"}]}]}]}`))
	b, _ := Decode([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"b"},{"name":"c"}]}]}]}`))
	got, ok := Concat([]Payload{a, b})
	if !ok {
		t.Fatal("expected payload")
	}
	if c := got.Counts(); c != (Counts{Resources: 2, Scopes: 2, Items: 3}) {
		t.Fatalf("counts = %+v", c)
	}
	if name := got.Traces.ResourceSpans().At(1).ScopeSpans().At(0).Spans().At(0).Name(); name != "b" {
		t.Fatalf("second resource first span = %q", name)
	}
	if a.Traces.SpanCount() != 1 {
		t.Fatal("Concat must not consume its inputs")
	}
}
