package bisect

import "context"

// Status is what one probe found for a version.
type Status string

const (
	// Good: every scenario passed.
	Good Status = "good"
	// Bad: a scenario failed.
	Bad Status = "bad"
	// Skipped: the version could not be judged, for example because the
	// merged configuration is invalid or a run was inconclusive.
	Skipped Status = "skipped"
	// Untested: the search never needed to probe the version.
	Untested Status = "untested"
)

// Probe tests the candidate at index i. Only an error that ends the whole
// search, such as the caller leaving, is returned; a version that cannot be
// judged is Skipped.
type Probe func(ctx context.Context, i int) (Status, error)

// Outcome is where a search ended. FirstBad and LastGood are candidate
// indexes, -1 when unknown.
type Outcome struct {
	Verdict  Verdict
	FirstBad int
	LastGood int
	// Statuses has one entry per candidate.
	Statuses []Status
	Probes   int
}

// Search finds the first bad candidate of n, in order, assuming that once a
// candidate is bad every later one is bad. It probes the last candidate,
// then the first, then halves the range between the newest good and the
// oldest bad candidate, skipping candidates that cannot be judged as git
// bisect does. When only skipped candidates separate a good and a bad one,
// the verdict is Inconclusive and FirstBad is the bad one.
func Search(ctx context.Context, n int, probe Probe) (Outcome, error) {
	out := Outcome{Verdict: Inconclusive, FirstBad: -1, LastGood: -1, Statuses: make([]Status, n)}
	for i := range out.Statuses {
		out.Statuses[i] = Untested
	}
	if n == 0 {
		return out, nil
	}
	test := func(i int) (Status, error) {
		s, err := probe(ctx, i)
		if err != nil {
			return "", err
		}
		out.Statuses[i] = s
		out.Probes++
		return s, nil
	}
	switch s, err := test(n - 1); {
	case err != nil:
		return out, err
	case s == Good:
		out.Verdict, out.LastGood = NoRegression, n-1
		return out, nil
	case s == Skipped:
		return out, nil
	}
	out.FirstBad = n - 1
	if n == 1 {
		out.Verdict = AlreadyBad
		return out, nil
	}
	switch s, err := test(0); {
	case err != nil:
		return out, err
	case s == Bad:
		out.Verdict, out.FirstBad = AlreadyBad, 0
		return out, nil
	case s == Skipped:
		return out, nil
	}
	out.LastGood = 0
	for {
		next := pick(out.Statuses, out.LastGood, out.FirstBad)
		if next < 0 {
			break
		}
		s, err := test(next)
		if err != nil {
			return out, err
		}
		switch s {
		case Good:
			out.LastGood = next
		case Bad:
			out.FirstBad = next
		}
	}
	if out.FirstBad == out.LastGood+1 {
		out.Verdict = Found
	}
	return out, nil
}

// pick returns the untested candidate between lo and hi nearest their
// middle, preferring the older one on a tie, or -1 when none is left.
func pick(statuses []Status, lo, hi int) int {
	mid := lo + (hi-lo)/2
	for d := 0; mid-d > lo || mid+d < hi; d++ {
		for _, i := range []int{mid - d, mid + d} {
			if i > lo && i < hi && statuses[i] == Untested {
				return i
			}
		}
	}
	return -1
}
