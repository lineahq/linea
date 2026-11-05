package pg

import (
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// TestBuildObjectsAreReferencedUntilTrimmed checks that build inputs and a
// done build's binary count toward Usage and survive Sweep, that trimming
// releases them, and that only the writes a reader waits for are on the
// change feed.
func TestBuildObjectsAreReferencedUntilTrimmed(t *testing.T) {
	f := newFixture(t)
	s := f.open(t, artifact.Limits{})
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	manifest, source, binary := []byte("dist:\n  name: edge\n"), []byte("archive"), []byte("binary")
	head, err := s.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	limits := artifact.BuildLimits{PerPrincipal: 5, Total: 5}
	a := artifact.BuildAdmission{ID: "build-1", Principal: "user:a", Name: "edge", Key: digestOf([]byte("key")), Spec: []byte(`{}`),
		Manifest: manifest, Sources: []artifact.BuildSource{{Module: "example.com/proc", Archive: source}}, Created: t0}
	if _, err := s.AdmitBuild(ctx, a, limits); err != nil {
		t.Fatal(err)
	}
	if used, err := s.Usage(ctx); err != nil || used != int64(len(manifest)+len(source)) {
		t.Fatalf("usage after admission = %d, %v", used, err)
	}
	if _, err := s.ClaimBuilds(ctx, "builder", 1, t0, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewBuilds(ctx, "builder", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.putBlob(ctx, digestOf(binary), binary); err != nil {
		t.Fatal(err)
	}
	done := artifact.BuildOutcome{State: artifact.BuildDone, BinaryDigest: digestOf(binary), BinaryBytes: int64(len(binary))}
	if _, err := s.FinishBuild(ctx, "build-1", "builder", 1, done, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// Admission, claim and finish are on the feed; the renewal is not.
	changes, _, err := s.Changes(ctx, head, artifact.MaxChanges)
	if err != nil || len(changes) != 3 {
		t.Fatalf("changes = %+v, %v; want admission, claim and finish", changes, err)
	}
	// A reused build holds the same binary and inputs: nothing is counted twice.
	a.ID, a.Created = "build-2", t0.Add(time.Minute)
	if got, err := s.AdmitBuild(ctx, a, limits); err != nil || got.ReusedFrom != "build-1" {
		t.Fatalf("reused = %+v, %v", got, err)
	}
	want := int64(len(manifest) + len(source) + len(binary))
	if used, err := s.Usage(ctx); err != nil || used != want {
		t.Fatalf("usage with a binary = %d, %v; want %d", used, err, want)
	}
	if n, err := s.Sweep(ctx, -time.Hour); err != nil || n != 0 {
		t.Fatalf("sweep of referenced objects = %d, %v", n, err)
	}
	if n, err := s.TrimBuilds(ctx, 1); err != nil || n != 1 {
		t.Fatalf("trim = %d, %v", n, err)
	}
	if n, err := s.Sweep(ctx, -time.Hour); err != nil || n != 0 {
		t.Fatalf("sweep after trimming the original = %d, %v; the reused build still references everything", n, err)
	}
	if n, err := s.TrimBuilds(ctx, 0); err != nil || n != 1 {
		t.Fatalf("trim = %d, %v", n, err)
	}
	if n, err := s.Sweep(ctx, -time.Hour); err != nil || n != 3 {
		t.Fatalf("sweep after trimming every build = %d, %v; want the 2 inputs and the binary", n, err)
	}
	if used, err := s.Usage(ctx); err != nil || used != 0 {
		t.Fatalf("usage after trimming = %d, %v", used, err)
	}
}
