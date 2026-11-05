package platform

import (
	"context"
	"fmt"
	"strings"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// ExportScenario returns a run as a reviewable scenario file: the YAML
// CreateScenarioFromRun derives, preceded by a comment header naming the run,
// the build that admitted it, the environment names a rerun needs, its
// reproducibility and what was left out. An export is never saved, so Save
// and Label are refused; the caller writes the file.
func (s *Service) ExportScenario(ctx context.Context, caller Caller, runID string, req ScenarioFromRunRequest) (_ *ScenarioFromRun, err error) {
	defer s.audit(&caller, "ExportScenario", &err)()
	if req.Save || req.Label != "" {
		if err := s.begin(ctx, caller, authz.RunRead); err != nil {
			return nil, err
		}
		return nil, classify(fmt.Errorf("%w: an export is never saved", engine.ErrInvalidRequest), CodeInvalidRequest)
	}
	return s.CreateScenarioFromRun(ctx, caller, runID, req)
}

// exportHeader is built from server-computed facts and IDs only, one fact per
// comment line. The configuration, host environment values and file contents
// captured by configuration providers are never part of the scenario.
func exportHeader(runID string, parts *engine.BundleParts) string {
	env := "none"
	if len(parts.HostEnvironment) > 0 {
		env = strings.Join(parts.HostEnvironment, ", ") + " (names only; where the scenario runs, set them and pass --import-env NAME)"
	}
	lines := []string{
		"Scenario exported from linea run " + runID + ".",
		"Admitted by linea " + parts.ProductVersion + " with Collector " + parts.CollectorVersion + ".",
		"Build: " + parts.Build + ".",
		"Required environment: " + env + ".",
		"Reproducibility: " + parts.Reproducibility + ". " + parts.ReproductionReason,
		"Not included: the Collector configuration (test with your own), host environment values, file contents captured by configuration providers.",
		"The inputs below are recorded telemetry; review them before sharing.",
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString("# ")
		b.WriteString(oneLine(line))
		b.WriteByte('\n')
	}
	return b.String()
}

// oneLine keeps a header fact on its comment line.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x85 || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
}
