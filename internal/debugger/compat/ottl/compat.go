// Package ottlcompat is the OTTL construction the debugger's replays use,
// the same for every Collector release from 0.160 on. Each OTTL API
// that changed between releases lives in a variant package named by the
// first release it applies to (since0160, since0161, since0162).
// variant.go, generated from the build's Collector release with the
// registry, imports the newest variant at or before that release; nothing
// else imports a variant, and no build tag selects one.
package ottlcompat

import (
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlexemplar"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlmetric"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlresource"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlscope"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspanevent"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// SchemaURLItem holds a resource's or scope's schema URL. It has the method
// set of ottl.SchemaURLItem, which 0.160 keeps internal.
type SchemaURLItem interface {
	SchemaUrl() string
	SetSchemaUrl(v string)
}

// Functions adds the release's experimental and profile converters to a
// standard function or converter set, as the release's transform processor
// and filter conditions do.
func Functions[K any](m map[string]ottl.Factory[K]) map[string]ottl.Factory[K] {
	return functions(m)
}

// NewResourceContext is ottlresource's transform context constructor.
func NewResourceContext(r pcommon.Resource, url SchemaURLItem) *ottlresource.TransformContext {
	return newResource(r, url)
}

// NewScopeContext is ottlscope's transform context constructor.
func NewScopeContext(s pcommon.InstrumentationScope, r pcommon.Resource, scopeURL, resourceURL SchemaURLItem) *ottlscope.TransformContext {
	return newScope(s, r, scopeURL, resourceURL)
}

// NewSpanContext is ottlspan's transform context constructor.
func NewSpanContext(rs ptrace.ResourceSpans, ss ptrace.ScopeSpans, span ptrace.Span) *ottlspan.TransformContext {
	return newSpan(rs, ss, span)
}

// NewSpanEventContext is ottlspanevent's transform context constructor.
func NewSpanEventContext(rs ptrace.ResourceSpans, ss ptrace.ScopeSpans, span ptrace.Span, ev ptrace.SpanEvent) *ottlspanevent.TransformContext {
	return newSpanEvent(rs, ss, span, ev)
}

// NewLogContext is ottllog's transform context constructor.
func NewLogContext(rl plog.ResourceLogs, sl plog.ScopeLogs, lr plog.LogRecord) *ottllog.TransformContext {
	return newLog(rl, sl, lr)
}

// NewMetricContext is ottlmetric's transform context constructor.
func NewMetricContext(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric) *ottlmetric.TransformContext {
	return newMetric(rm, sm, m)
}

// NewDataPointContext is ottldatapoint's transform context constructor.
func NewDataPointContext(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric, dp any) *ottldatapoint.TransformContext {
	return newDataPoint(rm, sm, m, dp)
}

// NewExemplarContext is ottlexemplar's transform context constructor.
func NewExemplarContext(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric, dp any, ex pmetric.Exemplar) *ottlexemplar.TransformContext {
	return newExemplar(rm, sm, m, dp, ex)
}
