package engine

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/recommend"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// deadWrites finds attribute writes no record kept. For each processor
// output hop where lineage records an attribute added or modified, it
// follows the record on through the same pipeline: the write is live when a
// processor on the way may read the key (by its configuration, or any key
// when that cannot be listed) or the record leaves the pipeline with it, and
// dead when a later processor removes or changes the key first. A record
// dropped on the way counts neither way; one matched only by content
// similarity, or not followed to its end, makes the whole write unknown. A
// write is reported when it was dead on at least one record, live on none,
// unknown on none, and one processor undid it every time.
func deadWrites(resolved *config.Resolved, g *topology.Graph, lineage []analysis.RecordLineage) ([]recommend.DeadWrite, error) {
	if g == nil {
		return nil, nil
	}
	d := deadWriter{resolved: resolved, pipelines: map[string]topology.Pipeline{}, access: map[accessKey]accessed{}}
	for _, p := range g.Pipelines {
		d.pipelines[p.ID] = p
	}
	type writeKey struct {
		pipeline, writer string
		level            features.Level
		key              string
	}
	type tally struct {
		dead        map[string]int // by overwriter
		live, other int
		removed     bool
		modified    bool
	}
	tallies := map[writeKey]*tally{}
	for _, l := range lineage {
		children := make([][]int, len(l.Hops))
		for i, h := range l.Hops {
			if h.Parent >= 0 && h.Parent < i {
				children[h.Parent] = append(children[h.Parent], i)
			}
		}
		for i, h := range l.Hops {
			if h.Kind != instrument.KindEgress || !d.isProcessor(h.Pipeline, h.Component) {
				continue
			}
			for _, w := range attributeWrites(h.Changes) {
				k := writeKey{h.Pipeline, h.Component, w.Level, w.Key}
				t := tallies[k]
				if t == nil {
					t = &tally{dead: map[string]int{}}
					tallies[k] = t
				}
				if h.Confidence == analysis.ConfidenceMedium {
					t.other++
					continue
				}
				o, err := d.follow(l.Hops, children, i, h.Pipeline, l.Signal, w)
				if err != nil {
					return nil, err
				}
				switch o.state {
				case writeDead:
					t.dead[o.by]++
					t.removed = t.removed || o.removed
					t.modified = t.modified || !o.removed
				case writeLive:
					t.live++
				case writeUnknown:
					t.other++
				}
			}
		}
	}
	var out []recommend.DeadWrite
	for k, t := range tallies {
		if t.live > 0 || t.other > 0 || len(t.dead) != 1 {
			continue
		}
		for by, n := range t.dead {
			out = append(out, recommend.DeadWrite{Pipeline: k.pipeline, Signal: d.pipelines[k.pipeline].Signal, Writer: k.writer,
				Overwriter: by, Level: string(k.level), Key: k.key, Removed: t.removed && !t.modified, Records: n})
		}
	}
	slices.SortFunc(out, func(a, b recommend.DeadWrite) int {
		return cmp.Or(cmp.Compare(a.Pipeline, b.Pipeline), cmp.Compare(a.Writer, b.Writer), cmp.Compare(a.Level, b.Level), cmp.Compare(a.Key, b.Key))
	})
	return out, nil
}

type writeState int

const (
	writeNeither writeState = iota // the record was dropped before either
	writeLive
	writeDead
	writeUnknown
)

type outcome struct {
	state   writeState
	by      string // the processor that undid the write
	removed bool
}

type accessKey struct{ processor, signal string }

type accessed struct {
	reads []features.FieldRead
	all   bool // what it reads could not be listed
}

type deadWriter struct {
	resolved  *config.Resolved
	pipelines map[string]topology.Pipeline
	access    map[accessKey]accessed
}

func (d *deadWriter) isProcessor(pipe, id string) bool {
	p, ok := d.pipelines[pipe]
	return ok && slices.Contains(processorIDs(p), id)
}

// follow walks the record from output hop i of the writer through the rest
// of pipe, and says whether the write w on it was read, undone or neither.
// Fan-out inside a pipeline does not happen; a record that reaches several
// hops is live when any path is, unknown when any is, else dead on the
// first.
func (d *deadWriter) follow(hops []analysis.Hop, children [][]int, i int, pipe, signal string, w features.FieldRead) (outcome, error) {
	if hops[i].End != "" {
		if hops[i].End == analysis.EndDropped {
			return outcome{}, nil
		}
		return outcome{state: writeUnknown}, nil
	}
	if len(children[i]) == 0 {
		return outcome{state: writeUnknown}, nil
	}
	var result outcome
	for _, c := range children[i] {
		o, err := d.step(hops, children, c, pipe, signal, w)
		if err != nil {
			return outcome{}, err
		}
		result = combine(result, o)
	}
	return result, nil
}

// step continues follow at hop c, a child of an output hop.
func (d *deadWriter) step(hops []analysis.Hop, children [][]int, c int, pipe, signal string, w features.FieldRead) (outcome, error) {
	h := hops[c]
	if h.Confidence == analysis.ConfidenceMedium {
		return outcome{state: writeUnknown}, nil
	}
	if h.Kind != instrument.KindIngress || h.Pipeline != pipe || !d.isProcessor(pipe, h.Component) {
		// The record left the pipeline (to an exporter or connector) with
		// the attribute.
		return outcome{state: writeLive}, nil
	}
	a, err := d.accessOf(h.Component, signal)
	if err != nil {
		return outcome{}, err
	}
	if a.all || slices.ContainsFunc(a.reads, func(r features.FieldRead) bool { return overlaps(r, w) }) {
		return outcome{state: writeLive}, nil
	}
	if h.End != "" {
		if h.End == analysis.EndDropped {
			return outcome{}, nil
		}
		return outcome{state: writeUnknown}, nil
	}
	var result outcome
	for _, e := range children[c] {
		eh := hops[e]
		if eh.Kind != instrument.KindEgress || eh.Component != h.Component || eh.Confidence == analysis.ConfidenceMedium {
			return outcome{state: writeUnknown}, nil
		}
		var o outcome
		if op, ok := undone(eh.Changes, w); ok {
			o = outcome{state: writeDead, by: h.Component, removed: op == analysis.OpRemoved}
		} else if o, err = d.follow(hops, children, e, pipe, signal, w); err != nil {
			return outcome{}, err
		}
		result = combine(result, o)
	}
	if len(children[c]) == 0 {
		return outcome{state: writeUnknown}, nil
	}
	return result, nil
}

// combine joins the outcomes of two paths of one record: unknown when
// either is, live when either is, and dead only when both paths that
// reached a processor were undone by the same one.
func combine(a, b outcome) outcome {
	switch {
	case a.state == writeUnknown || b.state == writeUnknown:
		return outcome{state: writeUnknown}
	case a.state == writeLive || b.state == writeLive:
		return outcome{state: writeLive}
	case a.state == writeNeither:
		return b
	case b.state == writeNeither:
		return a
	case a.by != b.by:
		return outcome{state: writeUnknown}
	}
	return outcome{state: writeDead, by: a.by, removed: a.removed && b.removed}
}

// accessOf is what processor's configuration may read on signal.
func (d *deadWriter) accessOf(processor, signal string) (accessed, error) {
	k := accessKey{processor, signal}
	if a, ok := d.access[k]; ok {
		return a, nil
	}
	var id component.ID
	if err := id.UnmarshalText([]byte(processor)); err != nil {
		return accessed{}, err
	}
	var sig pipeline.Signal
	if err := sig.UnmarshalText([]byte(signal)); err != nil {
		return accessed{}, err
	}
	reads, _, reason := configuredAccess(typeOf(processor), d.resolved.Config.Processors[id], sig)
	a := accessed{reads: reads, all: reason != ""}
	d.access[k] = a
	return a, nil
}

// attributeWrites are the attribute keys changes add or modify, once each.
func attributeWrites(changes []analysis.Change) []features.FieldRead {
	var out []features.FieldRead
	for _, c := range changes {
		if c.Op != analysis.OpAdded && c.Op != analysis.OpModified {
			continue
		}
		w, ok := attributeOf(c)
		if ok && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// undone reports whether changes remove or modify the attribute w.
func undone(changes []analysis.Change, w features.FieldRead) (analysis.Op, bool) {
	for _, c := range changes {
		if c.Op != analysis.OpRemoved && c.Op != analysis.OpModified {
			continue
		}
		if a, ok := attributeOf(c); ok && a == w {
			return c.Op, true
		}
	}
	return "", false
}

// attributeOf names the attribute a change is to, by its entity's level
// and the first key of its path (attributes["k"] or below it).
func attributeOf(c analysis.Change) (features.FieldRead, bool) {
	rest, ok := strings.CutPrefix(c.Path, "attributes[")
	if !ok {
		return features.FieldRead{}, false
	}
	q, err := strconv.QuotedPrefix(rest)
	if err != nil {
		return features.FieldRead{}, false
	}
	key, err := strconv.Unquote(q)
	if err != nil {
		return features.FieldRead{}, false
	}
	return features.FieldRead{Level: changeLevel(c), Field: "attributes", Key: key}, true
}

// changeLevel is the level a lineage change is at, by its entity:
// resource[i], resource[i].scope[j], or a record below them.
func changeLevel(c analysis.Change) features.Level {
	switch strings.Count(c.Entity, ".") {
	case 0:
		return features.LevelResource
	case 1:
		return features.LevelScope
	}
	return features.LevelRecord
}
