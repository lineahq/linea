package scenario

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Assertion is one expectation about the telemetry observed at a target
// boundary. Every condition it declares must hold.
type Assertion struct {
	// Name labels the assertion in reports; it defaults to its position.
	Name   string `yaml:"name"`
	Target Target `yaml:"target"`
	// Count bounds the number of records (spans, log records or metric data
	// points) at the target.
	Count *Bound `yaml:"count"`
	// Dropped bounds the records a processor received but did not emit.
	Dropped *Bound `yaml:"dropped"`
	// Attributes applies to the attributes of every record at the target.
	Attributes *Attributes `yaml:"attributes"`
	// ResourceAttributes applies to the resource of every record.
	ResourceAttributes *Attributes `yaml:"resource_attributes"`
}

// Target selects a boundary: the output of a recorded exporter, the
// output of one processor instance, or what a simulated endpoint of a
// component received.
type Target struct {
	Exporter  string `yaml:"exporter"`
	Processor string `yaml:"processor"`
	// Endpoint selects a simulated client endpoint of a component
	// that runs for real.
	Endpoint *EndpointTarget `yaml:"endpoint"`
	// Pipeline selects the processor instance; it may be omitted when the
	// processor is used by one pipeline of the signal.
	Pipeline string `yaml:"pipeline"`
	Signal   string `yaml:"signal"`
}

// EndpointTarget names a simulated endpoint: the component that declares
// it, as a topology ref ("extension/livecapture"), and where its client
// configuration sits in the component's configuration ("delivery").
type EndpointTarget struct {
	Component string `yaml:"component"`
	Path      string `yaml:"path"`
}

// String names the target, for example "exporter otlp/backend (logs)".
func (t Target) String() string {
	switch {
	case t.Exporter != "":
		return fmt.Sprintf("exporter %s (%s)", t.Exporter, t.Signal)
	case t.Endpoint != nil:
		return fmt.Sprintf("endpoint %s %s (%s)", t.Endpoint.Component, t.Endpoint.Path, t.Signal)
	case t.Pipeline != "":
		return fmt.Sprintf("processor %s in %s (%s)", t.Processor, t.Pipeline, t.Signal)
	}
	return fmt.Sprintf("processor %s (%s)", t.Processor, t.Signal)
}

// Bound is an exact or ranged count.
type Bound struct {
	Equals *int `yaml:"equals"`
	Min    *int `yaml:"min"`
	Max    *int `yaml:"max"`
}

// String describes the bound, for example "== 1" or ">= 1 and <= 3".
func (b Bound) String() string {
	if b.Equals != nil {
		return fmt.Sprintf("== %d", *b.Equals)
	}
	var parts []string
	if b.Min != nil {
		parts = append(parts, fmt.Sprintf(">= %d", *b.Min))
	}
	if b.Max != nil {
		parts = append(parts, fmt.Sprintf("<= %d", *b.Max))
	}
	return strings.Join(parts, " and ")
}

// Holds reports whether n is within the bound.
func (b Bound) Holds(n int) bool {
	if b.Equals != nil {
		return n == *b.Equals
	}
	return (b.Min == nil || n >= *b.Min) && (b.Max == nil || n <= *b.Max)
}

// Exceeded reports whether n is above the bound's upper limit. More data
// can only raise a count, so an exceeded bound is violated even when the run
// did not complete.
func (b Bound) Exceeded(n int) bool {
	if b.Equals != nil {
		return n > *b.Equals
	}
	return b.Max != nil && n > *b.Max
}

// Attributes are conditions on attribute maps. Keys are attribute names,
// matched exactly (no path syntax).
type Attributes struct {
	Present []string `yaml:"present"`
	Absent  []string `yaml:"absent"`
	// Equals maps keys to scalar values; the value type must match too, so
	// 200 does not equal "200" or 200.0.
	Equals map[string]any `yaml:"equals"`
	// Types maps keys to one of AttributeTypes.
	Types map[string]string `yaml:"types"`
}

// AttributeTypes are the accepted Attributes.Types values, after the OTLP
// AnyValue kinds.
var AttributeTypes = []string{"string", "bool", "int", "double", "bytes", "slice", "map"}

func (a Assertion) validate() error {
	t := a.Target
	n := 0
	for _, set := range []bool{t.Exporter != "", t.Processor != "", t.Endpoint != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errors.New("target needs exactly one of exporter, processor or endpoint")
	}
	if t.Processor == "" && t.Pipeline != "" {
		return errors.New("target.pipeline applies to processor targets only")
	}
	if e := t.Endpoint; e != nil {
		if kind, name, ok := strings.Cut(e.Component, "/"); !ok || kind == "" || name == "" {
			return fmt.Errorf("target.endpoint.component %q: want <kind>/<name>, for example extension/livecapture", e.Component)
		}
		if e.Path == "" {
			return errors.New("target.endpoint.path is required")
		}
	}
	switch t.Signal {
	case "traces", "metrics", "logs":
	case "":
		return errors.New("target.signal is required")
	default:
		return fmt.Errorf("target: unknown signal %q", t.Signal)
	}
	if a.Count == nil && a.Dropped == nil && a.Attributes == nil && a.ResourceAttributes == nil {
		return errors.New("no condition: declare count, dropped, attributes or resource_attributes")
	}
	if a.Dropped != nil && t.Processor == "" {
		return errors.New("dropped applies to processor targets only")
	}
	if a.Count != nil {
		if err := a.Count.validate(); err != nil {
			return fmt.Errorf("count: %w", err)
		}
	}
	if a.Dropped != nil {
		if err := a.Dropped.validate(); err != nil {
			return fmt.Errorf("dropped: %w", err)
		}
	}
	if a.Attributes != nil {
		if err := a.Attributes.validate(); err != nil {
			return fmt.Errorf("attributes: %w", err)
		}
	}
	if a.ResourceAttributes != nil {
		if err := a.ResourceAttributes.validate(); err != nil {
			return fmt.Errorf("resource_attributes: %w", err)
		}
	}
	return nil
}

func (b Bound) validate() error {
	if b.Equals == nil && b.Min == nil && b.Max == nil {
		return errors.New("declare equals, min or max")
	}
	if b.Equals != nil && (b.Min != nil || b.Max != nil) {
		return errors.New("equals cannot be combined with min or max")
	}
	for _, v := range []*int{b.Equals, b.Min, b.Max} {
		if v != nil && *v < 0 {
			return fmt.Errorf("negative bound %d", *v)
		}
	}
	if b.Min != nil && b.Max != nil && *b.Min > *b.Max {
		return fmt.Errorf("min %d is greater than max %d", *b.Min, *b.Max)
	}
	return nil
}

func (a Attributes) validate() error {
	if len(a.Present)+len(a.Absent)+len(a.Equals)+len(a.Types) == 0 {
		return errors.New("declare present, absent, equals or types")
	}
	for _, k := range a.Present {
		if slices.Contains(a.Absent, k) {
			return fmt.Errorf("%q is both present and absent", k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(a.Equals)) {
		switch v := a.Equals[k]; v.(type) {
		case string, bool, int, float64:
		default:
			return fmt.Errorf("equals %q: value must be a string, bool, int or double, got %T", k, v)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(a.Types)) {
		if typ := a.Types[k]; !slices.Contains(AttributeTypes, typ) {
			return fmt.Errorf("types %q: unknown type %q (want one of %s)", k, typ, strings.Join(AttributeTypes, ", "))
		}
	}
	return nil
}
