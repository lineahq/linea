package platform

import (
	"fmt"
	"slices"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// RunListing is what a run list says about a run with a report. It is
// derived from the retained report on every list, never stored, and never
// carries bundle contents, environment values or configuration text.
type RunListing struct {
	Scenario string             `json:"scenario,omitempty"`
	Runtime  *RunListingRuntime `json:"runtime,omitempty"`
	// Status is how execution ended; Injected of Inputs were sent.
	Status   runresult.Status `json:"status"`
	Inputs   int              `json:"inputs"`
	Injected int              `json:"injected"`
	// Truncated is set when the evidence budget cut the observations short.
	Truncated  bool              `json:"truncated,omitempty"`
	Assertions *AssertionSummary `json:"assertions,omitempty"`
}

// RunListingRuntime names the runtime a run executed on; the built-in one is
// named BuiltInRuntime.
type RunListingRuntime struct {
	Kind             string `json:"kind"`
	Name             string `json:"name"`
	CollectorVersion string `json:"collector_version"`
	Verified         bool   `json:"verified"`
}

// RunFilter narrows ListRuns; empty fields match every run. Scenario and
// Runtime match the listing's scenario and runtime name exactly, so runs
// without a report never match them. Result is one of RunResults.
type RunFilter struct {
	Scenario string
	Runtime  string
	Result   string
}

// RunResults are the values RunFilter.Result accepts: how execution ended for
// a run with a report, otherwise the run's state.
var RunResults = []string{
	string(runresult.StatusCompleted), string(runresult.StatusPartial), string(runresult.StatusFailed), string(runresult.StatusCancelled),
	string(StateQueued), string(StateAssigned), string(StateRunning), string(StateError), string(StateLost),
}

func (f RunFilter) validate() error {
	if f.Result != "" && !slices.Contains(RunResults, f.Result) {
		return fmt.Errorf("%w: result %q is not one of %v", engine.ErrInvalidRequest, f.Result, RunResults)
	}
	return nil
}

// RunQuery is a RunFilter and the page of its runs to return: Limit runs
// (DefaultRunPage when zero, at most MaxRunPage) after Cursor, a previous
// page's NextCursor for the same filter.
type RunQuery struct {
	RunFilter
	Limit  int
	Cursor string
}

// Run page sizes.
const (
	DefaultRunPage = artifact.DefaultRunPage
	MaxRunPage     = artifact.MaxRunPage
)

// RunPage is a page of runs, newest first; NextCursor is empty on the last
// page.
type RunPage struct {
	Runs       []Run  `json:"runs"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func (q RunQuery) validate() error {
	if q.Limit < 0 || q.Limit > MaxRunPage {
		return fmt.Errorf("%w: limit must be between 1 and %d", engine.ErrInvalidRequest, MaxRunPage)
	}
	return q.RunFilter.validate()
}

func (f RunFilter) matches(r Run) bool {
	s := r.Listing
	if f.Scenario != "" && (s == nil || s.Scenario != f.Scenario) {
		return false
	}
	if f.Runtime != "" && (s == nil || s.Runtime == nil || s.Runtime.Name != f.Runtime) {
		return false
	}
	return f.Result == "" || f.Result == runResult(r)
}

// runResult is how a run ended when it has a report, otherwise its state.
func runResult(r Run) string {
	if r.Listing != nil {
		return string(r.Listing.Status)
	}
	return string(r.State)
}

// runIndex is what the repository filters a run list on: the listing's
// scenario and runtime name and the run's result, as RunFilter matches them.
// A field beyond the index's bounds is left empty, so no filter matches it.
func runIndex(state State, rep *engine.Report) artifact.RunIndex {
	l := listing(rep)
	x := artifact.RunIndex{Result: runResult(Run{State: state, Listing: l})}
	if l != nil {
		x.Scenario = l.Scenario
		if l.Runtime != nil {
			x.Runtime = l.Runtime.Name
		}
	}
	if len(x.Scenario) > artifact.MaxRunIndexField {
		x.Scenario = ""
	}
	if len(x.Runtime) > artifact.MaxRunIndexField {
		x.Runtime = ""
	}
	return x
}

// listing derives a run's listing from its report; nil without a result.
func listing(rep *engine.Report) *RunListing {
	if rep == nil || rep.Result == nil {
		return nil
	}
	s := &RunListing{Scenario: rep.Scenario, Status: rep.Result.Status, Inputs: len(rep.Result.Inputs), Truncated: rep.Result.Truncation != nil}
	for _, in := range rep.Result.Inputs {
		if in.Injected {
			s.Injected++
		}
	}
	if rt := rep.Runtime; rt != nil {
		name := rt.Name
		if rt.Kind == engine.RuntimeBuiltIn {
			name = BuiltInRuntime
		}
		s.Runtime = &RunListingRuntime{Kind: rt.Kind, Name: name, CollectorVersion: rt.CollectorVersion, Verified: rt.Verified}
	}
	s.Assertions = SummarizeAssertions(rep.Assertions)
	return s
}
