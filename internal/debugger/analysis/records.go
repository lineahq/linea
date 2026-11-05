package analysis

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// IdentityContent matches records that have no stable ID (log records and
// metric data points) by content and keys; every match states its
// Confidence.
const IdentityContent Identity = "content"

// Confidence qualifies a match between records without a stable ID.
type Confidence string

const (
	// ConfidenceObject: the stage passed the record on as the same pdata
	// object (instrument.Event.Objects), so it is the same record whatever
	// the stage changed. A record that is also identical is reported exact.
	ConfidenceObject Confidence = "object"
	// ConfidenceExact: the record is identical before and after.
	ConfidenceExact Confidence = "exact"
	// ConfidenceHigh: matched by a strong key that occurs once on each
	// side. Logs: timestamps and trace context. Data points: metric name,
	// timestamps and attributes.
	ConfidenceHigh Confidence = "high"
	// ConfidenceMedium: matched by a weaker key that occurs once on each
	// side. Logs: body, severity and event name. Data points: metric name and
	// timestamps, then (for renamed metrics) timestamps and value. Metric
	// type is in no key, so a type conversion is reported as a metric.type
	// change.
	ConfidenceMedium Confidence = "medium"
)

// Matching summarizes how records were matched. Unmatched records are listed
// as removed or added only when the other side has no unmatched record left;
// otherwise their relationship is reported as unresolved.
type Matching struct {
	// Object counts records the stage passed on as the same object and
	// changed; unchanged ones count as Exact.
	Object          int `json:"object"`
	Exact           int `json:"exact"`
	High            int `json:"high"`
	Medium          int `json:"medium"`
	UnmatchedBefore int `json:"unmatched_before"`
	UnmatchedAfter  int `json:"unmatched_after"`
}

type tier struct {
	confidence Confidence
	// key derives a record's key from the record and its object number (0
	// when unknown).
	key func(r telemetry.Record, object int) (string, bool)
}

func fieldKey(names ...string) func(telemetry.Record, int) (string, bool) {
	return func(r telemetry.Record, _ int) (string, bool) {
		var b strings.Builder
		for _, n := range names {
			v, ok := r.Fields.Get(n)
			if !ok {
				b.WriteString("-;")
				continue
			}
			m := pcommon.NewMap()
			v.CopyTo(m.PutEmpty("v"))
			b.WriteString(telemetry.Fingerprint(m))
			b.WriteByte(';')
		}
		return b.String(), true
	}
}

var (
	// objectTier pairs records by object number: before any content tier,
	// since a record passed on keeps its object whatever changed.
	objectTier = tier{ConfidenceObject, func(_ telemetry.Record, object int) (string, bool) {
		return strconv.Itoa(object), object != 0
	}}
	exactTier = tier{ConfidenceExact, func(r telemetry.Record, _ int) (string, bool) { return telemetry.Fingerprint(r.Fields), true }}
	logTiers  = []tier{
		objectTier,
		exactTier,
		{ConfidenceHigh, func(r telemetry.Record, object int) (string, bool) {
			// Without any timestamp or trace context the key identifies
			// nothing.
			if r.Str("time_unix_nano") == "0" && r.Str("observed_time_unix_nano") == "0" &&
				r.Str("trace_id") == "" && r.Str("span_id") == "" {
				return "", false
			}
			return fieldKey("time_unix_nano", "observed_time_unix_nano", "trace_id", "span_id")(r, object)
		}},
		{ConfidenceMedium, fieldKey("body", "severity_number", "event_name")},
	}
	pointTiers = []tier{
		objectTier,
		exactTier,
		{ConfidenceHigh, fieldKey("metric.name", "start_time_unix_nano", "time_unix_nano", "attributes")},
		// Temporality conversions (deltatocumulative, cumulativetodelta)
		// rewrite a point's start time but keep its stream and time.
		{ConfidenceHigh, fieldKey("metric.name", "time_unix_nano", "attributes")},
		{ConfidenceMedium, fieldKey("metric.name", "start_time_unix_nano", "time_unix_nano")},
		// A renamed metric (metricstransform, transform set(metric.name))
		// keeps its points' timestamps and values.
		{ConfidenceMedium, fieldKey("start_time_unix_nano", "time_unix_nano", "value", "count", "sum", "bucket_counts")},
	}
)

type match struct {
	b, a       int
	confidence Confidence
}

// matchRecords pairs records tier by tier. Within a tier a key matches only
// if it occurs exactly once among the still unmatched records on each side,
// except for exact matches: identical records are interchangeable, so they
// are paired in order. bObj and aObj are the records' object numbers (nil,
// or 0 for a record, when unknown); a record paired by object and
// identical on both sides is an exact match.
func matchRecords(before, after []telemetry.Record, bObj, aObj []int, tiers []tier) (matches []match, unB, unA []int) {
	objectOf := func(objs []int, i int) int {
		if len(objs) != 0 && i < len(objs) {
			return objs[i]
		}
		return 0
	}
	for i := range before {
		unB = append(unB, i)
	}
	for i := range after {
		unA = append(unA, i)
	}
	for _, t := range tiers {
		group := func(recs []telemetry.Record, objs []int, idx []int) (map[string][]int, []string) {
			m := map[string][]int{}
			var order []string
			for _, i := range idx {
				k, ok := t.key(recs[i], objectOf(objs, i))
				if !ok {
					continue
				}
				if _, seen := m[k]; !seen {
					order = append(order, k)
				}
				m[k] = append(m[k], i)
			}
			return m, order
		}
		bm, order := group(before, bObj, unB)
		am, _ := group(after, aObj, unA)
		usedB, usedA := map[int]bool{}, map[int]bool{}
		for _, k := range order {
			bs, as := bm[k], am[k]
			n := min(len(bs), len(as))
			if t.confidence != ConfidenceExact && (len(bs) != 1 || len(as) != 1) {
				n = 0
			}
			for i := range n {
				c := t.confidence
				if c == ConfidenceObject && telemetry.Fingerprint(before[bs[i]].Fields) == telemetry.Fingerprint(after[as[i]].Fields) {
					c = ConfidenceExact
				}
				matches = append(matches, match{bs[i], as[i], c})
				usedB[bs[i]], usedA[as[i]] = true, true
			}
		}
		unB = slices.DeleteFunc(unB, func(i int) bool { return usedB[i] })
		unA = slices.DeleteFunc(unA, func(i int) bool { return usedA[i] })
	}
	slices.SortFunc(matches, func(x, y match) int { return x.b - y.b })
	return matches, unB, unA
}

// diffContent diffs log or metric payloads, whose records have no stable ID,
// pairing them by object number first (bObj, aObj in Records order; nil
// when unknown).
func diffContent(before, after telemetry.Payload, bObj, aObj []int) Diff {
	d := Diff{Identity: IdentityContent, Before: before.Counts(), After: after.Counts()}
	bRecs, aRecs := telemetry.Records(before), telemetry.Records(after)
	tiers := logTiers
	if before.Signal == pipeline.SignalMetrics {
		tiers = pointTiers
	}
	matches, unB, unA := matchRecords(bRecs, aRecs, bObj, aObj, tiers)

	m := &Matching{UnmatchedBefore: len(unB), UnmatchedAfter: len(unA)}
	var pairs []groupPair
	var changes []Change
	for _, x := range matches {
		b, a := bRecs[x.b], aRecs[x.a]
		pairs = append(pairs, groupPair{groupLoc{b.Resource, b.Scope}, groupLoc{a.Resource, a.Scope}})
		switch x.confidence {
		case ConfidenceExact:
			m.Exact++
			continue
		case ConfidenceObject:
			m.Object++
		case ConfidenceHigh:
			m.High++
		case ConfidenceMedium:
			m.Medium++
		}
		for _, c := range diffFields(b.Ref(before.Signal), b.Fields, a.Fields) {
			c.Confidence = x.confidence
			changes = append(changes, c)
		}
	}
	d.Matching = m

	refs := func(p telemetry.Payload, recs []telemetry.Record, idx []int) []RecordRef {
		out := make([]RecordRef, 0, len(idx))
		for _, i := range idx {
			out = append(out, recordRef(p.Signal, recs[i]))
		}
		return out
	}
	switch {
	case len(unB) > 0 && len(unA) == 0:
		d.Removed = refs(before, bRecs, unB)
	case len(unA) > 0 && len(unB) == 0:
		d.Added = refs(after, aRecs, unA)
	case len(unA) > 0:
		d.Unresolved = append(d.Unresolved, fmt.Sprintf(
			"%d record(s) before and %d after could not be matched; they may have been changed, removed or added", len(unB), len(unA)))
	}

	groupChanges, regrouped := diffGroups(before, after, pairs)
	if regrouped > 0 {
		d.Unresolved = append(d.Unresolved, fmt.Sprintf("%d resource(s) were merged or split; their resource and scope changes are not attributed", regrouped))
	}
	d.Changes = append(groupChanges, changes...)
	return d
}

func recordRef(signal pipeline.Signal, r telemetry.Record) RecordRef {
	ref := RecordRef{Ref: r.Ref(signal)}
	if signal == pipeline.SignalMetrics {
		ref.Name = r.Str("metric.name")
		return ref
	}
	ref.Name = r.Str("event_name")
	if body, ok := r.Fields.Get("body"); ok && ref.Name == "" {
		ref.Name = truncate(body.AsString(), 80)
	}
	return ref
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DiffFields diffs two records flattened by telemetry.Records under the given
// entity, as stage diffs do: paths name the field ("body", "attributes") and
// map keys below it (attributes["k"]).
func DiffFields(entity string, before, after pcommon.Map) []Change {
	return diffFields(entity, before, after)
}

// diffFields diffs two flattened records. Map fields (attributes, body maps)
// are diffed per key; other fields are compared as whole typed values.
func diffFields(entity string, b, a pcommon.Map) []Change {
	keys := map[string]bool{}
	for k := range b.All() {
		keys[k] = true
	}
	for k := range a.All() {
		keys[k] = true
	}
	var out []Change
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		bv, bok := b.Get(k)
		av, aok := a.Get(k)
		out = append(out, diffValue(entity, k, bv, bok, av, aok)...)
	}
	return out
}

// diffValue compares one typed value, recursing into maps that keep their
// type.
func diffValue(entity, path string, bv pcommon.Value, bok bool, av pcommon.Value, aok bool) []Change {
	switch {
	case !bok:
		return []Change{{Entity: entity, Path: path, Op: OpAdded, After: av.AsRaw(), AfterType: av.Type().String()}}
	case !aok:
		return []Change{{Entity: entity, Path: path, Op: OpRemoved, Before: bv.AsRaw(), BeforeType: bv.Type().String()}}
	case bv.Type() != av.Type():
		return []Change{{Entity: entity, Path: path, Op: OpTypeChanged,
			Before: bv.AsRaw(), After: av.AsRaw(), BeforeType: bv.Type().String(), AfterType: av.Type().String()}}
	case bv.Type() == pcommon.ValueTypeMap:
		return diffMap(entity, path, bv.Map(), av.Map())
	case !bv.Equal(av):
		return []Change{{Entity: entity, Path: path, Op: OpModified,
			Before: bv.AsRaw(), After: av.AsRaw(), BeforeType: bv.Type().String(), AfterType: av.Type().String()}}
	}
	return nil
}

type groupLoc struct{ resource, scope int }

type groupPair struct{ b, a groupLoc }

// diffGroups attributes resource and scope changes through matched records:
// a before resource (or scope) is diffed only when its matched records map
// one-to-one onto a single after resource (or scope). It returns the changes
// and the number of before resources that were merged or split.
func diffGroups(before, after telemetry.Payload, pairs []groupPair) ([]Change, int) {
	resFwd, resRev := map[int]map[int]bool{}, map[int]map[int]bool{}
	scopeFwd, scopeRev := map[groupLoc]map[groupLoc]bool{}, map[groupLoc]map[groupLoc]bool{}
	for _, p := range pairs {
		link(resFwd, p.b.resource, p.a.resource)
		link(resRev, p.a.resource, p.b.resource)
		link(scopeFwd, p.b, p.a)
		link(scopeRev, p.a, p.b)
	}

	var resChanges, scopeChanges []Change
	regrouped := 0
	for _, bi := range slices.Sorted(maps.Keys(resFwd)) {
		ai, ok := only(resFwd[bi])
		if !ok || len(resRev[ai]) != 1 {
			regrouped++
			continue
		}
		entity := "resource[" + strconv.Itoa(bi) + "]"
		br, bURL := before.ResourceAt(bi)
		ar, aURL := after.ResourceAt(ai)
		resChanges = append(resChanges, diffMap(entity, "attributes", br.Attributes(), ar.Attributes())...)
		resChanges = append(resChanges, diffScalar(entity, "schema_url", bURL, aURL)...)
	}
	bScopes := slices.SortedFunc(maps.Keys(scopeFwd), func(x, y groupLoc) int {
		if x.resource != y.resource {
			return x.resource - y.resource
		}
		return x.scope - y.scope
	})
	for _, bs := range bScopes {
		as, ok := only(scopeFwd[bs])
		if !ok || len(scopeRev[as]) != 1 {
			continue
		}
		entity := fmt.Sprintf("resource[%d].scope[%d]", bs.resource, bs.scope)
		b, _ := before.ScopeAt(bs.resource, bs.scope)
		a, _ := after.ScopeAt(as.resource, as.scope)
		scopeChanges = append(scopeChanges, DiffScope(entity, b, a)...)
	}
	return append(resChanges, scopeChanges...), regrouped
}

func link[K comparable](m map[K]map[K]bool, from, to K) {
	if m[from] == nil {
		m[from] = map[K]bool{}
	}
	m[from][to] = true
}

func only[K comparable](set map[K]bool) (K, bool) {
	var zero K
	if len(set) != 1 {
		return zero, false
	}
	for k := range set {
		return k, true
	}
	return zero, false
}
