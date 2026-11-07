package filterreplay

import (
	"slices"
	"testing"

	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/features"
)

// TestReadsListsTheFieldsConditionsRead: paths are listed from the parser,
// with their level and first literal key, in both configuration forms;
// enums and cache paths are not reads.
func TestReadsListsTheFieldsConditionsRead(t *testing.T) {
	rec, res := features.LevelRecord, features.LevelResource
	cases := []struct {
		name, config string
		signal       pipeline.Signal
		want         []features.FieldRead
	}{
		{"legacy log", `logs:
  log_record:
    - attributes["route"] != "pager"
    - IsMatch(body, "^health") and resource.attributes["env"] == "demo"
`, pipeline.SignalLogs, []features.FieldRead{read(rec, "attributes", "route", ""), read(rec, "body", "", ""), read(res, "attributes", "env", "")}},
		{"legacy resource", `logs:
  resource:
    - attributes["service.name"] == "noise"
`, pipeline.SignalLogs, []features.FieldRead{read(res, "attributes", "service.name", "")}},
		{"context prefixes and enums", `log_conditions:
  - conditions:
      - log.severity_number < SEVERITY_NUMBER_WARN and log.cache["x"] == nil
`, pipeline.SignalLogs, []features.FieldRead{read(rec, "severity_number", "", "")}},
		{"span event", `traces:
  spanevent:
    - name == "retry" and span.name == "GET"
`, pipeline.SignalTraces, []features.FieldRead{read(rec, "events", "", "name"), read(rec, "name", "", "")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := Reads(filterConfig(t, c.config), c.signal)
			if reason != "" {
				t.Fatal(reason)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("reads = %+v, want %+v", got, c.want)
			}
		})
	}
}

func read(level features.Level, field, key, event string) features.FieldRead {
	return features.FieldRead{Level: level, Field: field, Key: key, Event: event}
}
