package runtime

import (
	"encoding/json"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"
)

func TestReferenceComparisonIgnoresOnlyCompletelyEmptyBatches(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		equal      bool
	}{
		{"empty traces", `{"resourceSpans":[]}`, true},
		{"empty metrics", `{"resourceMetrics":[]}`, true},
		{"empty logs", `{"resourceLogs":[]}`, true},
		{"unknown signal", `{}`, false},
		{"resource metadata", `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"a"}}]}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &runresult.Result{Mode: runresult.ModeInstrumented, Status: runresult.StatusCompleted, Outputs: []runresult.Output{{Exporter: "otlp", Signal: "traces", Data: json.RawMessage(tc.data)}}}
			b := &runresult.Result{Mode: runresult.ModeReference, Status: runresult.StatusCompleted}
			if got := runresult.Compare(a, b); got.Equal != tc.equal {
				t.Fatalf("comparison: %+v", got)
			}
			if got := runresult.Compare(b, a); got.Equal != tc.equal {
				t.Fatalf("reverse comparison: %+v", got)
			}
		})
	}
}
