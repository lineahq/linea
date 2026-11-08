package analysis

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/instrument"
)

// shard is one batcher of the replay: its pending items, their size in the
// sizer's unit (items or bytes) and its timer.
type shard struct {
	key      string
	metadata map[string][]string
	unknown  bool
	refused  bool
	evicted  bool
	pending  int
	size     int
	nextTick time.Time
	arrivals int
	items    int
	flushes  int
}

// shards replays the batchers of one batching processor. Without
// metadata_keys there is one, whose timer starts with the processor; with
// them, batchprocessor v0.162.0 (multiShardBatcher) creates a batcher, and
// starts its timer, at the first request carrying a new combination of
// the listed keys' values from client.Info.Metadata. queue_batch
// (v0.165.0 multiBatcher) does the same per partition, without a
// cardinality limit: beyond batch::partition::cache_size partitions it
// flushes and removes the least recently used one.
type shards struct {
	in      BatchInput
	keys    []string
	limit   int
	byKey   map[string]int
	list    []shard
	roots   map[int][]int
	refused int
	// lru holds the indexes of queue_batch's live partitions, least
	// recently used first.
	lru     []int
	evicted int
}

func newShards(in BatchInput, start time.Time) *shards {
	s := &shards{in: in, byKey: map[string]int{}, limit: in.Settings.MetadataCardinalityLimit}
	if in.Settings.QueueBatch() {
		s.limit = 0
	}
	for _, k := range in.Settings.MetadataKeys {
		s.keys = append(s.keys, strings.ToLower(k))
	}
	slices.Sort(s.keys)
	s.keys = slices.Compact(s.keys)
	if len(s.keys) == 0 {
		sh := shard{}
		if in.Timeout > 0 && in.Settings.SendBatchSize > 0 && !start.IsZero() {
			sh.nextTick = start.Add(in.Timeout)
		}
		s.list = []shard{sh}
		return s
	}
	s.roots = inputRoots(in)
	return s
}

// arrive assigns an arrival of items, size in the sizer's unit, to its
// batcher and returns the batcher index.
func (s *shards) arrive(e instrument.Event, items, size int) int {
	i := 0
	timed := s.in.Timeout > 0 && s.in.Settings.SendBatchSize > 0 && !e.Time.IsZero()
	qb := s.in.Settings.QueueBatch()
	if len(s.keys) > 0 {
		key, md, unknown := s.keyOf(e.Seq)
		var ok bool
		if i, ok = s.byKey[key]; !ok {
			sh := shard{key: key, metadata: md, unknown: unknown}
			if s.limit > 0 && len(s.byKey) >= s.limit && !unknown {
				sh.refused = true
			}
			if timed {
				sh.nextTick = e.Time.Add(s.in.Timeout)
			}
			if qb && s.in.Settings.CacheSize > 0 && len(s.lru) >= s.in.Settings.CacheSize {
				s.evict()
			}
			i = len(s.list)
			s.list = append(s.list, sh)
			if !sh.refused {
				s.byKey[key] = i
			}
		}
		if qb {
			s.lru = slices.DeleteFunc(s.lru, func(j int) bool { return j == i })
			s.lru = append(s.lru, i)
		}
	}
	sh := &s.list[i]
	sh.arrivals++
	if sh.refused {
		s.refused++
		return i
	}
	if qb && timed && sh.pending == 0 {
		// The request starts a pending batch, which restarts the timer.
		sh.nextTick = e.Time.Add(s.in.Timeout)
	}
	sh.items += items
	sh.pending += items
	sh.size += size
	return i
}

// evict removes queue_batch's least recently used partition, which flushes
// what it holds; a later request with its values starts a new one.
func (s *shards) evict() {
	if len(s.lru) == 0 {
		return
	}
	i := s.lru[0]
	s.lru = s.lru[1:]
	s.list[i].evicted = true
	delete(s.byKey, s.list[i].key)
	s.evicted++
}

// of returns the batcher a flush came from: the batcher of the arrivals
// its records came from, else the one with items pending.
func (s *shards) of(sources []BatchSource, arrivalShard map[int]int) int {
	for _, src := range sources {
		if i, ok := arrivalShard[src.Arrival]; ok {
			return i
		}
	}
	for i, sh := range s.list {
		if sh.pending > 0 {
			return i
		}
	}
	return 0
}

// keyOf derives an arrival's batcher from the headers of the scenario
// inputs its records came from. Values of unlisted headers never leave.
func (s *shards) keyOf(arrival int) (string, map[string][]string, bool) {
	roots := s.roots[arrival]
	if len(roots) == 0 {
		return "?", nil, true
	}
	key := ""
	var md map[string][]string
	for n, root := range roots {
		if root < 0 {
			return "?", nil, true
		}
		h := s.in.InputHeaders[root]
		m := map[string][]string{}
		var b strings.Builder
		for _, k := range s.keys {
			v := slices.Clone(h[k])
			if v == nil {
				v = []string{}
			}
			m[k] = v
			b.WriteString(strconv.Quote(k))
			for _, x := range v {
				b.WriteString("," + strconv.Quote(x))
			}
			b.WriteString(";")
		}
		if n == 0 {
			key, md = b.String(), m
		} else if b.String() != key {
			return "?", nil, true
		}
	}
	return key, md, false
}

// report returns the batchers for the report (none without metadata_keys)
// and the notes the replay adds.
func (s *shards) report(notes []string) ([]BatchBatcher, []string) {
	if len(s.keys) == 0 {
		return nil, notes
	}
	out := make([]BatchBatcher, 0, len(s.list))
	unknown := 0
	for _, sh := range s.list {
		md := sh.metadata
		if md == nil {
			md = map[string][]string{}
		}
		out = append(out, BatchBatcher{Metadata: md, Unknown: sh.unknown, Refused: sh.refused, Evicted: sh.evicted, Arrivals: sh.arrivals, Items: sh.items, Flushes: sh.flushes})
		if sh.unknown {
			unknown += sh.arrivals
		}
	}
	if unknown > 0 {
		notes = append(notes, strconv.Itoa(unknown)+" requests could not be traced to the headers of scenario inputs (an earlier batch processor, an aggregating connector or records without lineage); they are shown in one batcher of unknown metadata")
	}
	if s.refused > 0 {
		notes = append(notes, strconv.Itoa(s.refused)+" requests carried a new combination of metadata values beyond metadata_cardinality_limit "+strconv.Itoa(s.limit)+"; the processor refuses them with a permanent error")
	}
	if s.evicted > 0 {
		notes = append(notes, strconv.Itoa(s.evicted)+" partitions were flushed and removed to stay within batch::partition::cache_size "+strconv.Itoa(s.in.Settings.CacheSize)+"; partitions queue_batch removes after batch::partition::idle_timeout are not replayed")
	}
	return out, notes
}

// inputRoots maps each arrival at the processor to the injected inputs
// (input.injected seqs) its records came from, or -1 for a record whose
// request context an earlier batch processor replaced or that an
// aggregating connector emitted.
func inputRoots(in BatchInput) map[int][]int {
	out := map[int][]int{}
	for _, l := range in.Lineage {
		for hi, h := range l.Hops {
			if h.Kind != instrument.KindIngress || h.Component != in.Processor || h.Pipeline != in.Pipeline {
				continue
			}
			root := -1
			if l.Origin == "" {
				root = traceRoot(l.Hops, hi, in)
			}
			if !slices.Contains(out[h.Seq], root) {
				out[h.Seq] = append(out[h.Seq], root)
			}
		}
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

// traceRoot walks a hop back to the injection, returning its seq, or -1
// when an earlier batch processor lies on the way: it sends with a new
// request context.
func traceRoot(hops []Hop, i int, in BatchInput) int {
	for steps := 0; steps <= len(hops); steps++ {
		h := hops[i]
		if h.Parent < 0 || h.Parent >= len(hops) {
			return h.Seq
		}
		if h.Kind == instrument.KindEgress && in.BatchProcessors[h.Component] && !(h.Component == in.Processor && h.Pipeline == in.Pipeline) {
			return -1
		}
		i = h.Parent
	}
	return -1
}
