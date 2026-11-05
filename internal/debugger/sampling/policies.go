package sampling

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/tailsamplingprocessor"
	"go.opentelemetry.io/collector/pdata/pcommon"
)

// policy is a policy configuration of any nesting level. The processor's
// shared policy fields are promoted from an unexported struct, so each
// level is copied into this one.
type policy struct {
	name    string
	typ     tailsamplingprocessor.PolicyType
	latency tailsamplingprocessor.LatencyCfg
	numeric tailsamplingprocessor.NumericAttributeCfg
	prob    tailsamplingprocessor.ProbabilisticCfg
	status  tailsamplingprocessor.StatusCodeCfg
	str     tailsamplingprocessor.StringAttributeCfg
	rate    tailsamplingprocessor.RateLimitingCfg
	bytes   tailsamplingprocessor.BytesLimitingCfg
	count   tailsamplingprocessor.SpanCountCfg
	state   tailsamplingprocessor.TraceStateCfg
	boolean tailsamplingprocessor.BooleanAttributeCfg
	ottl    tailsamplingprocessor.OTTLConditionCfg
	// subs are the sub-policies of and, composite, not and drop.
	subs []policy
	// maxSpans is a composite policy's max_total_spans_per_second.
	maxSpans int64
}

func fromPolicy(p tailsamplingprocessor.PolicyCfg) policy {
	l := policy{name: p.Name, typ: p.Type, latency: p.LatencyCfg, numeric: p.NumericAttributeCfg, prob: p.ProbabilisticCfg,
		status: p.StatusCodeCfg, str: p.StringAttributeCfg, rate: p.RateLimitingCfg, bytes: p.BytesLimitingCfg,
		count: p.SpanCountCfg, state: p.TraceStateCfg, boolean: p.BooleanAttributeCfg, ottl: p.OTTLConditionCfg}
	switch p.Type {
	case tailsamplingprocessor.And:
		for _, s := range p.AndCfg.SubPolicyCfg {
			l.subs = append(l.subs, fromAnd(s))
		}
	case tailsamplingprocessor.Drop:
		for _, s := range p.DropCfg.SubPolicyCfg {
			l.subs = append(l.subs, fromAnd(s))
		}
	case tailsamplingprocessor.Composite:
		l.maxSpans = p.CompositeCfg.MaxTotalSpansPerSecond
		for _, s := range p.CompositeCfg.SubPolicyCfg {
			l.subs = append(l.subs, fromComposite(s))
		}
	case tailsamplingprocessor.Not:
		l.subs = append(l.subs, fromNot(p.NotCfg.SubPolicy))
	}
	return l
}

func fromAnd(p tailsamplingprocessor.AndSubPolicyCfg) policy {
	l := policy{name: p.Name, typ: p.Type, latency: p.LatencyCfg, numeric: p.NumericAttributeCfg, prob: p.ProbabilisticCfg,
		status: p.StatusCodeCfg, str: p.StringAttributeCfg, rate: p.RateLimitingCfg, bytes: p.BytesLimitingCfg,
		count: p.SpanCountCfg, state: p.TraceStateCfg, boolean: p.BooleanAttributeCfg, ottl: p.OTTLConditionCfg}
	if p.Type == tailsamplingprocessor.Not {
		l.subs = append(l.subs, fromNot(p.NotCfg.SubPolicy))
	}
	return l
}

func fromComposite(p tailsamplingprocessor.CompositeSubPolicyCfg) policy {
	l := policy{name: p.Name, typ: p.Type, latency: p.LatencyCfg, numeric: p.NumericAttributeCfg, prob: p.ProbabilisticCfg,
		status: p.StatusCodeCfg, str: p.StringAttributeCfg, rate: p.RateLimitingCfg, bytes: p.BytesLimitingCfg,
		count: p.SpanCountCfg, state: p.TraceStateCfg, boolean: p.BooleanAttributeCfg, ottl: p.OTTLConditionCfg}
	if p.Type == tailsamplingprocessor.And {
		for _, s := range p.AndCfg.SubPolicyCfg {
			l.subs = append(l.subs, fromAnd(s))
		}
	}
	return l
}

func fromNot(p tailsamplingprocessor.NotSubPolicyCfg) policy {
	return policy{name: p.Name, typ: p.Type, latency: p.LatencyCfg, numeric: p.NumericAttributeCfg, prob: p.ProbabilisticCfg,
		status: p.StatusCodeCfg, str: p.StringAttributeCfg, rate: p.RateLimitingCfg, bytes: p.BytesLimitingCfg,
		count: p.SpanCountCfg, state: p.TraceStateCfg, boolean: p.BooleanAttributeCfg, ottl: p.OTTLConditionCfg}
}

// rateDependent says whether the policy's decision depends on other
// traces (rate or bytes limits, composite budgets).
func (p policy) rateDependent() bool {
	switch p.typ {
	case tailsamplingprocessor.RateLimiting, tailsamplingprocessor.BytesLimiting, tailsamplingprocessor.Composite:
		return true
	}
	return slices.ContainsFunc(p.subs, policy.rateDependent)
}

// rule says in words what the policy samples, following the evaluators of
// contrib v0.162.0 (internal/debugger/sampling).
func (p policy) rule() string {
	switch p.typ {
	case tailsamplingprocessor.AlwaysSample:
		return "samples every trace"
	case tailsamplingprocessor.Latency:
		r := fmt.Sprintf("samples traces lasting more than %dms", p.latency.ThresholdMs)
		if p.latency.UpperThresholdMs > 0 {
			r += fmt.Sprintf(" and at most %dms", p.latency.UpperThresholdMs)
		}
		return r + " (earliest span start to latest span end)"
	case tailsamplingprocessor.StatusCode:
		return "samples traces with a span whose status is " + strings.Join(p.status.StatusCodes, " or ")
	case tailsamplingprocessor.StringAttribute:
		verb := "is"
		if p.str.EnabledRegexMatching {
			verb = "matches"
		}
		r := fmt.Sprintf("samples traces where a span or resource attribute %s %s %s", p.str.Key, verb, oneOf(p.str.Values))
		if p.str.InvertMatch {
			r = fmt.Sprintf("samples traces unless a span or resource attribute %s %s %s", p.str.Key, verb, oneOf(p.str.Values))
		}
		return r
	case tailsamplingprocessor.NumericAttribute:
		r := fmt.Sprintf("samples traces where a span or resource attribute %s is between %d and %d", p.numeric.Key, p.numeric.MinValue, p.numeric.MaxValue)
		if p.numeric.InvertMatch {
			r = fmt.Sprintf("samples traces unless a span or resource attribute %s is between %d and %d", p.numeric.Key, p.numeric.MinValue, p.numeric.MaxValue)
		}
		return r
	case tailsamplingprocessor.BooleanAttribute:
		r := fmt.Sprintf("samples traces where a span or resource attribute %s is %t", p.boolean.Key, p.boolean.Value)
		if p.boolean.InvertMatch {
			r = fmt.Sprintf("samples traces unless a span or resource attribute %s is %t", p.boolean.Key, p.boolean.Value)
		}
		return r
	case tailsamplingprocessor.Probabilistic:
		return fmt.Sprintf("samples %s%% of traces, chosen by a hash of the trace ID", strconv.FormatFloat(p.prob.SamplingPercentage, 'f', -1, 64))
	case tailsamplingprocessor.RateLimiting:
		return fmt.Sprintf("samples traces while fewer than %d spans per second were sampled", p.rate.SpansPerSecond)
	case tailsamplingprocessor.BytesLimiting:
		return fmt.Sprintf("samples traces while fewer than %d bytes per second were sampled", p.bytes.BytesPerSecond)
	case tailsamplingprocessor.SpanCount:
		r := fmt.Sprintf("samples traces with at least %d spans", p.count.MinSpans)
		if p.count.MaxSpans > 0 {
			r += fmt.Sprintf(" and at most %d", p.count.MaxSpans)
		}
		return r
	case tailsamplingprocessor.TraceState:
		return fmt.Sprintf("samples traces where a span's trace state %s is %s", p.state.Key, oneOf(p.state.Values))
	case tailsamplingprocessor.TraceFlags:
		return "samples traces with a span whose sampled trace flag is set"
	case tailsamplingprocessor.OTTLCondition:
		conds := append(slices.Clone(p.ottl.SpanConditions), p.ottl.SpanEventConditions...)
		return "samples traces where a span or span event matches " + strings.Join(conds, " or ")
	case tailsamplingprocessor.And:
		return "samples traces that every sub-policy samples: " + p.subRules()
	case tailsamplingprocessor.Drop:
		return "drops traces that every sub-policy matches, whatever other policies decide: " + p.subRules()
	case tailsamplingprocessor.Not:
		return "samples traces that the sub-policy does not sample: " + p.subRules()
	case tailsamplingprocessor.Composite:
		return fmt.Sprintf("samples by sub-policy in order, within %d spans per second in total: %s", p.maxSpans, p.subRules())
	}
	return string(p.typ)
}

func (p policy) subRules() string {
	rules := make([]string, len(p.subs))
	for i, s := range p.subs {
		rules[i] = fmt.Sprintf("%s (%s)", s.name, s.rule())
	}
	return strings.Join(rules, "; ")
}

func oneOf(values []string) string {
	if len(values) == 1 {
		return values[0]
	}
	return "one of " + strings.Join(values, ", ")
}

// evidence is what the policy looks at in a trace's evaluated spans.
func (p policy) evidence(spans []span) string {
	switch p.typ {
	case tailsamplingprocessor.Latency:
		return "lasted " + duration(spans)
	case tailsamplingprocessor.StatusCode:
		return "span statuses: " + statuses(spans)
	case tailsamplingprocessor.StringAttribute:
		return attribute(spans, p.str.Key)
	case tailsamplingprocessor.NumericAttribute:
		return attribute(spans, p.numeric.Key)
	case tailsamplingprocessor.BooleanAttribute:
		return attribute(spans, p.boolean.Key)
	case tailsamplingprocessor.SpanCount, tailsamplingprocessor.RateLimiting:
		return fmt.Sprintf("%d spans", len(spans))
	case tailsamplingprocessor.TraceState:
		return traceState(spans, p.state.Key)
	case tailsamplingprocessor.TraceFlags:
		n := 0
		for _, s := range spans {
			if s.span.Flags()&1 != 0 {
				n++
			}
		}
		return fmt.Sprintf("sampled flag set on %d of %d spans", n, len(spans))
	case tailsamplingprocessor.And, tailsamplingprocessor.Drop, tailsamplingprocessor.Not, tailsamplingprocessor.Composite:
		var parts []string
		for _, s := range p.subs {
			if e := s.evidence(spans); e != "" && !slices.Contains(parts, e) {
				parts = append(parts, e)
			}
		}
		return strings.Join(parts, "; ")
	}
	return ""
}

// statuses counts span status codes, most frequent first.
func statuses(spans []span) string {
	counts := map[string]int{}
	var codes []string
	for _, s := range spans {
		c := strings.ToUpper(s.span.Status().Code().String())
		if counts[c] == 0 {
			codes = append(codes, c)
		}
		counts[c]++
	}
	return counted(codes, counts)
}

// attribute lists the values of key on the spans and their resources.
func attribute(spans []span, key string) string {
	counts := map[string]int{}
	var values []string
	seenResource := map[pcommon.Resource]bool{}
	add := func(m pcommon.Map) {
		if v, ok := m.Get(key); ok {
			s := v.AsString()
			if counts[s] == 0 {
				values = append(values, s)
			}
			counts[s]++
		}
	}
	for _, s := range spans {
		add(s.span.Attributes())
		if !seenResource[s.resource] {
			seenResource[s.resource] = true
			add(s.resource.Attributes())
		}
	}
	if len(values) == 0 {
		return key + " is not set"
	}
	return key + ": " + counted(values, counts)
}

func traceState(spans []span, key string) string {
	counts := map[string]int{}
	var values []string
	for _, s := range spans {
		for _, kv := range strings.Split(s.span.TraceState().AsRaw(), ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if ok && k == key {
				if counts[v] == 0 {
					values = append(values, v)
				}
				counts[v]++
			}
		}
	}
	if len(values) == 0 {
		return "trace state " + key + " is not set"
	}
	return "trace state " + key + ": " + counted(values, counts)
}

// counted renders values with their counts, most frequent first, ties in
// order of appearance.
func counted(values []string, counts map[string]int) string {
	slices.SortStableFunc(values, func(a, b string) int { return counts[b] - counts[a] })
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = v
		if counts[v] > 1 {
			parts[i] = fmt.Sprintf("%s ×%d", v, counts[v])
		}
	}
	return strings.Join(parts, ", ")
}
