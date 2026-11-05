// Package metrics provides optional, standard-library-only metric primitives.
// Callers own metric names and must use only fixed-cardinality dimensions.
package metrics

import (
	"fmt"
	"io"
	"sync"
	"time"
)

var bounds = [...]float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2, 5, 10}

// Duration is a zero-value-ready histogram. It retains no individual samples.
type Duration struct {
	mu      sync.Mutex
	count   uint64
	sum     float64
	buckets [len(bounds)]uint64
}

type Bucket struct {
	LE    float64 `json:"le"`
	Count uint64  `json:"count"`
}
type Snapshot struct {
	Count      uint64   `json:"count"`
	SumSeconds float64  `json:"sum_seconds"`
	Buckets    []Bucket `json:"buckets"`
}

func (d *Duration) Observe(elapsed time.Duration) {
	seconds := max(0, elapsed.Seconds())
	d.mu.Lock()
	defer d.mu.Unlock()
	d.count++
	d.sum += seconds
	for i, bound := range bounds {
		if seconds <= bound {
			d.buckets[i]++
		}
	}
}
func (d *Duration) Snapshot() Snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := Snapshot{Count: d.count, SumSeconds: d.sum, Buckets: make([]Bucket, len(bounds))}
	for i, bound := range bounds {
		s.Buckets[i] = Bucket{LE: bound, Count: d.buckets[i]}
	}
	return s
}

// Write emits Prometheus text using a compile-time metric name supplied by the
// owning service. It performs no network or storage operations.
func (s Snapshot) Write(w io.Writer, name string) {
	fmt.Fprintf(w, "# TYPE %s histogram\n", name)
	for _, b := range s.Buckets {
		fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", name, b.LE, b.Count)
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n", name, s.Count, name, s.SumSeconds, name, s.Count)
}
