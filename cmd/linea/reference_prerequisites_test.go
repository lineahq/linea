package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIReferencePreflightPreservesBothInputModes(t *testing.T) {
	dir := t.TempDir()
	configPath, scenarioPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "scenario.yaml")
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: example.invalid:4317, tls: {ca_file: private-ca}}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	for path, text := range map[string]string{configPath: yaml, scenarioPath: "version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\n"} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"quick", "scenario"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"run", "--config", configPath, "--fixture", fixture}
			if mode == "scenario" {
				args[3], args[4] = "--scenario", scenarioPath
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), append(args, "--compare"), &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "tls.ca_file") || strings.Contains(stderr.String(), "private-ca") {
				t.Fatalf("reference preflight: stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
			stdout.Reset()
			stderr.Reset()
			if code := run(context.Background(), args, &stdout, &stderr); code != 0 {
				t.Fatalf("instrumented execution: %d: %s", code, stderr.String())
			}
			if report := decodeReport(t, stdout.String()); report.Result.Status != "completed" || report.Comparison != nil {
				t.Fatal("instrumented TLS substitution changed")
			}
		})
	}
}
