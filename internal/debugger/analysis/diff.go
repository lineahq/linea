// Package analysis derives structural differences and per-stage outcomes from
// recorded snapshots. It never re-executes components: everything it reports
// is computed from observed stage.input and stage.output telemetry.
package analysis

import (
	"fmt"
	"sort"
	"strconv"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Op is the kind of a field-level change.
type Op string

const (
	OpAdded       Op = "added"
	OpRemoved     Op = "removed"
	OpModified    Op = "modified"
	OpTypeChanged Op = "type_changed"
)

// Identity states how records were matched between two snapshots.
type Identity string

const (
	// IdentitySpan matches spans by trace ID and span ID.
	IdentitySpan Identity = "span_id"
	// IdentityUnresolved means no stable identity was available, so only
	// counts are reported and no record is claimed to match another.
	IdentityUnresolved Identity = "unresolved"
)

// Change is one field-level difference on a matched entity. Before and After
// are the typed pdata values as raw Go values; their pdata types are carried
// separately so an int 5 and a string "5" stay distinguishable.
type Change struct {
	// Entity is "resource[i]", "resource[i].scope[j]" (indexes into the
	// before snapshot) or "span:<trace_id>:<span_id>".
	Entity     string `json:"entity"`
	Path       string `json:"path"`
	Op         Op     `json:"op"`
	Before     any    `json:"before,omitempty"`
	After      any    `json:"after,omitempty"`
	BeforeType string `json:"before_type,omitempty"`
	AfterType  string `json:"after_type,omitempty"`
	// Confidence qualifies changes on records matched without a stable ID
	// (IdentityContent); it is empty for spans and for resources and scopes.
	Confidence Confidence `json:"confidence,omitempty"`
}

// RecordRef identifies a record. Spans are identified by trace and span ID.
// Log records and data points are identified by Ref, their position in the
// payload: removed records index the before snapshot, added records the
// after snapshot. Name is the span name, the metric name, or the log event
// name or (truncated) body.
type RecordRef struct {
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Name    string `json:"name"`
}

// Diff is the structural difference between two payloads of one signal.
type Diff struct {
	Identity Identity         `json:"identity"`
	Before   telemetry.Counts `json:"before"`
	After    telemetry.Counts `json:"after"`
	// Removed are records present before and absent after.
	Removed []RecordRef `json:"removed,omitempty"`
	// Added are records absent before and present after.
	Added   []RecordRef `json:"added,omitempty"`
	Changes []Change    `json:"changes,omitempty"`
	// Matching summarizes record matching for IdentityContent diffs.
	Matching *Matching `json:"matching,omitempty"`
	// Unresolved explains relationships that could not be established, for
	// example resources merged or split by the stage.
	Unresolved []string `json:"unresolved,omitempty"`
}

// Empty reports whether the diff found no difference at all.
func (d Diff) Empty() bool {
	return d.Before == d.After && len(d.Removed) == 0 && len(d.Added) == 0 &&
		len(d.Changes) == 0 && len(d.Unresolved) == 0
}

// DiffPayloads compares two payloads. Spans are matched by span identity;
// log records and metric data points by content with stated confidence.
func DiffPayloads(before, after telemetry.Payload) (Diff, error) {
	return diffPayloads(before, after, nil, nil)
}

// diffPayloads is DiffPayloads with the record object numbers of both sides
// (in Records order, nil when unknown): log records and data points are
// paired by object first. Spans keep their span identity.
func diffPayloads(before, after telemetry.Payload, bObj, aObj []int) (Diff, error) {
	if before.Signal != after.Signal {
		return Diff{}, fmt.Errorf("cannot diff %s against %s", before.Signal, after.Signal)
	}
	if before.Signal != pipeline.SignalTraces {
		return diffContent(before, after, bObj, aObj), nil
	}
	return DiffTraces(before.Traces, after.Traces), nil
}

type spanKey struct {
	trace pcommon.TraceID
	span  pcommon.SpanID
}

func (k spanKey) entity() string {
	return "span:" + k.trace.String() + ":" + k.span.String()
}

type spanLoc struct {
	resource, scope int
	span            ptrace.Span
}

// indexSpans returns spans by identity. Identities that occur more than once
// are returned separately, since they cannot be matched reliably.
func indexSpans(td ptrace.Traces) (map[spanKey]spanLoc, map[spanKey]bool) {
	idx := map[spanKey]spanLoc{}
	dup := map[spanKey]bool{}
	for i, rs := range td.ResourceSpans().All() {
		for j, ss := range rs.ScopeSpans().All() {
			for _, s := range ss.Spans().All() {
				k := spanKey{s.TraceID(), s.SpanID()}
				if _, ok := idx[k]; ok {
					dup[k] = true
				}
				idx[k] = spanLoc{resource: i, scope: j, span: s}
			}
		}
	}
	for k := range dup {
		delete(idx, k)
	}
	return idx, dup
}

// DiffTraces matches spans by trace and span ID. A resource or scope is
// diffed only when its spans map one-to-one onto a single counterpart;
// otherwise the regrouping is reported as unresolved instead of guessing.
func DiffTraces(before, after ptrace.Traces) Diff {
	d := Diff{
		Identity: IdentitySpan,
		Before:   telemetry.FromTraces(before).Counts(),
		After:    telemetry.FromTraces(after).Counts(),
	}
	bIdx, bDup := indexSpans(before)
	aIdx, aDup := indexSpans(after)
	if n := len(bDup) + len(aDup); n > 0 {
		d.Unresolved = append(d.Unresolved, fmt.Sprintf("%d span identities occur more than once and were not matched", n))
	}

	var pairs []groupPair
	for _, k := range sortedKeys(bIdx) {
		b := bIdx[k]
		a, ok := aIdx[k]
		if !ok {
			if !aDup[k] {
				d.Removed = append(d.Removed, ref(k, b.span))
			}
			continue
		}
		pairs = append(pairs, groupPair{groupLoc{b.resource, b.scope}, groupLoc{a.resource, a.scope}})
		d.Changes = append(d.Changes, diffSpan(k.entity(), b.span, a.span)...)
	}
	for _, k := range sortedKeys(aIdx) {
		if _, ok := bIdx[k]; !ok && !bDup[k] {
			d.Added = append(d.Added, ref(k, aIdx[k].span))
		}
	}

	groupChanges, regrouped := diffGroups(telemetry.FromTraces(before), telemetry.FromTraces(after), pairs)
	if regrouped > 0 {
		d.Unresolved = append(d.Unresolved, fmt.Sprintf("%d resource(s) were merged or split; their resource and scope changes are not attributed", regrouped))
	}
	d.Changes = append(groupChanges, d.Changes...)
	return d
}

// DiffResource diffs resource attributes under the given entity name.
func DiffResource(entity string, before, after pcommon.Resource) []Change {
	return diffMap(entity, "attributes", before.Attributes(), after.Attributes())
}

// DiffScope diffs an instrumentation scope under the given entity name.
func DiffScope(entity string, before, after pcommon.InstrumentationScope) []Change {
	var out []Change
	out = append(out, diffScalar(entity, "name", before.Name(), after.Name())...)
	out = append(out, diffScalar(entity, "version", before.Version(), after.Version())...)
	out = append(out, diffMap(entity, "attributes", before.Attributes(), after.Attributes())...)
	return out
}

// DiffSpan diffs the fields of one span under the given entity name.
func DiffSpan(entity string, before, after ptrace.Span) []Change {
	return diffSpan(entity, before, after)
}

// SpanEntity names a span as DiffTraces does.
func SpanEntity(s ptrace.Span) string { return spanKey{s.TraceID(), s.SpanID()}.entity() }

func diffSpan(entity string, b, a ptrace.Span) []Change {
	var out []Change
	out = append(out, diffScalar(entity, "name", b.Name(), a.Name())...)
	out = append(out, diffScalar(entity, "kind", b.Kind().String(), a.Kind().String())...)
	out = append(out, diffScalar(entity, "parent_span_id", b.ParentSpanID().String(), a.ParentSpanID().String())...)
	out = append(out, diffScalar(entity, "trace_state", b.TraceState().AsRaw(), a.TraceState().AsRaw())...)
	out = append(out, diffScalar(entity, "start_time_unix_nano", uint64(b.StartTimestamp()), uint64(a.StartTimestamp()))...)
	out = append(out, diffScalar(entity, "end_time_unix_nano", uint64(b.EndTimestamp()), uint64(a.EndTimestamp()))...)
	out = append(out, diffScalar(entity, "status.code", b.Status().Code().String(), a.Status().Code().String())...)
	out = append(out, diffScalar(entity, "status.message", b.Status().Message(), a.Status().Message())...)
	out = append(out, diffMap(entity, "attributes", b.Attributes(), a.Attributes())...)
	// Events and links have no stable identity of their own; a change is
	// reported for the collection as a whole.
	if !eventsEqual(b.Events(), a.Events()) {
		out = append(out, Change{Entity: entity, Path: "events", Op: OpModified, Before: b.Events().Len(), After: a.Events().Len()})
	}
	if !linksEqual(b.Links(), a.Links()) {
		out = append(out, Change{Entity: entity, Path: "links", Op: OpModified, Before: b.Links().Len(), After: a.Links().Len()})
	}
	return out
}

func eventsEqual(a, b ptrace.SpanEventSlice) bool {
	if a.Len() != b.Len() {
		return false
	}
	for i := range a.Len() {
		x, y := a.At(i), b.At(i)
		if x.Name() != y.Name() || x.Timestamp() != y.Timestamp() || !x.Attributes().Equal(y.Attributes()) {
			return false
		}
	}
	return true
}

func linksEqual(a, b ptrace.SpanLinkSlice) bool {
	if a.Len() != b.Len() {
		return false
	}
	for i := range a.Len() {
		x, y := a.At(i), b.At(i)
		if x.TraceID() != y.TraceID() || x.SpanID() != y.SpanID() || !x.Attributes().Equal(y.Attributes()) {
			return false
		}
	}
	return true
}

func diffScalar[T comparable](entity, path string, b, a T) []Change {
	if b == a {
		return nil
	}
	return []Change{{Entity: entity, Path: path, Op: OpModified, Before: b, After: a}}
}

// diffMap compares attribute maps by key, recursing into nested maps so a
// change deep inside a structure is reported at its own path.
func diffMap(entity, path string, b, a pcommon.Map) []Change {
	var out []Change
	keys := map[string]bool{}
	for k := range b.All() {
		keys[k] = true
	}
	for k := range a.All() {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		p := path + "[" + strconv.Quote(k) + "]"
		bv, bok := b.Get(k)
		av, aok := a.Get(k)
		out = append(out, diffValue(entity, p, bv, bok, av, aok)...)
	}
	return out
}

func ref(k spanKey, s ptrace.Span) RecordRef {
	return RecordRef{TraceID: k.trace.String(), SpanID: k.span.String(), Name: s.Name()}
}

func sortedKeys(m map[spanKey]spanLoc) []spanKey {
	keys := make([]spanKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].trace != keys[j].trace {
			return keys[i].trace.String() < keys[j].trace.String()
		}
		return keys[i].span.String() < keys[j].span.String()
	})
	return keys
}
