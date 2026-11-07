package ottlreplay

import (
	"slices"
	"testing"

	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/features"
)

// TestAccessListsEditorTargetsAndReads: a statement writes the path its
// editor takes as a setter, at the level its context or prefix names, and
// reads every other path it names (the value, the where clause, converter
// arguments). Cache paths name no telemetry.
func TestAccessListsEditorTargetsAndReads(t *testing.T) {
	rec, res := features.LevelRecord, features.LevelResource
	cases := []struct {
		name   string
		block  string
		signal pipeline.Signal
		want   []features.FieldRead
		reads  []features.FieldRead
	}{
		{"log context", `    log_statements:
      - context: log
        statements:
          - set(attributes["tier"], resource.attributes["env"]) where body == "x"
          - set(severity_text, ConvertCase(severity_text, "upper"))
`, pipeline.SignalLogs, []features.FieldRead{{Level: rec, Field: "attributes", Key: "tier"}, {Level: rec, Field: "severity_text"}},
			[]features.FieldRead{{Level: res, Field: "attributes", Key: "env"}, {Level: rec, Field: "body"}, {Level: rec, Field: "severity_text"}}},
		{"inferred context", `    log_statements:
      - statements:
          - set(resource.attributes["env"], "prod")
          - delete_key(log.attributes, "secret")
          - set(log.cache["k"], log.body)
`, pipeline.SignalLogs, []features.FieldRead{{Level: res, Field: "attributes", Key: "env"}, {Level: rec, Field: "attributes"}},
			[]features.FieldRead{{Level: rec, Field: "body"}}},
		{"resource context", `    trace_statements:
      - context: resource
        statements:
          - merge_maps(attributes, attributes, "insert")
`, pipeline.SignalTraces, []features.FieldRead{{Level: res, Field: "attributes"}}, []features.FieldRead{{Level: res, Field: "attributes"}}},
		{"span event", `    trace_statements:
      - context: spanevent
        statements:
          - set(attributes["e"], span.name)
          - set(span.attributes["s"], name)
`, pipeline.SignalTraces, []features.FieldRead{{Level: rec, Field: "events", Event: "attributes", Key: "e"}, {Level: rec, Field: "attributes", Key: "s"}},
			[]features.FieldRead{{Level: rec, Field: "name"}, {Level: rec, Field: "events", Event: "name"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reads, got, reason := Access(transformConfig(t, c.block), c.signal)
			if reason != "" || !slices.Equal(got, c.want) {
				t.Fatalf("writes = %+v (reason %q), want %+v", got, reason, c.want)
			}
			if !slices.Equal(reads, c.reads) {
				t.Fatalf("reads = %+v, want %+v", reads, c.reads)
			}
		})
	}

	// Statements of the other signal are not listed.
	_, got, reason := Access(transformConfig(t, "    log_statements:\n      - context: log\n        statements:\n          - set(body, \"x\")\n"), pipeline.SignalTraces)
	if reason != "" || len(got) != 0 {
		t.Fatalf("traces writes = %+v (reason %q)", got, reason)
	}
}
