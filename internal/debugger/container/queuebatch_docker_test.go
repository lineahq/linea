package container

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/catalog"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// queueBatchConfig is samples/batch-timeline's configuration with
// queue_batch in place of the batch processor, at the same sizes.
const queueBatchConfig = `receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
processors:
  queue_batch:
    batch:
      min_size: 4
      max_size: 6
      flush_timeout: 300ms
exporters:
  debug: {}
service:
  pipelines:
    logs:
      receivers: [otlp]
      processors: [queue_batch]
      exporters: [debug]
`

// TestQueueBatchIsExplained builds the newest release with the queue_batch
// processor (from 0.165.0) and runs samples/batch-timeline's scenario
// through it: the replay's batches and items agree with the processor's own
// otelcol_processor_queuebatch_batch_send_size histogram, and every log
// that arrived is sent.
func TestQueueBatchIsExplained(t *testing.T) {
	d := requireDocker(t)
	table := versions.Embedded()
	version := table.Newest()
	r := table.Release(version)
	res, err := catalog.Compose(r.Catalog, builtInManifest(t), catalog.Request{
		Components: []string{"receivers/otlp", "processors/queue_batch", "exporters/debug"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unknown) > 0 {
		t.Skipf("Collector %s has no %v", version, res.Unknown)
	}
	rt, err := build(t, d, BuildRequest{Name: "test-queue-batch", Manifest: res.Manifest, Source: packedSource(t), Set: &DependencySet{GoMod: r.GoMod, GoSum: r.GoSum}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	w, err := d.Start(ctx, rt.Ref, rt.Name, RunOptions{Memory: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	scenario, err := os.ReadFile(filepath.Join("..", "..", "..", "samples", "batch-timeline", "scenario.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rep := executeOn(ctx, t, w, engine.Request{ConfigYAML: queueBatchConfig, ScenarioYAML: string(scenario),
		ScenarioDir: filepath.Join("..", "..", "..", "samples", "batch-timeline")})
	if rep.Result.Status != "completed" {
		t.Fatalf("status %s: %s", rep.Result.Status, rep.Result.StatusReason)
	}
	if len(rep.Batching) != 1 {
		t.Fatalf("batching = %d entries, want 1", len(rep.Batching))
	}
	b := rep.Batching[0]
	if s := b.Settings; s.Type != analysis.TypeQueueBatch || s.SendBatchSize != 4 || s.SendBatchMaxSize != 6 || s.Timeout != "300ms" || s.Sizer != "items" {
		t.Fatalf("settings = %+v", s)
	}
	arrived, sent := 0, 0
	for _, a := range b.Arrivals {
		arrived += a.Items
	}
	for _, f := range b.Flushes {
		sent += f.Items
		t.Logf("flush seq=%d items=%d trigger=%s split=%v wait=%+v", f.Seq, f.Items, f.Trigger, f.Split, f.Wait)
	}
	if arrived == 0 || sent != arrived {
		t.Fatalf("sent %d of %d items", sent, arrived)
	}
	if b.Verification != analysis.BatchMetricsAgree {
		t.Fatalf("verification = %s (notes %v)", b.Verification, b.Notes)
	}
}
