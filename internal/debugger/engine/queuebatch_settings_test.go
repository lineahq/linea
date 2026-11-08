package engine

import (
	"reflect"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/otelcol"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/config"
)

// TestQueueBatchSettingsReadsTheBatchSection: queue_batch's configuration
// is exporterhelper's QueueBatchConfig with batching on by default
// (queuebatchprocessor v0.165.0 createDefaultConfig); its batch section is
// read in the batch processor's terms, and without it every request is
// sent as it is.
func TestQueueBatchSettingsReadsTheBatchSection(t *testing.T) {
	for _, tt := range []struct {
		name    string
		conf    map[string]any
		want    analysis.BatchSettings
		timeout time.Duration
	}{
		{
			name:    "defaults",
			want:    analysis.BatchSettings{Type: analysis.TypeQueueBatch, SendBatchSize: 8192, Timeout: "200ms", Sizer: "items"},
			timeout: 200 * time.Millisecond,
		},
		{
			name: "configured",
			conf: map[string]any{"batch": map[string]any{
				"min_size": 100, "max_size": 500, "flush_timeout": "1s", "sizer": "bytes",
				"partition": map[string]any{"metadata_keys": []any{"X-Tenant"}},
			}},
			want:    analysis.BatchSettings{Type: analysis.TypeQueueBatch, SendBatchSize: 100, SendBatchMaxSize: 500, Timeout: "1s", Sizer: "bytes", MetadataKeys: []string{"X-Tenant"}},
			timeout: time.Second,
		},
		{
			name: "batching off",
			conf: map[string]any{"batch": map[string]any{"enabled": false}},
			want: analysis.BatchSettings{Type: analysis.TypeQueueBatch, Timeout: "0s", Unbatched: true},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// As the processor's factory builds it.
			cfg := exporterhelper.NewDefaultQueueConfig()
			cfg.BlockOnOverflow, cfg.NumConsumers, cfg.QueueSize = true, 1, 10
			_ = cfg.Batch.GetOrInsertDefault()
			if err := confmap.NewFromStringMap(tt.conf).Unmarshal(&cfg); err != nil {
				t.Fatal(err)
			}
			got, timeout, err := queueBatchSettings(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			// cache_size has a default from 0.165 on; the root module may
			// predate it.
			got.CacheSize = 0
			if !reflect.DeepEqual(got, tt.want) || timeout != tt.timeout {
				t.Fatalf("settings = %+v, %s; want %+v, %s", got, timeout, tt.want, tt.timeout)
			}
		})
	}
}

// TestQueueBatchContextLossFollowsItsBatchSection: queue_batch passes on
// only its batch::partition::metadata_keys when it joins requests, and
// without a batch section it sends every request with its own context.
func TestQueueBatchContextLossFollowsItsBatchSection(t *testing.T) {
	for _, tt := range []struct {
		name   string
		conf   map[string]any
		merges bool
		keys   []string
	}{
		{"defaults", nil, true, nil},
		{"partitioned", map[string]any{"batch": map[string]any{"partition": map[string]any{"metadata_keys": []any{"X-Tenant"}}}}, true, []string{"x-tenant"}},
		{"batching off", map[string]any{"batch": map[string]any{"enabled": false}}, false, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := exporterhelper.NewDefaultQueueConfig()
			cfg.BlockOnOverflow, cfg.NumConsumers, cfg.QueueSize = true, 1, 10
			_ = cfg.Batch.GetOrInsertDefault()
			if err := confmap.NewFromStringMap(tt.conf).Unmarshal(&cfg); err != nil {
				t.Fatal(err)
			}
			resolved := &config.Resolved{Config: &otelcol.Config{Processors: map[component.ID]component.Config{
				component.MustNewID("queue_batch"): &cfg,
			}}}
			if got := merges(resolved, "queue_batch"); got != tt.merges {
				t.Fatalf("merges = %v, want %v", got, tt.merges)
			}
			if got := batchMetadataKeys(resolved, "queue_batch"); !reflect.DeepEqual(got, tt.keys) {
				t.Fatalf("keys = %q, want %q", got, tt.keys)
			}
		})
	}
}
