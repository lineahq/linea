package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// TestLocalCLIReadsOnlyImportedHostValues: a local run never reads the host
// environment unless --import-env names the variable; a declared scenario
// value wins, and anything else stands in.
func TestLocalCLIReadsOnlyImportedHostValues(t *testing.T) {
	t.Setenv("OTEL_CLI_BINDING", "localhost")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	plain, declared := filepath.Join(dir, "plain.yaml"), filepath.Join(dir, "declared.yaml")
	collectorYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: '${env:OTEL_CLI_BINDING}:4317'}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	input := "inputs: [{data: '{\"resourceSpans\":[]}'}]\n"
	for path, text := range map[string]string{configPath: collectorYAML, plain: "version: 1\n" + input, declared: "version: 1\nenvironment: {OTEL_CLI_BINDING: 127.0.0.1}\n" + input} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "quick run stands in", args: []string{"--fixture", fixture}, want: "stand_in"},
		{name: "scenario run stands in", args: []string{"--scenario", plain}, want: "stand_in"},
		{name: "imported host value", args: []string{"--fixture", fixture, "--import-env", "OTEL_CLI_BINDING"}, want: "host"},
		{name: "declared value wins", args: []string{"--scenario", declared, "--import-env", "OTEL_CLI_BINDING"}, want: "scenario"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), append([]string{"run", "--config", configPath}, tt.args...), &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			report := decodeReport(t, stdout.String())
			if report.Result.Status != "completed" {
				t.Fatalf("status %s", report.Result.Status)
			}
			source := ""
			for _, r := range report.References {
				if r.Target == "OTEL_CLI_BINDING" {
					source = r.Source
				}
			}
			if source != tt.want {
				t.Fatalf("references = %+v, want source %s", report.References, tt.want)
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"run", "--config", configPath, "--fixture", fixture, "--import-env", "NOT-A-NAME"}, &stdout, &stderr); code != exitError || !strings.Contains(stderr.String(), "--import-env NOT-A-NAME") {
		t.Fatalf("invalid import: exit %d: %s", code, stderr.String())
	}
}

// TestPlatformCapturesOnlyImportedHostValues serves a platform composed with
// the CLI's environment policy and runs a config that reads the host. Both
// runs are admitted: without the import the name stands in.
func TestPlatformCapturesOnlyImportedHostValues(t *testing.T) {
	t.Setenv("OTEL_SERVE_BINDING", "localhost")
	collectorYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: '${env:OTEL_SERVE_BINDING}:4317'}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	for _, tt := range []struct {
		imports []string
		status  int
	}{{nil, http.StatusAccepted}, {[]string{"OTEL_SERVE_BINDING"}, http.StatusAccepted}} {
		policy, err := localEnvironment(tt.imports)
		if err != nil {
			t.Fatal(err)
		}
		service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{Environment: policy})
		if err != nil {
			t.Fatal(err)
		}
		h := api.New(api.Options{Platform: service})
		srv := httptest.NewServer(h)
		body, _ := json.Marshal(map[string]string{"config_yaml": collectorYAML, "fixture_json": `{"resourceSpans":[]}`})
		resp, err := http.Post(srv.URL+"/api/v1/runs", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		srv.Close()
		h.Close()
		service.Close()
		if resp.StatusCode != tt.status {
			t.Fatalf("imports %v: status %d, want %d", tt.imports, resp.StatusCode, tt.status)
		}
	}
}

// TestServerReadsNoHostFile composes a platform with a team server's path
// policy, which has no roots: a request that names a host file is refused on
// every route, and a ${file:} reference stands in unread.
func TestServerReadsNoHostFile(t *testing.T) {
	dir := t.TempDir()
	collectorYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {transform: {trace_statements: [{context: span, statements: ['set(span.attributes[\"a\"], \"b\")']}]}}\nexporters: {debug: {}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [transform], exporters: [debug]}}}\n"
	input := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0102030405060708","name":"s"}]}]}]}`
	for name, text := range map[string]string{
		"config.yaml":    collectorYAML,
		"input.json":     input,
		"scenario.yaml":  "version: 1\ninputs: [{file: input.json}]\n",
		"exporters.yaml": "debug: {verbosity: private-file-content}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	h := api.New(api.Options{Platform: service})
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	post := func(route string, body map[string]any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+route, "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out bytes.Buffer
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.String()
	}
	fileConfig := strings.Replace(collectorYAML, "exporters: {debug: {}}", "exporters: ${file:"+filepath.Join(dir, "exporters.yaml")+"}", 1)
	for _, tt := range []struct {
		name, route string
		body        map[string]any
		status      int
	}{
		{"run fixture", "/api/v1/runs", map[string]any{"config_yaml": collectorYAML, "fixture": filepath.Join(dir, "input.json")}, http.StatusBadRequest},
		{"run config", "/api/v1/runs", map[string]any{"config": filepath.Join(dir, "config.yaml"), "fixture_json": input}, http.StatusBadRequest},
		{"run scenario", "/api/v1/runs", map[string]any{"config_yaml": collectorYAML, "scenario": filepath.Join(dir, "scenario.yaml")}, http.StatusBadRequest},
		{"scenario files", "/api/v1/runs", map[string]any{"config_yaml": collectorYAML, "scenario_yaml": "version: 1\ninputs: [{file: " + filepath.Join(dir, "input.json") + "}]\n"}, http.StatusForbidden},
		{"validate file provider", "/api/v1/config/validate", map[string]any{"config_yaml": fileConfig}, http.StatusOK},
		{"graph", "/api/v1/config/graph", map[string]any{"config": filepath.Join(dir, "config.yaml")}, http.StatusBadRequest},
		{"ottl evaluate", "/api/v1/ottl/evaluate", map[string]any{"config": filepath.Join(dir, "config.yaml"), "processor": "transform", "input": json.RawMessage(input)}, http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, body := post(tt.route, tt.body)
			if status != tt.status || strings.Contains(body, "private-file-content") || strings.Contains(body, "resourceSpans") {
				t.Fatalf("%d %s", status, body)
			}
			// A ${file:} reference stands in unread.
			if tt.name == "validate file provider" && !strings.Contains(body, "(outside_roots)") {
				t.Fatalf("file provider: %s", body)
			}
		})
	}
}
