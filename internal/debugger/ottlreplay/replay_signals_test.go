package ottlreplay

import (
	"context"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// logsFixture is one resource with three log records.
func logsFixture() telemetry.Payload {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "checkout")
	sl := rl.ScopeLogs().AppendEmpty()
	for _, r := range []struct {
		severity, body string
		status         int64
		password       bool
	}{
		{"INFO", "order 42 accepted", 200, true},
		{"ERROR", "payment failed", 500, false},
		{"WARN", "retrying", 0, false},
	} {
		lr := sl.LogRecords().AppendEmpty()
		lr.SetSeverityText(r.severity)
		lr.Body().SetStr(r.body)
		if r.status != 0 {
			lr.Attributes().PutInt("http.status_code", r.status)
		}
		if r.password {
			lr.Attributes().PutStr("user.password", "hunter2")
		}
	}
	return telemetry.FromLogs(ld)
}

// metricsFixture has one metric of each data point type the processor
// iterates: a sum with two points (the first with an exemplar), a gauge, a
// histogram with an exemplar and a summary.
func metricsFixture() telemetry.Payload {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "api")
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("meter")

	sum := sm.Metrics().AppendEmpty()
	sum.SetName("requests")
	s := sum.SetEmptySum()
	s.SetIsMonotonic(true)
	s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	for i, route := range []string{"/a", "/b"} {
		dp := s.DataPoints().AppendEmpty()
		dp.Attributes().PutStr("route", route)
		dp.SetIntValue(int64(5 + i))
		if i == 0 {
			dp.Exemplars().AppendEmpty().SetIntValue(1)
		}
	}

	gauge := sm.Metrics().AppendEmpty()
	gauge.SetName("latency")
	gdp := gauge.SetEmptyGauge().DataPoints().AppendEmpty()
	gdp.Attributes().PutStr("route", "/a")
	gdp.SetDoubleValue(0.25)

	hist := sm.Metrics().AppendEmpty()
	hist.SetName("sizes")
	hdp := hist.SetEmptyHistogram().DataPoints().AppendEmpty()
	hdp.SetCount(3)
	hdp.ExplicitBounds().FromRaw([]float64{10})
	hdp.BucketCounts().FromRaw([]uint64{1, 2})
	hdp.Exemplars().AppendEmpty().SetDoubleValue(12)

	summary := sm.Metrics().AppendEmpty()
	summary.SetName("quantiles")
	qdp := summary.SetEmptySummary().DataPoints().AppendEmpty()
	qdp.SetCount(2)
	qdp.SetSum(4)
	return telemetry.FromMetrics(md)
}

// expectation is one statement's expected aggregate and first change.
type expectation struct {
	name                     string
	got                      Statement
	matched, notMatched      int
	unchanged, errors        int
	changeEntity, changePath string
	changeOp                 analysis.Op
}

func (e expectation) check(t *testing.T) {
	t.Helper()
	s := e.got
	if s.Matched != e.matched || s.NotMatched != e.notMatched || s.Unchanged != e.unchanged || len(s.Errors) != e.errors {
		t.Fatalf("stats = %+v", s)
	}
	if e.changePath == "" {
		if len(s.Changes) != 0 {
			t.Fatalf("unexpected changes %+v", s.Changes)
		}
		return
	}
	if len(s.Changes) == 0 {
		t.Fatal("no changes")
	}
	if c := s.Changes[0]; c.Entity != e.changeEntity || c.Path != e.changePath || c.Op != e.changeOp {
		t.Fatalf("first change = %+v", c)
	}
}

func replayVerified(t *testing.T, block string, in telemetry.Payload) *Report {
	t.Helper()
	cfg := transformConfig(t, block)
	out := runProcessor(t, cfg, in)
	rep, err := Replay(context.Background(), "transform", cfg, 2, in, 3, out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verification != VerificationReplayVerified {
		t.Fatalf("verification = %s, notes %v, mismatch %+v, groups %+v", rep.Verification, rep.Notes, rep.Mismatch, rep.Groups)
	}
	for _, g := range rep.Groups {
		if g.Status != StatusReplayed {
			t.Fatalf("group %d = %s: %s", g.Index, g.Status, g.Reason)
		}
	}
	return rep
}

func TestReplayExplainsLogStatements(t *testing.T) {
	rep := replayVerified(t, `    error_mode: ignore
    log_statements:
      - context: resource
        statements:
          - set(attributes["deployment.environment"], "test")
      - context: log
        statements:
          - set(attributes["level"], severity_text)
          - delete_key(attributes, "user.password")
          - set(body, Concat(["failed:", body], " ")) where severity_text == "ERROR"
          - merge_maps(attributes, attributes["http.status_code"], "insert") where attributes["http.status_code"] != nil
`, logsFixture())
	if rep.Signal != "logs" || rep.Groups[1].Context != "log" {
		t.Fatalf("signal %s, groups %+v", rep.Signal, rep.Groups)
	}
	log := rep.Groups[1].Statements
	for _, e := range []expectation{
		{"resource set", rep.Groups[0].Statements[0], 1, 0, 0, 0, "resource[0]", `attributes["deployment.environment"]`, analysis.OpAdded},
		{"set from a field", log[0], 3, 0, 0, 0, "resource[0].scope[0].log_record[0]", `attributes["level"]`, analysis.OpAdded},
		{"delete_key", log[1], 3, 0, 2, 0, "resource[0].scope[0].log_record[0]", `attributes["user.password"]`, analysis.OpRemoved},
		{"conditional body", log[2], 1, 2, 0, 0, "resource[0].scope[0].log_record[1]", "body", analysis.OpModified},
		{"error ignored", log[3], 2, 1, 0, 2, "", "", ""},
	} {
		t.Run(e.name, e.check)
	}
	if err := log[3].Errors[0]; err.Entity != "resource[0].scope[0].log_record[0]" || err.Phase != "function" {
		t.Errorf("error = %+v", err)
	}
}

func TestReplayInfersLogContext(t *testing.T) {
	rep := replayVerified(t, "    log_statements: ['set(log.attributes[\"a\"], \"b\")']\n", logsFixture())
	if g := rep.Groups[0]; g.Context != "log" || g.Statements[0].Matched != 3 {
		t.Fatalf("group = %+v", g)
	}
}

func TestReplayExplainsMetricStatements(t *testing.T) {
	rep := replayVerified(t, `    error_mode: ignore
    metric_statements:
      - context: metric
        statements:
          - set(metric.description, "total requests") where metric.name == "requests"
          - copy_metric(name="requests.copy") where metric.name == "requests"
      - context: datapoint
        statements:
          - set(datapoint.attributes["env"], "prod")
      - context: exemplar
        statements:
          - set(exemplar.filtered_attributes["sampled"], true)
`, metricsFixture())
	metric := rep.Groups[0].Statements
	for _, e := range []expectation{
		// The processor iterates metrics by index, so the appended copy is
		// visited too: five metrics, one match.
		{"metric field", metric[0], 1, 4, 0, 0, "resource[0].scope[0].metric[0]", "metric.description", analysis.OpModified},
		{"copy_metric appends", metric[1], 1, 4, 0, 0, "resource[0].scope[0].metric[4]", "metric", analysis.OpAdded},
		// requests 2, latency 1, sizes 1, quantiles 1, the copy 2.
		{"every data point", rep.Groups[1].Statements[0], 7, 0, 0, 0, "resource[0].scope[0].metric[0].data_point[0]", `attributes["env"]`, analysis.OpAdded},
		// requests, sizes and the copy carry one exemplar each; the summary
		// has none.
		{"every exemplar", rep.Groups[2].Statements[0], 3, 0, 0, 0, "resource[0].scope[0].metric[0].data_point[0]", "exemplars", analysis.OpModified},
	} {
		t.Run(e.name, e.check)
	}
}

func TestReplayRespectsMetricErrorMode(t *testing.T) {
	// merge_maps fails on the string route attribute of three data points.
	block := func(mode string) string {
		return "    error_mode: " + mode + "\n" +
			"    metric_statements:\n" +
			"      - context: datapoint\n" +
			"        statements:\n" +
			"          - merge_maps(datapoint.attributes, datapoint.attributes[\"route\"], \"insert\") where datapoint.attributes[\"route\"] != nil\n" +
			"          - set(datapoint.attributes[\"after\"], true)\n"
	}
	tests := []struct {
		mode             string
		wantErrors       int
		wantAfterMatched int
		wantRejected     bool
	}{
		{mode: "ignore", wantErrors: 3, wantAfterMatched: 5},
		{mode: "propagate", wantErrors: 1, wantAfterMatched: 0, wantRejected: true},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			cfg := transformConfig(t, block(tt.mode))
			in := metricsFixture()
			out := runProcessor(t, cfg, in)
			if (out == nil) != tt.wantRejected {
				t.Fatalf("processor rejected = %t", out == nil)
			}
			rep, err := Replay(context.Background(), "transform", cfg, 2, in, 3, out)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Verification != VerificationReplayVerified {
				t.Fatalf("verification = %s (%v) %+v", rep.Verification, rep.Notes, rep.Mismatch)
			}
			st := rep.Groups[0].Statements
			if len(st[0].Errors) != tt.wantErrors || st[0].Errors[0].Entity != "resource[0].scope[0].metric[0].data_point[0]" {
				t.Errorf("errors = %+v", st[0].Errors)
			}
			if st[1].Matched != tt.wantAfterMatched {
				t.Errorf("second statement matched %d, want %d", st[1].Matched, tt.wantAfterMatched)
			}
		})
	}
}

func TestReplayReportsLogDisagreement(t *testing.T) {
	cfg := transformConfig(t, "    log_statements: ['set(log.attributes[\"x\"], 1)']\n")
	in := logsFixture()
	unchanged := in.Clone()
	rep, err := Replay(context.Background(), "transform", cfg, 2, in, 3, &unchanged)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verification != VerificationDisagrees || rep.Mismatch == nil ||
		rep.Mismatch.Identity != analysis.IdentityContent || len(rep.Mismatch.Changes) != 3 {
		t.Fatalf("verification = %s, mismatch %+v", rep.Verification, rep.Mismatch)
	}
}

func TestReplayMarksFlattenedLogsUnsupported(t *testing.T) {
	cfg := transformConfig(t, "    log_statements: ['set(log.attributes[\"x\"], 1)']\n")
	// flatten_data needs a feature gate to pass config validation; the
	// replay must refuse it regardless.
	cfg.(*transformprocessor.Config).FlattenData = true
	rep, err := Replay(context.Background(), "transform", cfg, 2, logsFixture(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verification != VerificationNotVerifiable || rep.Groups[0].Status != StatusUnsupported {
		t.Fatalf("report = %+v", rep)
	}
}
