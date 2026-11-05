package analysis

import (
	"fmt"
	"strings"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Emission classifies how a stage produced output during a run.
type Emission string

const (
	// EmissionSynchronous: every output was emitted while its input was
	// being consumed, so each output is attributed to exactly one input.
	EmissionSynchronous Emission = "synchronous"
	// EmissionAsynchronous: outputs were emitted outside any consume call
	// (for example a batch flush) and are attributed to the stage only. A
	// connector stage is also asynchronous when the connector emitted that
	// way into another signal (spanmetrics: spans in, metrics on a timer);
	// those outputs belong to the stage of the signal they were emitted in.
	EmissionAsynchronous Emission = "asynchronous"
	// EmissionNone: the stage received input and emitted nothing.
	EmissionNone Emission = "none"
)

// Outcome classifies what happened to one stage input.
type Outcome string

const (
	// OutcomeEmitted: correlated output was observed; Diff describes it.
	OutcomeEmitted Outcome = "emitted"
	// OutcomeNoOutput: the run completed (all components shut down and
	// flushed) and the stage emitted nothing for this input, neither
	// correlated nor later. Diff shows every record as removed.
	OutcomeNoOutput Outcome = "no_output"
	// OutcomeDeferred: no correlated output, but the stage emitted
	// asynchronously; see Stage.Reconciliation.
	OutcomeDeferred Outcome = "deferred"
	// OutcomeUnresolved: the run did not complete, so absence of output is
	// not evidence of anything.
	OutcomeUnresolved Outcome = "unresolved"
)

// Stage kinds.
const (
	KindProcessor = "processor"
	KindConnector = "connector"
)

// Stage is the analysis of one instrumented processor instance (one
// component in one pipeline) or connector instance (one component and
// signal, shared by the pipelines it joins, so Pipeline is empty).
type Stage struct {
	Component string `json:"component"`
	Pipeline  string `json:"pipeline"`
	// Kind is "processor" or "connector".
	Kind     string       `json:"kind"`
	Type     string       `json:"type"`
	Signal   string       `json:"signal"`
	Emission Emission     `json:"emission"`
	Inputs   []StageInput `json:"inputs"`
	// Emissions are the seqs of outputs not attributable to one input.
	Emissions []int `json:"emissions,omitempty"`
	// Reconciliation compares everything the stage received with everything
	// it emitted. It is present for stages with Emissions when the run
	// completed, since only then is the output known to be final.
	Reconciliation *Diff `json:"reconciliation,omitempty"`
}

// StageInput is the outcome for one stage.input event.
type StageInput struct {
	Seq     int     `json:"seq"`
	Outputs []int   `json:"outputs,omitempty"`
	Outcome Outcome `json:"outcome"`
	// Error is the error the stage returned for this input (stage.error).
	Error string `json:"error,omitempty"`
	// Diff compares the input with its outputs. A connector's input has
	// no diff when it was delivered, since a route may copy records into
	// several pipelines; Routes and the record lineage describe it.
	Diff *Diff `json:"diff,omitempty"`
	// Routes are a connector's deliveries for this input, one per
	// pipeline.
	Routes []Route `json:"routes,omitempty"`
}

// Route is one delivery of a connector into a pipeline.
type Route struct {
	Pipeline string `json:"pipeline"`
	Seq      int    `json:"seq"`
	Items    int    `json:"items"`
}

// Analyze derives per-stage outcomes from recorded events. completed must be
// true only when the run shut down cleanly, which flushes buffering
// processors; otherwise missing output is reported as unresolved.
//
// Stages are keyed by component ID, pipeline and signal: the Collector
// builds one processor instance per pipeline.
func Analyze(events []instrument.Event, snaps instrument.Snapshots, completed bool) ([]Stage, error) {
	type key struct{ component, pipeline, signal string }
	var order []key
	stages := map[key]*Stage{}
	bySeq := map[int]instrument.Event{}
	for _, e := range events {
		bySeq[e.Seq] = e
		if e.Kind != instrument.KindIngress && e.Kind != instrument.KindEgress && e.Kind != instrument.KindStageError {
			continue
		}
		k := key{e.Component, e.Pipeline, e.Signal}
		kind := KindProcessor
		if e.Source == instrument.SourceConnectorWrapper {
			// A connector output's pipeline is where it was delivered,
			// and its signal may be another than the input's (count:
			// logs to metrics); it belongs to the stage of its input.
			k.pipeline, kind = "", KindConnector
			if in, ok := bySeq[e.InputSeq]; ok && e.InputSeq != 0 {
				k.signal = in.Signal
			}
		}
		s, ok := stages[k]
		if !ok {
			typ, _, _ := strings.Cut(e.Component, "/")
			s = &Stage{Component: e.Component, Pipeline: k.pipeline, Kind: kind, Type: typ, Signal: e.Signal}
			stages[k] = s
			order = append(order, k)
		}
		switch {
		case e.Kind == instrument.KindIngress:
			s.Inputs = append(s.Inputs, StageInput{Seq: e.Seq})
		case e.Kind == instrument.KindStageError:
			for i := range s.Inputs {
				if s.Inputs[i].Seq == e.InputSeq {
					s.Inputs[i].Error = e.Error
				}
			}
		case e.InputSeq != 0:
			for i := range s.Inputs {
				if s.Inputs[i].Seq == e.InputSeq {
					s.Inputs[i].Outputs = append(s.Inputs[i].Outputs, e.Seq)
					if kind == KindConnector {
						r := Route{Pipeline: e.Pipeline, Seq: e.Seq}
						if e.Counts != nil {
							r.Items = e.Counts.Items
						}
						s.Inputs[i].Routes = append(s.Inputs[i].Routes, r)
					}
				}
			}
		default:
			s.Emissions = append(s.Emissions, e.Seq)
		}
	}

	// Connectors that emitted outside any consume call, on any signal: a
	// connector that turns spans into metrics on a timer (spanmetrics)
	// emits into its metrics stage, not into that of its trace inputs.
	asyncConnectors := map[string]bool{}
	for _, s := range stages {
		if s.Kind == KindConnector && len(s.Emissions) > 0 {
			asyncConnectors[s.Component] = true
		}
	}

	out := make([]Stage, 0, len(order))
	for _, k := range order {
		s := stages[k]
		emittedLater := len(s.Emissions) > 0 || (s.Kind == KindConnector && asyncConnectors[s.Component])
		correlated := false
		for _, in := range s.Inputs {
			correlated = correlated || len(in.Outputs) > 0
		}
		switch {
		case emittedLater:
			s.Emission = EmissionAsynchronous
		case correlated:
			s.Emission = EmissionSynchronous
		default:
			s.Emission = EmissionNone
		}

		for i := range s.Inputs {
			in := &s.Inputs[i]
			before, err := snaps.Payload(bySeq[in.Seq].Snapshot)
			if err != nil {
				return nil, err
			}
			switch {
			case len(in.Outputs) > 0 && s.Kind == KindConnector:
				in.Outcome = OutcomeEmitted
			case len(in.Outputs) > 0:
				in.Outcome = OutcomeEmitted
				after, err := concatEvents(snaps, bySeq, in.Outputs, nil)
				if err != nil {
					return nil, err
				}
				d, err := diffPayloads(before, after, eventObjects(bySeq, []int{in.Seq}), eventObjects(bySeq, in.Outputs))
				if err != nil {
					return nil, err
				}
				in.Diff = &d
			case !completed:
				in.Outcome = OutcomeUnresolved
			case emittedLater:
				in.Outcome = OutcomeDeferred
			default:
				in.Outcome = OutcomeNoOutput
				d, err := DiffPayloads(before, telemetry.Empty(before.Signal))
				if err != nil {
					return nil, err
				}
				in.Diff = &d
			}
		}

		if len(s.Emissions) > 0 && completed {
			var ins []int
			for _, in := range s.Inputs {
				if len(in.Outputs) == 0 {
					ins = append(ins, in.Seq)
				}
			}
			after, err := concatEvents(snaps, bySeq, s.Emissions, nil)
			if err != nil {
				return nil, err
			}
			before, err := concatEvents(snaps, bySeq, ins, &after)
			if err != nil {
				return nil, err
			}
			d, err := diffPayloads(before, after, eventObjects(bySeq, ins), eventObjects(bySeq, s.Emissions))
			if err != nil {
				return nil, err
			}
			s.Reconciliation = &d
		}
		out = append(out, *s)
	}
	return out, nil
}

// eventObjects concatenates the record object numbers of seqs, aligned with
// concatEvents: an event without numbers for each of its records
// contributes zeros (unknown). It is nil when no event has numbers.
func eventObjects(bySeq map[int]instrument.Event, seqs []int) []int {
	var out []int
	known := false
	for _, seq := range seqs {
		e := bySeq[seq]
		items := 0
		if e.Counts != nil {
			items = e.Counts.Items
		}
		if len(e.Objects) == items && items > 0 {
			out, known = append(out, e.Objects...), true
			continue
		}
		out = append(out, make([]int, items)...)
	}
	if !known {
		return nil
	}
	return out
}

// concatEvents concatenates the snapshots of seqs. For an empty seqs it
// returns an empty payload of like's signal.
func concatEvents(snaps instrument.Snapshots, bySeq map[int]instrument.Event, seqs []int, like *telemetry.Payload) (telemetry.Payload, error) {
	if len(seqs) == 0 && like != nil {
		return telemetry.Empty(like.Signal), nil
	}
	payloads := make([]telemetry.Payload, 0, len(seqs))
	for _, seq := range seqs {
		p, err := snaps.Payload(bySeq[seq].Snapshot)
		if err != nil {
			return telemetry.Payload{}, err
		}
		if len(payloads) > 0 && p.Signal != payloads[0].Signal {
			return telemetry.Payload{}, fmt.Errorf("events %v mix signals", seqs)
		}
		payloads = append(payloads, p)
	}
	p, ok := telemetry.Concat(payloads)
	if !ok {
		return telemetry.Payload{}, fmt.Errorf("no events to concatenate")
	}
	return p, nil
}
