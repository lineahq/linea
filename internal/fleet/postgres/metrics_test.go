package postgres

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

func TestReplicaMetricsNeverQueryStorage(t *testing.T) {
	s := &Store{}
	s.heartbeat.Store(time.Now().UnixNano())
	s.leader.Store(true)
	s.batches.Add(2)
	s.batchItems.Add(7)
	s.takeovers.Add(1)
	var b strings.Builder
	s.writeReplicaMetrics(&b)
	for _, want := range []string{"linea_fleet_writer_queue 0\n", "linea_fleet_writer_batch_items_total 7\n", "linea_fleet_replica_leader 1\n", "linea_fleet_agent_takeovers_total 1\n"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q in %s", want, b.String())
		}
	}
}

func TestCursorRoundTripAndV1Expiry(t *testing.T) {
	epoch, p, err := parseCursor(cursor("abc", 42, 7))
	if err != nil || epoch != "abc" || p != (position{xid: 42, id: 7}) {
		t.Fatalf("round trip: %s %+v %v", epoch, p, err)
	}
	// base64url("abc:7"), the v1 format
	if _, _, err := parseCursor("YWJjOjc"); !errors.Is(err, fleet.ErrCursorExpired) {
		t.Fatalf("v1 cursor: %v", err)
	}
	if _, _, err := parseCursor("!!"); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatalf("garbage cursor: %v", err)
	}
	if !(position{1, 9}).less(position{2, 1}) || (position{2, 1}).less(position{2, 1}) {
		t.Fatal("position order")
	}
}
