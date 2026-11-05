// Package since0161 is the OTTL construction of Collector releases from
// 0.161.0 on to 0.161. Only the generated
// internal/debugger/compat/ottl/variant.go imports it.
//
// 0.161 renamed the context constructors NewTransformContext; the OTTL
// experimental and profile converter modules start with 0.162.
package since0161

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

// Since is the first Collector release this variant serves.
const Since = "0.161.0"

// schemaURLItem is ottl.SchemaURLItem, which 0.160 keeps internal.
type schemaURLItem = interface {
	SchemaUrl() string
	SetSchemaUrl(v string)
}

// Functions adds the release's experimental and profile converters to a
// standard function set, as the transform processor and the filter
// conditions of this release do.
func Functions[K any](m map[string]ottl.Factory[K]) map[string]ottl.Factory[K] {
	return m
}

func Resource(r pcommon.Resource, url schemaURLItem) *ottlresource.TransformContext {
	return ottlresource.NewTransformContext(r, url)
}

func Scope(s pcommon.InstrumentationScope, r pcommon.Resource, scopeURL, resourceURL schemaURLItem) *ottlscope.TransformContext {
	return ottlscope.NewTransformContext(s, r, scopeURL, resourceURL)
}

func Span(rs ptrace.ResourceSpans, ss ptrace.ScopeSpans, span ptrace.Span) *ottlspan.TransformContext {
	return ottlspan.NewTransformContext(rs, ss, span)
}

func SpanEvent(rs ptrace.ResourceSpans, ss ptrace.ScopeSpans, span ptrace.Span, ev ptrace.SpanEvent) *ottlspanevent.TransformContext {
	return ottlspanevent.NewTransformContext(rs, ss, span, ev)
}

func Log(rl plog.ResourceLogs, sl plog.ScopeLogs, lr plog.LogRecord) *ottllog.TransformContext {
	return ottllog.NewTransformContext(rl, sl, lr)
}

func Metric(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric) *ottlmetric.TransformContext {
	return ottlmetric.NewTransformContext(rm, sm, m)
}

func DataPoint(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric, dp any) *ottldatapoint.TransformContext {
	return ottldatapoint.NewTransformContext(rm, sm, m, dp)
}

func Exemplar(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric, dp any, ex pmetric.Exemplar) *ottlexemplar.TransformContext {
	return ottlexemplar.NewTransformContext(rm, sm, m, dp, ex)
}
