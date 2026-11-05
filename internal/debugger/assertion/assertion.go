// Package assertion evaluates scenario assertions against a recorded run.
// It reads only recorded observations: exporter outputs,
// processor events and what simulated endpoints received. It never
// re-executes components.
package assertion

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// Outcome is the result of a check, an assertion or a whole test.
type Outcome string

const (
	// Pass: satisfied by a completed run.
	Pass Outcome = "pass"
	// Fail: violated by what was observed.
	Fail Outcome = "fail"
	// Inconclusive: the run could not establish the outcome, for example
	// because it did not complete or no records reached the target. An
	// incomplete run never passes.
	Inconclusive Outcome = "inconclusive"
	// Error: the test setup, configuration or run failed, or the assertion
	// names a target the configuration does not have.
	Error Outcome = "error"
)

// rank orders outcomes by severity.
var rank = map[Outcome]int{Pass: 0, Inconclusive: 1, Fail: 2, Error: 3}

// Worst returns the more severe outcome.
func Worst(a, b Outcome) Outcome {
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// maxViolations caps the violations listed per check; Observed still
// counts all of them.
const maxViolations = 5

// Report is the outcome of every assertion of a scenario.
type Report struct {
	Outcome   Outcome          `json:"outcome"`
	RunStatus runresult.Status `json:"run_status"`
	Results   []Result         `json:"results"`
}

// Result is the outcome of one assertion: the worst of its checks.
type Result struct {
	Name    string  `json:"name"`
	Target  string  `json:"target"`
	Outcome Outcome `json:"outcome"`
	Checks  []Check `json:"checks"`
}

// Check is one condition of an assertion, with the expected and observed
// values and, unless it passed, a diagnostic. A "target"
// check reports an assertion whose target cannot be observed; it has only a
// message.
type Check struct {
	Check      string      `json:"check"`
	Key        string      `json:"key,omitempty"`
	Outcome    Outcome     `json:"outcome"`
	Expected   string      `json:"expected"`
	Observed   string      `json:"observed,omitempty"`
	Message    string      `json:"message,omitempty"`
	Violations []Violation `json:"violations,omitempty"`
}

// Line summarizes a check that did not pass. A target check has only a
// message.
func (c Check) Line() string {
	check := c.Check
	if c.Key != "" {
		check += " " + c.Key
	}
	if c.Expected == "" {
		return fmt.Sprintf("%s %s: %s", c.Outcome, check, c.Message)
	}
	observed := c.Observed
	if observed == "" {
		observed = "(none)"
	}
	return fmt.Sprintf("%s %s: expected %s, observed %s", c.Outcome, check, c.Expected, observed)
}

// Violation locates one offending record or resource.
type Violation struct {
	Location string `json:"location"`
	Observed string `json:"observed"`
}

// Evaluate checks the assertions against a run of the configuration whose
// topology is g.
func Evaluate(assertions []scenario.Assertion, g *topology.Graph, r *runresult.Result) (*Report, error) {
	rep := &Report{Outcome: Pass, RunStatus: r.Status, Results: []Result{}}
	for i, a := range assertions {
		res := Result{Name: a.Name, Target: a.Target.String(), Outcome: Pass}
		if res.Name == "" {
			res.Name = fmt.Sprintf("assertions[%d]", i)
		}
		obs, err := observe(a.Target, g, r)
		if err != nil {
			return nil, err
		}
		if obs.problem != "" {
			res.Checks = []Check{{Check: "target", Outcome: Error, Message: obs.problem}}
		} else {
			res.Checks = capStandIns(evaluate(a, obs, r.Completed(), g, r), standInsUpstream(a.Target, obs, g))
		}
		for _, c := range res.Checks {
			res.Outcome = Worst(res.Outcome, c.Outcome)
		}
		rep.Outcome = Worst(rep.Outcome, res.Outcome)
		rep.Results = append(rep.Results, res)
	}
	return rep, nil
}

// observation is the telemetry recorded at a target.
type observation struct {
	// problem explains why the target cannot be observed.
	problem string
	// pipeline is the resolved pipeline of a processor target.
	pipeline string
	payloads []located
	// received and emitted count the records entering and leaving a
	// processor target; errors counts its stage.error events.
	received, emitted, errors int
}

// located is a payload with a label for diagnostics.
type located struct {
	label   string
	payload telemetry.Payload
}

func observe(t scenario.Target, g *topology.Graph, r *runresult.Result) (observation, error) {
	switch {
	case t.Exporter != "":
		return observeExporter(t, g, r)
	case t.Endpoint != nil:
		return observeEndpoint(t, g, r)
	}
	return observeProcessor(t, g, r)
}

func observeExporter(t scenario.Target, g *topology.Graph, r *runresult.Result) (observation, error) {
	ref := "exporter/" + t.Exporter
	pipelines := pipelinesUsing(g, ref, t.Signal)
	if len(pipelines) == 0 {
		return observation{problem: fmt.Sprintf("no %s pipeline exports to %s; %s", t.Signal, t.Exporter, available(g, topology.KindExporter, t.Signal))}, nil
	}
	// Captured and stand-in exporters are recorded instead of sending;
	// local ones (debug, nop) run for real and are recorded too.
	if c, _ := g.Component(ref); c.Mode != topology.ModeCaptured && c.Mode != topology.ModeStandIn && c.Mode != topology.ModeReal {
		return observation{problem: fmt.Sprintf("exporter %s is not recorded (mode %s: %s); target the last processor of its pipeline instead", t.Exporter, c.Mode, orDefault(c.Note, "it does not run"))}, nil
	}
	var obs observation
	for _, o := range r.Outputs {
		if o.Exporter != t.Exporter || o.Signal != t.Signal {
			continue
		}
		p, err := telemetry.Decode(o.Data)
		if err != nil {
			return obs, fmt.Errorf("exporter %s output: %w", t.Exporter, err)
		}
		obs.payloads = append(obs.payloads, located{label: "exporter " + t.Exporter, payload: p})
	}
	return obs, nil
}

// observeEndpoint reads what a simulated endpoint of a component received.
// The endpoint must be one the run simulated: a component
// without client endpoints, or one that did not run, has none.
func observeEndpoint(t scenario.Target, g *topology.Graph, r *runresult.Result) (observation, error) {
	e := t.Endpoint
	if _, ok := g.Component(e.Component); !ok {
		return observation{problem: fmt.Sprintf("the configuration has no component %s", e.Component)}, nil
	}
	var paths []string
	for _, s := range r.SimulatedEndpoints {
		if s.Component == e.Component {
			paths = append(paths, s.Path)
		}
	}
	if !slices.Contains(paths, e.Path) {
		if len(paths) == 0 {
			return observation{problem: fmt.Sprintf("%s has no simulated endpoint in this run; only custom components that run for real with client endpoints in their configuration have them", e.Component)}, nil
		}
		return observation{problem: fmt.Sprintf("%s has no simulated endpoint %s; its simulated endpoints: %s", e.Component, e.Path, strings.Join(paths, ", "))}, nil
	}
	var obs observation
	for _, o := range r.EndpointOutputs {
		if o.Component != e.Component || o.Endpoint != e.Path || o.Signal != t.Signal {
			continue
		}
		p, err := telemetry.Decode(o.Data)
		if err != nil {
			return obs, fmt.Errorf("endpoint %s %s output: %w", e.Component, e.Path, err)
		}
		obs.payloads = append(obs.payloads, located{label: "endpoint " + e.Path, payload: p})
	}
	return obs, nil
}

func observeProcessor(t scenario.Target, g *topology.Graph, r *runresult.Result) (observation, error) {
	ref := "processor/" + t.Processor
	pipelines := pipelinesUsing(g, ref, t.Signal)
	var obs observation
	switch {
	case len(pipelines) == 0:
		obs.problem = fmt.Sprintf("no %s pipeline uses processor %s; %s", t.Signal, t.Processor, available(g, topology.KindProcessor, t.Signal))
		return obs, nil
	case t.Pipeline != "":
		if !slices.Contains(pipelines, t.Pipeline) {
			obs.problem = fmt.Sprintf("pipeline %s does not use processor %s; it runs in %s", t.Pipeline, t.Processor, strings.Join(pipelines, ", "))
			return obs, nil
		}
		obs.pipeline = t.Pipeline
	case len(pipelines) > 1:
		obs.problem = fmt.Sprintf("processor %s runs in several %s pipelines (%s); set target.pipeline", t.Processor, t.Signal, strings.Join(pipelines, ", "))
		return obs, nil
	default:
		obs.pipeline = pipelines[0]
	}
	for _, e := range r.Events {
		if e.Component != t.Processor || e.Pipeline != obs.pipeline || e.Signal != t.Signal {
			continue
		}
		switch e.Kind {
		case instrument.KindIngress:
			obs.received += items(e)
		case instrument.KindStageError:
			obs.errors++
		case instrument.KindEgress:
			obs.emitted += items(e)
			p, err := r.Snapshots.Payload(e.Snapshot)
			if err != nil {
				return obs, fmt.Errorf("processor %s output seq %d: %w", t.Processor, e.Seq, err)
			}
			obs.payloads = append(obs.payloads, located{label: fmt.Sprintf("output seq %d", e.Seq), payload: p})
		}
	}
	return obs, nil
}

// standInsUpstream describes the components before the target whose
// behavior the run does not reproduce: stand-in processors and connectors,
// and any component whose settings use stand-in values. Stand-in
// receivers and exporters change no data, so they do not count. For an
// endpoint the component that sent it counts too.
func standInsUpstream(t scenario.Target, obs observation, g *topology.Graph) []string {
	pipelines := []string{obs.pipeline}
	ref := "processor/" + t.Processor
	var out []string
	switch {
	case t.Exporter != "":
		ref = "exporter/" + t.Exporter
		pipelines = pipelinesUsing(g, ref, t.Signal)
	case t.Endpoint != nil:
		ref = t.Endpoint.Component
		pipelines = pipelinesUsing(g, ref, t.Signal)
		if c, ok := g.Component(ref); ok && len(c.StandInValues) > 0 {
			out = append(out, fmt.Sprintf("%s (configured with stand-ins for %s)", c.Ref, strings.Join(c.StandInValues, ", ")))
		}
	}
	for _, p := range pipelines {
		for _, up := range g.Upstream(p, ref) {
			c, ok := g.Component(up)
			if !ok {
				continue
			}
			var why string
			switch {
			case c.Mode == topology.ModeStandIn && (c.Kind == topology.KindProcessor || c.Kind == topology.KindConnector):
				why = fmt.Sprintf("%s (type %s is not in the distribution)", c.Ref, c.Type)
			case len(c.StandInValues) > 0:
				why = fmt.Sprintf("%s (configured with stand-ins for %s)", c.Ref, strings.Join(c.StandInValues, ", "))
			default:
				continue
			}
			if !slices.Contains(out, why) {
				out = append(out, why)
			}
		}
	}
	return out
}

// capStandIns makes every passed or failed check inconclusive when the data
// passed stand-ins: what a stand-in did is not what the real component
// would have done, so neither outcome is established.
func capStandIns(checks []Check, standIns []string) []Check {
	if len(standIns) == 0 {
		return checks
	}
	reason := "the data passed " + strings.Join(standIns, " and ") + ", so the outcome is not established"
	for i, c := range checks {
		if c.Outcome != Pass && c.Outcome != Fail {
			continue
		}
		checks[i].Outcome = Inconclusive
		if c.Message != "" {
			checks[i].Message = reason + "; " + c.Message
		} else {
			checks[i].Message = reason
		}
	}
	return checks
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func items(e instrument.Event) int {
	if e.Counts == nil {
		return 0
	}
	return e.Counts.Items
}

// pipelinesUsing lists the pipelines of the signal that contain ref.
func pipelinesUsing(g *topology.Graph, ref, signal string) []string {
	var out []string
	for _, p := range g.Pipelines {
		if p.Signal == signal && (slices.Contains(p.Processors, ref) || slices.Contains(p.Exporters, ref)) {
			out = append(out, p.ID)
		}
	}
	return out
}

// available lists the components of a kind used by pipelines of the signal,
// as a hint for a mistyped target.
func available(g *topology.Graph, kind topology.Kind, signal string) string {
	var ids []string
	for _, p := range g.Pipelines {
		if p.Signal != signal {
			continue
		}
		refs := p.Processors
		if kind == topology.KindExporter {
			refs = p.Exporters
		}
		for _, ref := range refs {
			if id := strings.TrimPrefix(ref, string(kind)+"/"); !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return fmt.Sprintf("the configuration has no %s pipeline", signal)
	}
	return fmt.Sprintf("%s %ss: %s", signal, kind, strings.Join(ids, ", "))
}

func evaluate(a scenario.Assertion, obs observation, completed bool, g *topology.Graph, r *runresult.Result) []Check {
	var checks []Check
	records := 0
	for _, l := range obs.payloads {
		records += l.payload.Counts().Items
	}
	if a.Count != nil {
		c := Check{Check: "count", Expected: a.Count.String(), Observed: fmt.Sprint(records)}
		switch {
		case completed && a.Count.Holds(records):
			c.Outcome = Pass
		case completed || a.Count.Exceeded(records):
			c.Outcome = Fail
			c.Message = fmt.Sprintf("%d %s reached %s, expected %s", records, noun(a.Target.Signal), a.Target, a.Count)
			if trail := trails(a.Target, obs, g, r); trail != "" {
				c.Message += "; records per stage: " + trail
			}
		default:
			c.Outcome = Inconclusive
			c.Message = incomplete(r)
		}
		checks = append(checks, c)
	}
	if a.Dropped != nil {
		dropped := obs.received - obs.emitted
		c := Check{
			Check:    "dropped",
			Expected: a.Dropped.String(),
			Observed: fmt.Sprintf("%d (received %d, emitted %d)", dropped, obs.received, obs.emitted),
		}
		switch {
		case !completed:
			// A buffering processor may not have flushed yet.
			c.Outcome = Inconclusive
			c.Message = incomplete(r)
		case a.Dropped.Holds(dropped):
			c.Outcome = Pass
		default:
			c.Outcome = Fail
			c.Message = fmt.Sprintf("processor %s in %s dropped %d of %d %s, expected %s", a.Target.Processor, obs.pipeline, dropped, obs.received, noun(a.Target.Signal), a.Dropped)
			if obs.errors > 0 {
				c.Message += fmt.Sprintf("; it returned %d error(s), see its stage.error events", obs.errors)
			}
		}
		checks = append(checks, c)
	}
	if a.Attributes != nil {
		checks = append(checks, attributeChecks("attributes", "record(s)", *a.Attributes, recordAttributes(obs), completed, r)...)
	}
	if a.ResourceAttributes != nil {
		checks = append(checks, attributeChecks("resource_attributes", "resource(s)", *a.ResourceAttributes, resourceAttributes(obs), completed, r)...)
	}
	return checks
}

func incomplete(r *runresult.Result) string {
	msg := fmt.Sprintf("the run did not complete (status %s), so the observed telemetry may be incomplete", r.Status)
	if r.StatusReason != "" {
		msg += ": " + r.StatusReason
	}
	return msg
}

func noun(signal string) string {
	switch signal {
	case "traces":
		return "span(s)"
	case "metrics":
		return "data point(s)"
	}
	return "log record(s)"
}

// trails shows, for each pipeline feeding the target, how many records
// entered the pipeline and left each processor, so a failed count points at
// the stage that changed it.
func trails(t scenario.Target, obs observation, g *topology.Graph, r *runresult.Result) string {
	var pipelines []string
	switch {
	case t.Exporter != "":
		pipelines = pipelinesUsing(g, "exporter/"+t.Exporter, t.Signal)
	case t.Endpoint != nil:
		// What a component sends to its endpoints follows no pipeline.
		return ""
	default:
		pipelines = []string{obs.pipeline}
	}
	var out []string
	for _, id := range pipelines {
		var p topology.Pipeline
		for _, q := range g.Pipelines {
			if q.ID == id {
				p = q
			}
		}
		if len(p.Processors) == 0 {
			continue
		}
		first := strings.TrimPrefix(p.Processors[0], "processor/")
		parts := []string{fmt.Sprintf("in %d", stageItems(r, first, id, instrument.KindIngress))}
		for _, ref := range p.Processors {
			name := strings.TrimPrefix(ref, "processor/")
			parts = append(parts, fmt.Sprintf("%s %d", name, stageItems(r, name, id, instrument.KindEgress)))
			if name == t.Processor {
				break
			}
		}
		out = append(out, fmt.Sprintf("%s: %s", id, strings.Join(parts, " -> ")))
	}
	return strings.Join(out, "; ")
}

func stageItems(r *runresult.Result, component, pipeline string, kind instrument.Kind) int {
	n := 0
	for _, e := range r.Events {
		if e.Kind == kind && e.Component == component && e.Pipeline == pipeline {
			n += items(e)
		}
	}
	return n
}

// subject is an attribute map with its location.
type subject struct {
	location string
	attrs    pcommon.Map
}

func recordAttributes(obs observation) []subject {
	var out []subject
	for _, l := range obs.payloads {
		for _, rec := range telemetry.Records(l.payload) {
			attrs := pcommon.NewMap()
			if v, ok := rec.Fields.Get("attributes"); ok {
				attrs = v.Map()
			}
			out = append(out, subject{location: l.label + " " + rec.Ref(l.payload.Signal), attrs: attrs})
		}
	}
	return out
}

// resourceAttributes returns the resources that carry at least one record.
func resourceAttributes(obs observation) []subject {
	var out []subject
	for _, l := range obs.payloads {
		seen := map[int]bool{}
		for _, rec := range telemetry.Records(l.payload) {
			if seen[rec.Resource] {
				continue
			}
			seen[rec.Resource] = true
			res, _ := l.payload.ResourceAt(rec.Resource)
			out = append(out, subject{location: fmt.Sprintf("%s resource[%d]", l.label, rec.Resource), attrs: res.Attributes()})
		}
	}
	return out
}

func attributeChecks(name, unit string, spec scenario.Attributes, subjects []subject, completed bool, r *runresult.Result) []Check {
	var checks []Check
	add := func(check, key, expected string, violates func(pcommon.Value, bool) bool) {
		c := Check{Check: name + "." + check, Key: key, Expected: expected}
		bad := 0
		for _, s := range subjects {
			v, ok := s.attrs.Get(key)
			if !violates(v, ok) {
				continue
			}
			bad++
			if len(c.Violations) < maxViolations {
				c.Violations = append(c.Violations, Violation{Location: s.location, Observed: describe(v, ok)})
			}
		}
		c.Observed = fmt.Sprintf("%d of %d %s violate", bad, len(subjects), unit)
		switch {
		case bad > 0:
			c.Outcome = Fail
			c.Message = fmt.Sprintf("%q is not %s on %d of %d %s", key, expected, bad, len(subjects), unit)
		case len(subjects) == 0:
			c.Outcome = Inconclusive
			c.Message = "no records reached the target, so the condition could not be checked; add a count assertion if none are expected"
		case !completed:
			c.Outcome = Inconclusive
			c.Message = incomplete(r)
		default:
			c.Outcome = Pass
		}
		checks = append(checks, c)
	}
	for _, k := range spec.Present {
		add("present", k, "present", func(_ pcommon.Value, ok bool) bool { return !ok })
	}
	for _, k := range spec.Absent {
		add("absent", k, "absent", func(_ pcommon.Value, ok bool) bool { return ok })
	}
	for _, k := range sortedKeys(spec.Equals) {
		want := spec.Equals[k]
		add("equals", k, describeExpected(want), func(v pcommon.Value, ok bool) bool { return !ok || !equal(v, want) })
	}
	for _, k := range sortedKeys(spec.Types) {
		want := spec.Types[k]
		add("types", k, "of type "+want, func(v pcommon.Value, ok bool) bool { return !ok || typeName(v.Type()) != want })
	}
	return checks
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// equal compares an attribute with a scenario scalar, type included.
func equal(v pcommon.Value, want any) bool {
	switch w := want.(type) {
	case string:
		return v.Type() == pcommon.ValueTypeStr && v.Str() == w
	case bool:
		return v.Type() == pcommon.ValueTypeBool && v.Bool() == w
	case int:
		return v.Type() == pcommon.ValueTypeInt && v.Int() == int64(w)
	case float64:
		return v.Type() == pcommon.ValueTypeDouble && v.Double() == w
	}
	return false
}

func typeName(t pcommon.ValueType) string {
	switch t {
	case pcommon.ValueTypeStr:
		return "string"
	case pcommon.ValueTypeBool:
		return "bool"
	case pcommon.ValueTypeInt:
		return "int"
	case pcommon.ValueTypeDouble:
		return "double"
	case pcommon.ValueTypeBytes:
		return "bytes"
	case pcommon.ValueTypeSlice:
		return "slice"
	case pcommon.ValueTypeMap:
		return "map"
	}
	return "empty"
}

func describe(v pcommon.Value, ok bool) string {
	if !ok {
		return "absent"
	}
	if v.Type() == pcommon.ValueTypeStr {
		return fmt.Sprintf("%q (string)", v.Str())
	}
	return fmt.Sprintf("%s (%s)", v.AsString(), typeName(v.Type()))
}

func describeExpected(want any) string {
	switch w := want.(type) {
	case string:
		return fmt.Sprintf("%q (string)", w)
	case int:
		return fmt.Sprintf("%d (int)", w)
	case float64:
		return fmt.Sprintf("%v (double)", w)
	}
	return fmt.Sprintf("%v (bool)", want)
}
