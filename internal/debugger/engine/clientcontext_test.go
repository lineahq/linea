package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/recommend"
)

// TestContextLossesFollowConfiguredReaders: what each component behind a
// batch processor reads from the client context comes from its
// configuration, and a metadata key the batch lists in metadata_keys
// (matched case-insensitively) is not lost.
func TestContextLossesFollowConfiguredReaders(t *testing.T) {
	ins, err := Inspect(context.Background(), InspectRequest{ConfigYAML: `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
        include_metadata: true
processors:
  batch:
    metadata_keys: [Region]
  attributes/ctx:
    actions:
      - key: tenant
        from_context: metadata.tenant
        action: insert
      - key: ip
        from_context: client.address
        action: insert
  transform:
    log_statements:
      - context: log
        statements:
          - set(log.attributes["region"], otelcol.client.metadata["region"])
          - set(log.attributes["g"], otelcol.grpc.metadata["x-id"])
  k8sattributes:
  resource:
    attributes:
      - key: env
        value: prod
        action: insert
extensions:
  headers_setter:
    headers:
      - key: X-Tenant
        from_context: tenant_id
exporters:
  otlp/out:
    endpoint: localhost:4317
    auth:
      authenticator: headers_setter
service:
  extensions: [headers_setter]
  pipelines:
    logs:
      receivers: [otlp]
      processors: [batch, attributes/ctx, transform, k8sattributes, resource]
      exporters: [otlp/out]
`})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]recommend.Recommendation{}
	for _, r := range ins.Recommendations {
		if r.Rule == "batch.drops_client_context" {
			got[r.Components[1]] = r
		}
	}
	if len(got) != 4 {
		t.Fatalf("recommendations = %+v (diagnostics %+v)", ins.Recommendations, ins.Diagnostics)
	}
	if r := got["processor/attributes/ctx"]; r.Change == nil || r.Change.Op != recommend.OpSwap ||
		!strings.Contains(r.Summary, `metadata "tenant", the client address`) {
		t.Fatalf("attributes/ctx = %+v", r)
	}
	if r := got["processor/transform"]; !strings.Contains(r.Summary, `gRPC metadata "x-id"`) || strings.Contains(r.Summary, `metadata "region"`) ||
		r.Change != nil || len(r.Blockers) != 1 || !strings.Contains(r.Blockers[0], "attributes/ctx sit between") {
		t.Fatalf("transform = %+v", r)
	}
	if r := got["processor/k8sattributes"]; !strings.Contains(r.Summary, "pod_association is empty") {
		t.Fatalf("k8sattributes = %+v", r)
	}
	if r := got["exporter/otlp/out"]; r.Change == nil || r.Change.Op != recommend.OpSetOption || r.Change.Value != `["region","tenant_id"]` ||
		!strings.Contains(r.Summary, "headers_setter headers[0] (X-Tenant) from_context") {
		t.Fatalf("otlp/out = %+v", r)
	}
	if _, ok := got["processor/resource"]; ok {
		t.Fatal("resource reads no client context")
	}
}
