package inventory

import (
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/open-telemetry/opamp-go/protobufs"
)

func str(k, v string) *protobufs.KeyValue {
	return &protobufs.KeyValue{Key: k, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: v}}}
}

func TestAttributesAllowlist(t *testing.T) {
	in, err := New([]string{"team"}, false)
	if err != nil {
		t.Fatal(err)
	}
	got := in.Attributes(&protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{str("service.name", "otelcol"), str("host.name", "node-a")},
		NonIdentifyingAttributes: []*protobufs.KeyValue{
			str("host.name", "ignored"), str("team", " payments "), str("secret-host", "x"), str("os.description", " "),
			str("process.command_line", "--token=abc"),
			{Key: "service.instance.id", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_IntValue{IntValue: 7}}},
			{Key: "service.bytes", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_BytesValue{BytesValue: []byte("x")}}},
			str("os.type", strings.Repeat("x", fleet.MaxValueBytes+1)),
		},
	})
	want := map[string]string{"service.name": "otelcol", "host.name": "node-a", "team": "payments", "service.instance.id": "7"}
	if len(got) != len(want) {
		t.Fatalf("attributes = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("attributes[%s] = %q, want %q (%v)", k, got[k], v, got)
		}
	}
	if in.Dropped() != 5 {
		t.Fatalf("dropped = %d", in.Dropped())
	}
	if _, err := New([]string{"bad key"}, false); err == nil {
		t.Fatal("invalid operator key accepted")
	}
	if _, err := New(make([]string, MaxOperatorKeys+1), false); err == nil {
		t.Fatal("too many operator keys accepted")
	}
}

func TestHealthComponents(t *testing.T) {
	health := &protobufs.ComponentHealth{Healthy: false, Status: "StatusRecoverableError", LastError: "dial https://user:pw@backend:4317/v1?token=x failed",
		ComponentHealthMap: map[string]*protobufs.ComponentHealth{
			"pipeline:traces": {Healthy: false, ComponentHealthMap: map[string]*protobufs.ComponentHealth{
				"receiver:otlp":       {Healthy: true, Status: "StatusOK"},
				"exporter:otlp/a":     {Healthy: false, Status: "StatusRecoverableError", LastError: "password=hunter2"},
				"not a component key": {Healthy: true},
			}},
			"pipeline:metrics": {Healthy: true, ComponentHealthMap: map[string]*protobufs.ComponentHealth{
				"receiver:otlp": {Healthy: true, Status: "StatusOK"},
			}},
			"extensions": {Healthy: true, ComponentHealthMap: map[string]*protobufs.ComponentHealth{
				"extension:health_check": {Healthy: true, Status: "StatusOK"},
			}},
		}}
	in, _ := New(nil, false)
	status, components := in.Health(health)
	if status.Status != "StatusRecoverableError" || status.Error != "" {
		t.Fatalf("status = %+v", status)
	}
	list := Merge(nil, components)
	if len(list) != 3 || list[0].ID != "exporter:otlp/a" || list[1].ID != "extension:health_check" || list[2].ID != "receiver:otlp" {
		t.Fatalf("components = %+v", list)
	}
	if got := strings.Join(list[2].Pipelines, ","); got != "metrics,traces" {
		t.Fatalf("pipelines = %s", got)
	}
	if list[0].Error != "" || fleet.Unhealthy(list) != 1 {
		t.Fatalf("errors stored without opt-in: %+v", list[0])
	}

	in, _ = New(nil, true)
	status, components = in.Health(health)
	if status.Error != "dial https://backend:4317 failed" {
		t.Fatalf("redacted error = %q", status.Error)
	}
	if c := components["exporter:otlp/a"]; c.Error != "password=***" {
		t.Fatalf("component error = %q", c.Error)
	}
}

func TestConfigSummary(t *testing.T) {
	cfg := `receivers:
  otlp: {protocols: {grpc: {endpoint: "0.0.0.0:4317"}}}
processors:
  batch: {}
exporters:
  otlphttp: {headers: {authorization: "Bearer secret"}}
connectors:
  spanmetrics: {}
extensions:
  health_check: {}
service:
  extensions: [health_check]
  pipelines:
    traces: {receivers: [otlp], processors: [batch], exporters: [otlphttp, spanmetrics]}
    metrics: {receivers: [spanmetrics], exporters: [otlphttp]}
`
	effective := func(body string) *protobufs.EffectiveConfig {
		return &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
			"": {Body: []byte(body), ContentType: "text/yaml"}}}}
	}
	in, _ := New(nil, false)
	hash, components := in.Config(effective(cfg))
	if len(hash) != 64 {
		t.Fatalf("hash = %q", hash)
	}
	list := Merge(components, nil)
	ids := []string{}
	for _, c := range list {
		ids = append(ids, c.ID+"["+strings.Join(c.Pipelines, ",")+"]")
	}
	want := "connector:spanmetrics[metrics,traces] exporter:otlphttp[metrics,traces] extension:health_check[] processor:batch[traces] receiver:otlp[traces]"
	if got := strings.Join(ids, " "); got != want {
		t.Fatalf("components = %s", got)
	}
	if strings.Contains(strings.Join(ids, ""), "secret") {
		t.Fatal("configuration content leaked")
	}

	again, _ := in.Config(effective(cfg))
	other, _ := in.Config(effective(cfg + "\n"))
	if again != hash || other == hash {
		t.Fatal("hash is not a stable content hash")
	}
	if h, c := in.Config(effective("receivers: [unclosed")); h == "" || c != nil || in.Unsummarized() != 1 {
		t.Fatalf("invalid yaml: hash=%q components=%v skipped=%d", h, c, in.Unsummarized())
	}
	if h, c := in.Config(effective(strings.Repeat("#", MaxConfigBytes+1))); h == "" || c != nil || in.Unsummarized() != 2 {
		t.Fatal("oversized configuration was parsed")
	}
	if h, c := in.Config(nil); h != "" || c != nil {
		t.Fatal("missing configuration produced a summary")
	}
}

func TestMergeBounds(t *testing.T) {
	healthy := false
	reported := map[string]*fleet.Component{}
	for i := range MaxComponents + 10 {
		id := "receiver:r" + strings.Repeat("x", 100) + string(rune('a'+i%26)) + strings.Repeat("y", i%50) + string(rune('A'+i/26))
		reported[id] = &fleet.Component{ID: id, Healthy: &healthy, Status: "StatusPermanentError", Error: strings.Repeat("e", MaxErrorBytes)}
	}
	list := Merge(nil, reported)
	if len(list) == 0 || len(list) > MaxComponents || size(list) > MaxComponentsBytes {
		t.Fatalf("merged %d components, %d bytes", len(list), size(list))
	}
	if list[0].Error != "" {
		t.Fatal("errors were not dropped first")
	}
	if Merge(nil, nil) == nil {
		t.Fatal("empty merge must be non-nil")
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"connect to postgres://u:p@db:5432/x?sslmode=disable": "connect to postgres://db:5432",
		"Authorization: Bearer abc.def":                       "Authorization: ***",
		"got bearer abcdef":                                   "got bearer ***",
		"api_key='k1' rejected":                               "api_key=*** rejected",
		"id ABCDEFGHIJKLMNOPQRSTUVWXYZ012345 bad":             "id *** bad",
		"line\nbreak\x00":                                     "linebreak",
		"password authentication failed":                      "password authentication failed",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Redact(strings.Repeat("é", 400)); len(got) > MaxErrorBytes || !strings.HasPrefix(got, "é") {
		t.Fatalf("truncation = %d bytes", len(got))
	}
}
