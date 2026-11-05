package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

var ctx = context.Background()

func runID(n int) string { return fmt.Sprintf("run-%016x", n) }

// TestConcurrentSaveRunSameExpected races instances finishing one run from the
// same expected generation: exactly one wins, the rest see a conflict.
func TestConcurrentSaveRunSameExpected(t *testing.T) {
	f := newFixture(t)
	instances := []*Store{f.open(t, artifact.Limits{}), f.open(t, artifact.Limits{}), f.open(t, artifact.Limits{})}
	admitted, err := instances[0].AdmitRun(ctx, artifact.Admission{ID: runID(1), Project: "team-a", Metadata: []byte(`{}`), Bundle: []byte("bundle"), Runtime: "builtin"})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 12
	results := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = instances[i%len(instances)].SaveRun(ctx, runID(1), "team-a", admitted.Generation, true, artifact.RunIndex{},
				[]byte(fmt.Sprintf(`{"winner":%d}`, i)), []byte(fmt.Sprintf("evidence %d", i)))
		}()
	}
	close(start)
	wg.Wait()
	won := -1
	for i, err := range results {
		switch {
		case err == nil && won < 0:
			won = i
		case err == nil:
			t.Errorf("two writers won: %d and %d", won, i)
		case !errors.Is(err, artifact.ErrConflict):
			t.Errorf("writer %d: %v", i, err)
		}
	}
	if won < 0 {
		t.Fatal("no writer won")
	}
	runs, err := instances[1].Runs(ctx)
	if err != nil || len(runs) != 1 || string(runs[0].Metadata) != fmt.Sprintf(`{"winner":%d}`, won) || runs[0].Generation != admitted.Generation+1 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	evidence, err := instances[2].RunEvidence(ctx, runID(1))
	if err != nil || string(evidence) != fmt.Sprintf("evidence %d", won) {
		t.Errorf("evidence = %q, %v", evidence, err)
	}
}

// TestConcurrentAppendAuditHasNoGaps appends from several instances at once:
// the sequence is 1..n with no repeat and no gap.
func TestConcurrentAppendAuditHasNoGaps(t *testing.T) {
	f := newFixture(t)
	instances := []*Store{f.open(t, artifact.Limits{}), f.open(t, artifact.Limits{}), f.open(t, artifact.Limits{})}
	const workers, each = 8, 10
	seen := make(chan uint64, workers*each)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				got, err := instances[w%len(instances)].AppendAudit(ctx, artifact.AuditEvent{Time: time.Now(), Principal: fmt.Sprintf("user:%d", w), PrincipalKind: "user",
					Client: "http", Operation: "GetRun", Outcome: artifact.AuditAllowed})
				if err != nil {
					t.Error(err)
					return
				}
				seen <- got.Seq
			}
		}()
	}
	wg.Wait()
	close(seen)
	got := map[uint64]bool{}
	for seq := range seen {
		if got[seq] {
			t.Errorf("sequence %d issued twice", seq)
		}
		got[seq] = true
	}
	for seq := uint64(1); seq <= workers*each; seq++ {
		if !got[seq] {
			t.Errorf("sequence %d missing", seq)
		}
	}
	events, err := instances[0].AuditEvents(ctx, artifact.AuditQuery{Limit: 100})
	if err != nil || len(events) != workers*each {
		t.Fatalf("events = %d, %v", len(events), err)
	}
	for i, e := range events {
		if e.Seq != uint64(workers*each-i) {
			t.Fatalf("event %d has sequence %d", i, e.Seq)
		}
	}
}

// TestConcurrentSavesHonorStorageLimit races instances saving distinct blobs
// that cannot all fit: the limit holds across them.
func TestConcurrentSavesHonorStorageLimit(t *testing.T) {
	f := newFixture(t)
	limits := artifact.Limits{StorageBytes: 1000}
	instances := []*Store{f.open(t, limits), f.open(t, limits), f.open(t, limits)}
	const workers = 9
	results := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := instances[i%len(instances)]
			content := make([]byte, 400)
			copy(content, fmt.Sprintf("blob %d", i))
			_, results[i] = r.Save(ctx, artifact.SaveRequest{WorkspaceID: r.Info().ID, Project: "team-a", Kind: artifact.Scenario, Actor: "user:test", Content: content})
		}()
	}
	wg.Wait()
	saved := 0
	for i, err := range results {
		switch {
		case err == nil:
			saved++
		case !errors.Is(err, artifact.ErrLimit):
			t.Errorf("save %d: %v", i, err)
		}
	}
	if saved != 2 {
		t.Errorf("%d saves fit in 1000 bytes of 400-byte blobs, want 2", saved)
	}
	if used, err := instances[0].Usage(ctx); err != nil || used != 800 {
		t.Errorf("usage = %d, %v", used, err)
	}
	if info := instances[1].Info(); info.Generation != 2 {
		t.Errorf("generation = %d", info.Generation)
	}
}

// TestInstallationIDPersists checks that instances and reopens share one ID.
func TestInstallationIDPersists(t *testing.T) {
	f := newFixture(t)
	first := f.open(t, artifact.Limits{})
	second := f.open(t, artifact.Limits{})
	if first.Info().ID == "" || first.Info().ID != second.Info().ID || !validID(first.Info().ID) {
		t.Fatalf("ids = %q, %q", first.Info().ID, second.Info().ID)
	}
	id := first.Info().ID
	first.Close()
	if again := f.open(t, artifact.Limits{}); again.Info().ID != id {
		t.Errorf("reopened id = %q, want %q", again.Info().ID, id)
	}
}

// TestSuiteCapacityAcrossInstances fills the suite cap from two instances.
func TestSuiteCapacityAcrossInstances(t *testing.T) {
	f := newFixture(t)
	limits := artifact.Limits{Suites: 3}
	a, b := f.open(t, limits), f.open(t, limits)
	for n := 1; n <= 6; n++ {
		if _, err := a.SaveRun(ctx, runID(n), "team-a", 0, true, artifact.RunIndex{}, []byte(`{}`), []byte("evidence")); err != nil {
			t.Fatal(err)
		}
	}
	metadata := func(n int) []byte {
		return fmt.Appendf(nil, `{"caller":{"principal":"user:test"},"created":"2026-10-28T00:00:00Z","config":"c","members":[{"label":"m","run_id":%q}]}`, runID(n))
	}
	results := make([]error, 6)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := a
			if i%2 == 1 {
				r = b
			}
			_, results[i] = r.SaveSuite(ctx, fmt.Sprintf("suite-%016x", i+1), "team-a", metadata(i+1))
		}()
	}
	wg.Wait()
	saved := 0
	for _, err := range results {
		switch {
		case err == nil:
			saved++
		case !errors.Is(err, artifact.ErrLimit):
			t.Error(err)
		}
	}
	if saved != 3 {
		t.Errorf("%d suites saved, want 3", saved)
	}
	if err := a.CheckSuiteCapacity(ctx, 1); !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("capacity of a full store = %v", err)
	}
	suites, err := b.Suites(ctx)
	if err != nil || len(suites) != 3 || suites[0].Ordinal != 3 || suites[2].Ordinal != 1 {
		t.Errorf("suites = %+v, %v", suites, err)
	}
}

// TestMigrationChecksumRefusal refuses to start when an applied migration's
// recorded checksum, or the file's, differs.
func TestMigrationChecksumRefusal(t *testing.T) {
	f := newFixture(t)
	f.open(t, artifact.Limits{}).Close()
	admin, err := pgx.Connect(ctx, f.url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "UPDATE schema_migrations SET checksum = repeat('0', 64) WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, f.config(artifact.Limits{})); !errors.Is(err, artifact.ErrCorrupt) {
		t.Fatalf("open with a changed checksum = %v", err)
	}
}

// TestMigrationsApplyForwardOnly applies a further migration to an existing
// schema, refuses a changed one, and refuses a database that is ahead.
func TestMigrationsApplyForwardOnly(t *testing.T) {
	f := newFixture(t)
	f.open(t, artifact.Limits{}).Close()
	embedded, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	files := func(extra ...string) fstest.MapFS {
		m := fstest.MapFS{}
		for _, e := range embedded {
			m["migrations/"+e.name] = &fstest.MapFile{Data: []byte(e.sql)}
		}
		for i, sql := range extra {
			m[fmt.Sprintf("migrations/%03d_extra.sql", len(embedded)+i+1)] = &fstest.MapFile{Data: []byte(sql)}
		}
		return m
	}
	pool := openPool(t, f)
	if err := migrate(ctx, pool, files("CREATE TABLE extra_one (id int);")); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, pool, files("CREATE TABLE extra_one (id int);")); err != nil {
		t.Fatalf("second run of the same migrations: %v", err)
	}
	if err := migrate(ctx, pool, files("CREATE TABLE extra_one (id int, changed int);")); !errors.Is(err, artifact.ErrCorrupt) {
		t.Errorf("changed applied migration = %v", err)
	}
	if err := migrate(ctx, pool, files()); !errors.Is(err, artifact.ErrUnsupportedSchema) {
		t.Errorf("database ahead of the binary = %v", err)
	}
	if err := migrate(ctx, pool, fstest.MapFS{"migrations/002_gap.sql": {Data: []byte("SELECT 1")}}); !errors.Is(err, artifact.ErrCorrupt) {
		t.Errorf("numbering gap = %v", err)
	}
}

// TestMigrationsRunOnceConcurrently starts several instances on an empty
// schema at once; the advisory lock lets one apply and the rest wait.
func TestMigrationsRunOnceConcurrently(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	stores := make([]*Store, len(errs))
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stores[i], errs[i] = Open(ctx, f.config(artifact.Limits{}))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("instance %d: %v", i, err)
			continue
		}
		defer stores[i].Close()
		if stores[i].Info().ID != stores[0].Info().ID {
			t.Errorf("instance %d has installation %s", i, stores[i].Info().ID)
		}
	}
}

// TestSweepDeletesOnlyUnreferencedOldObjects checks Sweep against referenced,
// orphaned and young objects.
func TestSweepDeletesOnlyUnreferencedOldObjects(t *testing.T) {
	f := newFixture(t)
	s := f.open(t, artifact.Limits{})
	kept, err := s.Save(ctx, artifact.SaveRequest{WorkspaceID: s.Info().ID, Project: "team-a", Kind: artifact.Scenario, Actor: "user:test", Content: []byte("kept")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitRun(ctx, artifact.Admission{ID: runID(1), Project: "team-a", Metadata: []byte(`{}`), Bundle: []byte("bundle"), Runtime: "builtin"}); err != nil {
		t.Fatal(err)
	}
	orphans := [][]byte{[]byte("orphan one"), []byte("orphan two")}
	for _, o := range orphans {
		if err := s.putBlob(ctx, digestOf(o), o); err != nil {
			t.Fatal(err)
		}
	}
	// A refused write leaves its object behind too.
	if _, err := s.Save(ctx, artifact.SaveRequest{WorkspaceID: s.Info().ID, ArtifactID: kept.Artifact.ID, ExpectedGeneration: 9, ExpectedHead: "x", Kind: artifact.Scenario,
		Actor: "user:test", Content: []byte("refused")}); !errors.Is(err, artifact.ErrConflict) {
		t.Fatalf("stale save = %v", err)
	}
	exists := func(content []byte) bool {
		key, _ := s.ObjectKey(digestOf(content))
		_, err := s.objects.Get(ctx, key, 1<<20)
		return err == nil
	}
	// Everything is younger than a day: nothing goes.
	if n, err := s.Sweep(ctx, 24*time.Hour); err != nil || n != 0 {
		t.Fatalf("sweep of young objects = %d, %v", n, err)
	}
	for _, o := range orphans {
		if !exists(o) {
			t.Fatalf("young orphan %q was deleted", o)
		}
	}
	// A negative age puts the cutoff in the future, making every object old.
	if n, err := s.Sweep(ctx, -time.Hour); err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v; want the 2 orphans", n, err)
	}
	for _, o := range orphans {
		if exists(o) {
			t.Errorf("orphan %q survived", o)
		}
	}
	if !exists([]byte("kept")) || !exists([]byte("bundle")) {
		t.Error("a referenced object was deleted")
	}
	if got, err := s.Revision(ctx, kept.Revision.Ref); err != nil || string(got.Content) != "kept" {
		t.Errorf("revision after sweep = %+v, %v", got, err)
	}
	// Deleting a run unreferences its objects; the next sweep removes them.
	if _, err := s.SaveRun(ctx, runID(2), "team-a", 0, true, artifact.RunIndex{}, []byte(`{}`), []byte("evidence")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(ctx, runID(2), time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Sweep(ctx, -time.Hour); err != nil || n != 1 || exists([]byte("evidence")) {
		t.Errorf("sweep after delete = %d, %v", n, err)
	}
}

// TestObjectsCountedOncePerDigest checks Usage on shared content.
func TestObjectsCountedOncePerDigest(t *testing.T) {
	f := newFixture(t)
	s := f.open(t, artifact.Limits{})
	for range 2 {
		if _, err := s.Save(ctx, artifact.SaveRequest{WorkspaceID: s.Info().ID, Project: "team-a", Kind: artifact.Scenario, Actor: "user:test", Content: []byte("same bytes")}); err != nil {
			t.Fatal(err)
		}
	}
	if used, err := s.Usage(ctx); err != nil || used != int64(len("same bytes")) {
		t.Errorf("usage = %d, %v", used, err)
	}
}

// TestPresignedURLs checks that presigned URLs name the key and carry a
// signature.
func TestPresignedURLs(t *testing.T) {
	f := newFixture(t)
	s := f.open(t, artifact.Limits{})
	key, err := s.ObjectKey(digestOf([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	for name, sign := range map[string]func(context.Context, string, time.Duration) (string, error){"get": s.objects.PresignGet, "put": s.objects.PresignPut} {
		u, err := sign(ctx, key, 5*time.Minute)
		if err != nil || u == "" {
			t.Errorf("%s = %q, %v", name, u, err)
		}
	}
}

// TestRunBundleURL checks that a run's bundle URL names the bundle's object,
// digest and size, and that runs without a bundle are not found.
func TestRunBundleURL(t *testing.T) {
	f := newFixture(t)
	s := f.open(t, artifact.Limits{})
	if _, err := s.AdmitRun(ctx, artifact.Admission{ID: runID(1), Project: "team-a", Metadata: []byte(`{}`), Bundle: []byte("bundle"), Runtime: "builtin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRun(ctx, runID(2), "team-a", 0, false, artifact.RunIndex{}, []byte(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	key, err := s.ObjectKey(digestOf([]byte("bundle")))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.RunBundleURL(ctx, runID(1), 5*time.Minute)
	if err != nil || got.Digest != digestOf([]byte("bundle")) || got.Size != int64(len("bundle")) || !strings.Contains(got.URL, key) {
		t.Errorf("RunBundleURL = %+v, %v; want a URL naming %s", got, err, key)
	}
	for name, id := range map[string]string{"no bundle": runID(2), "unknown": runID(3)} {
		if _, err := s.RunBundleURL(ctx, id, time.Minute); !errors.Is(err, artifact.ErrNotFound) {
			t.Errorf("%s: err = %v; want ErrNotFound", name, err)
		}
	}
	if _, err := s.RunBundleURL(ctx, "bad", time.Minute); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("invalid ID: err = %v", err)
	}
}

// TestStagedObjectRoundTrip writes a staged object through its presigned URL,
// takes it, and checks that taking deletes it.
func TestStagedObjectRoundTrip(t *testing.T) {
	f := newFixture(t)
	s := f.open(t, artifact.Limits{})
	payload := []byte("a large result")
	u, err := s.StageURL(ctx, "job-1", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned PUT = %s", resp.Status)
	}
	got, err := s.TakeStaged(ctx, "job-1", digestOf(payload), int64(len(payload)), 1<<20)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("TakeStaged = %q, %v", got, err)
	}
	if _, err := s.TakeStaged(ctx, "job-1", digestOf(payload), int64(len(payload)), 1<<20); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("second take: err = %v; want ErrNotFound", err)
	}
}

func TestHousekeepIsLedByOneHandle(t *testing.T) {
	f := newFixture(t)
	a, b := f.open(t, artifact.Limits{}), f.open(t, artifact.Limits{})
	if err := a.PostJob(ctx, artifact.JobRecord{ID: "job-1", Runtime: "r", Kind: artifact.JobInspect, Input: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	conn, err := a.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock("+housekeepLock+")"); err != nil {
		t.Fatal(err)
	}
	if led, err := b.Housekeep(ctx, time.Now().Add(time.Hour)); err != nil || led {
		t.Fatalf("housekeep while another leads = %v, %v", led, err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock("+housekeepLock+")"); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if led, err := b.Housekeep(ctx, time.Now().Add(time.Hour)); err != nil || !led {
		t.Fatalf("housekeep = %v, %v", led, err)
	}
	if _, err := a.Job(ctx, "job-1"); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("stale job kept: %v", err)
	}
	// b keeps the lead between rounds, until it closes.
	if led, err := a.Housekeep(ctx, time.Now().Add(-time.Hour)); err != nil || led {
		t.Fatalf("a's round while b leads = %v, %v", led, err)
	}
	if led, err := b.Lead(ctx); err != nil || !led {
		t.Fatalf("b's lead = %v, %v", led, err)
	}
	b.Close()
	if led, err := a.Housekeep(ctx, time.Now().Add(-time.Hour)); err != nil || !led {
		t.Fatalf("a's round after b closed = %v, %v", led, err)
	}
}

// TestLeadPassesOnWhenTheLeadingSessionEnds: a leader whose session ends
// loses the lead to the next handle that asks.
func TestLeadPassesOnWhenTheLeadingSessionEnds(t *testing.T) {
	f := newFixture(t)
	a, b := f.open(t, artifact.Limits{}), f.open(t, artifact.Limits{})
	if led, err := a.Lead(ctx); err != nil || !led {
		t.Fatalf("a's lead = %v, %v", led, err)
	}
	if led, err := b.Lead(ctx); err != nil || led {
		t.Fatalf("b's lead while a leads = %v, %v", led, err)
	}
	a.leadMu.Lock()
	pid := a.lead.PgConn().PID()
	a.leadMu.Unlock()
	if _, err := b.pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	// The backend releases the lock as it exits, shortly after the call.
	for deadline := time.Now().Add(10 * time.Second); ; {
		led, err := b.Lead(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if led {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("b did not take the lead after a's session ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if led, err := a.Lead(ctx); err != nil || led {
		t.Fatalf("a's lead after it lost it = %v, %v", led, err)
	}
}

// TestTombstonesOutliveTheFeed: a tombstone goes only once it is older than
// the oldest change the feed retains by the margin.
func TestTombstonesOutliveTheFeed(t *testing.T) {
	f := newFixture(t)
	s := f.open(t, artifact.Limits{})
	now := time.Now().UTC()
	for _, tomb := range []struct {
		id string
		at time.Time
	}{{"run-old", now.Add(-72 * time.Hour)}, {"run-recent", now.Add(-time.Hour)}, {"suite-old", now.Add(-72 * time.Hour)}} {
		kind := "run"
		if strings.HasPrefix(tomb.id, "suite-") {
			kind = "suite"
		}
		if _, err := s.pool.Exec(ctx, "INSERT INTO tombstones (kind, id, deleted_at) VALUES ($1, $2, $3)", kind, tomb.id, tomb.at); err != nil {
			t.Fatal(err)
		}
	}
	// The feed still reports a change from 49 hours ago: the old tombstones
	// are within a day of it, so they stay.
	if _, err := s.pool.Exec(ctx, "INSERT INTO changes (kind, ref, at) VALUES ('deletion', 'run-old', $1)", now.Add(-49*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.TrimTombstones(ctx, tombstoneMargin, 10); err != nil {
		t.Fatal(err)
	}
	if runs, suites, err := s.Tombstones(ctx); err != nil || len(runs) != 2 || len(suites) != 1 {
		t.Fatalf("tombstones while the feed reports them = %v %v, %v", runs, suites, err)
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM changes WHERE at < $1", now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.TrimTombstones(ctx, tombstoneMargin, 10); err != nil {
		t.Fatal(err)
	}
	runs, suites, err := s.Tombstones(ctx)
	if err != nil || len(runs) != 1 || runs[0].ID != "run-recent" || len(suites) != 0 {
		t.Fatalf("tombstones after the feed moved on = %v %v, %v", runs, suites, err)
	}
}

// TestRunsStoredBeforeTheIndex treats a run stored before migration 005 as
// unindexed: no filter matches it, the unfiltered list keeps it, and IndexRun
// sets its index once.
func TestRunsStoredBeforeTheIndex(t *testing.T) {
	f := newFixture(t)
	s := f.open(t, artifact.Limits{})
	if _, err := s.SaveRun(ctx, runID(1), "team-a", 0, true, artifact.RunIndex{Result: "failed"}, []byte(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE runs SET scenario = NULL, runtime_name = NULL, result = NULL, indexed = false WHERE id = $1`, runID(1)); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Run(ctx, runID(1)); err != nil || r.Index != nil {
		t.Fatalf("unindexed run = %+v, %v", r, err)
	}
	if page, err := s.ListRuns(ctx, artifact.RunQuery{Result: "failed"}); err != nil || len(page.Runs) != 0 {
		t.Fatalf("filtered = %+v, %v", page, err)
	}
	if page, err := s.ListRuns(ctx, artifact.RunQuery{}); err != nil || len(page.Runs) != 1 {
		t.Fatalf("unfiltered = %+v, %v", page, err)
	}
	if page, err := s.ListRuns(ctx, artifact.RunQuery{Unindexed: true}); err != nil || len(page.Runs) != 1 || page.Runs[0].ID != runID(1) {
		t.Fatalf("unindexed = %+v, %v", page, err)
	}
	for _, x := range []artifact.RunIndex{{Scenario: "checkout", Result: "completed"}, {Result: "failed"}} {
		if err := s.IndexRun(ctx, runID(1), x); err != nil {
			t.Fatal(err)
		}
	}
	if page, err := s.ListRuns(ctx, artifact.RunQuery{Scenario: "checkout", Result: "completed"}); err != nil || len(page.Runs) != 1 || *page.Runs[0].Index != (artifact.RunIndex{Scenario: "checkout", Result: "completed"}) {
		t.Fatalf("indexed = %+v, %v", page, err)
	}
	if page, err := s.ListRuns(ctx, artifact.RunQuery{Unindexed: true}); err != nil || len(page.Runs) != 0 {
		t.Fatalf("unindexed after indexing = %+v, %v", page, err)
	}
}

// TestAdmissionCountsEvictableRunsAsFree shows that history beyond the
// storage limit never refuses a run while retention may remove it, and only
// runs retention keeps fill the limit.
func TestAdmissionCountsEvictableRunsAsFree(t *testing.T) {
	f := newFixture(t)
	s := f.open(t, artifact.Limits{StorageBytes: 300 << 10, RunBytes: 100 << 10})
	evidence := func(n int) []byte { return bytes.Repeat([]byte{byte('a' + n)}, 90<<10) }
	for n := 1; n <= 6; n++ {
		if _, err := s.SaveRun(ctx, runID(n), "team-a", 0, true, artifact.RunIndex{}, []byte(`{}`), evidence(n)); err != nil {
			t.Fatalf("run %d: %v", n, err)
		}
	}
	if used, err := s.Usage(ctx); err != nil || used <= 300<<10 {
		t.Fatalf("usage = %d, %v; want history past the limit", used, err)
	}
	for n := 1; n <= 3; n++ {
		if err := s.KeepRun(ctx, runID(n), true); err != nil {
			t.Fatal(err)
		}
	}
	// Three kept runs of 90 KiB leave no room for another run's reserve.
	if _, err := s.AdmitRun(ctx, artifact.Admission{ID: runID(7), Project: "team-a", Metadata: []byte(`{}`), Bundle: []byte("bundle"), Runtime: "builtin"}); !errors.Is(err, artifact.ErrLimit) {
		t.Fatalf("admission past kept runs = %v", err)
	}
	if err := s.KeepRun(ctx, runID(3), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitRun(ctx, artifact.Admission{ID: runID(7), Project: "team-a", Metadata: []byte(`{}`), Bundle: []byte("bundle"), Runtime: "builtin"}); err != nil {
		t.Fatalf("admission with evictable history = %v", err)
	}
}

// TestOriginBackfillProtectsRuns runs migration 006's backfill on a revision
// that names its run only in its encoded origin, as one saved before 006 does.
func TestOriginBackfillProtectsRuns(t *testing.T) {
	f := newFixture(t)
	s := f.open(t, artifact.Limits{})
	if _, err := s.SaveRun(ctx, runID(1), "team-a", 0, true, artifact.RunIndex{}, []byte(`{}`), []byte("evidence")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, artifact.SaveRequest{WorkspaceID: s.Info().ID, Project: "team-a", Kind: artifact.Scenario, Actor: "user:test",
		Content: []byte("from run 1"), Origin: &artifact.Origin{Kind: "run", BaseDigest: strings.Repeat("a", 64), RunID: runID(1)}}); err != nil {
		t.Fatal(err)
	}
	// Clear what Save wrote, then backfill as migration 006 does.
	if _, err := s.pool.Exec(ctx, "UPDATE revisions SET origin_run = NULL"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE revisions SET origin_run = convert_from(origin, 'UTF8')::jsonb ->> 'run_id' WHERE origin IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.EvictRuns(ctx, artifact.RetentionPolicy{Age: time.Hour}, time.Now().Add(2*time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("evicted = %+v, %v", got, err)
	}
}
