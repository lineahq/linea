package livecaptureextension

import (
	"context"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlmetric"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlresource"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
)

// The largest filter a session compiles, as reported in the control
// protocol's Identity.filter_limits. Lengths count Unicode code points.
const (
	maxFilterConditions = 16
	maxConditionChars   = 256
	maxRegexChars       = 256
)

type conditions[K any] []*ottl.Condition[K]

func (cs conditions[K]) match(ctx context.Context, record K) (bool, error) {
	for _, condition := range cs {
		ok, err := condition.Eval(ctx, record)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// A literal pattern is mandatory, so regex compilation never happens per record.
type matchArguments[K any] struct {
	Target  ottl.StringLikeGetter[K]
	Pattern string
}

func readOnlyFunctions[K any]() map[string]ottl.Factory[K] {
	match := ottl.NewFactory[K]("IsMatch", &matchArguments[K]{}, func(_ ottl.FunctionContext, arguments ottl.Arguments) (ottl.ExprFunc[K], error) {
		args, ok := arguments.(*matchArguments[K])
		if !ok {
			return nil, fmt.Errorf("invalid IsMatch arguments")
		}
		if utf8.RuneCountInString(args.Pattern) > maxRegexChars {
			return nil, fmt.Errorf("regex exceeds %d characters", maxRegexChars)
		}
		re, err := regexp.Compile(args.Pattern)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, record K) (any, error) {
			value, exists, err := args.Target.Get(ctx, record)
			if err != nil || !exists {
				return false, err
			}
			return re.MatchString(value), nil
		}, nil
	})
	return ottl.CreateFactoryMap(match, ottlfuncs.NewIsStringFactory[K](), ottlfuncs.NewIsIntFactory[K](), ottlfuncs.NewIsDoubleFactory[K](), ottlfuncs.NewIsBoolFactory[K](), ottlfuncs.NewIsMapFactory[K](), ottlfuncs.NewIsListFactory[K](), ottlfuncs.NewTraceIDFactory[K](), ottlfuncs.NewSpanIDFactory[K]())
}

type compiledFilter struct {
	resource  conditions[*ottlresource.TransformContext]
	span      conditions[*ottlspan.TransformContext]
	log       conditions[*ottllog.TransformContext]
	metric    conditions[*ottlmetric.TransformContext]
	datapoint conditions[*ottldatapoint.TransformContext]
}

func parseConditions[K any](parser ottl.Parser[K], expressions []string, err error) (conditions[K], error) {
	if err != nil {
		return nil, err
	}
	result := make(conditions[K], 0, len(expressions))
	for _, expression := range expressions {
		condition, err := parser.ParseCondition(expression)
		if err != nil {
			return nil, err
		}
		result = append(result, condition)
	}
	return result, nil
}

func compileFilter(filter Filter, telemetry component.TelemetrySettings) (*compiledFilter, error) {
	total := 0
	for _, level := range [][]string{filter.Resource, filter.Traces.Span, filter.Logs.Log, filter.Metrics.Metric, filter.Metrics.Datapoint} {
		total += len(level)
		for _, expression := range level {
			if utf8.RuneCountInString(expression) > maxConditionChars {
				return nil, fmt.Errorf("condition exceeds %d characters", maxConditionChars)
			}
		}
	}
	if total > maxFilterConditions {
		return nil, fmt.Errorf("at most %d conditions are allowed", maxFilterConditions)
	}
	// OTTL diagnostics must never log production payloads, even at Collector debug level.
	telemetry.Logger = zap.NewNop()
	result := &compiledFilter{}
	rp, err := ottlresource.NewParser(readOnlyFunctions[*ottlresource.TransformContext](), telemetry)
	result.resource, err = parseConditions(rp, filter.Resource, err)
	if err != nil {
		return nil, fmt.Errorf("resource filter: %w", err)
	}
	sp, err := ottlspan.NewParser(readOnlyFunctions[*ottlspan.TransformContext](), telemetry)
	result.span, err = parseConditions(sp, filter.Traces.Span, err)
	if err != nil {
		return nil, fmt.Errorf("span filter: %w", err)
	}
	lp, err := ottllog.NewParser(readOnlyFunctions[*ottllog.TransformContext](), telemetry)
	result.log, err = parseConditions(lp, filter.Logs.Log, err)
	if err != nil {
		return nil, fmt.Errorf("log filter: %w", err)
	}
	mp, err := ottlmetric.NewParser(readOnlyFunctions[*ottlmetric.TransformContext](), telemetry)
	result.metric, err = parseConditions(mp, filter.Metrics.Metric, err)
	if err != nil {
		return nil, fmt.Errorf("metric filter: %w", err)
	}
	dp, err := ottldatapoint.NewParser(readOnlyFunctions[*ottldatapoint.TransformContext](), telemetry)
	result.datapoint, err = parseConditions(dp, filter.Metrics.Datapoint, err)
	if err != nil {
		return nil, fmt.Errorf("datapoint filter: %w", err)
	}
	return result, nil
}
