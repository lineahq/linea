package filterreplay

import (
	"fmt"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlmetric"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlresource"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlscope"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspanevent"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	ottlcompat "github.com/lineahq/linea/internal/debugger/compat/ottl"
)

// entry is one parsed condition and its index in the report.
type entry[K any] struct {
	index int
	cond  *ottl.Condition[K]
}

// group is one condition group as the processor builds it: the
// conditions of each context, each context an OR sequence evaluated
// resource first, then scope, then the record contexts.
type group struct {
	mode      ottl.ErrorMode
	resource  []entry[*ottlresource.TransformContext]
	scope     []entry[*ottlscope.TransformContext]
	log       []entry[*ottllog.TransformContext]
	span      []entry[*ottlspan.TransformContext]
	spanEvent []entry[*ottlspanevent.TransformContext]
	metric    []entry[*ottlmetric.TransformContext]
	dataPoint []entry[*ottldatapoint.TransformContext]
}

func (g *group) records() bool {
	return len(g.log)+len(g.span)+len(g.spanEvent)+len(g.metric)+len(g.dataPoint) > 0
}

// parsed is what the parser collection returns for one condition list:
// the conditions of the one context they were parsed in.
type parsed struct {
	context   string
	resource  []*ottl.Condition[*ottlresource.TransformContext]
	scope     []*ottl.Condition[*ottlscope.TransformContext]
	log       []*ottl.Condition[*ottllog.TransformContext]
	span      []*ottl.Condition[*ottlspan.TransformContext]
	spanEvent []*ottl.Condition[*ottlspanevent.TransformContext]
	metric    []*ottl.Condition[*ottlmetric.TransformContext]
	dataPoint []*ottl.Condition[*ottldatapoint.TransformContext]
}

func (p parsed) len() int {
	return len(p.resource) + len(p.scope) + len(p.log) + len(p.span) + len(p.spanEvent) + len(p.metric) + len(p.dataPoint)
}

// add appends p's conditions to g, numbering them from index.
func (g *group) add(p parsed, index int) {
	g.resource = appendEntries(g.resource, p.resource, index)
	g.scope = appendEntries(g.scope, p.scope, index)
	g.log = appendEntries(g.log, p.log, index)
	g.span = appendEntries(g.span, p.span, index)
	g.spanEvent = appendEntries(g.spanEvent, p.spanEvent, index)
	g.metric = appendEntries(g.metric, p.metric, index)
	g.dataPoint = appendEntries(g.dataPoint, p.dataPoint, index)
}

func appendEntries[K any](es []entry[K], cs []*ottl.Condition[K], index int) []entry[K] {
	for i, c := range cs {
		es = append(es, entry[K]{index: index + i, cond: c})
	}
	return es
}

// convert builds a condition converter that tags the conditions with their
// context.
func convert[K any](name string, set func(*parsed, []*ottl.Condition[K])) ottl.ParsedConditionsConverter[K, parsed] {
	return func(_ *ottl.ParserCollection[parsed], _ ottl.ConditionsGetter, cs []*ottl.Condition[K]) (parsed, error) {
		p := parsed{context: name}
		set(&p, cs)
		return p, nil
	}
}

func contextOption[K any](name string, parser ottl.Parser[K], set func(*parsed, []*ottl.Condition[K])) ottl.ParserCollectionOption[parsed] {
	return ottl.WithParserCollectionContext(name, &parser, ottl.WithConditionConverter(convert(name, set)))
}

// scopeFunctions is the processor's scope function set
// (filterottl.StandardScopeFuncs, which cannot be imported).
func scopeFunctions() map[string]ottl.Factory[*ottlscope.TransformContext] {
	return ottlcompat.Functions(ottlfuncs.StandardConverters[*ottlscope.TransformContext]())
}

// collection builds the parser collection the processor builds for the
// signal's *_conditions form: the record contexts, then resource and scope
// (filterprocessor v0.162.0, internal/condition).
func collection(signal pipeline.Signal, set component.TelemetrySettings) (*ottl.ParserCollection[parsed], error) {
	var opts []ottl.ParserCollectionOption[parsed]
	switch signal {
	case pipeline.SignalLogs:
		lp, err := ottllog.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultLogFunctions()...), set, ottllog.EnablePathContextNames())
		if err != nil {
			return nil, err
		}
		opts = append(opts, contextOption(ottllog.ContextName, lp, func(p *parsed, cs []*ottl.Condition[*ottllog.TransformContext]) { p.log = cs }))
	case pipeline.SignalTraces:
		sp, err := ottlspan.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultSpanFunctions()...), set, ottlspan.EnablePathContextNames())
		if err != nil {
			return nil, err
		}
		ep, err := ottlspanevent.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultSpanEventFunctions()...), set, ottlspanevent.EnablePathContextNames())
		if err != nil {
			return nil, err
		}
		opts = append(opts,
			contextOption(ottlspan.ContextName, sp, func(p *parsed, cs []*ottl.Condition[*ottlspan.TransformContext]) { p.span = cs }),
			contextOption(ottlspanevent.ContextName, ep, func(p *parsed, cs []*ottl.Condition[*ottlspanevent.TransformContext]) { p.spanEvent = cs }))
	case pipeline.SignalMetrics:
		mp, err := ottlmetric.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultMetricFunctions()...), set, ottlmetric.EnablePathContextNames())
		if err != nil {
			return nil, err
		}
		dp, err := ottldatapoint.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultDataPointFunctions()...), set, ottldatapoint.EnablePathContextNames())
		if err != nil {
			return nil, err
		}
		opts = append(opts,
			contextOption(ottlmetric.ContextName, mp, func(p *parsed, cs []*ottl.Condition[*ottlmetric.TransformContext]) { p.metric = cs }),
			contextOption(ottldatapoint.ContextName, dp, func(p *parsed, cs []*ottl.Condition[*ottldatapoint.TransformContext]) { p.dataPoint = cs }))
	default:
		return nil, fmt.Errorf("signal %s is not filtered", signal)
	}
	rp, err := ottlresource.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultResourceFunctions()...), set, ottlresource.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	scp, err := ottlscope.NewParser(scopeFunctions(), set, ottlscope.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	opts = append(opts,
		contextOption(ottlresource.ContextName, rp, func(p *parsed, cs []*ottl.Condition[*ottlresource.TransformContext]) { p.resource = cs }),
		contextOption(ottlscope.ContextName, scp, func(p *parsed, cs []*ottl.Condition[*ottlscope.TransformContext]) { p.scope = cs }))
	return ottl.NewParserCollection(set, opts...)
}

// contextConditions is one entry of a *_conditions list, read from the
// processor's configuration (its type is internal to the processor).
type contextConditions struct {
	context    string
	conditions []string
	errorMode  ottl.ErrorMode
}

// legacyList is one deprecated per-context list.
type legacyList struct {
	field      string
	context    string
	conditions []string
}

// plan is the processor's filtering of one signal: its groups and the
// report rows of their conditions.
type plan struct {
	groups     []group
	conditions []analysis.FilterCondition
	// unsupported says why the signal's filtering cannot be replayed.
	unsupported string
}

// newPlan parses the configuration the way the processor does for the
// signal: the *_conditions form when set, else the deprecated OTTL lists,
// else the match properties, which are not OTTL.
func newPlan(c *filterprocessor.Config, signal pipeline.Signal) (*plan, error) {
	set := componenttest.NewNopTelemetrySettings()
	var groups []contextConditions
	var legacy []legacyList
	var field string
	var matchProperties bool
	switch signal {
	case pipeline.SignalLogs:
		field = "log_conditions"
		for _, g := range c.LogConditions {
			groups = append(groups, contextConditions{string(g.Context), g.Conditions, g.ErrorMode})
		}
		if c.Logs.ResourceConditions != nil || c.Logs.LogConditions != nil {
			legacy = []legacyList{{"logs.resource", ottlresource.ContextName, c.Logs.ResourceConditions}, {"logs.log_record", ottllog.ContextName, c.Logs.LogConditions}}
		}
		matchProperties = c.Logs.Include != nil || c.Logs.Exclude != nil
	case pipeline.SignalTraces:
		field = "trace_conditions"
		for _, g := range c.TraceConditions {
			groups = append(groups, contextConditions{string(g.Context), g.Conditions, g.ErrorMode})
		}
		if c.Traces.ResourceConditions != nil || c.Traces.SpanConditions != nil || c.Traces.SpanEventConditions != nil {
			legacy = []legacyList{{"traces.resource", ottlresource.ContextName, c.Traces.ResourceConditions}, {"traces.span", ottlspan.ContextName, c.Traces.SpanConditions}, {"traces.spanevent", ottlspanevent.ContextName, c.Traces.SpanEventConditions}}
		}
		matchProperties = c.Spans.Include != nil || c.Spans.Exclude != nil
	case pipeline.SignalMetrics:
		field = "metric_conditions"
		for _, g := range c.MetricConditions {
			groups = append(groups, contextConditions{string(g.Context), g.Conditions, g.ErrorMode})
		}
		if c.Metrics.ResourceConditions != nil || c.Metrics.MetricConditions != nil || c.Metrics.DataPointConditions != nil {
			legacy = []legacyList{{"metrics.resource", ottlresource.ContextName, c.Metrics.ResourceConditions}, {"metrics.metric", ottlmetric.ContextName, c.Metrics.MetricConditions}, {"metrics.datapoint", ottldatapoint.ContextName, c.Metrics.DataPointConditions}}
		}
		matchProperties = c.Metrics.Include != nil || c.Metrics.Exclude != nil
	default:
		return &plan{unsupported: fmt.Sprintf("signal %s is not filtered", signal)}, nil
	}

	p := &plan{}
	switch {
	case len(groups) > 0:
		pc, err := collection(signal, set)
		if err != nil {
			return nil, err
		}
		for gi, cc := range groups {
			g := group{mode: c.ErrorMode}
			if cc.errorMode != "" {
				g.mode = cc.errorMode
			}
			if cc.context != "" {
				ps, err := pc.ParseConditionsWithContext(cc.context, ottl.NewConditionsGetter(cc.conditions), true)
				if err != nil {
					return nil, err
				}
				if ps.len() != len(cc.conditions) {
					return nil, fmt.Errorf("%s[%d]: parsed %d of %d conditions", field, gi, ps.len(), len(cc.conditions))
				}
				g.add(ps, len(p.conditions))
				for ci, text := range cc.conditions {
					p.conditions = append(p.conditions, row(fmt.Sprintf("%s[%d].conditions[%d]", field, gi, ci), ps.context, text, g.mode))
				}
			} else {
				// Without a context each condition's context is inferred on
				// its own.
				for ci, text := range cc.conditions {
					ps, err := pc.ParseConditions(ottl.NewConditionsGetter([]string{text}))
					if err != nil {
						return nil, err
					}
					if ps.len() != 1 {
						return nil, fmt.Errorf("%s[%d].conditions[%d]: parsed %d conditions", field, gi, ci, ps.len())
					}
					g.add(ps, len(p.conditions))
					p.conditions = append(p.conditions, row(fmt.Sprintf("%s[%d].conditions[%d]", field, gi, ci), ps.context, text, g.mode))
				}
			}
			p.groups = append(p.groups, g)
		}
	case legacy != nil:
		g := group{mode: c.ErrorMode}
		for _, l := range legacy {
			if l.conditions == nil {
				continue
			}
			ps, err := parseLegacy(l.context, l.conditions, set)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", l.field, err)
			}
			g.add(ps, len(p.conditions))
			for ci, text := range l.conditions {
				p.conditions = append(p.conditions, row(fmt.Sprintf("%s[%d]", l.field, ci), l.context, text, g.mode))
			}
		}
		p.groups = []group{g}
	case matchProperties:
		p.unsupported = "the processor filters " + signal.String() + " with include/exclude match properties, which are not OTTL conditions"
	}
	return p, nil
}

func row(source, context, text string, mode ottl.ErrorMode) analysis.FilterCondition {
	return analysis.FilterCondition{Source: source, Context: context, Condition: text, ErrorMode: string(mode)}
}

// parseLegacy parses a deprecated per-context list with the plain context
// parser, as filterottl.NewBoolExprFor* does.
func parseLegacy(context string, conditions []string, set component.TelemetrySettings) (parsed, error) {
	p := parsed{context: context}
	var err error
	switch context {
	case ottlresource.ContextName:
		p.resource, err = parseWith(ottlresource.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultResourceFunctions()...), set))(conditions)
	case ottllog.ContextName:
		p.log, err = parseWith(ottllog.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultLogFunctions()...), set))(conditions)
	case ottlspan.ContextName:
		p.span, err = parseWith(ottlspan.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultSpanFunctions()...), set))(conditions)
	case ottlspanevent.ContextName:
		p.spanEvent, err = parseWith(ottlspanevent.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultSpanEventFunctions()...), set))(conditions)
	case ottlmetric.ContextName:
		p.metric, err = parseWith(ottlmetric.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultMetricFunctions()...), set))(conditions)
	case ottldatapoint.ContextName:
		p.dataPoint, err = parseWith(ottldatapoint.NewParser(ottl.CreateFactoryMap(filterprocessor.DefaultDataPointFunctions()...), set))(conditions)
	default:
		err = fmt.Errorf("unknown context %s", context)
	}
	return p, err
}

func parseWith[K any](parser ottl.Parser[K], err error) func([]string) ([]*ottl.Condition[K], error) {
	return func(conditions []string) ([]*ottl.Condition[K], error) {
		if err != nil {
			return nil, err
		}
		return parser.ParseConditions(conditions)
	}
}
