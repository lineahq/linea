package platform

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// TestRetentionEvictsTheOldestUnprotectedRuns is the retention gate:
// filled past its policy, a service refuses no admission, and a retention
// pass evicts the oldest runs while kept and referenced ones stay. The
// system principal audits the pass with a count per reason.
func TestRetentionEvictsTheOldestUnprotectedRuns(t *testing.T) {
	ctx := context.Background()
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, "completed"), nil
	}
	s := openPlatform(t, memory.NewData(), Options{Prepare: unprepared, Execute: execute, Retention: RetentionPolicy{Runs: 5}})
	repository := s.repository.(artifact.Retention)
	var ids []string
	for n := 0; n < 13; n++ {
		ids = append(ids, waitRun(t, submit(t, s, request("x"))).ID)
		if n != 2 {
			continue
		}
		// The second run is kept and the third is a saved artifact's origin.
		if err := repository.KeepRun(ctx, ids[1], true); err != nil {
			t.Fatal(err)
		}
		if _, err := s.repository.Save(ctx, artifact.SaveRequest{WorkspaceID: s.repository.Info().ID, Project: artifact.DefaultProject, Kind: artifact.Scenario,
			Actor: "user:test", Content: []byte("from a run"), Origin: &artifact.Origin{Kind: "run", BaseDigest: strings.Repeat("a", 64), RunID: ids[2]}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.evictRuns(ctx, repository); err != nil {
		t.Fatal(err)
	}
	// Thirteen runs against a bound of five: the eight oldest unprotected go.
	kept := []string{ids[1], ids[2], ids[10], ids[11], ids[12]}
	for i, id := range ids {
		_, err := s.GetRun(ctx, testCaller, id)
		switch {
		case slices.Contains(kept, id) && err != nil:
			t.Errorf("run %d was evicted: %v", i, err)
		case !slices.Contains(kept, id) && (codeOf(err) != CodeNotFound || err.Error() != "run was deleted"):
			t.Errorf("run %d = %v, want deleted", i, err)
		}
	}
	events, err := s.auditLog.AuditEvents(ctx, artifact.AuditQuery{Operation: "EvictRuns", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	evicted := 0
	for _, e := range events {
		if e.Principal != "system" || e.PrincipalKind != "system" || e.Code != "" {
			t.Errorf("audit event = %+v", e)
		}
		for _, r := range e.Resources {
			n, err := strconv.Atoi(strings.TrimPrefix(r, "count:"))
			if err != nil {
				t.Fatalf("audit resource %q", r)
			}
			evicted += n
		}
	}
	if evicted != 8 {
		t.Errorf("audit records %d evicted runs in %+v", evicted, events)
	}
	// A pass with nothing due records nothing.
	if err := s.evictRuns(ctx, repository); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.auditLog.AuditEvents(ctx, artifact.AuditQuery{Operation: "EvictRuns", Limit: 10}); len(again) != len(events) {
		t.Errorf("an empty pass was audited: %+v", again)
	}
}

func retentionPlatform(t *testing.T) *Service {
	t.Helper()
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Principal: testCaller.Principal.ID}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Group: "engineers"}, Role: "engineer", Scope: ScopeAll},
		{Subject: Subject{Group: "readers"}, Role: "viewer", Scope: ScopeAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, "completed"), nil
	}
	return openPlatform(t, memory.NewData(), Options{Access: access, Prepare: unprepared, Execute: execute})
}

// TestOnlyOwnersAndAdminsChangeRuns: a run's owner may keep, unkeep and
// delete it; another engineer may not; an admin may; a viewer may do none
// of these, and only an admin prunes.
func TestOnlyOwnersAndAdminsChangeRuns(t *testing.T) {
	ctx := context.Background()
	s := retentionPlatform(t)
	alice := Caller{Principal: Principal{ID: "user:alice", Kind: PrincipalUser, Groups: []string{"engineers"}}, Client: "http"}
	bob := Caller{Principal: Principal{ID: "user:bob", Kind: PrincipalUser, Groups: []string{"engineers"}}, Client: "http"}
	vera := Caller{Principal: Principal{ID: "user:vera", Kind: PrincipalUser, Groups: []string{"readers"}}, Client: "http"}
	run, err := s.SubmitRun(ctx, alice, RunRequest{Mode: ModeQuick, Config: Source{Text: "# config"}, Quick: &QuickInput{Fixture: Source{Text: `{}`}}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.handle(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := waitRun(t, h).ID
	for name, c := range map[string]Caller{"another engineer": bob, "a viewer": vera} {
		if _, err := s.KeepRun(ctx, c, id); ErrorCode(err) != CodeForbidden {
			t.Errorf("%s keeps: %v", name, err)
		}
		if err := s.DeleteRun(ctx, c, id); ErrorCode(err) != CodeForbidden {
			t.Errorf("%s deletes: %v", name, err)
		}
		if _, err := s.PruneRuns(ctx, c, RunPruneRequest{DryRun: true}); ErrorCode(err) != CodeForbidden {
			t.Errorf("%s prunes: %v", name, err)
		}
	}
	if kept, err := s.KeepRun(ctx, alice, id); err != nil || !kept.Kept {
		t.Fatalf("owner keeps: %+v, %v", kept, err)
	}
	if got, err := s.GetRun(ctx, alice, id); err != nil || !got.Kept {
		t.Fatalf("kept run reads %+v, %v", got, err)
	}
	if kept, err := s.UnkeepRun(ctx, testCaller, id); err != nil || kept.Kept {
		t.Fatalf("admin unkeeps: %+v, %v", kept, err)
	}
	if _, err := s.KeepRun(ctx, alice, "run-unknown"); ErrorCode(err) != CodeNotFound {
		t.Errorf("keeping an unknown run: %v", err)
	}
	if err := s.DeleteRun(ctx, alice, id); err != nil {
		t.Fatalf("owner deletes: %v", err)
	}
}

// TestPruneDryRunMatchesThePrune is the prune gate: a dry run
// reports exactly what the prune then removes, kept runs stay, and the
// audit records both with their counts.
func TestPruneDryRunMatchesThePrune(t *testing.T) {
	ctx := context.Background()
	s := retentionPlatform(t)
	var ids []string
	for n := 0; n < 4; n++ {
		ids = append(ids, waitRun(t, submit(t, s, request("x"))).ID)
	}
	if _, err := s.KeepRun(ctx, testCaller, ids[1]); err != nil {
		t.Fatal(err)
	}
	if none, err := s.PruneRuns(ctx, testCaller, RunPruneRequest{Before: time.Now().Add(-time.Hour)}); err != nil || none.Runs != 0 {
		t.Fatalf("pruning older runs = %+v, %v", none, err)
	}
	if _, err := s.PruneRuns(ctx, testCaller, RunPruneRequest{RunFilter: RunFilter{Result: "unknown"}}); ErrorCode(err) != CodeInvalidRequest {
		t.Errorf("bad result filter: %v", err)
	}
	dry, err := s.PruneRuns(ctx, testCaller, RunPruneRequest{DryRun: true})
	if err != nil || dry.Runs != 3 || dry.Bytes <= 0 || !dry.DryRun {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	if _, err := s.GetRun(ctx, testCaller, ids[0]); err != nil {
		t.Fatalf("a dry run deleted a run: %v", err)
	}
	usage, err := s.GetStorageUsage(ctx, testCaller)
	if err != nil || usage.Runs != 4 || usage.KeptRuns != 1 || usage.OldestRun == nil {
		t.Fatalf("usage before = %+v, %v", usage, err)
	}
	pruned, err := s.PruneRuns(ctx, testCaller, RunPruneRequest{})
	if err != nil || pruned.Runs != dry.Runs || pruned.Bytes != dry.Bytes || pruned.DryRun {
		t.Fatalf("prune = %+v, %v; dry run said %+v", pruned, err, dry)
	}
	for i, id := range ids {
		_, err := s.GetRun(ctx, testCaller, id)
		if (i == 1) != (err == nil) {
			t.Errorf("run %d after prune: %v", i, err)
		}
	}
	if after, err := s.GetStorageUsage(ctx, testCaller); err != nil || after.Runs != 1 || after.Used >= usage.Used {
		t.Errorf("usage after = %+v, %v; before %+v", after, err, usage)
	}
	events, err := s.auditLog.AuditEvents(ctx, artifact.AuditQuery{Operation: "PruneRuns", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var dryRuns, prunes int
	for _, e := range events {
		if !slices.Contains(e.Resources, "runs:3") {
			continue
		}
		if !slices.Contains(e.Resources, fmt.Sprintf("bytes:%d", dry.Bytes)) {
			t.Errorf("audit event = %+v", e)
		}
		if slices.Contains(e.Resources, "dry_run") {
			dryRuns++
		} else {
			prunes++
		}
	}
	if dryRuns != 1 || prunes != 1 {
		t.Errorf("audit records %d dry runs and %d prunes in %+v", dryRuns, prunes, events)
	}
}

// leadingStore is shared in-memory upkeep whose lead the test sets.
type leadingStore struct {
	*memory.Store
	lead atomic.Bool
}

func (l *leadingStore) Lead(context.Context) (bool, error) { return l.lead.Load(), nil }

func (l *leadingStore) Housekeep(context.Context, time.Time) (bool, error) {
	return l.lead.Load(), nil
}

// TestOnlyTheUpkeepLeaderEvictsRuns: with shared upkeep, a retention pass
// on an instance that does not lead evicts nothing.
func TestOnlyTheUpkeepLeaderEvictsRuns(t *testing.T) {
	ctx := context.Background()
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, "completed"), nil
	}
	store := &leadingStore{Store: memory.Open(memory.NewData(), artifact.Limits{})}
	s, err := Open(store, Options{Prepare: unprepared, Execute: execute, Retention: RetentionPolicy{Runs: 1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var ids []string
	for range 3 {
		ids = append(ids, waitRun(t, submit(t, s, request("x"))).ID)
	}
	if err := s.retentionPass(ctx, store); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		if _, err := s.GetRun(ctx, testCaller, id); err != nil {
			t.Errorf("run %d was evicted without the lead: %v", i, err)
		}
	}
	store.lead.Store(true)
	if err := s.retentionPass(ctx, store); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids[:2] {
		if _, err := s.GetRun(ctx, testCaller, id); codeOf(err) != CodeNotFound {
			t.Errorf("run %d = %v, want evicted by the leader", i, err)
		}
	}
}

// TestAnotherInstancesAdmissionNudgesRetention: an instance that reads
// another's admission on the change feed asks for a retention pass, so the
// leader evicts without an admission of its own.
func TestAnotherInstancesAdmissionNudgesRetention(t *testing.T) {
	data := memory.NewData()
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, "completed"), nil
	}
	// a has no policy of its own, so only b's pass can evict; the run a
	// admits before b opens reaches b only as stored history.
	a := openPlatform(t, data, Options{Shared: true, Prepare: unprepared, Execute: execute})
	first := waitRun(t, submit(t, a, request("x"))).ID
	b := openPlatform(t, data, Options{Shared: true, Prepare: unprepared, Execute: execute, Retention: RetentionPolicy{Runs: 1}})
	waitRun(t, submit(t, a, request("x")))
	waitFor(t, "b's pass after a's admission", func() bool {
		_, err := b.GetRun(context.Background(), testCaller, first)
		return codeOf(err) == CodeNotFound
	})
}

// TestDeletedIDsAreForgottenAfterDeletionMemory bounds the deleted IDs a
// service remembers by the time since their deletion.
func TestDeletedIDsAreForgottenAfterDeletionMemory(t *testing.T) {
	d := newDeletedIDs()
	now := time.Now()
	d.add("run-old", now.Add(-deletionMemory-time.Minute))
	if d.has("run-old") {
		t.Error("an ID deleted before deletionMemory was remembered")
	}
	d.add("run-recent", now.Add(-time.Hour))
	d.at["run-stale"] = now.Add(-deletionMemory - time.Hour)
	d.pruned = now.Add(-2 * time.Hour)
	d.add("run-new", now)
	if !d.has("run-recent") || !d.has("run-new") {
		t.Errorf("recent IDs forgotten: %v", d.at)
	}
	if d.has("run-stale") {
		t.Error("a stale ID survived the prune")
	}
}
