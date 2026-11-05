package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A run's log keeps the Collector's lines with every environment value
// redacted, raw or JSON-escaped.
func TestRunLogRedactsEnvironmentValues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const secret = `tok-"3x9"-secret`
	config := `receivers:
  otlp:
    protocols:
      http:
processors:
  attributes:
    actions:
      - key: credential
        value: ${env:API_TOKEN}
        action: insert
exporters:
  debug:
    verbosity: detailed
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [attributes]
      exporters: [debug]
`
	scenario := `version: 1
name: log
environment: {API_TOKEN: '` + secret + `'}
inputs:
  - data: '{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"checkout","kind":1}]}]}]}'
`
	log := &LogTail{}
	if _, err := Execute(ctx, Request{ConfigYAML: config, ScenarioYAML: scenario, Log: log}); err != nil {
		t.Fatal(err)
	}
	text, truncated := log.Tail()
	if truncated || !strings.Contains(text, "credential") || !strings.Contains(text, redactedValue) {
		t.Fatalf("log (truncated %t):\n%s", truncated, text)
	}
	for _, leak := range []string{secret, `tok-\"3x9\"-secret`, "3x9"} {
		if strings.Contains(text, leak) {
			t.Fatalf("log shows %q:\n%s", leak, text)
		}
	}
}

// The tail keeps at most RunLogLimit bytes of whole lines.
func TestLogTailKeepsWholeLinesWithinTheLimit(t *testing.T) {
	var tail LogTail
	line := strings.Repeat("x", 99) + "\n"
	for range 2 * RunLogLimit / len(line) {
		_, _ = tail.Write([]byte(line))
	}
	_, _ = tail.Write([]byte("last\n"))
	text, truncated := tail.Tail()
	if !truncated || len(text) > RunLogLimit || !strings.HasPrefix(text, line) || !strings.HasSuffix(text, line+"last\n") {
		t.Fatalf("truncated %t, %d bytes, starts %q", truncated, len(text), text[:min(len(text), 20)])
	}

	var short LogTail
	short.Append("worker line\n", true)
	if text, truncated := short.Tail(); text != "worker line\n" || !truncated {
		t.Fatalf("appended = %q, %t", text, truncated)
	}
}
