package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
)

func TestHTTPReferencePreflightPreservesBothInputModes(t *testing.T) {
	for _, mode := range []string{"quick", "scenario"} {
		t.Run(mode, func(t *testing.T) {
			s, srv := newServerOptions(t, Options{})
			req := engine.Request{
				ConfigYAML:  "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlphttp: {endpoint: http://example.invalid:4318, proxy_url: http://private-proxy:8888}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlphttp]}}}\n",
				FixtureJSON: `{"resourceSpans":[]}`, Compare: true,
			}
			if mode == "scenario" {
				req.FixtureJSON = ""
				req.ScenarioYAML = "version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\n"
			}
			failure := decodeBody[Error](t, post(t, srv.URL+"/api/v1/runs", req), http.StatusUnprocessableEntity)
			if !strings.Contains(failure.Error, "proxy_url") || strings.Contains(failure.Error, "private-proxy") || len(listRuns(t, s.runs)) != 0 {
				t.Fatalf("reference prerequisite boundary lost: %+v", failure)
			}
			req.Compare = false
			created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", req), http.StatusAccepted)
			report := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)
			if report.Result.Status != "completed" || report.Comparison != nil {
				t.Fatal("instrumented proxy substitution changed")
			}
		})
	}
}
