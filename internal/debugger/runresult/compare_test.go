package runresult

import (
	"testing"

	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

func logsOutput(t *testing.T, bodies ...string) Output {
	t.Helper()
	ld := plog.NewLogs()
	for _, b := range bodies {
		rl := ld.ResourceLogs().AppendEmpty()
		rl.Resource().Attributes().PutStr("pipeline", b)
		rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr(b)
	}
	p := telemetry.FromLogs(ld)
	data, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return Output{Exporter: "otlp", Signal: "logs", Counts: p.Counts(), Data: data}
}

// TestCompareIgnoresDeliveryOrder: pipelines sharing an exporter flush in
// nondeterministic order, so reordered records are equal (with a note), while
// changed records still differ.
func TestCompareIgnoresDeliveryOrder(t *testing.T) {
	result := func(o Output) *Result {
		return &Result{Mode: ModeInstrumented, Status: StatusCompleted, Outputs: []Output{o}}
	}
	base := result(logsOutput(t, "a", "b"))
	tests := []struct {
		name      string
		other     Output
		equal     bool
		wantNotes int
	}{
		{"identical", logsOutput(t, "a", "b"), true, 0},
		{"reordered", logsOutput(t, "b", "a"), true, 1},
		{"changed", logsOutput(t, "a", "c"), false, 0},
		{"missing", logsOutput(t, "a"), false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			other := result(tt.other)
			other.Mode = ModeReference
			c := Compare(base, other)
			if c.Equal != tt.equal || len(c.Notes) != tt.wantNotes {
				t.Fatalf("comparison = %+v", c)
			}
		})
	}
}
