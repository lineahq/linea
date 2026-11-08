package recommend

import (
	"fmt"
	"slices"
	"strings"
)

// memoryLimiterNotFirst: a memory_limiter after other processors refuses
// data only after they have spent memory on it.
func memoryLimiterNotFirst(in Input) []Recommendation {
	var out []Recommendation
	for _, p := range in.Topology.Pipelines {
		ids := pipelineIDs(p)
		for i, id := range ids {
			if componentType(id) != "memory_limiter" || i == 0 {
				continue
			}
			order := append([]string{id}, slices.Delete(slices.Clone(ids), i, i+1)...)
			out = append(out, Recommendation{
				Kind: KindRisk, Confidence: ConfidenceStatic, Pipeline: p.ID,
				Components: []string{processorRef(id)},
				Summary: fmt.Sprintf("%s is processor %d of %d in pipeline %s: %s already hold the data it would refuse under memory pressure. Placed first, it refuses data before any other processor spends memory on it.",
					id, i+1, len(ids), p.ID, strings.Join(ids[:i], ", ")),
				Evidence: []Evidence{{SectionTopology, "pipeline/" + p.ID}},
				Change:   &ConfigChange{Op: OpSwap, Processors: order},
			})
			break
		}
	}
	return out
}

// unusedComponent: a configured component the service never uses is not
// built.
func unusedComponent(in Input) []Recommendation {
	var out []Recommendation
	for _, ref := range in.Topology.Unused {
		out = append(out, Recommendation{
			Kind: KindRedundancy, Confidence: ConfidenceStatic,
			Components: []string{ref},
			Summary:    fmt.Sprintf("%s is configured but the service never uses it, so the Collector does not build it. Remove it, or add it where it was meant to run.", ref),
			Evidence:   []Evidence{{SectionTopology, ref}},
		})
	}
	return out
}

// batchChained: a second batching processor (batch or queue_batch) in one
// pipeline regroups what the first already sent and adds its own wait.
func batchChained(in Input) []Recommendation {
	var out []Recommendation
	for _, p := range in.Topology.Pipelines {
		var chain []string
		for _, id := range pipelineIDs(p) {
			if batches(in, p.ID, id) {
				chain = append(chain, id)
			}
		}
		if len(chain) < 2 {
			continue
		}
		refs := make([]string, len(chain))
		for i, id := range chain {
			refs[i] = processorRef(id)
		}
		out = append(out, Recommendation{
			Kind: KindRedundancy, Confidence: ConfidenceStatic, Pipeline: p.ID,
			Components: refs,
			Summary: fmt.Sprintf("Pipeline %s batches %d times (%s): each later batching processor regroups what the one before it sent and adds its own wait. One batching processor, usually the last, is enough.",
				p.ID, len(chain), strings.Join(chain, ", ")),
			Evidence: []Evidence{{SectionTopology, "pipeline/" + p.ID}},
		})
	}
	return out
}
