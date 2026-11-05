package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/registry"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/otelcol"
	"go.uber.org/zap"
)

func writeConfigFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func rawSettings(t *testing.T, settings otelcol.ConfigProviderSettings) map[string]any {
	t.Helper()
	raw, err := resolveRawSettings(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return raw.ToStringMap()
}

func TestCapturedProvidersMatchUpstreamAndSurviveSourceMutation(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	child, root := filepath.Join(dir, "processor.yaml"), filepath.Join(dir, "config.yaml")
	childYAML := "attributes: [{key: captured, value: '${env:OTEL_SNAPSHOT_VALUE}', action: upsert}]\n"
	rootYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\r\nprocessors: {resource: '${file:" + child + "}'}\r\nexporters: {otlp: {endpoint: example.invalid:4317}}\r\nservice: {pipelines: {traces: {receivers: [otlp], processors: [resource], exporters: [otlp]}}}\r\n"
	writeConfigFile(t, child, childYAML)
	writeConfigFile(t, root, rootYAML)
	t.Setenv("OTEL_SNAPSHOT_VALUE", "captured-value")
	resolved, err := CaptureSource(context.Background(), Source{Path: root}, factories, nil, CaptureLimits{})
	if err != nil {
		t.Fatal(err)
	}
	upstream := rawSettings(t, ProviderSettings(resolved.URIs, nil))
	if got := rawSettings(t, resolved.ProviderSettings(resolved.URIs)); !reflect.DeepEqual(got, upstream) {
		t.Fatalf("frozen differs from upstream: %#v vs %#v", got, upstream)
	}
	if len(resolved.Snapshot.data.Values) != 3 {
		t.Fatalf("dependencies = %+v", resolved.Snapshot.data.Values)
	}
	writeConfigFile(t, root, "invalid: [")
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_SNAPSHOT_VALUE", "mutated-value")
	if got := rawSettings(t, resolved.ProviderSettings(resolved.URIs)); !reflect.DeepEqual(got, upstream) {
		t.Fatal("retrieval consulted mutated sources")
	}
	if _, err := resolved.Snapshot.Resolve(context.Background(), factories); err != nil {
		t.Fatal("typed resolution reread sources", err)
	}
	if !bytes.Equal(resolved.Snapshot.SourceBytes(), []byte(rootYAML)) {
		t.Fatal("root bytes changed")
	}
	copy := resolved.Snapshot.SourceBytes()
	copy[0] = '!'
	if !bytes.Equal(resolved.Snapshot.SourceBytes(), []byte(rootYAML)) {
		t.Fatal("source bytes are mutable")
	}
	raw, err := json.Marshal(resolved.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := DecodeSnapshot(raw, CaptureLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rawSettings(t, reopened.ProviderSettings(resolved.URIs)), upstream) {
		t.Fatal("serialization changed captured retrievals")
	}
}

func TestCapturedEnvironmentRetainsDefaultsTypesAndEmptySemantics(t *testing.T) {
	const name = "OTEL_CAPTURE_MISSING"
	old, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, old)
		} else {
			_ = os.Unsetenv(name)
		}
	})
	cases := []struct {
		name, uri string
		env       map[string]string
	}{
		{"unset", "env:" + name, nil},
		{"empty default", "env:" + name + ":-", nil},
		{"number default", "env:" + name + ":-9223372036854775807", nil},
		{"bool default", "env:" + name + ":-false", nil},
		{"map default", "env:" + name + ":-{a: 1}", nil},
		{"override empty", "env:" + name + ":-unused", map[string]string{name: ""}},
		{"override number", "env:" + name, map[string]string{name: "42"}},
		{"override string", "env:" + name, map[string]string{name: "a: [not yaml"}},
		{"override map", "env:" + name, map[string]string{name: "{a: 1}"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			upstream := scenarioEnvFactory(tt.env).Create(confmap.ProviderSettings{Logger: zap.NewNop()})
			defer upstream.Shutdown(context.Background())
			want, err := upstream.Retrieve(context.Background(), tt.uri, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer want.Close(context.Background())
			c := &capture{limits: (CaptureLimits{}).defaults(), values: map[string]capturedValue{}}
			provider := &captureProvider{Provider: upstream, capture: c}
			got, err := provider.Retrieve(context.Background(), tt.uri, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantRaw, _ := want.AsRaw()
			gotRaw, _ := got.AsRaw()
			wantText, _ := want.AsString()
			gotText, _ := got.AsString()
			if !reflect.DeepEqual(wantRaw, gotRaw) || wantText != gotText {
				t.Fatalf("retrieved changed: (%#v, %q) vs (%#v, %q)", gotRaw, gotText, wantRaw, wantText)
			}
			_ = os.Setenv(name, "later")
			defer os.Unsetenv(name)
			frozen := frozenFactory("env", c.values).Create(confmap.ProviderSettings{})
			replay, err := frozen.Retrieve(context.Background(), tt.uri, nil)
			if err != nil {
				t.Fatal(err)
			}
			replayRaw, _ := replay.AsRaw()
			replayText, _ := replay.AsString()
			if !reflect.DeepEqual(replayRaw, wantRaw) || replayText != wantText {
				t.Fatal("frozen environment changed")
			}
		})
	}
}

func TestFrozenProvidersRejectUncapturedDependenciesAndAllowPureYAMLOverrides(t *testing.T) {
	factories, _ := registry.Factories()
	text := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	r, err := CaptureSource(context.Background(), Source{YAML: text}, factories, nil, CaptureLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, override := range []string{"yaml:exporters::otlp::endpoint: ${env:UNCAPTURED}", "yaml:processors: ${file:uncaptured.yaml}"} {
		uris := append(append([]string(nil), r.URIs...), override)
		if _, err := resolveRawSettings(context.Background(), r.ProviderSettings(uris)); !errors.Is(err, ErrUncaptured) {
			t.Fatalf("uncaptured URI resolved: %v", err)
		}
	}
	uris := append(append([]string(nil), r.URIs...), "yaml:exporters::otlp::endpoint: 127.0.0.1:4317")
	if _, err := resolveSettings(context.Background(), r.ProviderSettings(uris), factories); err != nil {
		t.Fatal("pure reference override refused", err)
	}
	if r.Snapshot.data.URIs[0] != "yaml:"+text {
		t.Fatal("override changed snapshot root")
	}
}

func TestCaptureBoundsCancellationAndDecodedSnapshotIntegrity(t *testing.T) {
	factories, _ := registry.Factories()
	root := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigFile(t, root, strings.Repeat("a", 1000))
	if _, err := CaptureSource(context.Background(), Source{Path: root}, factories, nil, CaptureLimits{Bytes: 256}); !errors.Is(err, ErrCaptureLimit) {
		t.Fatalf("unbounded file capture = %v", err)
	}
	child := filepath.Join(filepath.Dir(root), "child.yaml")
	writeConfigFile(t, child, "attributes: []")
	writeConfigFile(t, root, "processors: {resource: '${file:"+child+"}'}")
	if _, err := CaptureSource(context.Background(), Source{Path: root}, factories, nil, CaptureLimits{Dependencies: 1}); !errors.Is(err, ErrCaptureLimit) {
		t.Fatalf("unbounded dependency capture = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CaptureSource(ctx, Source{Path: root}, factories, nil, CaptureLimits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture = %v", err)
	}
	for _, raw := range []string{`{}`, `{"schema":2}`, `{"schema":1,"uris":[],"values":{}}`, `{"schema":1,"uris":["file:missing"],"values":{}}`, `{"schema":1,"uris":["yaml:x"],"values":{"http:x":{"content_base64":""}}}`, `{"schema":1,"uris":["yaml:x"],"values":{"env:X":{"content_base64":"YQ==","empty_string":true}}}`} {
		if _, err := DecodeSnapshot([]byte(raw), CaptureLimits{}); !errors.Is(err, ErrSnapshot) {
			t.Fatalf("invalid snapshot accepted: %s, %v", raw, err)
		}
	}
}

func TestCapturedRelativeFileURIsKeepCollectorWorkingDirectorySemantics(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir("nested", 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfigFile(t, "dependency.yaml", "attributes: [{key: captured, value: cwd, action: upsert}]")
	writeConfigFile(t, filepath.Join("nested", "dependency.yaml"), "attributes: [{key: captured, value: configdir, action: upsert}]")
	text := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {resource: '${file:dependency.yaml}'}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [resource], exporters: [otlp]}}}\n"
	writeConfigFile(t, filepath.Join("nested", "config.yaml"), text)
	factories, _ := registry.Factories()
	r, err := CaptureSource(context.Background(), Source{Path: filepath.Join("nested", "config.yaml")}, factories, nil, CaptureLimits{})
	if err != nil {
		t.Fatal(err)
	}
	want := rawSettings(t, ProviderSettings(r.URIs, nil))
	if bytes.Contains(r.Snapshot.data.Values["file:dependency.yaml"].Content, []byte("configdir")) {
		t.Fatal("resolved against config directory rather than upstream cwd")
	}
	t.Chdir(t.TempDir())
	if got := rawSettings(t, r.ProviderSettings(r.URIs)); !reflect.DeepEqual(got, want) {
		t.Fatal("frozen relative file URI followed the new cwd")
	}
}

func TestSnapshotDecodingAccountsForRepeatedEscapedRootURI(t *testing.T) {
	uri := "file:/" + strings.Repeat("&", 3900)
	snapshot := &Snapshot{data: snapshotData{Schema: snapshotSchema, URIs: []string{uri}, Values: map[string]capturedValue{uri: {Content: []byte("root: bytes")}}, Environment: &EnvironmentPolicy{}}}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSnapshot(raw, CaptureLimits{Bytes: 4000})
	if err != nil || !bytes.Equal(decoded.SourceBytes(), snapshot.SourceBytes()) {
		t.Fatalf("encoded overhead rejected an in-budget snapshot: %v", err)
	}
	if _, err := DecodeSnapshot(raw, CaptureLimits{Bytes: 3000}); !errors.Is(err, ErrCaptureLimit) {
		t.Fatalf("decoded bound was not enforced: %v", err)
	}
}
