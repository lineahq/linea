// Package testbed runs the capture processor through the OpenTelemetry
// Collector Contrib testbed: a load generator sends OTLP to a real Collector
// child process and a mock backend counts what the main pipeline delivers,
// while the testbed samples the Collector's CPU and RSS. The scenarios mirror
// contrib's 10k items per second tests so the figures are comparable to
// https://opentelemetry.io/docs/collector/benchmarks/.
//
// Tests run only when RUN_TESTBED is set; see
// docs/collector-components/benchmarks.md.
package testbed

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/testbed/testbed"
)

// collectorEnv names a prebuilt Collector binary. Without it, TestMain builds
// the test-only distribution from collector/distribution.
const collectorEnv = "LINEA_TESTBED_COLLECTOR"

var performanceResultsSummary testbed.TestResultsSummary = &testbed.PerformanceResults{}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if os.Getenv("RUN_TESTBED") == "" {
		log.Print("RUN_TESTBED is not set, skipping capture processor benchmarks")
		return 0
	}
	binary := os.Getenv(collectorEnv)
	if binary == "" {
		dir, err := os.MkdirTemp("", "linea-testbed-")
		if err != nil {
			log.Print(err)
			return 1
		}
		defer os.RemoveAll(dir)
		binary = filepath.Join(dir, "otelcol-livecapture")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		log.Printf("Building the test distribution into %s", binary)
		build := exec.Command("go", "build", "-trimpath", "-o", binary, ".")
		build.Dir = filepath.Join("..", "..", "distribution")
		// This module runs with GOWORK=off; the distribution builds in the
		// repository workspace as it does everywhere else.
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GOWORK=") {
				build.Env = append(build.Env, kv)
			}
		}
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			log.Printf("build the test distribution: %v", err)
			return 1
		}
	}
	abs, err := filepath.Abs(binary)
	if err != nil {
		log.Print(err)
		return 1
	}
	testbed.GlobalConfig.DefaultAgentExeRelativeFile = abs
	if err := testbed.Start(performanceResultsSummary); err != nil {
		log.Print(err)
		return 1
	}
	code := m.Run()
	testbed.SaveResults(performanceResultsSummary)
	return code
}
