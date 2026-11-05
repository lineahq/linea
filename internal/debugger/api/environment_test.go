package api

import (
	"net/http"
	"testing"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestHTTPEnvironmentPolicyBelongsToServiceOwner(t *testing.T) {
	const name = "OTEL_HTTP_BINDING"
	t.Setenv(name, "captured:4317")
	collectorYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: '${env:" + name + "}'}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	for _, mode := range []string{"quick", "scenario"} {
		for _, policy := range []string{"local compatibility", "injected strict", "injected allowlist"} {
			t.Run(mode+"/"+policy, func(t *testing.T) {
				opts := Options{}
				if policy != "local compatibility" {
					capturePolicy := config.EnvironmentPolicy{}
					if policy == "injected allowlist" {
						capturePolicy.Host = []string{name}
					}
					opts.Platform = platform.New(platform.Options{Environment: capturePolicy})
					t.Cleanup(opts.Platform.Close)
				}
				s, srv := newServerOptions(t, opts)
				req := engine.Request{ConfigYAML: collectorYAML, FixtureJSON: `{"resourceSpans":[]}`}
				if mode == "scenario" {
					req.FixtureJSON = ""
					req.ScenarioYAML = "version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\n"
				}
				response := post(t, srv.URL+"/api/v1/runs", req)
				if policy == "injected strict" {
					decodeBody[Error](t, response, http.StatusUnprocessableEntity)
					if len(listRuns(t, s.runs)) != 0 {
						t.Fatal("denied binding admitted a run")
					}
					return
				}
				created := decodeBody[Run](t, response, http.StatusAccepted)
				want := "legacy_host"
				if policy == "injected allowlist" {
					want = "allowlisted_host"
				}
				if created.Bundle == nil || created.Bundle.Environment != want {
					t.Fatalf("policy ownership lost: %+v", created.Bundle)
				}
				report := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)
				if report.Result.Status != "completed" {
					t.Fatal("bound execution failed", report.Result.Status)
				}
			})
		}
	}
}
