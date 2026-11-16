package pg

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// TestLeasesUseTheDatabaseClock runs two instances whose clocks are an hour
// apart. Each passes times from its own clock, and neither may judge the
// other's live lease expired.
func TestLeasesUseTheDatabaseClock(t *testing.T) {
	f := newFixture(t)
	behind, ahead := f.open(t, artifact.Limits{}), f.open(t, artifact.Limits{})
	const skew = time.Hour
	ahead.clock.local = func() time.Time { return time.Now().Add(skew) }
	if err := behind.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 2; n++ {
		a := artifact.Admission{ID: runID(n), Project: artifact.DefaultProject, Metadata: []byte(`{}`), Bundle: []byte{byte(n)}, Runtime: "a"}
		if _, err := behind.AdmitRun(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	const lease = time.Minute
	expires := time.Now().Add(lease).Truncate(time.Microsecond)
	got, err := behind.Claim(ctx, "w-behind", "a", 1, expires)
	if err != nil || len(got) != 1 || !got[0].Expires.Equal(expires) {
		t.Fatalf("claim behind = %+v, %v", got, err)
	}
	if got, err := ahead.Claim(ctx, "w-ahead", "a", 1, ahead.clock.now().Add(lease)); err != nil || len(got) != 1 {
		t.Fatalf("claim ahead = %+v, %v", got, err)
	}
	for _, c := range []struct {
		name string
		s    *Store
	}{{"behind", behind}, {"ahead", ahead}} {
		now := c.s.clock.now()
		if expired, err := c.s.Expired(ctx, now); err != nil || len(expired) != 0 {
			t.Errorf("%s: expired now = %v, %v", c.name, claimed(expired), err)
		}
		// The ahead instance's clock is an hour past the behind one's lease
		// on the behind one's clock; it must not requeue it.
		if c.s == ahead {
			if _, err := ahead.Requeue(ctx, got[0], now); !errors.Is(err, artifact.ErrConflict) {
				t.Errorf("ahead requeued a live lease: %v", err)
			}
		}
		expired, err := c.s.Expired(ctx, now.Add(2*lease))
		if err != nil || !slices.Equal(claimed(expired), []string{runID(1), runID(2)}) {
			t.Errorf("%s: expired after the leases = %v, %v", c.name, claimed(expired), err)
		}
	}
	// Times come back on the reader's clock.
	a, err := ahead.Assignment(ctx, runID(1))
	if want := expires.Add(skew); err != nil || a.Expires.Sub(want).Abs() > time.Second {
		t.Errorf("lease read ahead = %v, want about %v (%v)", a.Expires, want, err)
	}
}

// TestRunLimitAcrossInstances admits one principal's runs through two
// instances at once: the limit holds across them.
func TestRunLimitAcrossInstances(t *testing.T) {
	f := newFixture(t)
	a, b := f.open(t, artifact.Limits{}), f.open(t, artifact.Limits{})
	if err := a.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	const limit = 3
	results := make(chan error, 8)
	for n := 1; n <= 8; n++ {
		r := a
		if n%2 == 0 {
			r = b
		}
		go func() {
			_, err := r.AdmitRun(ctx, artifact.Admission{ID: runID(n), Project: artifact.DefaultProject, Metadata: []byte(`{}`),
				Bundle: []byte{byte(n)}, Runtime: "a", Principal: "user:alice", PrincipalRuns: limit})
			results <- err
		}()
	}
	admitted := 0
	for range 8 {
		switch err := <-results; {
		case err == nil:
			admitted++
		case !errors.Is(err, artifact.ErrRunLimit):
			t.Error(err)
		}
	}
	if admitted != limit {
		t.Errorf("%d runs admitted, want %d", admitted, limit)
	}
}

func claimed(as []artifact.Assignment) []string {
	ids := []string{}
	for _, a := range as {
		ids = append(ids, a.RunID)
	}
	slices.Sort(ids)
	return ids
}
