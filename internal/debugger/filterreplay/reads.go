package filterreplay

import (
	"context"
	"fmt"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlresource"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlscope"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspanevent"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/features"
)

// Reads lists the fields the processor's conditions for signal read. Each
// condition is parsed by the OTTL parser with the processor's function set
// and a path parser that records every path instead of resolving it
// (ottl.NewParser takes any PathExpressionParser, pkg/ottl v0.162.0); paths
// are never matched as text. reason is set when the conditions cannot be
// listed, which callers must treat as reading everything.
func Reads(cfg component.Config, signal pipeline.Signal) (reads []features.FieldRead, reason string) {
	c, ok := cfg.(*filterprocessor.Config)
	if !ok {
		return nil, fmt.Sprintf("unexpected configuration %T", cfg)
	}
	p, err := newPlan(c, signal)
	if err != nil {
		return nil, "the conditions could not be parsed: " + err.Error()
	}
	if p.unsupported != "" {
		return nil, p.unsupported
	}
	set := componenttest.NewNopTelemetrySettings()
	reads = []features.FieldRead{}
	for _, cond := range p.conditions {
		var got []features.FieldRead
		switch cond.Context {
		case ottllog.ContextName:
			got, err = readsOf(filterprocessor.DefaultLogFunctions(), cond.Context, cond.Condition, set)
		case ottlspan.ContextName:
			got, err = readsOf(filterprocessor.DefaultSpanFunctions(), cond.Context, cond.Condition, set)
		case ottlspanevent.ContextName:
			got, err = readsOf(filterprocessor.DefaultSpanEventFunctions(), cond.Context, cond.Condition, set)
		case ottlresource.ContextName:
			got, err = readsOf(filterprocessor.DefaultResourceFunctions(), cond.Context, cond.Condition, set)
		case ottlscope.ContextName:
			got, err = readsOfMap(scopeFunctions(), cond.Context, cond.Condition, set)
		default:
			return nil, fmt.Sprintf("the fields read in context %s are not listed", cond.Context)
		}
		if err != nil {
			return nil, fmt.Sprintf("condition %d could not be parsed for its paths: %v", cond.Index, err)
		}
		reads = append(reads, got...)
	}
	return reads, ""
}

func readsOf[K any](fns []ottl.Factory[K], ctxName, text string, set component.TelemetrySettings) ([]features.FieldRead, error) {
	return readsOfMap(ottl.CreateFactoryMap(fns...), ctxName, text, set)
}

func readsOfMap[K any](fns map[string]ottl.Factory[K], ctxName, text string, set component.TelemetrySettings) ([]features.FieldRead, error) {
	var out []features.FieldRead
	record := func(p ottl.Path[K]) (ottl.GetSetter[K], error) {
		// The parser checks that every segment and key was used.
		for s := p; s != nil; s = s.Next() {
			s.Keys()
		}
		if r, ok := fieldRead(ctxName, p); ok {
			out = append(out, r)
		}
		return ottl.StandardGetSetter[K]{
			Getter: func(context.Context, K) (any, error) { return nil, nil },
			Setter: func(context.Context, K, any) error { return nil },
		}, nil
	}
	// Enums are constants, not reads; any symbol parses.
	anyEnum := func(*ottl.EnumSymbol) (*ottl.Enum, error) { e := ottl.Enum(0); return &e, nil }
	parser, err := ottl.NewParser[K](fns, record, set, ottl.WithEnumParser[K](anyEnum))
	if err != nil {
		return nil, err
	}
	if _, err := parser.ParseCondition(text); err != nil {
		return nil, err
	}
	return out, nil
}

// fieldRead maps a path in context ctxName to the level and field it reads
// (features.PathField).
func fieldRead[K any](ctxName string, p ottl.Path[K]) (features.FieldRead, bool) {
	return features.PathField(ctxName, p.Context(), pathSegments(p))
}

// pathSegments lists a path's segments with their first literal key.
func pathSegments[K any](p ottl.Path[K]) []features.PathSegment {
	var out []features.PathSegment
	for s := p; s != nil; s = s.Next() {
		seg := features.PathSegment{Name: s.Name()}
		if keys := s.Keys(); len(keys) > 0 {
			var zero K
			if v, err := keys[0].String(context.Background(), zero); err == nil && v != nil {
				seg.Key = *v
			}
		}
		out = append(out, seg)
	}
	return out
}
