package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDurationCumulativeBucketsAndOverflow(t *testing.T) {
	var d Duration
	d.Observe(time.Millisecond)
	d.Observe(3 * time.Millisecond)
	d.Observe(12 * time.Second)
	s := d.Snapshot()
	if s.Count != 3 || s.Buckets[0].Count != 1 || s.Buckets[1].Count != 2 || s.Buckets[len(s.Buckets)-1].Count != 2 {
		t.Fatal("histogram lost boundary or overflow observation")
	}
	var out bytes.Buffer
	s.Write(&out, "test_seconds")
	if !strings.Contains(out.String(), "test_seconds_bucket{le=\"+Inf\"} 3") || !strings.Contains(out.String(), "test_seconds_count 3") {
		t.Fatal("invalid histogram exposition")
	}
}
