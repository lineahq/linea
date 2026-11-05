package analysis

import (
	"fmt"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Per-entity diffs for callers that know which entity they changed, such as
// the OTTL replay. Entities are named by position, as telemetry.Record.Ref
// names them: "resource[i].scope[j].log_record[k]",
// "resource[i].scope[j].metric[m]" and "...metric[m].data_point[k]".

// DiffLogRecord diffs the fields of one log record under the given entity.
func DiffLogRecord(entity string, before, after plog.LogRecord) []Change {
	return diffFields(entity, telemetry.LogRecordFields(before), telemetry.LogRecordFields(after))
}

// DiffMetric diffs one metric: its own fields under entity, and its data
// points by position under entity.data_point[k]. A changed number of data
// points is reported on the metric as data_points.
func DiffMetric(entity string, before, after pmetric.Metric) []Change {
	out := diffFields(entity, telemetry.MetricFields(before), telemetry.MetricFields(after))
	bp, ap := telemetry.PointFields(before), telemetry.PointFields(after)
	for k := range min(len(bp), len(ap)) {
		out = append(out, diffFields(fmt.Sprintf("%s.data_point[%d]", entity, k), pointOnly(bp[k]), pointOnly(ap[k]))...)
	}
	if len(bp) != len(ap) {
		out = append(out, Change{Entity: entity, Path: "data_points", Op: OpModified, Before: len(bp), After: len(ap)})
	}
	return out
}

// DiffMetrics diffs the metrics of one scope by position. scope names the
// scope entity; metrics beyond the shorter list are reported as added or
// removed by name.
func DiffMetrics(scope string, before, after pmetric.MetricSlice) []Change {
	var out []Change
	for m := range max(before.Len(), after.Len()) {
		entity := fmt.Sprintf("%s.metric[%d]", scope, m)
		switch {
		case m >= after.Len():
			out = append(out, Change{Entity: entity, Path: "metric", Op: OpRemoved, Before: before.At(m).Name()})
		case m >= before.Len():
			out = append(out, Change{Entity: entity, Path: "metric", Op: OpAdded, After: after.At(m).Name()})
		default:
			out = append(out, DiffMetric(entity, before.At(m), after.At(m))...)
		}
	}
	return out
}

// pointOnly drops the metric fields a flattened data point carries, so they
// are reported once on the metric.
func pointOnly(f pcommon.Map) pcommon.Map {
	f.RemoveIf(func(k string, _ pcommon.Value) bool { return strings.HasPrefix(k, "metric.") })
	return f
}
