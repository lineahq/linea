package contract

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// poolBinary is a run binary of platform for the payload name.
func poolBinary(name, platform string) artifact.RunBinary {
	return artifact.RunBinary{Digest: digestOf(name), Bytes: int64(len(name)), Platform: platform}
}

// registerBinary catalogues b under name, as a server build leaves it.
func registerBinary(t *testing.T, r artifact.Repository, name string, n int, b artifact.RunBinary) {
	t.Helper()
	os, arch, _ := strings.Cut(b.Platform, "/")
	e := runtimeEntry(name, n)
	e.Image, e.Digest = "", ""
	e.Binary = &artifact.RuntimeBinary{Digest: b.Digest, Bytes: b.Bytes, OS: os, Arch: arch}
	e.Build = buildID(n)
	if err := r.SaveRuntime(ctx, e); err != nil {
		t.Fatal(err)
	}
}

// admitPinned admits run n of runtime pinned to b, which may be nil.
func admitPinned(r artifact.Repository, n int, runtime string, priority int, b *artifact.RunBinary) error {
	a := admission(runID(n), artifact.DefaultProject, []byte("bundle "+runID(n)))
	a.Runtime, a.Priority, a.Binary = runtime, priority, b
	_, err := r.AdmitRun(ctx, a)
	return err
}

func mustAdmitPinned(t *testing.T, r artifact.Repository, n int, runtime string, priority int, b *artifact.RunBinary) {
	t.Helper()
	if err := admitPinned(r, n, runtime, priority, b); err != nil {
		t.Fatal(err)
	}
}

// pinnedTo checks that the assignment names the run and the binary, or none
// when want is nil.
func pinnedTo(t *testing.T, when string, a artifact.Assignment, want *artifact.RunBinary) {
	t.Helper()
	if want == nil && a.Binary != nil || want != nil && (a.Binary == nil || *a.Binary != *want) {
		t.Errorf("%s: %s binary = %+v; want %+v", when, a.RunID, a.Binary, want)
	}
}

// runBinaries: a run is admitted with or without the binary it runs on, and
// the binary is part of its assignment through every step of the queue. An
// invalid binary, one of the wrong size and one no catalog entry names are
// refused, and refuse the run too.
func runBinaries(t *testing.T, b Backend) {
	r := openRuns(t, b)
	amd := poolBinary("amd64 worker", "linux/amd64")
	registerBinary(t, r, "edge", 1, amd)

	mustAdmitPinned(t, r, 1, "edge", 0, &amd)
	mustAdmitPinned(t, r, 2, "edge", 0, nil)
	got, err := r.Assignment(ctx, runID(1))
	if err != nil || got.State != artifact.QueueQueued {
		t.Fatalf("pinned run's assignment = %+v, %v", got, err)
	}
	pinnedTo(t, "assignment", got, &amd)
	got, err = r.Assignment(ctx, runID(2))
	if err != nil {
		t.Fatal(err)
	}
	pinnedTo(t, "unpinned assignment", got, nil)

	// The caller's binary is not shared with the store.
	mine := amd
	a := admission(runID(3), artifact.DefaultProject, []byte("bundle 3"))
	a.Runtime, a.Binary = "edge", &mine
	if _, err := r.AdmitRun(ctx, a); err != nil {
		t.Fatal(err)
	}
	mine.Platform = "linux/arm64"
	got, err = r.Assignment(ctx, runID(3))
	if err != nil {
		t.Fatal(err)
	}
	pinnedTo(t, "after the caller changed its copy", got, &amd)

	next := 10
	refused := func(name string, want error, edit func(*artifact.RunBinary)) {
		t.Helper()
		bin := amd
		edit(&bin)
		next++
		id := next
		if err := admitPinned(r, id, "edge", 0, &bin); !errors.Is(err, want) {
			t.Errorf("%s = %v; want %v", name, err, want)
		}
		if _, err := r.Run(ctx, runID(id)); !errors.Is(err, artifact.ErrNotFound) {
			t.Errorf("%s: the run exists after the refusal: %v", name, err)
		}
	}
	invalid := func(name string, edit func(*artifact.RunBinary)) {
		t.Helper()
		refused(name, artifact.ErrInvalidRequest, edit)
	}
	invalid("digest", func(b *artifact.RunBinary) { b.Digest = "sha256:ABC" })
	invalid("empty binary", func(b *artifact.RunBinary) { b.Bytes = 0 })
	invalid("oversized binary", func(b *artifact.RunBinary) { b.Bytes = artifact.MaxRuntimeBinary + 1 })
	invalid("platform without an architecture", func(b *artifact.RunBinary) { b.Platform = "linux" })
	invalid("platform in capitals", func(b *artifact.RunBinary) { b.Platform = "Linux/amd64" })
	invalid("empty platform", func(b *artifact.RunBinary) { b.Platform = "" })
	invalid("size other than the stored binary's", func(b *artifact.RunBinary) { b.Bytes++ })
	refused("digest no entry names", artifact.ErrNotFound, func(b *artifact.RunBinary) {
		*b = poolBinary("never registered", "linux/amd64")
	})
	if _, err := r.Assignment(ctx, runID(1)); err != nil {
		t.Errorf("refusals disturbed run 1: %v", err)
	}
}

// binaryClaims: a pool worker's claim takes only the queued runs of its
// runtime pinned to a binary of its platform, in priority and admission
// order; a worker of the runtime's identity takes any run of the runtime,
// pinned or not.
func binaryClaims(t *testing.T, b Backend) {
	r := openRuns(t, b)
	amd, arm := poolBinary("amd64 worker", "linux/amd64"), poolBinary("arm64 worker", "linux/arm64")
	registerBinary(t, r, "a", 1, amd)
	registerBinary(t, r, "b", 2, arm)
	mustAdmitPinned(t, r, 1, "a", 0, &amd)
	mustAdmitPinned(t, r, 2, "a", 0, &arm)
	mustAdmitPinned(t, r, 3, "a", 0, nil)
	mustAdmitPinned(t, r, 4, "b", 0, &arm)
	mustAdmitPinned(t, r, 5, "a", -1, &amd)
	mustAdmitPinned(t, r, 6, "a", 0, &amd)

	for _, platform := range []string{"", "linux", "Linux/amd64", "linux/"} {
		if _, err := r.ClaimBinary(ctx, "pool", "a", platform, 1, epoch.Add(time.Minute)); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("claim for platform %q = %v; want ErrInvalidRequest", platform, err)
		}
	}
	if _, err := r.ClaimBinary(ctx, "", "a", "linux/amd64", 1, epoch.Add(time.Minute)); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("claim without a worker = %v; want ErrInvalidRequest", err)
	}
	if got, err := r.ClaimBinary(ctx, "pool", "a", "linux/amd64", 0, epoch.Add(time.Minute)); err != nil || len(got) != 0 {
		t.Errorf("claim of none = %v, %v", claimed(got), err)
	}
	if got, err := r.ClaimBinary(ctx, "pool", "a", "darwin/arm64", 5, epoch.Add(time.Minute)); err != nil || len(got) != 0 {
		t.Errorf("claim for a platform no run is pinned to = %v, %v", claimed(got), err)
	}

	got, err := r.ClaimBinary(ctx, "pool", "a", "linux/amd64", 2, epoch.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{runID(5), runID(1)}) {
		t.Fatalf("pool claim = %v, %v", claimed(got), err)
	}
	for _, a := range got {
		pinnedTo(t, "pool claim", a, &amd)
		if a.Worker != "pool" || a.State != artifact.QueueAssigned || a.Attempt != 1 || a.Runtime != "a" || !a.Expires.Equal(epoch.Add(time.Minute)) {
			t.Errorf("pool assignment = %+v", a)
		}
	}
	// A run another claim holds is skipped, and the rest follow.
	got, err = r.ClaimBinary(ctx, "pool2", "a", "linux/amd64", 5, epoch.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{runID(6)}) {
		t.Fatalf("second pool claim = %v, %v", claimed(got), err)
	}
	// A dedicated worker takes the other pinned runs and the unpinned one,
	// but only of its runtime.
	got, err = r.Claim(ctx, "dedicated", "a", 5, epoch.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{runID(2), runID(3)}) {
		t.Fatalf("dedicated claim = %v, %v", claimed(got), err)
	}
	pinnedTo(t, "dedicated claim of a pinned run", got[0], &arm)
	pinnedTo(t, "dedicated claim of an unpinned run", got[1], nil)
	got, err = r.ClaimBinary(ctx, "pool", "a", "linux/arm64", 5, epoch.Add(time.Minute))
	if err != nil || len(got) != 0 {
		t.Errorf("claim after the runtime's runs are held = %v, %v", claimed(got), err)
	}
	got, err = r.ClaimBinary(ctx, "pool", "b", "linux/arm64", 5, epoch.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{runID(4)}) {
		t.Fatalf("claim of the other runtime = %v, %v", claimed(got), err)
	}
	pinnedTo(t, "other runtime", got[0], &arm)
}

// binaryLeases: the binary stays on the assignment through renewal, expiry,
// requeue, release and cancellation, and ends with the run.
func binaryLeases(t *testing.T, b Backend) {
	r := openRuns(t, b)
	amd := poolBinary("amd64 worker", "linux/amd64")
	registerBinary(t, r, "a", 1, amd)
	mustAdmitPinned(t, r, 1, "a", 0, &amd)
	mustAdmitPinned(t, r, 2, "a", 1, &amd)
	mustAdmitPinned(t, r, 3, "a", 2, &amd)

	got, err := r.ClaimBinary(ctx, "pool", "a", "linux/amd64", 1, epoch.Add(time.Minute))
	if err != nil || len(got) != 1 {
		t.Fatalf("claim = %v, %v", got, err)
	}
	first := got[0]
	pinnedTo(t, "claim", first, &amd)
	if err := r.Start(ctx, first); err != nil {
		t.Fatal(err)
	}
	if a, err := r.Assignment(ctx, first.RunID); err != nil || a.State != artifact.QueueRunning {
		t.Errorf("running assignment = %+v, %v", a, err)
	} else {
		pinnedTo(t, "running assignment", a, &amd)
	}
	held, err := r.Renew(ctx, "pool", epoch.Add(2*time.Minute))
	if err != nil || len(held) != 1 {
		t.Fatalf("renew = %v, %v", held, err)
	}
	pinnedTo(t, "renew", held[0], &amd)

	later := epoch.Add(3 * time.Minute)
	expired, err := r.Expired(ctx, later)
	if err != nil || len(expired) != 1 {
		t.Fatalf("expired = %v, %v", claimed(expired), err)
	}
	pinnedTo(t, "expired", expired[0], &amd)
	if _, err := r.Requeue(ctx, expired[0], later); err != nil {
		t.Fatal(err)
	}
	if a, err := r.Assignment(ctx, first.RunID); err != nil || a.State != artifact.QueueQueued {
		t.Errorf("requeued assignment = %+v, %v", a, err)
	} else {
		pinnedTo(t, "requeued assignment", a, &amd)
	}
	got, err = r.ClaimBinary(ctx, "pool2", "a", "linux/amd64", 1, later.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{first.RunID}) || got[0].Attempt != 2 {
		t.Fatalf("reclaim = %+v, %v", got, err)
	}
	pinnedTo(t, "reclaim", got[0], &amd)
	if _, err := r.Release(ctx, got[0]); err != nil {
		t.Fatal(err)
	}
	if a, err := r.Assignment(ctx, first.RunID); err != nil || a.State != artifact.QueueQueued {
		t.Errorf("released assignment = %+v, %v", a, err)
	} else {
		pinnedTo(t, "released assignment", a, &amd)
	}

	// Cancelling a queued run and one a worker holds both report it.
	got, err = r.ClaimBinary(ctx, "pool", "a", "linux/amd64", 1, later.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{first.RunID}) {
		t.Fatalf("claim before cancel = %v, %v", claimed(got), err)
	}
	if a, err := r.RequestCancel(ctx, first.RunID); err != nil || a.Worker != "pool" {
		t.Errorf("cancel held = %+v, %v", a, err)
	} else {
		pinnedTo(t, "cancel of a held run", a, &amd)
	}
	if a, err := r.RequestCancel(ctx, runID(3)); err != nil || a.Worker != "" {
		t.Errorf("cancel queued = %+v, %v", a, err)
	} else {
		pinnedTo(t, "cancel of a queued run", a, &amd)
	}

	// The run ends and its queue entry, binary included, goes with it.
	if _, err := r.SaveRun(ctx, first.RunID, artifact.DefaultProject, got[0].Generation, true, artifact.RunIndex{}, []byte(`{}`), []byte("evidence")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Assignment(ctx, first.RunID); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("finished run's assignment = %v", err)
	}
}

// binaryOutlivesEntry: a run keeps the binary it was admitted with after
// the catalog entry that named it is replaced or deleted, and no new run
// can be admitted with a binary no entry names.
func binaryOutlivesEntry(t *testing.T, b Backend) {
	r := openRuns(t, b)
	old, shared := poolBinary("old worker", "linux/amd64"), poolBinary("shared worker", "linux/amd64")
	registerBinary(t, r, "edge", 1, old)
	registerBinary(t, r, "other", 2, shared)
	registerBinary(t, r, "same", 3, shared)
	mustAdmitPinned(t, r, 1, "edge", 0, &old)
	mustAdmitPinned(t, r, 2, "other", 0, &shared)

	// A new build replaces the name's binary; the run keeps the old one.
	fresh := poolBinary("new worker", "linux/amd64")
	registerBinary(t, r, "edge", 4, fresh)
	if err := admitPinned(r, 3, "edge", 0, &old); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("admission with the replaced binary = %v; want ErrNotFound", err)
	}
	mustAdmitPinned(t, r, 4, "edge", 0, &fresh)
	// Two entries name one binary; deleting one leaves it admissible.
	if err := r.DeleteRuntime(ctx, "same"); err != nil {
		t.Fatal(err)
	}
	mustAdmitPinned(t, r, 5, "other", 0, &shared)
	if err := r.DeleteRuntime(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	if err := admitPinned(r, 6, "other", 0, &shared); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("admission with a deleted entry's binary = %v; want ErrNotFound", err)
	}

	r.Close()
	r = openRuns(t, b)
	got, err := r.ClaimBinary(ctx, "pool", "edge", "linux/amd64", 5, epoch.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{runID(1), runID(4)}) {
		t.Fatalf("claim = %v, %v", claimed(got), err)
	}
	pinnedTo(t, "run of the replaced binary", got[0], &old)
	pinnedTo(t, "run of the new binary", got[1], &fresh)
	got, err = r.ClaimBinary(ctx, "pool", "other", "linux/amd64", 5, epoch.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{runID(2), runID(5)}) {
		t.Fatalf("claim after the entry was deleted = %v, %v", claimed(got), err)
	}
	pinnedTo(t, "run of the deleted entry's binary", got[0], &shared)
}
