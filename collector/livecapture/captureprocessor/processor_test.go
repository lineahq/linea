package captureprocessor

import (
	"context"
	"errors"
	"testing"

	"github.com/lineahq/linea/collector/livecapture/livecaptureextension"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
)

type host map[component.ID]component.Component

// ctxKey keys the value that shows a context passes through unchanged.
type ctxKey struct{}

func (h host) GetExtensions() map[component.ID]component.Component { return h }

func processorSetup(t testing.TB) (*livecaptureextension.Manager, host) {
	t.Helper()
	factory := livecaptureextension.NewFactory()
	extensionID := component.NewID(component.MustNewType("livecapture"))
	ext, err := factory.Create(context.Background(), extension.Settings{ID: extensionID}, factory.CreateDefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	manager := ext.(*livecaptureextension.Manager)
	h := host{extensionID: manager}
	if err := manager.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return manager, h
}

func TestPassThroughAllSignalsAndDownstreamErrors(t *testing.T) {
	manager, h := processorSetup(t)
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig()
	set := processor.Settings{ID: component.NewIDWithName(component.MustNewType("capture"), "ingress")}
	downstream := errors.New("backend rejected original")
	traces := ptrace.NewTraces()
	logs := plog.NewLogs()
	metrics := pmetric.NewMetrics()
	ctx := context.WithValue(context.Background(), ctxKey{}, "preserved")
	tc, _ := consumer.NewTraces(func(got context.Context, data ptrace.Traces) error {
		if got != ctx || data != traces {
			t.Fatal("original traces/context replaced")
		}
		return downstream
	})
	tp, err := factory.CreateTraces(ctx, set, cfg, tc)
	if err != nil {
		t.Fatal(err)
	}
	if err := tp.Start(ctx, h); err != nil {
		t.Fatal(err)
	}
	lc, _ := consumer.NewLogs(func(got context.Context, data plog.Logs) error {
		if got != ctx || data != logs {
			t.Fatal("original logs/context replaced")
		}
		return downstream
	})
	lp, err := factory.CreateLogs(ctx, set, cfg, lc)
	if err != nil {
		t.Fatal(err)
	}
	if err := lp.Start(ctx, h); err != nil {
		t.Fatal(err)
	}
	mc, _ := consumer.NewMetrics(func(got context.Context, data pmetric.Metrics) error {
		if got != ctx || data != metrics {
			t.Fatal("original metrics/context replaced")
		}
		return downstream
	})
	mp, err := factory.CreateMetrics(ctx, set, cfg, mc)
	if err != nil {
		t.Fatal(err)
	}
	if err := mp.Start(ctx, h); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(tp.ConsumeTraces(ctx, traces), downstream) || !errors.Is(lp.ConsumeLogs(ctx, logs), downstream) || !errors.Is(mp.ConsumeMetrics(ctx, metrics), downstream) {
		t.Fatal("downstream error changed")
	}
	if tp.Capabilities().MutatesData || lp.Capabilities().MutatesData || mp.Capabilities().MutatesData {
		t.Fatal("processor claims mutation")
	}
	if len(manager.CapturePoints()) != 3 {
		t.Fatal("signals did not share extension manager")
	}
}

func TestInactivePathAllocatesNothing(t *testing.T) {
	_, h := processorSetup(t)
	next, _ := consumer.NewTraces(func(context.Context, ptrace.Traces) error { return nil })
	factory := NewFactory()
	p, err := factory.CreateTraces(context.Background(), processor.Settings{ID: component.NewIDWithName(component.MustNewType("capture"), "ingress")}, factory.CreateDefaultConfig(), next)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	data := ptrace.NewTraces()
	allocations := testing.AllocsPerRun(1000, func() {
		if err := p.ConsumeTraces(context.Background(), data); err != nil {
			t.Fatal(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("inactive allocations = %f", allocations)
	}
}

func TestPipelineBindingAndMissingExtension(t *testing.T) {
	factory := NewFactory()
	next, _ := consumer.NewTraces(func(context.Context, ptrace.Traces) error { return nil })
	set := processor.Settings{ID: component.NewIDWithName(component.MustNewType("capture"), "ingress")}
	cfg := factory.CreateDefaultConfig().(*Config)
	cfg.Pipeline = "logs/app"
	if _, err := factory.CreateTraces(context.Background(), set, cfg, next); err == nil {
		t.Fatal("wrong signal pipeline accepted")
	}
	cfg.Pipeline = "traces/app"
	p, err := factory.CreateTraces(context.Background(), set, cfg, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background(), host{}); err == nil {
		t.Fatal("missing extension accepted")
	}
	_, h := processorSetup(t)
	if err := p.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	other, _ := factory.CreateTraces(context.Background(), set, cfg, next)
	if err := other.Start(context.Background(), h); err == nil {
		t.Fatal("ambiguous shared point accepted")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkInactive(b *testing.B) {
	_, h := processorSetup(b)
	factory := NewFactory()
	next, _ := consumer.NewTraces(func(context.Context, ptrace.Traces) error { return nil })
	p, err := factory.CreateTraces(context.Background(), processor.Settings{ID: component.NewIDWithName(component.MustNewType("capture"), "ingress")}, factory.CreateDefaultConfig(), next)
	if err != nil {
		b.Fatal(err)
	}
	if err := p.Start(context.Background(), h); err != nil {
		b.Fatal(err)
	}
	data := ptrace.NewTraces()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = p.ConsumeTraces(context.Background(), data)
	}
}
