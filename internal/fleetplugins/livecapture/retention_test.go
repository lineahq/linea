package livecapture

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func peerID(n int) string { return fmt.Sprintf("%064x", n) }

// ageRepo moves the repository clock forward by d, so every peer it knows has
// been unused for d longer.
func ageRepo(r *memoryRepo, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	previous := r.now
	r.now = func() time.Time { return previous().Add(d) }
}

func peerCount(r *memoryRepo) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.peers)
}

func TestMaintainPeersDeletesOnlyUnusedPeers(t *testing.T) {
	ctx := context.Background()
	r := newMemoryRepo()
	for i := 1; i <= 5; i++ {
		if _, err := r.NextGeneration(ctx, peerID(i)); err != nil {
			t.Fatal(err)
		}
	}
	// 1 is idle, 2 is connected, 3 holds a lease, 4 is named by a retained
	// operation, 5 is idle.
	r.leases[peerID(3)] = "lease-op"
	r.ops["retained"] = Operation{ID: "retained", State: "stopped", Targets: []Target{{PeerID: peerID(4), Generation: 1}}}
	ageRepo(r, 25*time.Hour)
	deleted, err := r.MaintainPeers(ctx, []string{peerID(2)}, peerRetention, peerPruneBatch)
	if err != nil || deleted != 2 {
		t.Fatalf("deleted %d, %v", deleted, err)
	}
	for _, n := range []int{2, 3, 4} {
		if r.peers[peerID(n)] == nil {
			t.Fatalf("peer %d was deleted while in use", n)
		}
	}
	if r.peers[peerID(1)] != nil || r.peers[peerID(5)] != nil {
		t.Fatal("idle peers were kept")
	}
}

func TestMaintainPeersKeepsRecentPeersAndRefreshesConnectedOnes(t *testing.T) {
	ctx := context.Background()
	r := newMemoryRepo()
	_, _ = r.NextGeneration(ctx, peerID(1))
	_, _ = r.NextGeneration(ctx, peerID(2))
	ageRepo(r, 23*time.Hour)
	if deleted, _ := r.MaintainPeers(ctx, []string{peerID(1)}, peerRetention, peerPruneBatch); deleted != 0 {
		t.Fatal("a peer younger than the retention period was deleted")
	}
	// Peer 1 was refreshed 23 h in, so two hours later it has been unused for
	// 2 h; peer 2 has been unused for 25 h.
	ageRepo(r, 2*time.Hour)
	if deleted, _ := r.MaintainPeers(ctx, nil, peerRetention, peerPruneBatch); deleted != 1 || r.peers[peerID(1)] == nil || r.peers[peerID(2)] != nil {
		t.Fatal("refresh did not protect the connected peer")
	}
}

func TestMaintainPeersBoundsEachPass(t *testing.T) {
	ctx := context.Background()
	r := newMemoryRepo()
	for i := 1; i <= 7; i++ {
		_, _ = r.NextGeneration(ctx, peerID(i))
	}
	ageRepo(r, 48*time.Hour)
	for left := 7; left > 0; left -= 3 {
		deleted, err := r.MaintainPeers(ctx, nil, peerRetention, 3)
		if err != nil || deleted != min(3, left) {
			t.Fatalf("pass deleted %d with %d left, %v", deleted, left, err)
		}
	}
	if peerCount(r) != 0 {
		t.Fatal("peers remain")
	}
}

func TestDeletedPeerReturnsAboveEveryDeletedGeneration(t *testing.T) {
	ctx := context.Background()
	r := newMemoryRepo()
	var last uint64
	for i := 0; i < 3; i++ {
		last, _ = r.NextGeneration(ctx, peerID(1))
	}
	if last != 3 {
		t.Fatalf("generation %d", last)
	}
	ageRepo(r, 48*time.Hour)
	if deleted, _ := r.MaintainPeers(ctx, nil, peerRetention, peerPruneBatch); deleted != 1 {
		t.Fatal("peer not deleted")
	}
	// The same peer and a different new one both start above the old maximum.
	for _, id := range []string{peerID(1), peerID(2)} {
		gen, err := r.NextGeneration(ctx, id)
		if err != nil || gen <= last {
			t.Fatalf("generation %d repeats one that was issued before (%d), %v", gen, last, err)
		}
	}
}

func TestPeerCapacityIsFreedByRetention(t *testing.T) {
	ctx := context.Background()
	r := newMemoryRepo()
	for i := 0; i < 10000; i++ {
		if _, err := r.NextGeneration(ctx, peerID(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.NextGeneration(ctx, peerID(10000)); !errors.Is(err, ErrCapacity) {
		t.Fatal("full table accepted a new peer")
	}
	ageRepo(r, 25*time.Hour)
	if deleted, _ := r.MaintainPeers(ctx, nil, peerRetention, peerPruneBatch); deleted != peerPruneBatch {
		t.Fatalf("deleted %d, want one full batch", deleted)
	}
	if _, err := r.NextGeneration(ctx, peerID(10000)); err != nil {
		t.Fatal("a pass did not make room", err)
	}
}

type recordingPeerRepo struct {
	*memoryRepo
	err       error
	connected []string
	idle      time.Duration
	limit     int
	passes    chan struct{}
}

func (r *recordingPeerRepo) MaintainPeers(ctx context.Context, connected []string, idle time.Duration, limit int) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	r.connected, r.idle, r.limit = connected, idle, limit
	n, err := r.memoryRepo.MaintainPeers(ctx, connected, idle, limit)
	if r.passes != nil {
		select {
		case r.passes <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestRetirePeersPassesConnectedPeersAndSurvivesFailure(t *testing.T) {
	s, _, p := fixture(t)
	repo := &recordingPeerRepo{memoryRepo: s.repo.(*memoryRepo)}
	s.repo = repo
	s.peerIdle = time.Hour
	repo.err = ErrUnavailable
	if s.retirePeers() != 0 || !s.dbOK {
		t.Fatal("a failed pass must neither report deletions nor end the plugin")
	}
	repo.err = nil
	s.retirePeers()
	if len(repo.connected) != 1 || repo.connected[0] != p.info.ID || repo.idle != time.Hour || repo.limit != peerPruneBatch {
		t.Fatalf("pass arguments: %v %v %v", repo.connected, repo.idle, repo.limit)
	}
}

func TestMaintenanceLoopRetiresIdlePeers(t *testing.T) {
	s, _, _ := fixture(t)
	memory := s.repo.(*memoryRepo)
	if _, err := memory.NextGeneration(context.Background(), peerID(9)); err != nil {
		t.Fatal(err)
	}
	ageRepo(memory, 48*time.Hour)
	repo := &recordingPeerRepo{memoryRepo: memory, passes: make(chan struct{}, 1)}
	s.repo = repo
	s.wg.Add(1)
	go s.maintain()
	select {
	case <-repo.passes:
	case <-time.After(5 * time.Second):
		t.Fatal("the maintenance loop never ran a peer pass")
	}
	s.cancel()
	s.wg.Wait()
	if peerCount(memory) != 0 {
		t.Fatal("the idle peer survived the pass")
	}
}

func TestRuntimeGrantsAllowPeerDeleteOnly(t *testing.T) {
	privileges := func(table string) []string {
		var found []string
		for _, statement := range strings.Split(runtimeGrants(`"role"`), ";") {
			statement = strings.TrimSpace(statement)
			grant, target, ok := strings.Cut(strings.TrimPrefix(statement, "GRANT "), " ON ")
			if ok && strings.HasPrefix(target, table+" TO ") {
				found = append(found, grant)
			}
		}
		return found
	}
	contains := func(list []string, want string) bool {
		for _, item := range list {
			if strings.Contains(item, want) {
				return true
			}
		}
		return false
	}
	if !contains(privileges("capture.peers"), "DELETE") || !contains(privileges("capture.leases"), "DELETE") {
		t.Fatal("the runtime role cannot delete peers and leases")
	}
	for _, table := range []string{"capture.operations", "capture.metadata", "capture.schema_version"} {
		if contains(privileges(table), "DELETE") {
			t.Fatalf("runtime role may delete from %s", table)
		}
	}
	if !contains(privileges("capture.metadata"), "UPDATE (generation_floor)") {
		t.Fatal("the runtime role cannot raise the generation floor")
	}
	for _, grant := range privileges("capture.metadata") {
		if strings.HasPrefix(grant, "UPDATE") && grant != "UPDATE (generation_floor)" || strings.Contains(grant, "SELECT,INSERT,UPDATE") {
			t.Fatalf("metadata update is wider than generation_floor: %s", grant)
		}
	}
}

func TestPeerRetentionMigrationIsVersionFour(t *testing.T) {
	if len(migrations) != 4 || migrations[3] != peerRetentionMigration {
		t.Fatal("the peer retention migration must be version 4")
	}
	for _, want := range []string{"capture.peers ADD COLUMN seen_at", "capture.metadata ADD COLUMN generation_floor"} {
		if !strings.Contains(migrations[3], want) {
			t.Fatalf("migration 4 lacks %q", want)
		}
	}
}
