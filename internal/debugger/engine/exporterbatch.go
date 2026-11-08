package engine

import (
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/recommend"
)

// exporterBatches lists the exporters that batch in their own sending
// queue: exporterhelper's sending_queue with a batch section. Verified
// against exporterhelper v0.162.0 and v0.165.0: NewDefaultQueueConfig
// leaves the batch section a configoptional.Default, so batching is on only
// when the configuration sets sending_queue::batch or the alpha gate
// pkg.exporterhelper.queueBatchEnabled is on (then the default is Some);
// the resolved configuration reflects both, and a disabled queue or batch
// marshals without the section.
func exporterBatches(resolved *config.Resolved) []recommend.ExporterBatch {
	if resolved == nil || resolved.Config == nil {
		return nil
	}
	var out []recommend.ExporterBatch
	for cid := range resolved.Config.Exporters {
		conf, ok := marshalled(resolved.Config.Exporters, cid.String())
		if !ok || !conf.IsSet("sending_queue::batch") {
			continue
		}
		batch, err := conf.Sub("sending_queue::batch")
		if err != nil || len(batch.AllKeys()) == 0 {
			continue
		}
		timeout, err := durationOf(batch.Get("flush_timeout"))
		if err != nil {
			continue
		}
		e := recommend.ExporterBatch{Exporter: cid.String(), MinSize: intOf(batch.Get("min_size")),
			MaxSize: intOf(batch.Get("max_size")), FlushTimeout: timeout.String(), Sizer: textOf(batch.Get("sizer"))}
		if keys, ok := batch.Get("partition::metadata_keys").([]any); ok {
			for _, k := range keys {
				if v, ok := k.(string); ok {
					e.MetadataKeys = append(e.MetadataKeys, v)
				}
			}
		}
		if e.Sizer == "" {
			e.Sizer = batchSizer(resolved.Config.Exporters[cid], "sending_queue", "batch")
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b recommend.ExporterBatch) int { return strings.Compare(a.Exporter, b.Exporter) })
	return out
}
