package sampling

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/tailsamplingprocessor"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

type testSpan struct {
	trace    byte
	name     string
	ms       int
	error    bool
	tenant   string
	parented bool
}

func traces(spans ...testSpan) telemetry.Payload {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "shop")
	ss := rs.ScopeSpans().AppendEmpty()
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for i, s := range spans {
		sp := ss.Spans().AppendEmpty()
		sp.SetTraceID(pcommon.TraceID{s.trace})
		sp.SetSpanID(pcommon.SpanID{s.trace, byte(i + 1)})
		if s.parented {
			sp.SetParentSpanID(pcommon.SpanID{s.trace, 0xff})
		}
		sp.SetName(s.name)
		sp.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
		sp.SetEndTimestamp(pcommon.NewTimestampFromTime(start.Add(time.Duration(s.ms) * time.Millisecond)))
		if s.error {
			sp.Status().SetCode(ptrace.StatusCodeError)
		}
		if s.tenant != "" {
			sp.Attributes().PutStr("tenant", s.tenant)
		}
	}
	return telemetry.FromTraces(td)
}

func config(t *testing.T, policies ...tailsamplingprocessor.PolicyCfg) *tailsamplingprocessor.Config {
	t.Helper()
	cfg := tailsamplingprocessor.NewFactory().CreateDefaultConfig().(*tailsamplingprocessor.Config)
	cfg.PolicyCfgs = policies
	return cfg
}

func policyCfg(name string, typ tailsamplingprocessor.PolicyType, set func(*tailsamplingprocessor.PolicyCfg)) tailsamplingprocessor.PolicyCfg {
	var p tailsamplingprocessor.PolicyCfg
	p.Name, p.Type = name, typ
	if set != nil {
		set(&p)
	}
	return p
}

func traceID(b byte) string { return pcommon.TraceID{b}.String() }

func TestTailExplainsEachPolicyOnTheEvaluatedSpans(t *testing.T) {
	cfg := config(t,
		policyCfg("errors", tailsamplingprocessor.StatusCode, func(p *tailsamplingprocessor.PolicyCfg) { p.StatusCodeCfg.StatusCodes = []string{"ERROR"} }),
		policyCfg("slow", tailsamplingprocessor.Latency, func(p *tailsamplingprocessor.PolicyCfg) { p.LatencyCfg.ThresholdMs = 500 }),
		policyCfg("tenants", tailsamplingprocessor.StringAttribute, func(p *tailsamplingprocessor.PolicyCfg) {
			p.StringAttributeCfg.Key, p.StringAttributeCfg.Values = "tenant", []string{"gold"}
		}),
	)
	in := traces(
		testSpan{trace: 1, name: "checkout", ms: 40, error: true},
		testSpan{trace: 2, name: "search", ms: 850, tenant: "free"},
		testSpan{trace: 3, name: "health", ms: 2},
	)
	decisions := []Decision{
		{Seq: 5, TraceID: traceID(1), Outcome: "sampled", Policy: "errors", Spans: traces(testSpan{trace: 1, name: "checkout", ms: 40, error: true})},
		{Seq: 6, TraceID: traceID(2), Outcome: "sampled", Policy: "slow", Spans: traces(testSpan{trace: 2, name: "search", ms: 850, tenant: "free"})},
		// The recorded decision disagrees with the replay.
		{Seq: 7, TraceID: traceID(3), Outcome: "sampled", Policy: "slow", Spans: traces(testSpan{trace: 3, name: "health", ms: 2})},
	}
	got, err := Tail(context.Background(), "tail_sampling", "traces", cfg, []Input{{Seq: 4, Payload: in}}, decisions, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Policies) != 3 || got.Policies[1].Rule != "samples traces lasting more than 500ms (earliest span start to latest span end)" {
		t.Fatalf("policies = %+v", got.Policies)
	}
	if len(got.Traces) != 3 {
		t.Fatalf("traces = %+v", got.Traces)
	}
	search := got.Traces[1]
	if search.Name != "search" || search.Verification != analysis.ReplayVerified || search.Duration != "850ms" {
		t.Fatalf("search = %+v", search)
	}
	want := []analysis.PolicyResult{
		{Policy: "errors", Result: "not_sampled", Evidence: "span statuses: UNSET"},
		{Policy: "slow", Result: "sampled", Evidence: "lasted 850ms"},
		{Policy: "tenants", Result: "not_sampled", Evidence: "tenant: free"},
	}
	for i, w := range want {
		if search.Policies[i] != w {
			t.Errorf("policy %d = %+v, want %+v", i, search.Policies[i], w)
		}
	}
	if got.Traces[0].Policies[0].Result != "sampled" || got.Traces[0].Policies[2].Evidence != "tenant is not set" {
		t.Errorf("checkout = %+v", got.Traces[0].Policies)
	}
	if got.Traces[2].Verification != analysis.ReplayDiffers {
		t.Errorf("health verification = %s, want %s", got.Traces[2].Verification, analysis.ReplayDiffers)
	}
	if strings.Join(got.Traces[1].Records, ",") != "4:resource[0].scope[0].span[1]" {
		t.Errorf("records = %v", got.Traces[1].Records)
	}
}

func TestTailReportsDropPoliciesLateSpansAndPendingTraces(t *testing.T) {
	cfg := config(t,
		policyCfg("no-health", tailsamplingprocessor.Drop, func(p *tailsamplingprocessor.PolicyCfg) {
			var sub tailsamplingprocessor.AndSubPolicyCfg
			sub.Name, sub.Type = "health", tailsamplingprocessor.StringAttribute
			sub.StringAttributeCfg.Key, sub.StringAttributeCfg.Values = "tenant", []string{"probe"}
			p.DropCfg.SubPolicyCfg = []tailsamplingprocessor.AndSubPolicyCfg{sub}
		}),
		policyCfg("all", tailsamplingprocessor.AlwaysSample, nil),
	)
	cfg.DropPendingTracesOnShutdown = true
	probe := testSpan{trace: 1, name: "probe", ms: 1, tenant: "probe"}
	late := testSpan{trace: 1, name: "probe child", ms: 1, tenant: "probe", parented: true}
	inputs := []Input{
		{Seq: 2, Payload: traces(probe)},
		{Seq: 6, Payload: traces(late, testSpan{trace: 2, name: "unfinished", ms: 3})},
	}
	decisions := []Decision{{Seq: 4, TraceID: traceID(1), Outcome: "dropped", Policy: "no-health", Spans: traces(probe)}}
	got, err := Tail(context.Background(), "tail_sampling", "traces", cfg, inputs, decisions, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Policies[0].Rule != "drops traces that every sub-policy matches, whatever other policies decide: health (samples traces where a span or resource attribute tenant is probe)" {
		t.Errorf("drop rule = %q", got.Policies[0].Rule)
	}
	dropped := got.Traces[0]
	if dropped.Verification != analysis.ReplayVerified || dropped.LateSpans != 1 || dropped.Late != "dropped" {
		t.Fatalf("dropped trace = %+v", dropped)
	}
	if dropped.Policies[0] != (analysis.PolicyResult{Policy: "no-health", Result: "dropped", Evidence: "tenant: probe"}) {
		t.Errorf("drop policy = %+v", dropped.Policies[0])
	}
	pending := got.Traces[1]
	if pending.Decision != analysis.TracePendingDropped || pending.Name != "unfinished" || len(pending.Policies) != 0 {
		t.Errorf("pending trace = %+v", pending)
	}
	got, err = Tail(context.Background(), "tail_sampling", "traces", cfg, inputs, decisions, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Traces[1].Decision != analysis.TracePending {
		t.Errorf("an incomplete run's trace is %s, want pending", got.Traces[1].Decision)
	}
}

func TestTailNotesRateDependentPolicies(t *testing.T) {
	cfg := config(t, policyCfg("budget", tailsamplingprocessor.RateLimiting, func(p *tailsamplingprocessor.PolicyCfg) { p.RateLimitingCfg.SpansPerSecond = 10 }))
	got, err := Tail(context.Background(), "tail_sampling", "traces", cfg, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Notes) != 1 || !strings.Contains(got.Notes[0], "evaluates each trace alone") {
		t.Errorf("notes = %v", got.Notes)
	}
}
