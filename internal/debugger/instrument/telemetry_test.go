package instrument

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func observedSettings(t *testing.T, rec *Recorder) (component.TelemetrySettings, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	set := componenttest.NewNopTelemetrySettings()
	set.Logger = zap.New(core)
	return rec.observe(observed{kind: ComponentProcessor, component: "filter/drop", pipeline: "logs", signal: "logs"}, set), logs
}

// TestComponentLogsAreEventsInOrder: a component's entries from info up
// are component.log events in order, with their fields as strings, and
// still reach the component's own logger.
func TestComponentLogsAreEventsInOrder(t *testing.T) {
	rec := NewRecorder()
	set, own := observedSettings(t, rec)
	log := set.Logger.With(zap.String("otelcol.component.id", "filter/drop"), zap.String("tenant", "lab"))
	log.Debug("not recorded")
	log.Info("first", zap.Int("count", 3))
	log.Warn("second", zap.Error(errors.New("boom")), zap.Strings("names", []string{"a", "b"}))
	log.Info(strings.Repeat("x", maxLogText+10))

	var got []Event
	for _, e := range rec.Events() {
		if e.Kind == KindComponentLog {
			got = append(got, e)
		}
	}
	if len(got) != 3 {
		t.Fatalf("component.log events %+v", got)
	}
	first, second := got[0], got[1]
	if first.Component != "filter/drop" || first.ComponentKind != ComponentProcessor || first.Pipeline != "logs" ||
		first.Source != SourceComponentTelemetry || first.Log.Level != "info" || first.Log.Message != "first" ||
		first.Log.Fields["count"] != "3" || first.Log.Fields["tenant"] != "lab" || len(first.Log.Fields) != 2 {
		t.Errorf("first %+v %+v", first, first.Log)
	}
	if second.Log.Level != "warn" || second.Log.Fields["error"] != "boom" || second.Log.Fields["names"] != `["a","b"]` {
		t.Errorf("second %+v", second.Log)
	}
	if m := got[2].Log.Message; len(m) > maxLogText+len("…") || !strings.HasSuffix(m, "…") {
		t.Errorf("long message kept %d bytes", len(m))
	}
	if own.Len() != 4 {
		t.Errorf("the component's own logger got %d entries, want 4", own.Len())
	}
}

// TestComponentLogsStopAtTheBudget: log entries are observations, so the
// evidence budget stops them; the run's lifecycle is still recorded.
func TestComponentLogsStopAtTheBudget(t *testing.T) {
	rec := NewRecorder()
	rec.SetBudget(NewBudget(0, 2))
	set, _ := observedSettings(t, rec)
	for range 5 {
		set.Logger.Info("entry")
	}
	rec.Record(Event{Kind: KindRunCompleted, Source: SourceOrchestrator})
	var logs, ends int
	for _, e := range rec.Events() {
		switch e.Kind {
		case KindComponentLog:
			logs++
		case KindRunCompleted:
			ends++
		}
	}
	if logs != 2 || ends != 1 {
		t.Fatalf("%d log events and %d run.completed, want 2 and 1", logs, ends)
	}
}

// TestComponentMetricsAreReadAtTheEnd: each data point of a component's
// own metrics, with its attributes; histograms as count and sum.
func TestComponentMetricsAreReadAtTheEnd(t *testing.T) {
	rec := NewRecorder()
	set, _ := observedSettings(t, rec)
	meter := set.MeterProvider.Meter("test")
	counter, _ := meter.Int64Counter("otelcol_processor_filter_logs.filtered", metric.WithUnit("1"))
	counter.Add(context.Background(), 2, metric.WithAttributes(attribute.String("reason", "match")))
	counter.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", "match")))
	latency, _ := meter.Float64Histogram("latency", metric.WithUnit("ms"))
	latency.Record(context.Background(), 1.5)
	latency.Record(context.Background(), 2.5)

	got := rec.ComponentMetrics(context.Background())
	if len(got) != 2 {
		t.Fatalf("component metrics %+v", got)
	}
	h, c := got[0], got[1]
	if h.Name != "latency" || h.Type != "histogram" || *h.Count != 2 || *h.Sum != 4 || h.Unit != "ms" {
		t.Errorf("histogram %+v", h)
	}
	if c.Component != "filter/drop" || c.ComponentKind != ComponentProcessor || c.Pipeline != "logs" || c.Type != "sum" ||
		c.IntValue == nil || *c.IntValue != 3 || c.Value != nil || c.Attributes["reason"] != "match" {
		t.Errorf("counter %+v", c)
	}
	if again := rec.ComponentMetrics(context.Background()); len(again) != 0 {
		t.Errorf("metrics read twice: %+v", again)
	}
}
