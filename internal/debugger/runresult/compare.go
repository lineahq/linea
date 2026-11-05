package runresult

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Comparison reports whether two runs delivered identical final telemetry to
// the same exporters.
type Comparison struct {
	Equal       bool     `json:"equal"`
	Differences []string `json:"differences,omitempty"`
	// Notes record equivalences that are not byte-identical, such as the
	// same records delivered in a different order.
	Notes []string `json:"notes,omitempty"`
	// Incomplete is the reason outputs were not compared: a run that did not
	// keep all of its output (evidence_budget) cannot be shown equal or
	// different.
	Incomplete string `json:"incomplete,omitempty"`
}

// Compare checks input acceptance and exporter outputs of a and b. Telemetry
// is equal when its canonical OTLP JSON is identical, or when both deliver
// the same multiset of records, each with its resource and scope: pipelines
// sharing an exporter flush in nondeterministic order at shutdown, and
// grouping into resources may differ. Such equivalences are noted. Batch
// counts are not compared: the reference OTLP exporter's queue may
// legitimately split or merge requests differently.
func Compare(a, b *Result) Comparison {
	var truncated []string
	for _, r := range []*Result{a, b} {
		if r.Truncation != nil {
			truncated = append(truncated, string(r.Mode))
		}
	}
	if len(truncated) > 0 {
		return Comparison{Incomplete: TruncationEvidenceBudget,
			Notes: []string{"outputs not compared: the " + strings.Join(truncated, " and ") + " run reached the evidence budget"}}
	}
	var diffs, notes []string
	byExporter := func(r *Result) map[string]Output {
		m := map[string]Output{}
		for _, o := range r.Outputs {
			// OTLP exporters suppress completely empty batches. Instrumented
			// capture still observes the call; it carries no delivered telemetry.
			// Preserve resource/scope metadata even when it has no records.
			if o.Counts.Resources == 0 {
				if p, err := telemetry.Decode(o.Data); err == nil && p.Counts().Resources == 0 {
					continue
				}
			}
			m[o.Exporter+" ("+o.Signal+")"] = o
		}
		return m
	}
	am, bm := byExporter(a), byExporter(b)
	// Components running against a mock add values that differ between any
	// two runs; those are compared normalized.
	mocked := len(a.Mocks) > 0 || len(b.Mocks) > 0
	if mocked {
		notes = append(notes, "mocked components: log observed timestamps, collector.instance.id and attribute order are not compared")
	}
	for name, ao := range am {
		bo, ok := bm[name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("exporter %s: output only in %s run", name, a.Mode))
			continue
		}
		ad, bd := ao.Data, bo.Data
		if mocked {
			ad, bd = normalizedMocked(ad), normalizedMocked(bd)
		}
		switch {
		case bytes.Equal(ad, bd):
		case sameRecords(ad, bd):
			notes = append(notes, fmt.Sprintf("exporter %s: same records in a different order or grouping", name))
		default:
			diffs = append(diffs, fmt.Sprintf("exporter %s: telemetry differs (%s %+v, %s %+v)",
				name, a.Mode, ao.Counts, b.Mode, bo.Counts))
		}
	}
	if a.Status != b.Status {
		diffs = append(diffs, fmt.Sprintf("run status differs: %s %s, %s %s", a.Mode, a.Status, b.Mode, b.Status))
	}
	if len(a.Inputs) != len(b.Inputs) {
		diffs = append(diffs, fmt.Sprintf("input count differs: %d vs %d", len(a.Inputs), len(b.Inputs)))
	} else {
		// Error texts differ between in-process injection and gRPC status
		// messages, so only acceptance is compared.
		for i := range a.Inputs {
			if (a.Inputs[i].Error == "") != (b.Inputs[i].Error == "") {
				diffs = append(diffs, fmt.Sprintf("input %d: accepted differently (%s error %q, %s error %q)",
					i+1, a.Mode, a.Inputs[i].Error, b.Mode, b.Inputs[i].Error))
			}
		}
	}
	for name := range bm {
		if _, ok := am[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("exporter %s: output only in %s run", name, b.Mode))
		}
	}
	slices.Sort(diffs)
	slices.Sort(notes)
	return Comparison{Equal: len(diffs) == 0, Differences: diffs, Notes: notes}
}

// sameRecords reports whether two OTLP JSON documents hold the same records
// with the same resources and scopes, in any order.
func sameRecords(a, b []byte) bool {
	pa, err := telemetry.Decode(a)
	if err != nil {
		return false
	}
	pb, err := telemetry.Decode(b)
	if err != nil || pa.Signal != pb.Signal {
		return false
	}
	fa, fb := telemetry.ContextFingerprints(pa), telemetry.ContextFingerprints(pb)
	slices.Sort(fa)
	slices.Sort(fb)
	return slices.Equal(fa, fb)
}

// normalizedMocked returns data with NormalizeMocked applied, or data
// unchanged when it cannot be decoded.
func normalizedMocked(data []byte) []byte {
	p, err := telemetry.Decode(data)
	if err != nil {
		return data
	}
	NormalizeMocked(p)
	out, err := p.JSON()
	if err != nil {
		return data
	}
	return out
}
