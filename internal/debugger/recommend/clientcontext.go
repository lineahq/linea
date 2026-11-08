package recommend

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/lineahq/linea/internal/debugger/analysis"
)

// ClientRead is one value a component's configuration reads from the
// request's client context.
type ClientRead struct {
	// What is address, auth, metadata or grpc_metadata; Key the auth
	// attribute or metadata key, empty for all of them.
	What, Key string
	// How names the setting that reads it.
	How string
}

// ContextLoss is a component behind a batch processor that reads client
// context values the batch processor does not pass on. The engine computes
// it from the configuration (contextLosses in internal/debugger/engine).
type ContextLoss struct {
	Pipeline string
	// Component is the reader's topology ref (processor/..., exporter/...,
	// connector/...).
	Component string
	// Batch is the first batch processor before it that drops a read, with
	// its metadata_keys; Batches every batch processor before the reader.
	Batch        string
	MetadataKeys []string
	Batches      []string
	Lost         []ClientRead
}

// batchDropsClientContext: a component reads from the client context what a
// batch processor before it does not pass on. The batch processor sends with
// a new context holding only the values of its metadata_keys, so the reader
// finds nothing there.
func batchDropsClientContext(in Input) []Recommendation {
	var out []Recommendation
	for _, l := range in.ContextLosses {
		if len(l.Lost) == 0 {
			continue
		}
		p, ok := pipeline(in.Topology, l.Pipeline)
		if !ok {
			continue
		}
		reader := strings.TrimPrefix(l.Component, strings.SplitN(l.Component, "/", 2)[0]+"/")
		var names, hows []string
		var newKeys []string
		onlyMetadata := true
		for _, r := range l.Lost {
			names = append(names, clientReadName(r))
			hows = append(hows, r.How)
			if r.What == "metadata" && r.Key != "" {
				if k := strings.ToLower(r.Key); !slices.Contains(newKeys, k) {
					newKeys = append(newKeys, k)
				}
			} else {
				onlyMetadata = false
			}
		}
		qb := componentType(l.Batch) == analysis.TypeQueueBatch
		keysOption, sends := "metadata_keys", "sends with a new context"
		if qb {
			// queue_batch keeps a request's context when it sends it alone.
			keysOption, sends = "batch::partition::metadata_keys", "sends a batch joining several requests with a new context"
		}
		kept := "no metadata (its " + keysOption + " is empty)"
		if len(l.MetadataKeys) > 0 {
			kept = "the metadata keys " + strings.Join(l.MetadataKeys, ", ")
		}
		rec := Recommendation{
			Kind: KindRisk, Confidence: ConfidenceStatic, Pipeline: l.Pipeline,
			Components: []string{processorRef(l.Batch), l.Component},
			Summary: fmt.Sprintf("%s reads %s from the request's client context in pipeline %s (%s), but runs after %s, which %s holding only %s: there it finds nothing.",
				reader, strings.Join(names, ", "), l.Pipeline, strings.Join(hows, "; "), l.Batch, sends, kept),
			Evidence: []Evidence{{SectionTopology, "pipeline/" + l.Pipeline}},
			Coverage: []string{"Read from the configuration: whether requests carry these values depends on the receivers (include_metadata for metadata) and the clients."},
		}
		if qb {
			rec.Coverage = append(rec.Coverage, l.Batch+" loses the context only for sends joining several requests: a request sent alone, because it reached batch::min_size by itself or was the only one pending at batch::flush_timeout, keeps it.")
		}
		ids := pipelineIDs(p)
		first := slices.Index(ids, l.Batches[0])
		at := slices.Index(ids, reader)
		isProcessor := strings.HasPrefix(l.Component, "processor/") && at > first
		switch {
		case onlyMetadata && len(l.Batches) == 1:
			keys := append(slices.Clone(l.MetadataKeys), newKeys...)
			rec.Change = &ConfigChange{Op: OpSetOption, Component: l.Batch, Option: keysOption, Value: jsonList(keys)}
			if qb {
				rec.Coverage = append(rec.Coverage, fmt.Sprintf("%s then keeps a partition for each distinct combination of these values (beyond batch::partition::cache_size it flushes the least recently used one), so batches may get smaller.", l.Batch))
			} else {
				rec.Coverage = append(rec.Coverage, fmt.Sprintf("%s then keeps a batcher for each distinct combination of these values (up to metadata_cardinality_limit), so batches may get smaller.", l.Batch))
			}
		case isProcessor && !slices.ContainsFunc(ids[first:at], func(id string) bool { return !analysis.IsBatcher(componentType(id)) }):
			order := slices.Delete(slices.Clone(ids), at, at+1)
			order = slices.Insert(order, first, reader)
			rec.Change = &ConfigChange{Op: OpSwap, Processors: order}
		case onlyMetadata:
			rec.Blockers = []string{fmt.Sprintf("No change suggested: the metadata keys of every batching processor before %s (%s) would have to list %s.",
				reader, strings.Join(l.Batches, ", "), strings.Join(newKeys, ", "))}
		case isProcessor:
			var between []string
			for _, id := range ids[first:at] {
				if !analysis.IsBatcher(componentType(id)) {
					between = append(between, id)
				}
			}
			rec.Blockers = []string{fmt.Sprintf("No change suggested: %s sit between %s and %s; they may write what %s reads, so moving it ahead of them is not known to be safe.",
				strings.Join(between, ", "), l.Batches[0], reader, reader)}
		default:
			rec.Blockers = []string{fmt.Sprintf("No change suggested: metadata keys cannot carry %s, and %s cannot run before a processor.", strings.Join(names, ", "), reader)}
		}
		out = append(out, rec)
	}
	return out
}

func clientReadName(r ClientRead) string {
	switch r.What {
	case "address":
		return "the client address"
	case "auth":
		if r.Key == "" {
			return "the authentication data"
		}
		return "auth attribute " + strconv.Quote(r.Key)
	case "grpc_metadata":
		if r.Key == "" {
			return "the gRPC metadata"
		}
		return "gRPC metadata " + strconv.Quote(r.Key)
	}
	if r.Key == "" {
		return "the client metadata"
	}
	return "metadata " + strconv.Quote(r.Key)
}

// jsonList is a list of strings as a JSON (and so YAML flow) value.
func jsonList(items []string) string {
	b, _ := json.Marshal(items)
	return string(b)
}
