// Package since0160 is the OTTL construction of Collector releases from
// 0.160.0 on to 0.160. Only the generated
// internal/debugger/compat/ottl/variant.go imports it.
//
// In 0.160 the context constructors are NewTransformContextPtr and the OTTL
// experimental and profile converter modules do not exist yet. This variant
// does not compile against later releases, so it is its own module; the
// worker source drops its go.mod and the build compiles it as part of the
// worker module.
package since0160

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
const Since = "0.160.0"

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
	return ottlresource.NewTransformContextPtr(r, url)
}

func Scope(s pcommon.InstrumentationScope, r pcommon.Resource, scopeURL, resourceURL schemaURLItem) *ottlscope.TransformContext {
	return ottlscope.NewTransformContextPtr(s, r, scopeURL, resourceURL)
}

func Span(rs ptrace.ResourceSpans, ss ptrace.ScopeSpans, span ptrace.Span) *ottlspan.TransformContext {
	return ottlspan.NewTransformContextPtr(rs, ss, span)
}

func SpanEvent(rs ptrace.ResourceSpans, ss ptrace.ScopeSpans, span ptrace.Span, ev ptrace.SpanEvent) *ottlspanevent.TransformContext {
	return ottlspanevent.NewTransformContextPtr(rs, ss, span, ev)
}

func Log(rl plog.ResourceLogs, sl plog.ScopeLogs, lr plog.LogRecord) *ottllog.TransformContext {
	return ottllog.NewTransformContextPtr(rl, sl, lr)
}

func Metric(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric) *ottlmetric.TransformContext {
	return ottlmetric.NewTransformContextPtr(rm, sm, m)
}

func DataPoint(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric, dp any) *ottldatapoint.TransformContext {
	return ottldatapoint.NewTransformContextPtr(rm, sm, m, dp)
}

func Exemplar(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric, dp any, ex pmetric.Exemplar) *ottlexemplar.TransformContext {
	return ottlexemplar.NewTransformContextPtr(rm, sm, m, dp, ex)
}
