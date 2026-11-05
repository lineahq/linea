package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/instrument"
)

const debugOnlyYAML = `receivers: {otlp: {protocols: {grpc: {}}}}
processors: {batch: {}}
exporters:
  debug: {verbosity: basic}
  nop: {}
service:
  pipelines:
    logs: {receivers: [otlp], processors: [batch], exporters: [debug, nop]}
`

// TestLocalExportersAreRecorded: debug and nop run unmodified, and what
// reaches them is an output with an exporter.captured event, in the
// reference run too, so the two compare equal.
func TestLocalExportersAreRecorded(t *testing.T) {
	opts := Options{ConfigYAML: debugOnlyYAML, Inputs: []Input{logInput("kept")}}
	r := runCompleted(t, opts)
	got := map[string]runresult.Output{}
	for _, o := range r.Outputs {
		got[o.Exporter] = o
	}
	for _, name := range []string{"debug", "nop"} {
		o, ok := got[name]
		if !ok || o.Counts.Items != 1 || !strings.Contains(string(o.Data), "kept") {
			t.Errorf("output of %s = %+v (all %v)", name, o, r.Outputs)
		}
	}
	var captured int
	for _, e := range r.Events {
		if e.Kind == instrument.KindExport {
			captured++
			if e.Mode != instrument.ModeReal {
				t.Errorf("%s event mode = %s, want real: the exporter ran", e.Component, e.Mode)
			}
		}
	}
	if captured != 2 {
		t.Errorf("%d exporter.captured events, want 2", captured)
	}
	ref, err := RunReference(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if c := runresult.Compare(r, ref); !c.Equal {
		t.Fatalf("comparison with the reference: %+v", c)
	}
}
