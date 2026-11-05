package pg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

const maxBinary = artifact.MaxRuntimeBinary

// builderFixture is a store with one build claimed by "builder" in attempt 1.
type builderFixture struct {
	s        *Store
	t0       time.Time
	manifest []byte
	source   []byte
}

func newBuilderFixture(t *testing.T, limits artifact.Limits) builderFixture {
	t.Helper()
	f := newFixture(t)
	b := builderFixture{s: f.open(t, limits), t0: time.Now().UTC().Truncate(time.Millisecond), manifest: []byte("dist:\n  name: edge\n"), source: []byte("archive")}
	b.admit(t, "build-1", "key-1")
	if got, err := b.s.ClaimBuilds(ctx, "builder", 1, b.t0, b.t0.Add(time.Minute)); err != nil || len(got) != 1 {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	return b
}

func (b builderFixture) admit(t *testing.T, id, key string) {
	t.Helper()
	a := artifact.BuildAdmission{ID: id, Principal: "user:a", Name: "edge", Key: digestOf([]byte(key)), Spec: []byte(`{}`),
		Manifest: b.manifest, Sources: []artifact.BuildSource{{Module: "example.com/proc", Archive: b.source}}, Created: b.t0}
	if _, err := b.s.AdmitBuild(ctx, a, artifact.BuildLimits{PerPrincipal: 5, Total: 5}); err != nil {
		t.Fatal(err)
	}
}

// stage uploads payload as the staged object name through its presigned URL,
// as a builder does.
func (b builderFixture) stage(t *testing.T, name string, payload []byte) {
	t.Helper()
	u, err := b.s.StageURL(ctx, name, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("staging PUT = %s", res.Status)
	}
}

func (b builderFixture) staged(t *testing.T, name string) bool {
	t.Helper()
	key, err := stagingKey(b.s.prefix, name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.s.objects.Open(ctx, key)
	if errors.Is(err, ErrObjectNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	return true
}

func (b builderFixture) stored(t *testing.T, payload []byte) bool {
	t.Helper()
	got, err := b.s.readBlob(ctx, digestOf(payload), int64(len(payload)))
	if errors.Is(err, artifact.ErrCorrupt) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(got, payload)
}

func (b builderFixture) usage(t *testing.T) int64 {
	t.Helper()
	n, err := b.s.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (b builderFixture) sweep(t *testing.T) int {
	t.Helper()
	n, err := b.s.Sweep(ctx, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (b builderFixture) store(payload []byte, name string) error {
	return b.s.StoreBuildBinary(ctx, "build-1", "builder", 1, name, digestOf(payload), int64(len(payload)), maxBinary)
}

func fetch(t *testing.T, url string) []byte {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET = %s, %v", res.Status, err)
	}
	return body
}

// TestBuildInputURL checks that a build's own inputs, and only those, are
// readable through presigned URLs.
func TestBuildInputURL(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{})
	for _, in := range []struct {
		digest string
		want   []byte
	}{{digestOf(b.manifest), b.manifest}, {digestOf(b.source), b.source}} {
		u, err := b.s.BuildInputURL(ctx, "build-1", in.digest, time.Minute)
		if err != nil || u.Digest != in.digest || u.Size != int64(len(in.want)) || !bytes.Equal(fetch(t, u.URL), in.want) {
			t.Errorf("input %s = %+v, %v", in.digest, u, err)
		}
	}
	// Another build's own input is no input of build-1.
	b2 := b
	b2.source = []byte("another archive")
	b2.admit(t, "build-2", "key-2")
	other := digestOf(b2.source)
	for name, run := range map[string]func() error{
		"another build's input": func() error { _, err := b.s.BuildInputURL(ctx, "build-1", other, time.Minute); return err },
		"malformed":             func() error { _, err := b.s.BuildInputURL(ctx, "build-1", "nope", time.Minute); return err },
		"unknown build":         func() error { _, err := b.s.BuildInputURL(ctx, "build-9", digestOf(b.source), time.Minute); return err },
	} {
		if err := run(); !errors.Is(err, artifact.ErrNotFound) {
			t.Errorf("%s: err = %v; want ErrNotFound", name, err)
		}
	}
	if _, err := b.s.BuildInputURL(ctx, "build-1", digestOf(b.source), 0); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("zero lifetime: err = %v", err)
	}
}

// TestStoreBuildBinary uploads through a staged URL, stores the binary under
// the lease and finishes the build with it.
func TestStoreBuildBinary(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{})
	binary := bytes.Repeat([]byte("worker"), 4096)
	b.stage(t, "bin-1", binary)
	if err := b.store(binary, "bin-1"); err != nil {
		t.Fatal(err)
	}
	if b.staged(t, "bin-1") {
		t.Error("the staged object survived")
	}
	inputs := int64(len(b.manifest) + len(b.source))
	if got := b.usage(t); got != inputs+int64(len(binary)) {
		t.Errorf("usage with the stored binary = %d, want %d", got, inputs+int64(len(binary)))
	}
	// Storing the same binary again is idempotent.
	b.stage(t, "bin-1", binary)
	if err := b.store(binary, "bin-1"); err != nil {
		t.Fatalf("second store: %v", err)
	}
	done := artifact.BuildOutcome{State: artifact.BuildDone, BinaryDigest: digestOf(binary), BinaryBytes: int64(len(binary))}
	rec, err := b.s.FinishBuild(ctx, "build-1", "builder", 1, done, b.t0.Add(time.Second))
	if err != nil || rec.BinaryDigest != digestOf(binary) {
		t.Fatalf("finish = %+v, %v", rec, err)
	}
	if got := b.usage(t); got != inputs+int64(len(binary)) {
		t.Errorf("usage after finishing = %d", got)
	}
	if !b.stored(t, binary) {
		t.Error("the stored binary does not read back")
	}
	if n := b.sweep(t); n != 0 {
		t.Errorf("sweep deleted %d referenced objects", n)
	}
	// The lease ended with the build.
	b.stage(t, "bin-2", binary)
	if err := b.store(binary, "bin-2"); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("store after finishing = %v; want ErrConflict", err)
	}
	if b.staged(t, "bin-2") {
		t.Error("a refused store left the staged object")
	}
}

func TestStoreBuildBinaryRefusals(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{})
	binary := []byte("the binary")
	digest := digestOf(binary)
	size := int64(len(binary))
	type call struct {
		builder string
		attempt int
		digest  string
		size    int64
		limit   int64
		payload []byte // staged before the call; nil stages nothing
	}
	for _, c := range []struct {
		name string
		call call
		want error
	}{
		{"other digest", call{"builder", 1, digestOf([]byte("other")), size, maxBinary, binary}, artifact.ErrCorrupt},
		{"other content", call{"builder", 1, digest, size, maxBinary, []byte("the b1nary")}, artifact.ErrCorrupt},
		{"shorter object", call{"builder", 1, digest, size, maxBinary, binary[:4]}, artifact.ErrCorrupt},
		{"longer object", call{"builder", 1, digest, size, maxBinary, append(bytes.Clone(binary), 'x')}, artifact.ErrCorrupt},
		{"claimed size", call{"builder", 1, digest, size + 1, maxBinary, binary}, artifact.ErrCorrupt},
		{"missing object", call{"builder", 1, digest, size, maxBinary, nil}, artifact.ErrNotFound},
		{"other builder", call{"someone", 1, digest, size, maxBinary, binary}, artifact.ErrConflict},
		{"other attempt", call{"builder", 2, digest, size, maxBinary, binary}, artifact.ErrConflict},
		{"beyond the limit", call{"builder", 1, digest, size, size - 1, binary}, artifact.ErrLimit},
		{"empty", call{"builder", 1, digestOf(nil), 0, maxBinary, []byte{}}, artifact.ErrInvalidRequest},
		{"malformed digest", call{"builder", 1, "nope", size, maxBinary, binary}, artifact.ErrInvalidRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.call.payload != nil {
				b.stage(t, "bin-1", c.call.payload)
			}
			before := b.usage(t)
			err := b.s.StoreBuildBinary(ctx, "build-1", c.call.builder, c.call.attempt, "bin-1", c.call.digest, c.call.size, c.call.limit)
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v; want %v", err, c.want)
			}
			if b.staged(t, "bin-1") {
				t.Error("the staged object survived")
			}
			if b.stored(t, binary) {
				t.Error("a refused store left the binary in storage")
			}
			if got := b.usage(t); got != before {
				t.Errorf("usage changed from %d to %d", before, got)
			}
		})
	}
	if err := b.s.StoreBuildBinary(ctx, "build-9", "builder", 1, "bin-1", digest, size, maxBinary); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unknown build = %v; want ErrNotFound", err)
	}
	if err := b.s.StoreBuildBinary(ctx, "build-1", "builder", 1, "Bad Name", digest, size, maxBinary); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("invalid name = %v; want ErrInvalidRequest", err)
	}
}

// TestStoreBuildBinaryLeaseLost checks that a builder that lost its lease
// stores nothing, and that the binary of a lease that ends is released.
func TestStoreBuildBinaryLeaseLost(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{})
	binary := []byte("the binary")
	b.stage(t, "bin-1", binary)
	if err := b.store(binary, "bin-1"); err != nil {
		t.Fatal(err)
	}
	if n := b.sweep(t); n != 0 {
		t.Fatalf("sweep deleted %d objects while the lease is held", n)
	}
	// The lease runs out and the build returns to the queue.
	expired, err := b.s.ExpireBuilds(ctx, b.t0.Add(time.Hour), 3, []byte(`{}`))
	if err != nil || len(expired) != 1 || expired[0].State != artifact.BuildQueued {
		t.Fatalf("expire = %+v, %v", expired, err)
	}
	b.stage(t, "bin-2", binary)
	if err := b.store(binary, "bin-2"); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("store after the lease ran out = %v; want ErrConflict", err)
	}
	if got := b.usage(t); got != int64(len(b.manifest)+len(b.source)) {
		t.Errorf("usage after the lease ended = %d; the stored binary should no longer count", got)
	}
	if n := b.sweep(t); n != 1 {
		t.Errorf("sweep deleted %d objects; want the orphaned binary", n)
	}
}

// TestStoreBuildBinaryIsKeptWhileLeased checks the orphan window: a stored
// binary is no row's binary until FinishBuild, and neither Sweep nor the
// trimming of other builds may take it.
func TestStoreBuildBinaryIsKeptWhileLeased(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{})
	first, second := []byte("first binary"), []byte("a second binary")
	b.stage(t, "bin-1", first)
	if err := b.store(first, "bin-1"); err != nil {
		t.Fatal(err)
	}
	if n, err := b.s.TrimBuilds(ctx, 0); err != nil || n != 0 {
		t.Fatalf("trim = %d, %v; a running build is not trimmed", n, err)
	}
	if n := b.sweep(t); n != 0 || !b.stored(t, first) {
		t.Fatalf("sweep deleted %d objects; the stored binary must stay", n)
	}
	// A second upload replaces the first for this build.
	b.stage(t, "bin-2", second)
	if err := b.store(second, "bin-2"); err != nil {
		t.Fatal(err)
	}
	inputs := int64(len(b.manifest) + len(b.source))
	if got := b.usage(t); got != inputs+int64(len(second)) {
		t.Errorf("usage = %d; want the inputs and the second binary only", got)
	}
	if n := b.sweep(t); n != 1 || b.stored(t, first) || !b.stored(t, second) {
		t.Errorf("sweep deleted %d objects; want only the replaced first binary", n)
	}
	done := artifact.BuildOutcome{State: artifact.BuildDone, BinaryDigest: digestOf(second), BinaryBytes: int64(len(second))}
	if _, err := b.s.FinishBuild(ctx, "build-1", "builder", 1, done, b.t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if n := b.sweep(t); n != 0 || !b.stored(t, second) {
		t.Errorf("sweep deleted %d objects after the build finished", n)
	}
}

// TestRuntimeBinaryOutlivesItsBuild checks that a catalogued binary stays
// after the build that made it is trimmed, and goes with the entry.
func TestRuntimeBinaryOutlivesItsBuild(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{})
	binary := []byte("the worker binary")
	b.stage(t, "bin-1", binary)
	if err := b.store(binary, "bin-1"); err != nil {
		t.Fatal(err)
	}
	done := artifact.BuildOutcome{State: artifact.BuildDone, BinaryDigest: digestOf(binary), BinaryBytes: int64(len(binary))}
	if _, err := b.s.FinishBuild(ctx, "build-1", "builder", 1, done, b.t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	entry := artifact.RuntimeEntry{Name: "edge", Identity: digestOf([]byte("identity")), Hello: []byte(`{}`), RegisteredBy: "admin", Registered: b.t0,
		Binary: &artifact.RuntimeBinary{Digest: digestOf(binary), Bytes: int64(len(binary)), OS: "linux", Arch: "amd64"}, Build: "build-1"}
	if err := b.s.SaveRuntime(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if n, err := b.s.TrimBuilds(ctx, 0); err != nil || n != 1 {
		t.Fatalf("trim = %d, %v", n, err)
	}
	// The inputs go; the binary stays.
	if n := b.sweep(t); n != 2 || !b.stored(t, binary) {
		t.Fatalf("sweep deleted %d objects; want the 2 inputs and not the binary", n)
	}
	if got := b.usage(t); got != int64(len(binary)) {
		t.Errorf("usage = %d, want the binary's %d bytes", got, len(binary))
	}
	got, err := b.s.Runtime(ctx, "edge")
	if err != nil || got.Binary == nil || *got.Binary != *entry.Binary || got.Build != "build-1" {
		t.Errorf("runtime = %+v, %v", got, err)
	}
	if err := b.s.DeleteRuntime(ctx, "edge"); err != nil {
		t.Fatal(err)
	}
	if n := b.sweep(t); n != 1 || b.stored(t, binary) {
		t.Errorf("sweep deleted %d objects; want the binary once nothing references it", n)
	}
}

// TestStoreBuildBinaryStorageLimit checks that the binary counts against the
// storage limit like any stored object.
func TestStoreBuildBinaryStorageLimit(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{StorageBytes: 4096})
	big := bytes.Repeat([]byte("x"), 4096)
	b.stage(t, "bin-1", big)
	if err := b.store(big, "bin-1"); !errors.Is(err, artifact.ErrLimit) {
		t.Fatalf("store beyond the storage limit = %v; want ErrLimit", err)
	}
	if b.staged(t, "bin-1") || b.stored(t, big) {
		t.Error("a refused store left objects behind")
	}
	small := bytes.Repeat([]byte("y"), 4096-len(b.manifest)-len(b.source))
	b.stage(t, "bin-2", small)
	if err := b.store(small, "bin-2"); err != nil {
		t.Fatalf("store that exactly fits = %v", err)
	}
}

// catalogued stores a binary through the fixture's build, finishes the build
// and catalogues the binary as runtime name.
func (b builderFixture) catalogued(t *testing.T, name string, binary []byte) artifact.RunBinary {
	t.Helper()
	b.stage(t, "bin-1", binary)
	if err := b.store(binary, "bin-1"); err != nil {
		t.Fatal(err)
	}
	done := artifact.BuildOutcome{State: artifact.BuildDone, BinaryDigest: digestOf(binary), BinaryBytes: int64(len(binary))}
	if _, err := b.s.FinishBuild(ctx, "build-1", "builder", 1, done, b.t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	entry := artifact.RuntimeEntry{Name: name, Identity: digestOf([]byte("identity " + name)), Hello: []byte(`{}`), RegisteredBy: "admin", Registered: b.t0,
		Binary: &artifact.RuntimeBinary{Digest: digestOf(binary), Bytes: int64(len(binary)), OS: "linux", Arch: "amd64"}, Build: "build-1"}
	if err := b.s.SaveRuntime(ctx, entry); err != nil {
		t.Fatal(err)
	}
	return artifact.RunBinary{Digest: digestOf(binary), Bytes: int64(len(binary)), Platform: "linux/amd64"}
}

func (b builderFixture) admitPinned(t *testing.T, id, bundle string, pin artifact.RunBinary) {
	t.Helper()
	a := artifact.Admission{ID: id, Project: artifact.DefaultProject, Metadata: []byte(`{}`), Bundle: []byte(bundle), Runtime: "edge", Binary: &pin}
	if _, err := b.s.AdmitRun(ctx, a); err != nil {
		t.Fatal(err)
	}
}

func (b builderFixture) finishRun(t *testing.T, id string) {
	t.Helper()
	rec, err := b.s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.SaveRun(ctx, id, artifact.DefaultProject, rec.Generation, true, artifact.RunIndex{}, []byte(`{}`), []byte("evidence")); err != nil {
		t.Fatal(err)
	}
}

// TestRuntimeBinaryURL checks that a catalogued binary, and one an
// unfinished run is pinned to, reads through a presigned URL, and nothing
// else does.
func TestRuntimeBinaryURL(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{})
	if _, err := b.s.RuntimeBinaryURL(ctx, digestOf([]byte("unknown")), time.Minute); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unreferenced digest = %v; want ErrNotFound", err)
	}
	binary := bytes.Repeat([]byte("worker"), 1024)
	pin := b.catalogued(t, "edge", binary)
	for name, c := range map[string]struct {
		digest   string
		lifetime time.Duration
	}{"malformed digest": {"sha256:xyz", time.Minute}, "empty digest": {"", time.Minute}, "no lifetime": {pin.Digest, 0}} {
		if _, err := b.s.RuntimeBinaryURL(ctx, c.digest, c.lifetime); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("%s = %v; want ErrInvalidRequest", name, err)
		}
	}
	u, err := b.s.RuntimeBinaryURL(ctx, pin.Digest, time.Minute)
	if err != nil || u.Digest != pin.Digest || u.Size != pin.Bytes || !bytes.Equal(fetch(t, u.URL), binary) {
		t.Fatalf("catalogued binary = %+v, %v", u, err)
	}
	// An unfinished run keeps the URL available after the entry is gone.
	b.admitPinned(t, "run-0000000000000001", "bundle", pin)
	if err := b.s.DeleteRuntime(ctx, "edge"); err != nil {
		t.Fatal(err)
	}
	if u, err := b.s.RuntimeBinaryURL(ctx, pin.Digest, time.Minute); err != nil || u.Size != pin.Bytes || !bytes.Equal(fetch(t, u.URL), binary) {
		t.Fatalf("binary of an unfinished run = %+v, %v", u, err)
	}
	b.finishRun(t, "run-0000000000000001")
	if _, err := b.s.RuntimeBinaryURL(ctx, pin.Digest, time.Minute); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("binary after the run finished = %v; want ErrNotFound", err)
	}
}

// TestRunBinaryIsKeptWhileUnfinished checks that the binary of an unfinished
// run survives the catalog entry going and a sweep, counts once however many
// rows name it, and is collectable once the last run ends.
func TestRunBinaryIsKeptWhileUnfinished(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{})
	binary := bytes.Repeat([]byte("pinned"), 2048)
	pin := b.catalogued(t, "edge", binary)
	if n, err := b.s.TrimBuilds(ctx, 0); err != nil || n != 1 {
		t.Fatalf("trim = %d, %v", n, err)
	}
	if n := b.sweep(t); n != 2 {
		t.Fatalf("sweep deleted %d objects; want the 2 build inputs", n)
	}
	withEntry := b.usage(t)
	if withEntry != int64(len(binary)) {
		t.Fatalf("usage = %d; want the binary's %d bytes", withEntry, len(binary))
	}

	// Admission names the binary in a second row; it is no new bytes.
	b.admitPinned(t, "run-0000000000000011", "bundle one", pin)
	b.admitPinned(t, "run-0000000000000012", "bundle two", pin)
	if got, want := b.usage(t), withEntry+int64(len("bundle one")+len("bundle two")); got != want {
		t.Errorf("usage with three references = %d; want %d, the binary once", got, want)
	}
	if err := b.s.DeleteRuntime(ctx, "edge"); err != nil {
		t.Fatal(err)
	}
	if n := b.sweep(t); n != 0 || !b.stored(t, binary) {
		t.Fatalf("sweep deleted %d objects; the pinned binary must stay", n)
	}
	// A new run cannot be pinned to a binary no catalog entry names.
	a := artifact.Admission{ID: "run-0000000000000013", Project: artifact.DefaultProject, Metadata: []byte(`{}`), Bundle: []byte("bundle three"), Runtime: "edge", Binary: &pin}
	if _, err := b.s.AdmitRun(ctx, a); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("admission after the entry went = %v; want ErrNotFound", err)
	}

	b.finishRun(t, "run-0000000000000011")
	if n := b.sweep(t); n != 0 || !b.stored(t, binary) {
		t.Fatalf("sweep deleted %d objects; the second run still names the binary", n)
	}
	b.finishRun(t, "run-0000000000000012")
	if got, want := b.usage(t), int64(len("bundle one")+len("bundle two")+len("evidence")); got != want {
		t.Errorf("usage after both runs finished = %d; want %d", got, want)
	}
	if n := b.sweep(t); n != 1 || b.stored(t, binary) {
		t.Errorf("sweep deleted %d objects; want the binary once nothing references it", n)
	}
}

// TestBuildsReserveRoomForTheirBinary checks that every queued or running
// build without a stored binary reserves BuildBytes, that admission beyond
// the limit is refused until a build ends, and that a build storing its own
// binary is not refused by its own reservation.
func TestBuildsReserveRoomForTheirBinary(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{StorageBytes: 4096, BuildBytes: 1024})
	b.admit(t, "build-2", "key-2")
	b.admit(t, "build-3", "key-3")
	a := artifact.BuildAdmission{ID: "build-4", Principal: "user:a", Name: "edge", Key: digestOf([]byte("key-4")), Spec: []byte(`{}`),
		Manifest: b.manifest, Sources: []artifact.BuildSource{{Module: "example.com/proc", Archive: b.source}}, Created: b.t0}
	limits := artifact.BuildLimits{PerPrincipal: 5, Total: 5}
	if _, err := b.s.AdmitBuild(ctx, a, limits); !errors.Is(err, artifact.ErrLimit) {
		t.Fatalf("a fourth build = %v; want ErrLimit, four reservations and the inputs exceed the limit", err)
	}
	if _, err := b.s.CancelBuild(ctx, "build-3", b.t0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.AdmitBuild(ctx, a, limits); err != nil {
		t.Fatalf("a fourth build after one was cancelled = %v", err)
	}
	// Larger than its reservation, but it fits once the reservation is its own.
	binary := bytes.Repeat([]byte("b"), 1500)
	b.stage(t, "bin-1", binary)
	if err := b.store(binary, "bin-1"); err != nil {
		t.Fatalf("store = %v", err)
	}
	// build-1 now counts its binary, not its reservation: 2 reservations, the
	// inputs and the binary leave less than a third reservation.
	a.ID, a.Key = "build-5", digestOf([]byte("key-5"))
	if _, err := b.s.AdmitBuild(ctx, a, limits); !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("a build beyond the stored binary = %v; want ErrLimit", err)
	}
}

// hookedObjects runs copied after every successful Copy.
type hookedObjects struct {
	ObjectStore
	copied func(dst string)
}

func (h hookedObjects) Copy(ctx context.Context, src, dst, ifMatch string) error {
	if err := h.ObjectStore.Copy(ctx, src, dst, ifMatch); err != nil {
		return err
	}
	h.copied(dst)
	return nil
}

// TestStoreBuildBinaryCopiesOutsideTheLock checks that writes needing the
// installation lock proceed while a binary is copied.
func TestStoreBuildBinaryCopiesOutsideTheLock(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{})
	b.s.objects = hookedObjects{ObjectStore: b.s.objects, copied: func(string) {
		admitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		a := artifact.BuildAdmission{ID: "build-2", Principal: "user:a", Name: "edge", Key: digestOf([]byte("key-2")), Spec: []byte(`{}`),
			Manifest: b.manifest, Created: b.t0}
		if _, err := b.s.AdmitBuild(admitCtx, a, artifact.BuildLimits{PerPrincipal: 5, Total: 5}); err != nil {
			t.Errorf("admission during the copy = %v", err)
		}
	}}
	binary := []byte("the binary")
	b.stage(t, "bin-1", binary)
	if err := b.store(binary, "bin-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Build(ctx, "build-2"); err != nil {
		t.Errorf("build-2 = %v", err)
	}
}

// TestStoreBuildBinaryRechecksAfterTheCopy checks that the commit after the
// copy refuses a lease lost during it, and a copy a sweep deleted, and leaves
// the copy to Sweep.
func TestStoreBuildBinaryRechecksAfterTheCopy(t *testing.T) {
	binary := []byte("the binary")
	t.Run("lease lost", func(t *testing.T) {
		b := newBuilderFixture(t, artifact.Limits{})
		b.s.objects = hookedObjects{ObjectStore: b.s.objects, copied: func(string) {
			if _, err := b.s.ExpireBuilds(ctx, b.t0.Add(time.Hour), 3, []byte(`{}`)); err != nil {
				t.Error(err)
			}
		}}
		b.stage(t, "bin-1", binary)
		before := b.usage(t)
		if err := b.store(binary, "bin-1"); !errors.Is(err, artifact.ErrConflict) {
			t.Fatalf("store = %v; want ErrConflict", err)
		}
		if got := b.usage(t); got != before {
			t.Errorf("usage = %d; want %d, the refused binary does not count", got, before)
		}
		if n := b.sweep(t); n != 1 || b.stored(t, binary) {
			t.Errorf("sweep deleted %d objects; want the unreferenced copy", n)
		}
	})
	t.Run("copy swept", func(t *testing.T) {
		b := newBuilderFixture(t, artifact.Limits{})
		inner := b.s.objects
		b.s.objects = hookedObjects{ObjectStore: inner, copied: func(dst string) {
			if err := inner.Delete(ctx, dst); err != nil {
				t.Error(err)
			}
		}}
		b.stage(t, "bin-1", binary)
		if err := b.store(binary, "bin-1"); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("store = %v; want ErrNotFound", err)
		}
		if got := b.usage(t); got != int64(len(b.manifest)+len(b.source)) {
			t.Errorf("usage = %d; the build must not reference the deleted copy", got)
		}
	})
}

// TestRuntimeEntryNamesItsBuildsBinary checks that an entry naming a build
// the store still has must name the binary that build holds.
func TestRuntimeEntryNamesItsBuildsBinary(t *testing.T) {
	b := newBuilderFixture(t, artifact.Limits{})
	binary := []byte("the binary")
	entry := func(digest string, size int64) artifact.RuntimeEntry {
		return artifact.RuntimeEntry{Name: "edge", Identity: digestOf([]byte("identity")), Hello: []byte(`{}`), RegisteredBy: "admin", Registered: b.t0,
			Binary: &artifact.RuntimeBinary{Digest: digest, Bytes: size, OS: "linux", Arch: "amd64"}, Build: "build-1"}
	}
	held := entry(digestOf(binary), int64(len(binary)))
	if err := b.s.SaveRuntime(ctx, held); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("entry before the build stored its binary = %v; want ErrConflict", err)
	}
	b.stage(t, "bin-1", binary)
	if err := b.store(binary, "bin-1"); err != nil {
		t.Fatal(err)
	}
	for name, e := range map[string]artifact.RuntimeEntry{
		"other digest": entry(digestOf([]byte("other")), int64(len(binary))),
		"other size":   entry(digestOf(binary), int64(len(binary))+1),
	} {
		if err := b.s.SaveRuntime(ctx, e); !errors.Is(err, artifact.ErrConflict) {
			t.Errorf("%s = %v; want ErrConflict", name, err)
		}
	}
	if err := b.s.SaveRuntime(ctx, held); err != nil {
		t.Fatalf("entry naming the staged binary = %v", err)
	}
	done := artifact.BuildOutcome{State: artifact.BuildDone, BinaryDigest: digestOf(binary), BinaryBytes: int64(len(binary))}
	if _, err := b.s.FinishBuild(ctx, "build-1", "builder", 1, done, b.t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := b.s.SaveRuntime(ctx, held); err != nil {
		t.Errorf("entry naming the done build's binary = %v", err)
	}
	if _, err := b.s.TrimBuilds(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := b.s.SaveRuntime(ctx, held); err != nil {
		t.Errorf("entry whose build is trimmed = %v; the catalog keeps its own reference", err)
	}
}
