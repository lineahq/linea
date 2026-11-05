// Package captureprocessor provides synchronous-copy, pass-through capture points.
package captureprocessor

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"

	"github.com/lineahq/linea/collector/livecapture/livecaptureextension"
)

type Config struct {
	Extension   component.ID `mapstructure:"extension"`
	Pipeline    string       `mapstructure:"pipeline"`
	Sensitivity string       `mapstructure:"sensitivity"`
}

func (c *Config) Validate() error {
	if c.Extension.Type() != component.MustNewType("livecapture") {
		return fmt.Errorf("extension must name a livecapture extension")
	}
	if c.Sensitivity != "raw" && c.Sensitivity != "redacted" {
		return fmt.Errorf("sensitivity must be raw or redacted")
	}
	return nil
}

func NewFactory() processor.Factory {
	return processor.NewFactory(component.MustNewType("capture"), func() component.Config {
		return &Config{Extension: component.NewID(component.MustNewType("livecapture")), Sensitivity: "raw"}
	},
		processor.WithTraces(func(_ context.Context, set processor.Settings, cfg component.Config, next consumer.Traces) (processor.Traces, error) {
			p, err := newProcessor(set, cfg.(*Config), livecaptureextension.Traces)
			if err != nil {
				return nil, err
			}
			p.nextTraces = next
			return p, nil
		}, component.StabilityLevelAlpha),
		processor.WithMetrics(func(_ context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
			p, err := newProcessor(set, cfg.(*Config), livecaptureextension.Metrics)
			if err != nil {
				return nil, err
			}
			p.nextMetrics = next
			return p, nil
		}, component.StabilityLevelAlpha),
		processor.WithLogs(func(_ context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
			p, err := newProcessor(set, cfg.(*Config), livecaptureextension.Logs)
			if err != nil {
				return nil, err
			}
			p.nextLogs = next
			return p, nil
		}, component.StabilityLevelAlpha))
}

type captureProcessor struct {
	config      Config
	point       livecaptureextension.CapturePoint
	manager     *livecaptureextension.Manager
	nextTraces  consumer.Traces
	nextMetrics consumer.Metrics
	nextLogs    consumer.Logs
}

func newProcessor(settings processor.Settings, config *Config, signal livecaptureextension.Signal) (*captureProcessor, error) {
	if settings.ID.Name() == "" {
		return nil, fmt.Errorf("capture processor must be named, e.g. capture/ingress")
	}
	pipeline := config.Pipeline
	if pipeline == "" {
		pipeline = string(signal)
	}
	if strings.SplitN(pipeline, "/", 2)[0] != string(signal) {
		return nil, fmt.Errorf("pipeline %q does not match signal %s", pipeline, signal)
	}
	return &captureProcessor{config: *config, point: livecaptureextension.CapturePoint{Point: livecaptureextension.Point{Pipeline: pipeline, Point: settings.ID.Name()}, Signal: signal, Sensitivity: config.Sensitivity}}, nil
}

func (p *captureProcessor) Start(_ context.Context, host component.Host) error {
	extension, exists := host.GetExtensions()[p.config.Extension]
	if !exists {
		return fmt.Errorf("extension %s is not enabled", p.config.Extension)
	}
	manager, ok := extension.(*livecaptureextension.Manager)
	if !ok {
		return fmt.Errorf("extension %s does not implement live capture", p.config.Extension)
	}
	if err := manager.Register(p.point); err != nil {
		return err
	}
	p.manager = manager
	return nil
}
func (p *captureProcessor) Shutdown(context.Context) error {
	if p.manager != nil {
		p.manager.Unregister(p.point)
	}
	return nil
}
func (p *captureProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}
func (p *captureProcessor) ConsumeTraces(ctx context.Context, data ptrace.Traces) error {
	p.manager.CaptureTraces(ctx, p.point.Point, data)
	return p.nextTraces.ConsumeTraces(ctx, data)
}
func (p *captureProcessor) ConsumeMetrics(ctx context.Context, data pmetric.Metrics) error {
	p.manager.CaptureMetrics(ctx, p.point.Point, data)
	return p.nextMetrics.ConsumeMetrics(ctx, data)
}
func (p *captureProcessor) ConsumeLogs(ctx context.Context, data plog.Logs) error {
	p.manager.CaptureLogs(ctx, p.point.Point, data)
	return p.nextLogs.ConsumeLogs(ctx, data)
}
