package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// NormalizedRunID replaces run IDs in normalized reports.
const NormalizedRunID = "run-normalized"

// NormalizedInstanceID replaces the collector.instance.id data point
// attribute in normalized reports.
const NormalizedInstanceID = runresult.NormalizedInstanceID

// Normalize removes the values that legitimately differ between two
// executions of the same deterministic scenario: run IDs, event IDs, event
// times and wall-clock durations. Components running against a mock order
// the attributes they add arbitrarily (they build them from Go maps: the
// filelog receiver's stanza entries, the k8sattributes tags), and the
// filelog receiver stamps observed times from the clock. So in runs with
// mocks, attributes are sorted by key, log observed timestamps cleared,
// collector.instance.id replaced, and the snapshots re-addressed. Two normalized
// reports of such a scenario are byte-identical, which makes them
// suitable for golden files. Rebased input timestamps depend on when the
// run happened, so the shift is undone in all recorded telemetry and the
// offset omitted. Components' own metrics measured in time units keep
// only their counts.
func (r *Report) Normalize() {
	shift := r.Timestamps.rebaseShift()
	if r.Timestamps != nil {
		r.Timestamps.Offset = ""
	}
	for _, res := range []*runresult.Result{r.Result, r.Reference} {
		if res == nil {
			continue
		}
		if res.RunID != "" {
			res.RunID = NormalizedRunID
		}
		for i := range res.Events {
			e := &res.Events[i]
			e.RunID = NormalizedRunID
			e.ID = fmt.Sprintf("%s:%d", NormalizedRunID, e.Seq)
			e.Time = time.Time{}
		}
		for i := range res.ComponentMetrics {
			if m := &res.ComponentMetrics[i]; timeUnits[m.Unit] {
				m.IntValue, m.Value, m.Sum = nil, nil, nil
			}
		}
		if mocked := len(res.Mocks) > 0; mocked || shift != 0 {
			normalizeTelemetryOf(res, func(p telemetry.Payload) {
				p.Shift(-shift)
				if mocked {
					runresult.NormalizeMocked(p)
				}
			})
		}
	}
}

// normalizeTelemetryOf applies f to the telemetry in a result's snapshots
// and outputs (endpoint outputs too), re-keying the snapshots by their new content. Telemetry that
// cannot be decoded is left as it is.
func normalizeTelemetryOf(res *runresult.Result, f func(telemetry.Payload)) {
	renamed := map[string]string{}
	snapshots := make(instrument.Snapshots, len(res.Snapshots))
	for ref, data := range res.Snapshots {
		if normalized, ok := normalizeTelemetry(data, f); ok {
			sum := sha256.Sum256(normalized)
			renamed[ref] = "sha256:" + hex.EncodeToString(sum[:])
			snapshots[renamed[ref]] = normalized
			continue
		}
		snapshots[ref] = data
	}
	res.Snapshots = snapshots
	for i := range res.Events {
		if to, ok := renamed[res.Events[i].Snapshot]; ok {
			res.Events[i].Snapshot = to
		}
	}
	for i := range res.Outputs {
		if normalized, ok := normalizeTelemetry(res.Outputs[i].Data, f); ok {
			res.Outputs[i].Data = normalized
		}
	}
	for i := range res.EndpointOutputs {
		if normalized, ok := normalizeTelemetry(res.EndpointOutputs[i].Data, f); ok {
			res.EndpointOutputs[i].Data = normalized
		}
	}
}

// normalizeTelemetry decodes data, applies f and encodes the result.
func normalizeTelemetry(data json.RawMessage, f func(telemetry.Payload)) (json.RawMessage, bool) {
	p, err := telemetry.Decode(data)
	if err != nil {
		return nil, false
	}
	f(p)
	out, err := p.JSON()
	return out, err == nil
}

// timeUnits are the UCUM units of durations a component measures, whose
// values differ between executions.
var timeUnits = map[string]bool{"s": true, "ms": true, "us": true, "ns": true, "min": true, "h": true}
