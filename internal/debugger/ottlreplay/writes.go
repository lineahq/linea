package ottlreplay

import (
	"context"
	"fmt"
	"reflect"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlresource"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlscope"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspanevent"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pipeline"

	ottlcompat "github.com/lineahq/linea/internal/debugger/compat/ottl"
	"github.com/lineahq/linea/internal/debugger/features"
)

// Writes lists the fields a transform processor's statements for signal
// may write: the paths its editors take as setters (the target of set,
// delete_key, merge_maps and the like), whatever their where clause. Each
// statement is parsed by the OTTL parser with the processor's function set,
// a path parser that records every path instead of resolving it, and
// editors wrapped so that parsing hands over their arguments: a path passed
// where the editor declares a Setter, GetSetter, PMapGetSetter or
// PSliceGetSetter is written (ottl.NewParser, pkg/ottl v0.162.0). A
// statement that cannot be parsed sets reason; callers must then treat the
// processor as writing everything.
func Writes(cfg component.Config, signal pipeline.Signal) (writes []features.FieldRead, reason string) {
	c, ok := cfg.(*transformprocessor.Config)
	if !ok {
		return nil, fmt.Sprintf("unexpected configuration %T", cfg)
	}
	statements, lowest := c.LogStatements, ottllog.ContextName
	switch signal {
	case pipeline.SignalLogs:
	case pipeline.SignalTraces:
		statements, lowest = c.TraceStatements, ottlspanevent.ContextName
	default:
		return nil, fmt.Sprintf("the fields %s statements write are not listed", signal)
	}
	set := componenttest.NewNopTelemetrySettings()
	writes = []features.FieldRead{}
	for i, cs := range statements {
		// A group without a context runs in the one its paths infer, and
		// its paths then all name their context; the lowest context of the
		// signal maps every such prefix.
		ctx := string(cs.Context)
		if ctx == "" {
			ctx = lowest
		}
		for j, text := range cs.Statements {
			got, err := statementWrites(ctx, text, set)
			if err != nil {
				return nil, fmt.Sprintf("statement %d of group %d could not be parsed for what it writes: %v", j, i, err)
			}
			writes = append(writes, got...)
		}
	}
	return writes, ""
}

func statementWrites(ctx, text string, set component.TelemetrySettings) ([]features.FieldRead, error) {
	switch ctx {
	case ottlresource.ContextName:
		return writesOf(ottlcompat.Functions(ottlfuncs.StandardFuncs[*ottlresource.TransformContext]()), ctx, text, set)
	case ottlscope.ContextName:
		return writesOf(ottlcompat.Functions(ottlfuncs.StandardFuncs[*ottlscope.TransformContext]()), ctx, text, set)
	case ottlspan.ContextName:
		return writesOf(ottl.CreateFactoryMap(transformprocessor.DefaultSpanFunctions()...), ctx, text, set)
	case ottlspanevent.ContextName:
		return writesOf(ottl.CreateFactoryMap(transformprocessor.DefaultSpanEventFunctions()...), ctx, text, set)
	case ottllog.ContextName:
		return writesOf(ottl.CreateFactoryMap(transformprocessor.DefaultLogFunctions()...), ctx, text, set)
	}
	return nil, fmt.Errorf("context %s is not listed", ctx)
}

func writesOf[K any](fns map[string]ottl.Factory[K], ctxName, text string, set component.TelemetrySettings) ([]features.FieldRead, error) {
	var out []features.FieldRead
	record := func(p ottl.Path[K]) (ottl.GetSetter[K], error) {
		f, ok := features.PathField(ctxName, p.Context(), pathSegments(p))
		return ottl.StandardGetSetter[K]{
			Getter: func(context.Context, K) (any, error) { return nil, nil },
			Setter: func(context.Context, K, any) error {
				if ok {
					out = append(out, f)
				}
				return nil
			},
		}, nil
	}
	wrapped := make(map[string]ottl.Factory[K], len(fns))
	for name, f := range fns {
		wrapped[name] = ottl.NewFactory(f.Name(), f.CreateDefaultArguments(), func(_ ottl.FunctionContext, args ottl.Arguments) (ottl.ExprFunc[K], error) {
			setTargets[K](args)
			return func(context.Context, K) (any, error) { return nil, nil }, nil
		})
	}
	// Enums are constants; any symbol parses.
	anyEnum := func(*ottl.EnumSymbol) (*ottl.Enum, error) { e := ottl.Enum(0); return &e, nil }
	parser, err := ottl.NewParser[K](wrapped, record, set, ottl.WithEnumParser[K](anyEnum))
	if err != nil {
		return nil, err
	}
	if _, err := parser.ParseStatement(text); err != nil {
		return nil, err
	}
	return out, nil
}

// setTargets calls Set on every argument the function declares with a Set
// method (Setter, GetSetter, PMapGetSetter, PSliceGetSetter), so that the
// recording path parser notes the paths it writes. Arguments declared as
// getters are read only, even when the value passed could be set.
func setTargets[K any](args ottl.Arguments) {
	v := reflect.ValueOf(args)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	ctx, tCtx := reflect.ValueOf(context.Background()), reflect.Zero(reflect.TypeFor[K]())
	for i := range v.NumField() {
		f := v.Field(i)
		if f.Kind() != reflect.Interface || f.IsNil() || !f.CanInterface() {
			continue
		}
		set, ok := f.Type().MethodByName("Set")
		if !ok || set.Type.NumIn() != 3 {
			continue
		}
		f.MethodByName("Set").Call([]reflect.Value{ctx, tCtx, reflect.Zero(set.Type.In(2))})
	}
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
