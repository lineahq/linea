package main

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRunVerifiesRecommendationsLocallyAndConnected: --verify-recommendations
// runs logs-cleanup's filter move on the run's inputs, raises its confidence
// in the printed report and lists each verification, on the in-process
// service and on a team server alike. A change the platform cannot verify is
// listed as such and does not fail the command.
func TestRunVerifiesRecommendationsLocallyAndConnected(t *testing.T) {
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	args := []string{"run", "--config", filepath.Join(dir, "config.yaml"), "--scenario", filepath.Join(dir, "scenario.yaml"),
		"--format", "text", "--verify-recommendations"}
	verified := regexp.MustCompile(`recommendation \d+ filter\.move_earlier: same output on this input \(candidate run \S+\)`)
	measured := regexp.MustCompile(`(?m)^      record_passes of \S+: \d+ -> \d+$`)
	for name, extra := range map[string][]string{"local": nil, "connected": {"--server", startServe(t)}} {
		t.Run(name, func(t *testing.T) {
			code, out, stderr := runCLI(t, append(args, extra...)...)
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if !strings.Contains(out, "verified_on_input] filter.move_earlier") || !verified.MatchString(out) {
				t.Fatalf("output:\n%s", out)
			}
			// The candidate measures the record passes the move saves.
			if !measured.MatchString(out) {
				t.Fatalf("no measured record passes in output:\n%s", out)
			}
			// Statement and option changes are not verified yet.
			if strings.Contains(out, "change: remove") && !strings.Contains(out, "not verifiable") {
				t.Fatalf("output:\n%s", out)
			}
		})
	}

	// Without the flag nothing is verified.
	code, out, stderr := runCLI(t, args[:len(args)-1]...)
	if code != exitOK || strings.Contains(out, "verifications:") || !strings.Contains(out, "observed] filter.move_earlier") {
		t.Fatalf("exit %d: %s%s", code, out, stderr)
	}
}
