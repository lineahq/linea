package platform

import (
	"errors"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestReferencePreflightBeforeAdmissionInBothModes(t *testing.T) {
	for _, mode := range []string{"quick", "scenario"} {
		t.Run(mode, func(t *testing.T) {
			s := openPlatform(t, memory.NewData(), Options{})
			t.Cleanup(s.Close)
			req := engine.Request{
				ConfigYAML:  "receivers: {otlp: {protocols: {grpc: {tls: {cert_file: private-cert, key_file: private-key}}}}}\nexporters: {otlp: {endpoint: example.invalid:4317, tls: {ca_file: private-ca}}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n",
				FixtureJSON: `{"resourceSpans":[]}`, Compare: true,
			}
			if mode == "scenario" {
				req.FixtureJSON = ""
				req.ScenarioYAML = "version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\n"
			}
			if h, err := s.Submit(req); h != nil || !errors.Is(err, runresult.ErrReferencePrerequisite) || strings.Contains(err.Error(), "private-") || len(s.List()) != 0 {
				t.Fatalf("invalid reference admitted: %v, %v", h, err)
			}
			// These same settings never run on the substituted instrumented
			// transports. They must remain accepted without host file reads.
			req.Compare = false
			r := submit(t, s, req)
			view := waitRun(t, r)
			if view.State != StateDone || view.Report.Result.Status != "completed" || view.Bundle.Reproducibility != "unverified" {
				t.Fatalf("instrumented transport incorrectly blocked: %+v", view)
			}
		})
	}
}
