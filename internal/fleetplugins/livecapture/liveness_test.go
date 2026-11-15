package livecapture

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/collector/livecapture/controlpb"
	"github.com/lineahq/linea/internal/fleetplugins/livecapture/translog"
	"github.com/lineahq/linea/pluginsdk"
)

// Database liveness is decided by one rule: three consecutive failed pings, or
// a lost connection at once. A busy connection is not a sample.
func TestStorageProbeOutcomes(t *testing.T) {
	failed := ErrUnavailable
	busy := ErrStorageBusy
	lost := ErrStorageLost
	tests := []struct {
		name   string
		probes []error
		fatal  int // 1-based probe that is fatal, 0 for none
	}{
		{"healthy", []error{nil, nil, nil, nil}, 0},
		{"one failed ping", []error{failed, nil, nil}, 0},
		{"two failed pings", []error{failed, failed, nil, nil}, 0},
		{"three consecutive failed pings", []error{failed, failed, failed}, 3},
		{"a success resets the count", []error{failed, failed, nil, failed, failed, nil}, 0},
		{"a busy queue is no sample", []error{busy, busy, busy, busy, busy, nil}, 0},
		{"busy neither resets nor counts", []error{failed, busy, failed, busy, failed}, 5},
		{"closed connection is fatal at once", []error{nil, lost}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			got := 0
			for i, err := range tt.probes {
				s.mu.Lock()
				fatal := s.observeStorageLocked(err)
				s.mu.Unlock()
				if fatal {
					got = i + 1
					break
				}
			}
			if got != tt.fatal {
				t.Fatalf("fatal at probe %d, want %d", got, tt.fatal)
			}
		})
	}
	if !errors.Is(ErrStorageBusy, ErrUnavailable) || !errors.Is(ErrStorageLost, ErrUnavailable) {
		t.Fatal("storage probe errors do not wrap ErrUnavailable")
	}
}

// Only the maintenance loop pings the database. An API request, a delivery
// authentication and a control wakeup read the flag it keeps.
func TestRequestsReadStorageFlagWithoutPinging(t *testing.T) {
	setResendInterval(t, time.Hour)
	s, request, _ := fixture(t)
	repo := s.repo.(*memoryRepo)
	repo.probes.Store(0)

	call := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+s.config.serviceToken)
		req.Header.Set(pluginsdk.CallerIDHeader, "owner")
		req.Header.Set(pluginsdk.CallerRoleHeader, "admin")
		req.Header.Set(pluginsdk.InstanceHeader, s.instance)
		req.Header.Set(pluginsdk.EpochHeader, s.epoch)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w.Code
	}
	for _, path := range []string{pluginsdk.ManifestPath, "/readyz", pluginsdk.APIPath + "v1/peers", pluginsdk.APIPath + "v1/metrics"} {
		if code := call(path); code != 200 {
			t.Fatalf("%s answered %d", path, code)
		}
	}
	if _, err := s.authenticate(bearer(strings.Repeat("d", 64))); err != nil {
		t.Fatal(err)
	}
	if got := repo.probes.Load(); got != 0 {
		t.Fatalf("requests pinged the database %d times", got)
	}

	// A control wakeup (a capture is created for the peer) sends the new desired
	// state without a ping.
	stream, _, send := watch(t, s)
	send(&controlpb.CollectorReport{})
	if first, err := stream.Recv(); err != nil || len(first.GetSessions()) != 0 {
		t.Fatalf("first desired state %v, %v", first, err)
	}
	id := identityKey("credential", Identity{Cluster: "cluster", Pod: "typed", Group: "test"})
	s.mu.Lock()
	p := s.peers[id]
	s.mu.Unlock()
	if p == nil {
		t.Fatal("peer not registered")
	}
	request.Targets = []TargetRequest{{p.info.ID, p.info.Generation}}
	if _, err := s.Create(context.Background(), "owner", request); err != nil {
		t.Fatal(err)
	}
	if next, err := stream.Recv(); err != nil || len(next.GetSessions()) != 1 {
		t.Fatalf("desired state after a change %v, %v", next, err)
	}
	if got := repo.probes.Load(); got != 0 {
		t.Fatalf("control wakeup pinged the database %d times", got)
	}

	// Once maintain declares the database fatal, the flag stops all three.
	s.mu.Lock()
	s.fatalLocked(translog.ReasonStorageFailures)
	s.mu.Unlock()
	if code := call(pluginsdk.APIPath + "v1/peers"); code != 503 {
		t.Fatalf("request after a fatal database answered %d", code)
	}
	if _, err := s.authenticate(bearer(strings.Repeat("d", 64))); err == nil {
		t.Fatal("authentication succeeded after a fatal database")
	}
}
