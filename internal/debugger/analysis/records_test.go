package analysis

import (
	"slices"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

type logSpec struct {
	time  int64
	body  string
	attrs map[string]any
}

func newLogs(specs ...logSpec) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "api")
	sl := rl.ScopeLogs().AppendEmpty()
	for _, s := range specs {
		lr := sl.LogRecords().AppendEmpty()
		lr.SetTimestamp(pcommon.Timestamp(s.time))
		lr.Body().SetStr(s.body)
		if err := lr.Attributes().FromRaw(s.attrs); err != nil {
			panic(err)
		}
	}
	return ld
}

type changeKey struct {
	entity, path string
	op           Op
	confidence   Confidence
}

func changeKeys(d Diff) []changeKey {
	var out []changeKey
	for _, c := range d.Changes {
		out = append(out, changeKey{c.Entity, c.Path, c.Op, c.Confidence})
	}
	return out
}

func TestDiffLogsMatchesWithConfidence(t *testing.T) {
	tests := []struct {
		name         string
		before       plog.Logs
		after        func() plog.Logs
		wantChanges  []changeKey
		wantMatching Matching
		wantRemoved  []RecordRef
		wantAdded    int
		unresolved   bool
	}{
		{
			name:   "attributes changed, matched by timestamp",
			before: newLogs(logSpec{1, "login", map[string]any{"user.password": "hunter2", "a": 1}}, logSpec{2, "login", map[string]any{"a": 2}}),
			after: func() plog.Logs {
				return newLogs(logSpec{1, "login", map[string]any{"a": 1}}, logSpec{2, "login", map[string]any{"a": 2}})
			},
			wantChanges: []changeKey{
				{"resource[0].scope[0].log_record[0]", `attributes["user.password"]`, OpRemoved, ConfidenceHigh},
			},
			wantMatching: Matching{Exact: 1, High: 1},
		},
		{
			name:   "no timestamps, matched by unique body",
			before: newLogs(logSpec{0, "a", map[string]any{"n": 1}}, logSpec{0, "b", map[string]any{"n": 1}}),
			after: func() plog.Logs {
				return newLogs(logSpec{0, "b", map[string]any{"n": 1.0}}, logSpec{0, "a", map[string]any{"n": 1}})
			},
			wantChanges: []changeKey{
				{"resource[0].scope[0].log_record[1]", `attributes["n"]`, OpTypeChanged, ConfidenceMedium},
			},
			wantMatching: Matching{Exact: 1, Medium: 1},
		},
		{
			name:         "filtered record is removed",
			before:       newLogs(logSpec{1, "healthcheck ok", nil}, logSpec{2, "order placed", nil}),
			after:        func() plog.Logs { return newLogs(logSpec{2, "order placed", nil}) },
			wantMatching: Matching{Exact: 1, UnmatchedBefore: 1},
			wantRemoved:  []RecordRef{{Ref: "resource[0].scope[0].log_record[0]", Name: "healthcheck ok"}},
		},
		{
			name:         "record added",
			before:       newLogs(logSpec{1, "x", nil}),
			after:        func() plog.Logs { return newLogs(logSpec{1, "x", nil}, logSpec{2, "y", nil}) },
			wantMatching: Matching{Exact: 1, UnmatchedAfter: 1},
			wantAdded:    1,
		},
		{
			name:   "indistinguishable changed records stay unresolved",
			before: newLogs(logSpec{0, "same", map[string]any{"k": "1"}}, logSpec{0, "same", map[string]any{"k": "2"}}),
			after: func() plog.Logs {
				return newLogs(logSpec{0, "same", map[string]any{"k": "3"}}, logSpec{0, "same", map[string]any{"k": "4"}})
			},
			wantMatching: Matching{UnmatchedBefore: 2, UnmatchedAfter: 2},
			unresolved:   true,
		},
		{
			name:         "identical duplicates pair up",
			before:       newLogs(logSpec{0, "dup", nil}, logSpec{0, "dup", nil}),
			after:        func() plog.Logs { return newLogs(logSpec{0, "dup", nil}) },
			wantMatching: Matching{Exact: 1, UnmatchedBefore: 1},
			wantRemoved:  []RecordRef{{Ref: "resource[0].scope[0].log_record[1]", Name: "dup"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := DiffPayloads(telemetry.FromLogs(tt.before), telemetry.FromLogs(tt.after()))
			if err != nil {
				t.Fatal(err)
			}
			if d.Identity != IdentityContent || d.Matching == nil || *d.Matching != tt.wantMatching {
				t.Fatalf("identity %s, matching %+v, want %+v", d.Identity, d.Matching, tt.wantMatching)
			}
			if got := changeKeys(d); !slices.Equal(got, tt.wantChanges) {
				t.Errorf("changes = %+v, want %+v", got, tt.wantChanges)
			}
			if !slices.Equal(d.Removed, tt.wantRemoved) || len(d.Added) != tt.wantAdded {
				t.Errorf("removed = %+v, added = %+v", d.Removed, d.Added)
			}
			if (len(d.Unresolved) > 0) != tt.unresolved {
				t.Errorf("unresolved = %v", d.Unresolved)
			}
		})
	}
}

func TestDiffLogsAttributesResourceChanges(t *testing.T) {
	before := newLogs(logSpec{1, "x", nil})
	after := newLogs(logSpec{1, "x", nil})
	after.ResourceLogs().At(0).Resource().Attributes().PutStr("deployment.environment", "test")
	d, err := DiffPayloads(telemetry.FromLogs(before), telemetry.FromLogs(after))
	if err != nil {
		t.Fatal(err)
	}
	want := []changeKey{{"resource[0]", `attributes["deployment.environment"]`, OpAdded, ""}}
	if got := changeKeys(d); !slices.Equal(got, want) || d.Matching.Exact != 1 {
		t.Fatalf("changes = %+v, matching %+v", got, d.Matching)
	}
}

type pointSpec struct {
	name  string
	time  int64
	attrs map[string]any
	value any // int64 or float64
}

func newGauge(specs ...pointSpec) pmetric.Metrics {
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	for _, s := range specs {
		m := sm.Metrics().AppendEmpty()
		m.SetName(s.name)
		p := m.SetEmptyGauge().DataPoints().AppendEmpty()
		p.SetTimestamp(pcommon.Timestamp(s.time))
		if err := p.Attributes().FromRaw(s.attrs); err != nil {
			panic(err)
		}
		switch v := s.value.(type) {
		case int64:
			p.SetIntValue(v)
		case float64:
			p.SetDoubleValue(v)
		}
	}
	return md
}

func TestDiffMetricsMatchesDataPoints(t *testing.T) {
	tests := []struct {
		name         string
		before       pmetric.Metrics
		after        func() pmetric.Metrics
		wantChanges  []changeKey
		wantMatching Matching
	}{
		{
			name:   "value type changed",
			before: newGauge(pointSpec{"cpu", 1, map[string]any{"host": "a"}, int64(5)}),
			after:  func() pmetric.Metrics { return newGauge(pointSpec{"cpu", 1, map[string]any{"host": "a"}, 5.0}) },
			wantChanges: []changeKey{
				{"resource[0].scope[0].metric[0].data_point[0]", "value", OpTypeChanged, ConfidenceHigh},
			},
			wantMatching: Matching{High: 1},
		},
		{
			name:   "attribute renamed",
			before: newGauge(pointSpec{"cpu", 1, map[string]any{"host": "a"}, int64(5)}, pointSpec{"cpu", 2, map[string]any{"host": "b"}, int64(6)}),
			after: func() pmetric.Metrics {
				return newGauge(pointSpec{"cpu", 1, map[string]any{"host.name": "a"}, int64(5)}, pointSpec{"cpu", 2, map[string]any{"host": "b"}, int64(6)})
			},
			wantChanges: []changeKey{
				{"resource[0].scope[0].metric[0].data_point[0]", `attributes["host"]`, OpRemoved, ConfidenceMedium},
				{"resource[0].scope[0].metric[0].data_point[0]", `attributes["host.name"]`, OpAdded, ConfidenceMedium},
			},
			wantMatching: Matching{Exact: 1, Medium: 1},
		},
		{
			name:   "metric renamed and labelled",
			before: newGauge(pointSpec{"duration_ms", 1, map[string]any{"route": "/a"}, 250.0}, pointSpec{"duration_ms", 1, map[string]any{"route": "/b"}, 40.0}),
			after: func() pmetric.Metrics {
				return newGauge(pointSpec{"duration", 1, map[string]any{"route": "/a", "unit": "ms"}, 250.0}, pointSpec{"duration", 1, map[string]any{"route": "/b", "unit": "ms"}, 40.0})
			},
			wantChanges: []changeKey{
				{"resource[0].scope[0].metric[0].data_point[0]", `attributes["unit"]`, OpAdded, ConfidenceMedium},
				{"resource[0].scope[0].metric[0].data_point[0]", "metric.name", OpModified, ConfidenceMedium},
				{"resource[0].scope[0].metric[1].data_point[0]", `attributes["unit"]`, OpAdded, ConfidenceMedium},
				{"resource[0].scope[0].metric[1].data_point[0]", "metric.name", OpModified, ConfidenceMedium},
			},
			wantMatching: Matching{Medium: 2},
		},
		{
			name:   "delta accumulated into a cumulative total",
			before: newGauge(pointSpec{"orders", 2, nil, int64(4)}),
			after: func() pmetric.Metrics {
				md := newGauge(pointSpec{"orders", 2, nil, int64(7)})
				md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints().At(0).SetStartTimestamp(1)
				return md
			},
			wantChanges: []changeKey{
				{"resource[0].scope[0].metric[0].data_point[0]", "start_time_unix_nano", OpModified, ConfidenceHigh},
				{"resource[0].scope[0].metric[0].data_point[0]", "value", OpModified, ConfidenceHigh},
			},
			wantMatching: Matching{High: 1},
		},
		{
			name:   "sum converted to gauge",
			before: newGauge(pointSpec{"requests", 1, nil, int64(3)}),
			after: func() pmetric.Metrics {
				md := pmetric.NewMetrics()
				m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
				m.SetName("requests")
				p := m.SetEmptySum().DataPoints().AppendEmpty()
				p.SetTimestamp(1)
				p.SetIntValue(3)
				return md
			},
			wantChanges: []changeKey{
				{"resource[0].scope[0].metric[0].data_point[0]", "metric.aggregation_temporality", OpAdded, ConfidenceHigh},
				{"resource[0].scope[0].metric[0].data_point[0]", "metric.is_monotonic", OpAdded, ConfidenceHigh},
				{"resource[0].scope[0].metric[0].data_point[0]", "metric.type", OpModified, ConfidenceHigh},
			},
			wantMatching: Matching{High: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := DiffPayloads(telemetry.FromMetrics(tt.before), telemetry.FromMetrics(tt.after()))
			if err != nil {
				t.Fatal(err)
			}
			if d.Matching == nil || *d.Matching != tt.wantMatching {
				t.Fatalf("matching %+v, want %+v", d.Matching, tt.wantMatching)
			}
			if got := changeKeys(d); !slices.Equal(got, tt.wantChanges) {
				t.Errorf("changes = %+v, want %+v", got, tt.wantChanges)
			}
		})
	}
}
