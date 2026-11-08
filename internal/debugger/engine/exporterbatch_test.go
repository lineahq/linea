package engine

import (
	"reflect"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/otelcol"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/recommend"
)

// TestExporterBatchesReadsTheSendingQueue: an exporter batches only with a
// sending_queue::batch section; the default queue and a disabled queue or
// batch do not.
func TestExporterBatchesReadsTheSendingQueue(t *testing.T) {
	for _, tt := range []struct {
		name string
		conf map[string]any
		want []recommend.ExporterBatch
	}{
		{"default queue", nil, nil},
		{"batch section", map[string]any{"sending_queue": map[string]any{"batch": map[string]any{
			"min_size": 100, "flush_timeout": "1s", "partition": map[string]any{"metadata_keys": []any{"tenant"}}}}},
			[]recommend.ExporterBatch{{Exporter: "otlp", MinSize: 100, FlushTimeout: "1s", Sizer: "items", MetadataKeys: []string{"tenant"}}}},
		{"batch off", map[string]any{"sending_queue": map[string]any{"batch": map[string]any{"enabled": false}}}, nil},
		{"queue off", map[string]any{"sending_queue": map[string]any{"enabled": false, "batch": map[string]any{"min_size": 100}}}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := otlpexporter.NewFactory()
			cfg := f.CreateDefaultConfig()
			if err := confmap.NewFromStringMap(tt.conf).Unmarshal(cfg); err != nil {
				t.Fatal(err)
			}
			resolved := &config.Resolved{Config: &otelcol.Config{Exporters: map[component.ID]component.Config{
				component.NewID(f.Type()): cfg,
			}}}
			got := exporterBatches(resolved)
			for i := range got {
				got[i].Exporter = "otlp"
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("exporterBatches = %+v, want %+v", got, tt.want)
			}
		})
	}
}
