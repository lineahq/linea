// Package stampprocessor is a custom processor for the runtime build tests:
// it is no Collector component, so a runtime has it only when its
// manifest names it with a source override. It sets the log record
// attribute "stamp" to the configured value.
package stampprocessor

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
)

// Config is the processor's configuration.
type Config struct {
	Value string `mapstructure:"value"`
}

// NewFactory returns the factory of the "stamp" processor.
func NewFactory() processor.Factory {
	return processor.NewFactory(component.MustNewType("stamp"),
		func() component.Config { return &Config{Value: "stamped"} },
		processor.WithLogs(createLogs, component.StabilityLevelDevelopment))
}

func createLogs(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
	value := cfg.(*Config).Value
	return processorhelper.NewLogs(ctx, set, cfg, next, func(_ context.Context, ld plog.Logs) (plog.Logs, error) {
		rls := ld.ResourceLogs()
		for i := 0; i < rls.Len(); i++ {
			sls := rls.At(i).ScopeLogs()
			for j := 0; j < sls.Len(); j++ {
				lrs := sls.At(j).LogRecords()
				for k := 0; k < lrs.Len(); k++ {
					lrs.At(k).Attributes().PutStr("stamp", value)
				}
			}
		}
		return ld, nil
	}, processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true}))
}
