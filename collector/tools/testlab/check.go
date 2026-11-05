package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

// checkLab exercises real generator -> Collector -> extension -> receiver flow.
// It is a deliberate test action: existing capture stops and the viewer clears.
func checkLab(base string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	call := func(method, path string, value any, target any) error {
		var body []byte
		if value != nil {
			body, _ = json.Marshal(value)
		}
		request, err := http.NewRequest(method, base+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if value != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			data, _ := io.ReadAll(response.Body)
			return fmt.Errorf("%s %s: HTTP %d: %s", method, path, response.StatusCode, data)
		}
		if target != nil {
			return json.NewDecoder(response.Body).Decode(target)
		}
		return nil
	}
	wait := func(test func() (bool, error)) error {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			done, err := test()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return fmt.Errorf("test condition did not complete within 20s")
	}
	var current struct {
		Collector *collectorStatus `json:"collector"`
		Generator generatorState   `json:"generator"`
		Buffer    bufferStats      `json:"buffer"`
	}
	getState := func() error { return call("GET", "/api/state", nil, &current) }
	if err := wait(func() (bool, error) {
		if err := getState(); err != nil {
			return false, nil // Docker may have started the process before HTTP binds.
		}
		return current.Collector != nil && len(current.Collector.CapturePoints) == 6 && current.Generator.Traces > 0, nil
	}); err != nil {
		return err
	}
	if err := call("DELETE", "/api/capture", nil, nil); err != nil {
		return err
	}
	defer call("DELETE", "/api/capture", nil, nil)
	if err := call("DELETE", "/api/captures", nil, nil); err != nil {
		return err
	}
	if err := call("POST", "/api/generator", map[string]any{"enabled": true, "rate": 10}, nil); err != nil {
		return err
	}
	defer call("POST", "/api/generator", map[string]any{"enabled": true, "rate": 5}, nil)
	time.Sleep(300 * time.Millisecond)
	var page eventPage
	if err := call("GET", "/api/captures", nil, &page); err != nil {
		return err
	}
	if len(page.Entries) != 0 {
		return fmt.Errorf("viewer received data with capture off")
	}
	options := map[string]any{"signals": []string{"traces", "logs", "metrics"}, "points": []string{"ingress", "egress"}, "duration_seconds": 60, "sampling_rate": 1}
	if err := call("POST", "/api/capture", options, nil); err != nil {
		return err
	}
	if err := wait(func() (bool, error) {
		if err := call("GET", "/api/captures", nil, &page); err != nil {
			return false, err
		}
		seen := map[string]bool{}
		for _, item := range page.Entries {
			if item.SessionID == "" || item.Service == "" || bytes.Contains(item.OTLP, []byte("SYNTHETIC-ONLY-SECRET")) || bytes.Contains(item.OTLP, []byte("synthetic_private_value")) {
				return false, fmt.Errorf("missing capture identity or redaction failed")
			}
			seen[item.Signal+"/"+item.Point] = true
		}
		return len(seen) == 6, nil
	}); err != nil {
		return err
	}
	if err := call("DELETE", "/api/capture", nil, nil); err != nil {
		return err
	}
	if err := getState(); err != nil {
		return err
	}
	before := current.Buffer.Received
	generated := current.Generator.Traces
	time.Sleep(400 * time.Millisecond)
	if err := getState(); err != nil {
		return err
	}
	if current.Buffer.Received != before || current.Generator.Traces <= generated {
		return fmt.Errorf("stop did not isolate capture from the continuing generator")
	}
	if err := call("DELETE", "/api/captures", nil, nil); err != nil {
		return err
	}
	options["service"] = "checkout"
	// Exercise user-supplied OTTL in every supported context against real OTLP.
	options["ottl"] = map[string][]string{
		"resource":  {`IsString(attributes["service.name"])`},
		"span":      {`status.code == STATUS_CODE_ERROR`},
		"log":       {`severity_number >= SEVERITY_NUMBER_WARN`},
		"metric":    {`name == "test.request.duration"`},
		"datapoint": {`attributes["http.response.status_code"] >= 500`},
	}
	options["points"] = []string{"ingress"}
	options["duration_seconds"] = 5
	if err := call("POST", "/api/capture", options, nil); err != nil {
		return err
	}
	if err := wait(func() (bool, error) {
		if err := call("GET", "/api/captures", nil, &page); err != nil {
			return false, err
		}
		seen := map[string]bool{}
		for _, item := range page.Entries {
			if item.Service != "checkout" || item.Point != "ingress" {
				return false, fmt.Errorf("service/point filter leaked another record")
			}
			if err := checkErrorRecord(item); err != nil {
				return false, err
			}
			seen[item.Signal] = true
		}
		return len(seen) == 3, nil
	}); err != nil {
		return err
	}
	if err := wait(func() (bool, error) {
		if err := getState(); err != nil {
			return false, err
		}
		sessions := current.Collector.Sessions
		return len(sessions) > 0 && sessions[len(sessions)-1].State == "expired", nil
	}); err != nil {
		return err
	}
	before = current.Buffer.Received
	time.Sleep(300 * time.Millisecond)
	if err := getState(); err != nil {
		return err
	}
	if before != current.Buffer.Received {
		return fmt.Errorf("capture continued after TTL")
	}
	options["ottl"] = map[string][]string{"span": {`name ==`}}
	if err := call("POST", "/api/capture", options, nil); err == nil {
		return fmt.Errorf("invalid OTTL was accepted by the Collector")
	}
	fmt.Println("Test lab check passed: generator continues with capture off; all signals/points, redaction, stop, custom OTTL in all contexts, invalid-expression rejection, and automatic expiry work.")
	return nil
}

func checkErrorRecord(item entry) error {
	fail := fmt.Errorf("error filter leaked a non-error %s record", item.Signal)
	switch item.Signal {
	case "traces":
		request := ptraceotlp.NewExportRequest()
		if err := request.UnmarshalJSON(item.OTLP); err != nil {
			return err
		}
		for _, resource := range request.Traces().ResourceSpans().All() {
			for _, scope := range resource.ScopeSpans().All() {
				for _, span := range scope.Spans().All() {
					if span.Status().Code() != ptrace.StatusCodeError {
						return fail
					}
				}
			}
		}
	case "logs":
		request := plogotlp.NewExportRequest()
		if err := request.UnmarshalJSON(item.OTLP); err != nil {
			return err
		}
		for _, resource := range request.Logs().ResourceLogs().All() {
			for _, scope := range resource.ScopeLogs().All() {
				for _, record := range scope.LogRecords().All() {
					if record.SeverityNumber() < plog.SeverityNumberWarn {
						return fail
					}
				}
			}
		}
	case "metrics":
		request := pmetricotlp.NewExportRequest()
		if err := request.UnmarshalJSON(item.OTLP); err != nil {
			return err
		}
		for _, resource := range request.Metrics().ResourceMetrics().All() {
			for _, scope := range resource.ScopeMetrics().All() {
				for _, metric := range scope.Metrics().All() {
					for _, point := range metric.Gauge().DataPoints().All() {
						if code, ok := point.Attributes().Get("http.response.status_code"); !ok || code.Int() < 500 {
							return fail
						}
					}
				}
			}
		}
	default:
		return fmt.Errorf("unexpected captured signal %q", item.Signal)
	}
	return nil
}
