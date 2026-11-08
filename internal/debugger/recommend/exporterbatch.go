package recommend

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/topology"
)

// ExporterBatch is an exporter that batches in its own sending queue
// (exporterhelper's sending_queue::batch). The engine reads it from the
// configuration (exporterBatches in internal/debugger/engine), so static
// recommendations have it too.
type ExporterBatch struct {
	// Exporter is the exporter's configuration ID.
	Exporter     string
	MinSize      int
	MaxSize      int
	FlushTimeout string
	// Sizer is empty when the configuration does not name it.
	Sizer        string
	MetadataKeys []string
}

// batchBeforeExporterBatching: a pipeline batches in a processor and again
// in the sending queue of every exporter it sends to. The exporters regroup
// what the processor sent and wait for their own flush_timeout, so one of
// the two batching steps does nothing the other does not.
func batchBeforeExporterBatching(in Input) []Recommendation {
	if len(in.ExporterBatches) == 0 {
		return nil
	}
	byID := map[string]ExporterBatch{}
	for _, e := range in.ExporterBatches {
		byID[e.Exporter] = e
	}
	prefix := string(topology.KindExporter) + "/"
	var out []Recommendation
	for _, p := range in.Topology.Pipelines {
		var chain []string
		for _, id := range pipelineIDs(p) {
			if batches(in, p.ID, id) {
				chain = append(chain, id)
			}
		}
		if len(chain) == 0 || len(p.Exporters) == 0 {
			continue
		}
		var exps []ExporterBatch
		for _, ref := range p.Exporters {
			e, ok := byID[strings.TrimPrefix(ref, prefix)]
			if !ok || !strings.HasPrefix(ref, prefix) {
				exps = nil
				break
			}
			exps = append(exps, e)
		}
		if len(exps) == 0 {
			continue
		}
		refs := make([]string, 0, len(chain)+len(exps))
		for _, id := range chain {
			refs = append(refs, processorRef(id))
		}
		described := make([]string, len(exps))
		evidence := []Evidence{{SectionTopology, "pipeline/" + p.ID}}
		for i, e := range exps {
			refs = append(refs, prefix+e.Exporter)
			described[i] = fmt.Sprintf("%s (%s)", e.Exporter, exporterBatchText(e))
		}
		processors := "processor " + chain[len(chain)-1]
		if len(chain) > 1 {
			processors = "processors " + strings.Join(chain, ", ")
		}
		noun, verbs := "exporter", "regroups what the processor sent and adds its"
		if len(exps) > 1 {
			noun, verbs = "exporters", "regroup what the processor sent and add their"
		}
		out = append(out, Recommendation{
			Kind: KindRedundancy, Confidence: ConfidenceStatic, Pipeline: p.ID,
			Components: refs,
			Summary: fmt.Sprintf("Pipeline %s batches in %s and again in the sending queue of %s %s: the %s %s own wait. One of the two is enough. Upstream's batching migration moves batching into the exporters, so it is the processor that can go.",
				p.ID, processors, noun, strings.Join(described, ", "), noun, verbs),
			Evidence: evidence,
			Coverage: exporterBatchCoverage(in, chain, exps),
		})
	}
	return out
}

// exporterBatchText states an exporter's batch section in its own option
// names.
func exporterBatchText(e ExporterBatch) string {
	parts := []string{fmt.Sprintf("sending_queue::batch::min_size %d", e.MinSize)}
	if e.MaxSize > 0 {
		parts = append(parts, fmt.Sprintf("max_size %d", e.MaxSize))
	}
	parts = append(parts, "flush_timeout "+e.FlushTimeout)
	if e.Sizer != "" {
		parts = append(parts, "sizer "+e.Sizer)
	}
	return strings.Join(parts, ", ")
}

// exporterBatchCoverage says what removing the processor would change that
// the configuration alone does not show.
func exporterBatchCoverage(in Input, chain []string, exps []ExporterBatch) []string {
	notes := []string{
		"The run does not show the exporters' own batches: a debug run replaces network exporters with a capture and records a local exporter's input before its sending queue.",
		"An exporter's sending queue also batches what other pipelines send to it; the processor batched this pipeline only.",
	}
	keys := map[string]bool{}
	for _, b := range in.Batching {
		if b != nil && slices.Contains(chain, b.Processor) {
			for _, k := range b.Settings.MetadataKeys {
				keys[k] = true
			}
		}
	}
	for _, e := range exps {
		if len(e.MetadataKeys) == 0 && len(keys) > 0 {
			notes = append(notes, fmt.Sprintf("Exporter %s does not partition its batches (sending_queue::batch::partition::metadata_keys is empty): without the processor its batches would mix the metadata values the processor kept apart.", e.Exporter))
		}
	}
	notes = append(notes, "Processors after the batching processor see single requests once it is gone; a processor that relies on larger requests keeps a reason for it.")
	return notes
}
