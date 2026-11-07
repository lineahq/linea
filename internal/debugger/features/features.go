// Package features holds the debugger's optional analysis features:
// explanations that replay a Collector component with its own code, and so
// compile into a runtime only when its manifest has that component. The
// generated registry imports each feature's package for the components the
// manifest names, and the package provides its function here in init. A
// runtime without the component has no such function; the engine then skips
// the analysis and reports name the feature in the runtime's
// missing_features.
//
// This package imports no Collector component.
package features

import (
	"context"
	"slices"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Name names an optional feature in hellos and reports.
type Name string

const (
	// OTTLReplay replays transform processor statements one at a time
	// (stage evidence and OTTL previews).
	OTTLReplay Name = "ottl_replay"
	// CountAggregation attributes count connector data points to the
	// records they counted.
	CountAggregation Name = "count_aggregation"
	// RoutingExplanation says which routing connector rule sent each record.
	RoutingExplanation Name = "routing_explanation"
	// SamplingExplanation explains tail_sampling decisions per policy.
	SamplingExplanation Name = "sampling_explanation"
	// FilterExplanation says which filter processor condition removed each
	// item.
	FilterExplanation Name = "filter_explanation"
)

// Feature is an optional feature and the component module that brings it.
type Feature struct {
	Name Name
	// Component is the component, as <kind>/<type>.
	Component string
	// Module is the component's Go module.
	Module string
	// Package is the debugger package that implements the feature, which
	// the generated registry imports when the manifest names Module.
	Package string
}

// All are the optional features, in report order.
var All = []Feature{
	{OTTLReplay, "processor/transform", "github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor", "github.com/lineahq/linea/internal/debugger/ottlreplay"},
	{CountAggregation, "connector/count", "github.com/open-telemetry/opentelemetry-collector-contrib/connector/countconnector", "github.com/lineahq/linea/internal/debugger/aggregation"},
	{RoutingExplanation, "connector/routing", "github.com/open-telemetry/opentelemetry-collector-contrib/connector/routingconnector", "github.com/lineahq/linea/internal/debugger/routing"},
	{SamplingExplanation, "processor/tail_sampling", "github.com/open-telemetry/opentelemetry-collector-contrib/processor/tailsamplingprocessor", "github.com/lineahq/linea/internal/debugger/sampling"},
	{FilterExplanation, "processor/filter", "github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor", "github.com/lineahq/linea/internal/debugger/filterreplay"},
}

// ReplayFunc replays a transform processor's statements on one stage input
// and checks the result against its correlated output (nil when none).
type ReplayFunc func(ctx context.Context, processor string, cfg component.Config, inputSeq int, input telemetry.Payload, outputSeq int, output *telemetry.Payload) (*ottldebug.Report, error)

// EvaluateFunc replays a transform processor's statements without an
// observed output; output is nil when processing stopped.
type EvaluateFunc func(ctx context.Context, processor string, cfg component.Config, input telemetry.Payload) (rep *ottldebug.Report, output *telemetry.Payload, err error)

// CountFunc explains one input of a count connector.
type CountFunc func(ctx context.Context, connector string, cfg component.Config, inputSeq int, input telemetry.Payload, outputSeq int, output *telemetry.Payload) (*analysis.Aggregation, error)

// Delivery is what a routing connector delivered into one pipeline for an
// input.
type Delivery struct {
	Pipeline string
	Payload  telemetry.Payload
}

// RoutingFunc explains one input of a routing connector. inputError is the
// error the connector returned for the input, if any.
type RoutingFunc func(ctx context.Context, name string, cfg component.Config, inputSeq int, input telemetry.Payload, inputError string, deliveries []Delivery) (*analysis.Routing, error)

// SamplingInput is a stage input of a tail_sampling processor.
type SamplingInput struct {
	Seq     int
	Payload telemetry.Payload
}

// SamplingDecision is a recorded stage.decision event.
type SamplingDecision struct {
	Seq     int
	TraceID string
	Outcome string
	Policy  string
	// Spans are the spans the policies evaluated (the event's snapshot).
	Spans telemetry.Payload
}

// SamplingFunc explains the decisions of one tail_sampling processor in one
// pipeline; completed says whether the run shut down cleanly.
type SamplingFunc func(ctx context.Context, processor, pipeline string, cfg component.Config, inputs []SamplingInput, decisions []SamplingDecision, completed bool) (*analysis.Sampling, error)

// FilterFunc explains one input of a filter processor against its
// correlated output (nil when none). stageError is the error the processor
// returned for the input, if any.
type FilterFunc func(ctx context.Context, processor string, cfg component.Config, inputSeq int, input telemetry.Payload, outputSeq int, output *telemetry.Payload, stageError string) (*analysis.Filtering, error)

// FilterVerdictFunc evaluates a filter processor's conditions on any
// payload and returns the positions of the items they would remove (refs
// as in analysis.FilterDrop); reason is set when the verdict cannot be
// determined.
type FilterVerdictFunc func(ctx context.Context, cfg component.Config, input telemetry.Payload) (drops []string, reason string)

// Level is where in a payload a field lives.
type Level string

const (
	LevelResource Level = "resource"
	LevelScope    Level = "scope"
	// LevelRecord: the record itself (log record, span or data point).
	LevelRecord Level = "record"
	// LevelClient: the request's client context (OTTL otelcol.client and
	// otelcol.grpc paths), not telemetry. Fields: address, auth (Key an
	// auth attribute), metadata and grpc_metadata (Key the header).
	LevelClient Level = "client"
)

// FieldRead is one field a condition reads: its level, its top field as
// OTTL names it ("attributes", "body", "severity_text") and the first map
// key below it, if literal. Event is the span event field a span event
// condition reads (Field is then "events").
type FieldRead struct {
	Level Level
	Field string
	Key   string
	Event string
}

// FilterReadsFunc lists the fields a filter processor's conditions for a
// signal read; reason is set when they cannot be listed.
type FilterReadsFunc func(cfg component.Config, signal pipeline.Signal) (reads []FieldRead, reason string)

// AccessFunc lists the fields a processor's configuration for a signal may
// read and write, as FieldRead values (an empty Key is any key of the
// field); reason is set when they cannot be listed, which callers must
// treat as reading and writing everything.
type AccessFunc func(cfg component.Config, signal pipeline.Signal) (reads, writes []FieldRead, reason string)

// PathSegment is one segment of an OTTL path: its name and its first map
// key when that key is a literal string.
type PathSegment struct {
	Name string
	Key  string
}

// PathField maps an OTTL path, given by its segments, in context ctx to the
// level and field it names. pathContext is the context the parser resolved
// for the path, if any. A leading context name is the context's own prefix
// (log.attributes) or another level (resource.attributes from the log
// context). Cache paths name no telemetry (ok is false).
func PathField(ctx, pathContext string, segs []PathSegment) (f FieldRead, ok bool) {
	if len(segs) == 0 {
		return FieldRead{}, false
	}
	if pathContext == "otelcol" || segs[0].Name == "otelcol" {
		if segs[0].Name == "otelcol" {
			segs = segs[1:]
		}
		return clientField(segs)
	}
	level := levelOf(ctx)
	if pathContext != "" && pathContext != ctx {
		level = levelOf(pathContext)
	}
	switch name := segs[0].Name; {
	case name == ctx && len(segs) > 1:
		segs = segs[1:]
	case (name == "resource" || name == "scope" || name == "instrumentation_scope") && len(segs) > 1:
		level = levelOf(name)
		segs = segs[1:]
	case name == "span" && ctx == "spanevent" && len(segs) > 1:
		// A span event path naming its span.
		segs = segs[1:]
		ctx = "span"
	}
	if segs[0].Name == "cache" {
		return FieldRead{}, false
	}
	f = FieldRead{Level: level, Field: segs[0].Name, Key: segs[0].Key}
	if ctx == "spanevent" && level == LevelRecord {
		// Span events are fields of their span's record.
		f.Field, f.Event = "events", segs[0].Name
	}
	return f, true
}

// clientField maps an otelcol path below its context name:
// client.addr, client.auth.attributes["k"], client.metadata["k"],
// grpc.metadata["k"] (pkg/ottl v0.162.0 contexts/internal/ctxotelcol).
func clientField(segs []PathSegment) (FieldRead, bool) {
	if len(segs) < 2 {
		return FieldRead{}, false
	}
	f := FieldRead{Level: LevelClient, Field: segs[1].Name, Key: segs[1].Key}
	switch {
	case segs[0].Name == "client" && segs[1].Name == "addr":
		f.Field = "address"
	case segs[0].Name == "client" && segs[1].Name == "auth":
		f.Key = ""
		if len(segs) > 2 {
			f.Key = segs[2].Key
		}
	case segs[0].Name == "grpc" && segs[1].Name == "metadata":
		f.Field = "grpc_metadata"
	case segs[0].Name != "client":
		return FieldRead{}, false
	}
	return f, true
}

func levelOf(ctx string) Level {
	switch ctx {
	case "resource":
		return LevelResource
	case "scope", "instrumentation_scope":
		return LevelScope
	}
	return LevelRecord
}

// The provided functions; nil when the runtime lacks the feature. They are
// set only during package initialization.
var (
	replay   ReplayFunc
	evaluate EvaluateFunc
	count    CountFunc
	routing  RoutingFunc
	sampling SamplingFunc
	filter   FilterFunc
	verdict  FilterVerdictFunc
	reads    FilterReadsFunc
	access   AccessFunc
)

// ProvideTransform provides OTTLReplay and the fields a transform
// processor's statements access. Call it only from an init function.
func ProvideTransform(r ReplayFunc, e EvaluateFunc, a AccessFunc) {
	replay, evaluate, access = r, e, a
}

// ProvideCount provides CountAggregation. Call it only from an init function.
func ProvideCount(f CountFunc) { count = f }

// ProvideRouting provides RoutingExplanation. Call it only from an init
// function.
func ProvideRouting(f RoutingFunc) { routing = f }

// ProvideSampling provides SamplingExplanation. Call it only from an init
// function.
func ProvideSampling(f SamplingFunc) { sampling = f }

// ProvideFilter provides FilterExplanation. Call it only from an init
// function.
func ProvideFilter(f FilterFunc, v FilterVerdictFunc, r FilterReadsFunc) {
	filter, verdict, reads = f, v, r
}

// Replay is the transform statement replay, or nil.
func Replay() ReplayFunc { return replay }

// Evaluate is the transform statement evaluation of OTTL previews, or nil.
func Evaluate() EvaluateFunc { return evaluate }

// TransformAccess lists the fields a transform processor's statements
// read and write, or nil.
func TransformAccess() AccessFunc { return access }

// Count is the count connector explanation, or nil.
func Count() CountFunc { return count }

// Routing is the routing connector explanation, or nil.
func Routing() RoutingFunc { return routing }

// Sampling is the tail_sampling explanation, or nil.
func Sampling() SamplingFunc { return sampling }

// Filter is the filter processor explanation, or nil.
func Filter() FilterFunc { return filter }

// FilterVerdict is the filter processor's verdict on any payload, or nil.
func FilterVerdict() FilterVerdictFunc { return verdict }

// FilterReads lists the fields a filter processor's conditions read, or nil.
func FilterReads() FilterReadsFunc { return reads }

// Has reports whether this process provides the feature.
func Has(n Name) bool {
	switch n {
	case OTTLReplay:
		return replay != nil && evaluate != nil && access != nil
	case CountAggregation:
		return count != nil
	case RoutingExplanation:
		return routing != nil
	case SamplingExplanation:
		return sampling != nil
	case FilterExplanation:
		return filter != nil && verdict != nil && reads != nil
	}
	return false
}

// Missing lists the features this process lacks, in report order; nil when
// it has them all.
func Missing() []Name {
	var out []Name
	for _, f := range All {
		if !Has(f.Name) {
			out = append(out, f.Name)
		}
	}
	return out
}

// Lacking returns the features whose component module is not among
// modules (a manifest's), in report order.
func Lacking(modules []string) []Feature {
	var out []Feature
	for _, f := range All {
		if !slices.Contains(modules, f.Module) {
			out = append(out, f)
		}
	}
	return out
}

// Packages maps each feature's component module to its package, for the
// registry generator (manifest.RegistryOptions.Features).
func Packages() map[string]string {
	out := make(map[string]string, len(All))
	for _, f := range All {
		out[f.Module] = f.Package
	}
	return out
}
