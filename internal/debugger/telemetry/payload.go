// Package telemetry holds signal-agnostic OTLP payloads and their JSON
// encoding.
package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pipeline"
)

// Payload is one OTLP batch of a single signal. Exactly one of Traces,
// Metrics or Logs is meaningful, selected by Signal.
type Payload struct {
	Signal  pipeline.Signal
	Traces  ptrace.Traces
	Metrics pmetric.Metrics
	Logs    plog.Logs
}

// Counts summarizes the structure of a payload.
type Counts struct {
	Resources int `json:"resources"`
	Scopes    int `json:"scopes"`
	Items     int `json:"items"`
}

// FromTraces wraps traces without copying.
func FromTraces(td ptrace.Traces) Payload { return Payload{Signal: pipeline.SignalTraces, Traces: td} }

// FromMetrics wraps metrics without copying.
func FromMetrics(md pmetric.Metrics) Payload {
	return Payload{Signal: pipeline.SignalMetrics, Metrics: md}
}

// FromLogs wraps logs without copying.
func FromLogs(ld plog.Logs) Payload { return Payload{Signal: pipeline.SignalLogs, Logs: ld} }

// Decode parses an OTLP JSON document, detecting its signal from the
// top-level resourceSpans, resourceMetrics or resourceLogs key.
func Decode(data []byte) (Payload, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return Payload{}, fmt.Errorf("parse OTLP JSON: %w", err)
	}
	var found []pipeline.Signal
	for key, signal := range map[string]pipeline.Signal{
		"resourceSpans":   pipeline.SignalTraces,
		"resourceMetrics": pipeline.SignalMetrics,
		"resourceLogs":    pipeline.SignalLogs,
	} {
		if _, ok := keys[key]; ok {
			found = append(found, signal)
		}
	}
	if len(found) != 1 {
		return Payload{}, errors.New("OTLP JSON must contain exactly one of resourceSpans, resourceMetrics or resourceLogs")
	}

	var (
		p   = Payload{Signal: found[0]}
		err error
	)
	switch p.Signal {
	case pipeline.SignalTraces:
		p.Traces, err = (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(data)
	case pipeline.SignalMetrics:
		p.Metrics, err = (&pmetric.JSONUnmarshaler{}).UnmarshalMetrics(data)
	case pipeline.SignalLogs:
		p.Logs, err = (&plog.JSONUnmarshaler{}).UnmarshalLogs(data)
	}
	if err != nil {
		return Payload{}, fmt.Errorf("decode OTLP %s: %w", p.Signal, err)
	}
	return p, nil
}

// JSON encodes the payload as OTLP JSON. The result is a deep snapshot.
func (p Payload) JSON() ([]byte, error) {
	var data []byte
	var err error
	var key string
	switch p.Signal {
	case pipeline.SignalTraces:
		data, err = (&ptrace.JSONMarshaler{}).MarshalTraces(p.Traces)
		key = "resourceSpans"
	case pipeline.SignalMetrics:
		data, err = (&pmetric.JSONMarshaler{}).MarshalMetrics(p.Metrics)
		key = "resourceMetrics"
	case pipeline.SignalLogs:
		data, err = (&plog.JSONMarshaler{}).MarshalLogs(p.Logs)
		key = "resourceLogs"
	default:
		return nil, fmt.Errorf("unsupported signal %q", p.Signal)
	}
	// pdata omits the top-level resource array for empty signals. Keep the
	// signal explicit so a recorded snapshot remains independently decodable.
	if err == nil && string(data) == "{}" {
		return []byte(`{"` + key + `":[]}`), nil
	}
	return data, err
}

// Clone returns a deep copy.
func (p Payload) Clone() Payload {
	c := Payload{Signal: p.Signal}
	switch p.Signal {
	case pipeline.SignalTraces:
		c.Traces = ptrace.NewTraces()
		p.Traces.CopyTo(c.Traces)
	case pipeline.SignalMetrics:
		c.Metrics = pmetric.NewMetrics()
		p.Metrics.CopyTo(c.Metrics)
	case pipeline.SignalLogs:
		c.Logs = plog.NewLogs()
		p.Logs.CopyTo(c.Logs)
	}
	return c
}

// Counts reports resource, scope and item (span, data point, log record)
// totals.
func (p Payload) Counts() Counts {
	var c Counts
	switch p.Signal {
	case pipeline.SignalTraces:
		rs := p.Traces.ResourceSpans()
		c.Resources = rs.Len()
		for i := range rs.Len() {
			c.Scopes += rs.At(i).ScopeSpans().Len()
		}
		c.Items = p.Traces.SpanCount()
	case pipeline.SignalMetrics:
		rm := p.Metrics.ResourceMetrics()
		c.Resources = rm.Len()
		for i := range rm.Len() {
			c.Scopes += rm.At(i).ScopeMetrics().Len()
		}
		c.Items = p.Metrics.DataPointCount()
	case pipeline.SignalLogs:
		rl := p.Logs.ResourceLogs()
		c.Resources = rl.Len()
		for i := range rl.Len() {
			c.Scopes += rl.At(i).ScopeLogs().Len()
		}
		c.Items = p.Logs.LogRecordCount()
	}
	return c
}

// Concat merges payloads of one signal into a single payload by appending
// their resource entries in order. It returns ok=false for an empty input.
func Concat(payloads []Payload) (Payload, bool) {
	if len(payloads) == 0 {
		return Payload{}, false
	}
	out := Payload{Signal: payloads[0].Signal}
	switch out.Signal {
	case pipeline.SignalTraces:
		out.Traces = ptrace.NewTraces()
		for _, p := range payloads {
			p.Clone().Traces.ResourceSpans().MoveAndAppendTo(out.Traces.ResourceSpans())
		}
	case pipeline.SignalMetrics:
		out.Metrics = pmetric.NewMetrics()
		for _, p := range payloads {
			p.Clone().Metrics.ResourceMetrics().MoveAndAppendTo(out.Metrics.ResourceMetrics())
		}
	case pipeline.SignalLogs:
		out.Logs = plog.NewLogs()
		for _, p := range payloads {
			p.Clone().Logs.ResourceLogs().MoveAndAppendTo(out.Logs.ResourceLogs())
		}
	}
	return out, true
}

// Empty returns a payload of the signal with no resources.
func Empty(signal pipeline.Signal) Payload {
	return Payload{Signal: signal, Traces: ptrace.NewTraces(), Metrics: pmetric.NewMetrics(), Logs: plog.NewLogs()}
}
