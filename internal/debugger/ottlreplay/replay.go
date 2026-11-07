// Package ottlreplay replays transform processor OTTL statements one at a
// time on a recorded stage input using the upstream OTTL parser and
// evaluator, then checks the replay against the processor's actual output.
// Its report types are package ottldebug's.
//
// It is an optional feature: the generated registry imports it only
// when the runtime's manifest has the transform processor, and it registers
// itself with package features.
//
// The replay is an explanation, not the observation: the authoritative data
// is the processor's stage.output. A replay is reported as replay_verified
// only when its final telemetry equals that output byte for byte.
//
// Scope: trace_statements (resource, scope, span), log_statements (resource,
// scope, log) and metric_statements (resource, scope, metric, datapoint,
// exemplar). Everything else, such as span events, group conditions,
// shared_cache and flatten_data, is reported as unsupported rather than
// approximated.
package ottlreplay

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlexemplar"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlmetric"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlresource"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlscope"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	ottlcompat "github.com/lineahq/linea/internal/debugger/compat/ottl"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// The report types are ottldebug's; these names keep the replay readable.
type (
	Report         = ottldebug.Report
	Group          = ottldebug.Group
	Statement      = ottldebug.Statement
	ExecutionError = ottldebug.ExecutionError
	Status         = ottldebug.Status
	Verification   = ottldebug.Verification
)

const (
	StatusReplayed             = ottldebug.StatusReplayed
	StatusUnsupported          = ottldebug.StatusUnsupported
	StatusNotEvaluated         = ottldebug.StatusNotEvaluated
	VerificationReplayVerified = ottldebug.VerificationReplayVerified
	VerificationDisagrees      = ottldebug.VerificationDisagrees
	VerificationNotVerifiable  = ottldebug.VerificationNotVerifiable
)

func init() { features.ProvideTransform(Replay, Evaluate, Access) }

// Replay replays cfg's statements for the input's signal on input and
// compares the result to output, the processor's correlated stage.output
// (nil when there was none).
func Replay(ctx context.Context, processor string, cfg component.Config, inputSeq int, input telemetry.Payload, outputSeq int, output *telemetry.Payload) (*Report, error) {
	rep, data, supported, aborted, err := replay(ctx, processor, cfg, inputSeq, input)
	if err != nil {
		return nil, err
	}
	rep.Output = outputSeq
	if rep.Verification != "" {
		return rep, nil
	}
	switch {
	case !supported:
		rep.Verification = VerificationNotVerifiable
	case aborted && output == nil:
		rep.Verification = VerificationReplayVerified
		rep.Notes = append(rep.Notes, "the processor and the replay both rejected the input")
	case aborted || output == nil:
		rep.Verification = VerificationDisagrees
		rep.Notes = append(rep.Notes, fmt.Sprintf("replay rejected=%t, processor emitted=%t", aborted, output != nil))
	default:
		got, err := data.JSON()
		if err != nil {
			return nil, err
		}
		want, err := output.JSON()
		if err != nil {
			return nil, err
		}
		if bytes.Equal(got, want) {
			rep.Verification = VerificationReplayVerified
			break
		}
		rep.Verification = VerificationDisagrees
		d, err := analysis.DiffPayloads(*output, data)
		if err != nil {
			return nil, err
		}
		rep.Mismatch = &d
	}
	return rep, nil
}

// Evaluate replays cfg's statements on input when there is no observed
// output to check against, as for a statement being edited. The report is
// therefore never verified. output is the replayed payload, or nil when
// processing stopped (error_mode propagate), in which case the processor
// would drop the input. Statement results are only as reliable as the
// replay; groups that are not replayed are reported as such.
func Evaluate(ctx context.Context, processor string, cfg component.Config, input telemetry.Payload) (rep *Report, output *telemetry.Payload, err error) {
	rep, data, supported, aborted, err := replay(ctx, processor, cfg, 0, input)
	if err != nil {
		return nil, nil, err
	}
	if rep.Verification != "" {
		return rep, nil, nil
	}
	rep.Verification = VerificationNotVerifiable
	switch {
	case aborted:
		rep.Notes = append(rep.Notes, "processing stopped: the processor would drop this input")
		return rep, nil, nil
	case !supported:
		rep.Notes = append(rep.Notes, "some groups were not replayed; the result shows only the groups before them")
	}
	rep.Notes = append(rep.Notes, "evaluated without an observed output: rerun to verify against the processor")
	return rep, &data, nil
}

// replay runs the statements on a copy of input. A report whose
// Verification is already set needs no comparison (the signal is not
// replayed).
func replay(ctx context.Context, processor string, cfg component.Config, inputSeq int, input telemetry.Payload) (rep *Report, data telemetry.Payload, supported, aborted bool, err error) {
	tcfg, ok := cfg.(*transformprocessor.Config)
	if !ok {
		return nil, data, false, false, fmt.Errorf("%s: not a transform processor config (%T)", processor, cfg)
	}
	rep = &Report{Processor: processor, Input: inputSeq, Signal: input.Signal.String(), Groups: []Group{}}
	statements := tcfg.TraceStatements
	switch input.Signal {
	case pipeline.SignalTraces:
	case pipeline.SignalLogs:
		statements = tcfg.LogStatements
	case pipeline.SignalMetrics:
		statements = tcfg.MetricStatements
	default:
		rep.Verification = VerificationNotVerifiable
		rep.Notes = append(rep.Notes, input.Signal.String()+" statements are not replayed")
		return rep, data, false, false, nil
	}
	// flatten_data gives every log record its own resource and scope before
	// the statements run and regroups them afterwards.
	flatten := input.Signal == pipeline.SignalLogs && tcfg.FlattenData

	pc, err := newParserCollection(input.Signal, tcfg.ErrorMode)
	if err != nil {
		return nil, data, false, false, err
	}

	data = input.Clone()
	supported = true
	for i, cs := range statements {
		mode := tcfg.ErrorMode
		if cs.ErrorMode != "" {
			mode = cs.ErrorMode
		}
		g := Group{Index: i, Context: string(cs.Context), ErrorMode: string(mode), Conditions: cs.Conditions}
		for j, text := range cs.Statements {
			g.Statements = append(g.Statements, Statement{Index: j, Text: text})
		}
		switch {
		case aborted:
			g.Status, g.Reason = StatusNotEvaluated, "an earlier statement stopped processing (error_mode propagate)"
		case !supported:
			g.Status, g.Reason = StatusNotEvaluated, "follows an unsupported group, so its input is unknown"
		case flatten:
			g.Status, g.Reason = StatusUnsupported, "flatten_data is not replayed"
		case len(cs.Conditions) > 0:
			// Group conditions are compiled by the processor with the filter
			// processor's internal function set, which is not importable.
			g.Status, g.Reason = StatusUnsupported, "group conditions are not replayed"
		case cs.SharedCache:
			g.Status, g.Reason = StatusUnsupported, "shared_cache is not replayed"
		default:
			spec := groupSpec{statements: cs.Statements, errorMode: mode}
			var prog program
			if cs.Context != "" {
				prog, err = pc.ParseStatementsWithContext(string(cs.Context), spec, true)
			} else {
				prog, err = pc.ParseStatements(spec, ottl.WithContextInferenceConditions(cs.Conditions))
			}
			if err != nil {
				g.Status, g.Reason = StatusUnsupported, fmt.Sprintf("not parseable by the replay contexts for %s (%s): %v",
					input.Signal, strings.Join(replayContexts[input.Signal], ", "), err)
				break
			}
			g.Status = StatusReplayed
			if g.Context == "" {
				g.Context = prog.context()
			}
			for j, eff := range prog.texts() {
				if eff != g.Statements[j].Text {
					g.Statements[j].Effective = eff
				}
			}
			if err := prog.run(ctx, data, &g); err != nil {
				aborted = true
				rep.Error = err.Error()
			}
		}
		if g.Status != StatusReplayed {
			supported = false
		}
		rep.Groups = append(rep.Groups, g)
	}

	return rep, data, supported, aborted, nil
}

// groupSpec carries a group's statements and effective error mode through
// the parser collection to the statement converters.
type groupSpec struct {
	statements []string
	errorMode  ottl.ErrorMode
}

func (g groupSpec) GetStatements() []string { return g.statements }

// program is a parsed statement group bound to its OTTL context.
type program interface {
	context() string
	texts() []string
	run(ctx context.Context, data telemetry.Payload, g *Group) error
}

// replayContexts lists the OTTL contexts replayed per signal, matching the
// transform processor's parser collection for that signal.
var replayContexts = map[pipeline.Signal][]string{
	pipeline.SignalTraces:  {ottlresource.ContextName, ottlscope.ContextName, ottlspan.ContextName},
	pipeline.SignalLogs:    {ottlresource.ContextName, ottlscope.ContextName, ottllog.ContextName},
	pipeline.SignalMetrics: {ottlresource.ContextName, ottlscope.ContextName, ottlmetric.ContextName, ottldatapoint.ContextName, ottlexemplar.ContextName},
}

// newParserCollection mirrors the transform processor's parser collection
// for signal: the same contexts, function sets and path-context handling.
// Only the signal's own contexts are registered, so context inference picks
// the context the processor would.
func newParserCollection(signal pipeline.Signal, errorMode ottl.ErrorMode) (*ottl.ParserCollection[program], error) {
	settings := componenttest.NewNopTelemetrySettings()
	rp, err := ottlresource.NewParser(ottlcompat.Functions(ottlfuncs.StandardFuncs[*ottlresource.TransformContext]()), settings, ottlresource.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	sp, err := ottlscope.NewParser(ottlcompat.Functions(ottlfuncs.StandardFuncs[*ottlscope.TransformContext]()), settings, ottlscope.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	opts := []ottl.ParserCollectionOption[program]{
		ottl.WithParserCollectionContext(ottlresource.ContextName, &rp, ottl.WithStatementConverter(convert(ottlresource.ContextName, runResource))),
		ottl.WithParserCollectionContext(ottlscope.ContextName, &sp, ottl.WithStatementConverter(convert(ottlscope.ContextName, runScope))),
		ottl.WithParserCollectionErrorMode[program](errorMode),
	}
	switch signal {
	case pipeline.SignalTraces:
		p, err := ottlspan.NewParser(ottl.CreateFactoryMap(transformprocessor.DefaultSpanFunctions()...), settings, ottlspan.EnablePathContextNames())
		if err != nil {
			return nil, err
		}
		opts = append(opts, ottl.WithParserCollectionContext(ottlspan.ContextName, &p, ottl.WithStatementConverter(convert(ottlspan.ContextName, runSpan))))
	case pipeline.SignalLogs:
		p, err := ottllog.NewParser(ottl.CreateFactoryMap(transformprocessor.DefaultLogFunctions()...), settings, ottllog.EnablePathContextNames())
		if err != nil {
			return nil, err
		}
		opts = append(opts, ottl.WithParserCollectionContext(ottllog.ContextName, &p, ottl.WithStatementConverter(convert(ottllog.ContextName, runLog))))
	case pipeline.SignalMetrics:
		mp, err := ottlmetric.NewParser(ottl.CreateFactoryMap(transformprocessor.DefaultMetricFunctions()...), settings, ottlmetric.EnablePathContextNames())
		if err != nil {
			return nil, err
		}
		dp, err := ottldatapoint.NewParser(ottl.CreateFactoryMap(transformprocessor.DefaultDataPointFunctions()...), settings, ottldatapoint.EnablePathContextNames())
		if err != nil {
			return nil, err
		}
		ep, err := ottlexemplar.NewParser(ottl.CreateFactoryMap(transformprocessor.DefaultExemplarFunctions()...), settings, ottlexemplar.EnablePathContextNames())
		if err != nil {
			return nil, err
		}
		opts = append(opts,
			ottl.WithParserCollectionContext(ottlmetric.ContextName, &mp, ottl.WithStatementConverter(convert(ottlmetric.ContextName, runMetric))),
			ottl.WithParserCollectionContext(ottldatapoint.ContextName, &dp, ottl.WithStatementConverter(convert(ottldatapoint.ContextName, runDataPoint))),
			ottl.WithParserCollectionContext(ottlexemplar.ContextName, &ep, ottl.WithStatementConverter(convert(ottlexemplar.ContextName, runExemplar))))
	}
	return ottl.NewParserCollection(settings, opts...)
}

// runner executes parsed statements over every entity of one context in
// data, in the same iteration order as the processor.
type runner[K any] func(ctx context.Context, data telemetry.Payload, seq *sequence[K], g *Group) error

type compiled[K any] struct {
	name string
	seq  *sequence[K]
	exec runner[K]
}

func (c compiled[K]) context() string { return c.name }

func (c compiled[K]) texts() []string {
	out := make([]string, len(c.seq.statements))
	for i, s := range c.seq.statements {
		out[i] = s.String()
	}
	return out
}

func (c compiled[K]) run(ctx context.Context, data telemetry.Payload, g *Group) error {
	return c.exec(ctx, data, c.seq, g)
}

func convert[K any](name string, run runner[K]) ottl.ParsedStatementsConverter[K, program] {
	return func(_ *ottl.ParserCollection[program], sg ottl.StatementsGetter, parsed []*ottl.Statement[K]) (program, error) {
		spec, ok := sg.(groupSpec)
		if !ok {
			return nil, fmt.Errorf("unexpected statements getter %T", sg)
		}
		return compiled[K]{name: name, seq: &sequence[K]{statements: parsed, errorMode: spec.errorMode}, exec: run}, nil
	}
}

// sequence executes statements one by one, mirroring
// ottl.StatementSequence.Execute while recording each statement's outcome
// and the changes it made.
type sequence[K any] struct {
	statements []*ottl.Statement[K]
	errorMode  ottl.ErrorMode
}

// execute runs all statements against tCtx. snapshot captures the entity's
// current state and returns a function producing the changes made since.
func (s *sequence[K]) execute(ctx context.Context, tCtx K, entity string, g *Group, snapshot func() func() []analysis.Change) error {
	for i, st := range s.statements {
		res := &g.Statements[i]
		changes := snapshot()
		_, matched, err := st.Execute(ctx, tCtx)
		if err != nil {
			phase := "condition"
			if matched {
				phase = "function"
				res.Matched++
			}
			res.Errors = append(res.Errors, ExecutionError{Entity: entity, Phase: phase, Message: err.Error()})
			res.Changes = append(res.Changes, changes()...)
			if s.errorMode == ottl.PropagateError {
				for j := i + 1; j < len(s.statements); j++ {
					g.Statements[j].NotEvaluated++
				}
				return fmt.Errorf("failed to execute statement: %v, %w", st.String(), err)
			}
			continue
		}
		if !matched {
			res.NotMatched++
			continue
		}
		res.Matched++
		if c := changes(); len(c) > 0 {
			res.Changes = append(res.Changes, c...)
		} else {
			res.Unchanged++
		}
	}
	return nil
}
