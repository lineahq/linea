package engine

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/recommend"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// contextLosses lists, per pipeline, the components behind a batching
// processor that read something from the request's client context the
// batch does not pass on. The batch processor sends with
// client.NewContext(context.Background(), ...) holding only the values of
// its metadata_keys (lowercased; batchprocessor v0.162.0 newShard), so the
// client address, authentication data and gRPC metadata are gone behind
// it, and so is every metadata key it does not list. What each component
// reads comes from its configuration (clientReads).
// queue_batch (v0.165.0) does the same for a send that joins several
// requests, keeping only its batch::partition::metadata_keys; a send of
// one request keeps that request's context.
// See docs/internals/recommendations.md.
func contextLosses(resolved *config.Resolved, g *topology.Graph) []recommend.ContextLoss {
	if resolved == nil || resolved.Config == nil || g == nil {
		return nil
	}
	var out []recommend.ContextLoss
	for _, p := range g.Pipelines {
		ids := processorIDs(p)
		first := slices.IndexFunc(ids, func(id string) bool { return merges(resolved, id) })
		if first < 0 {
			continue
		}
		var refs []string
		for _, id := range ids[first+1:] {
			refs = append(refs, string(topology.KindProcessor)+"/"+id)
		}
		refs = append(refs, p.Exporters...)
		for _, ref := range refs {
			reads := clientReads(resolved, ref)
			if len(reads) == 0 {
				continue
			}
			at := len(ids)
			if kind, id, _ := strings.Cut(ref, "/"); kind == string(topology.KindProcessor) {
				at = slices.Index(ids, id)
			}
			loss := recommend.ContextLoss{Pipeline: p.ID, Component: ref}
			for _, b := range ids[first:at] {
				if !merges(resolved, b) {
					continue
				}
				keys := batchMetadataKeys(resolved, b)
				var lost []recommend.ClientRead
				for _, r := range reads {
					if r.What != "metadata" || r.Key == "" || !slices.Contains(keys, strings.ToLower(r.Key)) {
						lost = append(lost, r)
					}
				}
				if len(lost) > 0 {
					loss.Batch, loss.MetadataKeys, loss.Lost = b, keys, lost
					break
				}
			}
			if loss.Batch != "" {
				loss.Batches = slices.DeleteFunc(slices.Clone(ids[first:at]), func(id string) bool { return !merges(resolved, id) })
				out = append(out, loss)
			}
		}
	}
	return out
}

// merges reports whether processor id joins requests into batches it sends
// with a new context: every batch processor, and a queue_batch processor
// with a batch section (without one it sends each request with its own
// context; exporterhelper v0.165.0).
func merges(resolved *config.Resolved, id string) bool {
	switch typeOf(id) {
	case analysis.TypeBatch:
		return true
	case analysis.TypeQueueBatch:
		var cid component.ID
		if err := cid.UnmarshalText([]byte(id)); err != nil {
			return false
		}
		s, _, err := queueBatchSettings(resolved.Config.Processors[cid])
		return err == nil && !s.Unbatched
	}
	return false
}

// batchMetadataKeys are a batching processor's metadata_keys (queue_batch:
// batch::partition::metadata_keys), lowercased as the processor keeps them.
func batchMetadataKeys(resolved *config.Resolved, id string) []string {
	conf, ok := marshalled(resolved.Config.Processors, id)
	if !ok {
		return nil
	}
	field := "metadata_keys"
	if typeOf(id) == analysis.TypeQueueBatch {
		field = "batch::partition::metadata_keys"
	}
	raw, _ := conf.Get(field).([]any)
	var keys []string
	for _, k := range raw {
		if s, ok := k.(string); ok {
			keys = append(keys, strings.ToLower(s))
		}
	}
	return keys
}

// clientReads lists what the component ref ("processor/x", "exporter/y",
// "connector/z") reads from the request's client context by its
// configuration, verified against contrib v0.162.0 (k8sattributes v1.1.0):
//
//   - attributes, resource: actions with from_context (client.address,
//     metadata.<k>, auth.<k>, or a bare key, which reads metadata;
//     internal/coreinternal/attraction getAttributeValueFromContext);
//   - transform, filter, routing: OTTL paths otelcol.client.addr,
//     otelcol.client.auth..., otelcol.client.metadata[...] and
//     otelcol.grpc.metadata[...]; routing also its deprecated request
//     context, request["k"], which matches gRPC metadata or client metadata;
//   - k8sattributes: the connection address when pod_association is empty
//     or one of its sources is from: connection (extractPodID);
//   - an exporter whose auth.authenticator is a headers_setter extension:
//     its headers' from_context (metadata) and from_attribute (auth).
func clientReads(resolved *config.Resolved, ref string) []recommend.ClientRead {
	kind, id, _ := strings.Cut(ref, "/")
	switch kind {
	case string(topology.KindProcessor):
		conf, ok := marshalled(resolved.Config.Processors, id)
		if !ok {
			return nil
		}
		switch typeOf(id) {
		case "attributes":
			return fromContextActions(conf, "actions")
		case "resource":
			return fromContextActions(conf, "attributes")
		case "transform", "filter":
			return ottlClientReads(conf, false)
		case "k8sattributes":
			return podAssociationReads(conf)
		}
	case string(topology.KindConnector):
		conf, ok := marshalled(resolved.Config.Connectors, id)
		if !ok || typeOf(id) != "routing" {
			return nil
		}
		return routingReads(conf)
	case string(topology.KindExporter):
		conf, ok := marshalled(resolved.Config.Exporters, id)
		if !ok {
			return nil
		}
		return headersSetterReads(resolved, conf)
	}
	return nil
}

// marshalled reads a component's configuration as a map.
func marshalled(configs map[component.ID]component.Config, id string) (*confmap.Conf, bool) {
	var cid component.ID
	if err := cid.UnmarshalText([]byte(id)); err != nil {
		return nil, false
	}
	cfg, ok := configs[cid]
	if !ok {
		return nil, false
	}
	conf := confmap.New()
	if err := conf.Marshal(cfg); err != nil {
		return nil, false
	}
	return conf, true
}

func fromContextActions(conf *confmap.Conf, field string) []recommend.ClientRead {
	actions, _ := conf.Get(field).([]any)
	var out []recommend.ClientRead
	for i, a := range actions {
		m, _ := a.(map[string]any)
		from, _ := m["from_context"].(string)
		if from == "" {
			continue
		}
		r := recommend.ClientRead{How: fmt.Sprintf("%s[%d] from_context %q", field, i, from)}
		switch {
		case from == "client.address":
			r.What = "address"
		case strings.HasPrefix(from, "metadata."):
			r.What, r.Key = "metadata", strings.TrimPrefix(from, "metadata.")
		case strings.HasPrefix(from, "auth."):
			r.What, r.Key = "auth", strings.TrimPrefix(from, "auth.")
		default:
			r.What, r.Key = "metadata", from
		}
		out = append(out, r)
	}
	return out
}

var (
	// otelcolPath matches an otelcol path in OTTL text; the prefix group is
	// optional only where the route's context is otelcol.
	otelcolPath    = regexp.MustCompile(`\botelcol\.(client\.addr\b|client\.auth\b(?:\.attributes\["((?:[^"\\]|\\.)*)"\])?|client\.metadata\b(?:\["((?:[^"\\]|\\.)*)"\])?|grpc\.metadata\b(?:\["((?:[^"\\]|\\.)*)"\])?)`)
	bareClientPath = regexp.MustCompile(`\b(client\.addr\b|client\.auth\b(?:\.attributes\["((?:[^"\\]|\\.)*)"\])?|client\.metadata\b(?:\["((?:[^"\\]|\\.)*)"\])?|grpc\.metadata\b(?:\["((?:[^"\\]|\\.)*)"\])?)`)
	requestKey     = regexp.MustCompile(`\brequest\["((?:[^"\\]|\\.)*)"\]`)
)

// ottlClientReads finds otelcol paths in every string of an OTTL-bearing
// configuration (statements, conditions).
func ottlClientReads(conf *confmap.Conf, bare bool) []recommend.ClientRead {
	var out []recommend.ClientRead
	for _, s := range configStrings(conf.ToStringMap()) {
		out = appendPaths(out, s, otelcolPath)
		if bare {
			out = appendPaths(out, s, bareClientPath)
		}
	}
	return out
}

func appendPaths(out []recommend.ClientRead, text string, re *regexp.Regexp) []recommend.ClientRead {
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		path := m[1]
		r := recommend.ClientRead{How: "OTTL path " + m[0]}
		switch {
		case strings.HasPrefix(path, "client.addr"):
			r.What = "address"
		case strings.HasPrefix(path, "client.auth"):
			r.What, r.Key = "auth", m[2]
		case strings.HasPrefix(path, "client.metadata"):
			r.What, r.Key = "metadata", m[3]
		default:
			r.What, r.Key = "grpc_metadata", m[4]
		}
		if !slices.ContainsFunc(out, func(o recommend.ClientRead) bool { return o.What == r.What && o.Key == r.Key }) {
			out = append(out, r)
		}
	}
	return out
}

// configStrings are every string value under v.
func configStrings(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case map[string]any:
		var out []string
		for _, k := range configKeys(t) {
			out = append(out, configStrings(t[k])...)
		}
		return out
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, configStrings(e)...)
		}
		return out
	}
	return nil
}

func configKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func routingReads(conf *confmap.Conf) []recommend.ClientRead {
	var out []recommend.ClientRead
	table, _ := conf.Get("table").([]any)
	for i, t := range table {
		m, _ := t.(map[string]any)
		ctx, _ := m["context"].(string)
		cond, _ := m["condition"].(string)
		stmt, _ := m["statement"].(string)
		text := cond + "\n" + stmt
		if ctx == "request" {
			for _, k := range requestKey.FindAllStringSubmatch(cond, -1) {
				out = append(out, recommend.ClientRead{What: "metadata", Key: k[1],
					How: fmt.Sprintf("table[%d] request condition (gRPC metadata or client metadata)", i)})
			}
			continue
		}
		out = appendPaths(out, text, otelcolPath)
		if ctx == "otelcol" {
			out = appendPaths(out, text, bareClientPath)
		}
	}
	return out
}

func podAssociationReads(conf *confmap.Conf) []recommend.ClientRead {
	assoc, _ := conf.Get("pod_association").([]any)
	if len(assoc) == 0 {
		return []recommend.ClientRead{{What: "address", How: "pod_association is empty, so pods are identified by the connection address after the k8s.pod.ip attributes"}}
	}
	for i, a := range assoc {
		m, _ := a.(map[string]any)
		sources, _ := m["sources"].([]any)
		for _, s := range sources {
			sm, _ := s.(map[string]any)
			if from, _ := sm["from"].(string); from == "connection" {
				return []recommend.ClientRead{{What: "address", How: fmt.Sprintf("pod_association[%d] source from: connection", i)}}
			}
		}
	}
	return nil
}

func headersSetterReads(resolved *config.Resolved, conf *confmap.Conf) []recommend.ClientRead {
	var out []recommend.ClientRead
	for _, k := range conf.AllKeys() {
		if k != "auth::authenticator" && !strings.HasSuffix(k, "::auth::authenticator") {
			continue
		}
		ext, _ := conf.Get(k).(string)
		if typeOf(ext) != "headers_setter" {
			continue
		}
		econf, ok := marshalled(resolved.Config.Extensions, ext)
		if !ok {
			continue
		}
		headers, _ := econf.Get("headers").([]any)
		for i, h := range headers {
			m, _ := h.(map[string]any)
			key, _ := m["key"].(string)
			if from, _ := m["from_context"].(string); from != "" {
				out = append(out, recommend.ClientRead{What: "metadata", Key: from, How: fmt.Sprintf("%s headers[%d] (%s) from_context", ext, i, key)})
			}
			if from, _ := m["from_attribute"].(string); from != "" {
				out = append(out, recommend.ClientRead{What: "auth", Key: from, How: fmt.Sprintf("%s headers[%d] (%s) from_attribute", ext, i, key)})
			}
		}
	}
	return out
}
