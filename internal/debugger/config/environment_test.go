package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/registry"
	"go.opentelemetry.io/collector/confmap"
	"go.uber.org/zap"
)

func TestEnvironmentPolicyPreservesAuthorizedUpstreamSemantics(t *testing.T) {
	const name = "OTEL_POLICY_VALUE"
	for _, tc := range []struct {
		name, host, suffix string
		declared           map[string]string
	}{
		{"host", "private value", "", nil},
		{"host empty", "", ":-unused", nil},
		{"host typed", "{a: 1}", "", nil},
		{"declared empty", "host", ":-unused", map[string]string{name: ""}},
		{"declared typed", "host", "", map[string]string{name: "42"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(name, tc.host)
			uri := "env:" + name + tc.suffix
			upstream := scenarioEnvFactory(tc.declared).Create(confmap.ProviderSettings{Logger: zap.NewNop()})
			defer upstream.Shutdown(context.Background())
			want, err := upstream.Retrieve(context.Background(), uri, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer want.Close(context.Background())
			got, origin, err := boundEnvironment(context.Background(), uri, tc.declared, EnvironmentPolicy{Host: []string{name}})
			if err != nil {
				t.Fatal(err)
			}
			defer got.Close(context.Background())
			wantRaw, _ := want.AsRaw()
			gotRaw, _ := got.AsRaw()
			wantText, _ := want.AsString()
			gotText, _ := got.AsString()
			if !reflect.DeepEqual(wantRaw, gotRaw) || wantText != gotText {
				t.Fatalf("authorized retrieval changed: %#v / %q vs %#v / %q", gotRaw, gotText, wantRaw, wantText)
			}
			if tc.declared == nil && origin != "host" || tc.declared != nil && origin != "declared" {
				t.Fatal("wrong binding origin", origin)
			}
		})
	}
}

func TestDeclaredEnvironmentIgnoresHostAndRequiresBindingOrDefault(t *testing.T) {
	const name = "OTEL_POLICY_DEFAULT"
	t.Setenv(name, "private host value")
	for _, tc := range []struct {
		suffix string
		want   any
	}{{":-", ""}, {":-42", 42}, {":-false", false}, {":-{a: 1}", map[string]any{"a": 1}}} {
		t.Run(tc.suffix, func(t *testing.T) {
			r, origin, err := boundEnvironment(context.Background(), "env:"+name+tc.suffix, nil, EnvironmentPolicy{})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close(context.Background())
			raw, _ := r.AsRaw()
			if !reflect.DeepEqual(raw, tc.want) || origin != "default" {
				t.Fatalf("host overrode declared default: %#v (%s)", raw, origin)
			}
		})
	}
	if _, _, err := boundEnvironment(context.Background(), "env:"+name, nil, EnvironmentPolicy{}); !errors.Is(err, ErrEnvironmentBinding) || strings.Contains(err.Error(), "private host value") {
		t.Fatalf("implicit host binding: %v", err)
	}
}

func TestExplicitHostImportPreservesUnsetDefaultsAndRejectsInvalidPolicy(t *testing.T) {
	const name = "OTEL_POLICY_UNSET"
	t.Setenv(name, "temporary")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ":-", ":-false", ":-42"} {
		t.Run(suffix, func(t *testing.T) {
			uri := "env:" + name + suffix
			upstream := scenarioEnvFactory(nil).Create(confmap.ProviderSettings{Logger: zap.NewNop()})
			defer upstream.Shutdown(context.Background())
			want, err := upstream.Retrieve(context.Background(), uri, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer want.Close(context.Background())
			got, origin, err := boundEnvironment(context.Background(), uri, nil, EnvironmentPolicy{Host: []string{name}})
			if err != nil {
				t.Fatal(err)
			}
			defer got.Close(context.Background())
			a, _ := want.AsRaw()
			b, _ := got.AsRaw()
			if !reflect.DeepEqual(a, b) || (suffix == "" && origin != "host_unset") || (suffix != "" && origin != "default") {
				t.Fatal("unset semantics changed", a, b, origin)
			}
		})
	}
	for _, policy := range []EnvironmentPolicy{{Host: []string{"bad-name"}}, {Host: []string{strings.Repeat("x", 257)}}, {Host: []string{name}, LegacyHost: true}, {Host: make([]string, 129)}} {
		if err := policy.Validate(); !errors.Is(err, ErrEnvironmentPolicy) {
			t.Fatal("invalid policy accepted", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := boundEnvironment(ctx, "env:"+name, nil, EnvironmentPolicy{LegacyHost: true}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
}

func TestNestedProviderReferencesCannotBypassEnvironmentPolicy(t *testing.T) {
	factories, _ := registry.Factories()
	t.Setenv("OTEL_POLICY_NESTED", "private:4317")
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: '${env:OTEL_POLICY_ROOT}:4317'}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	resolved, err := CaptureSourceWithEnvironment(context.Background(), Source{YAML: yaml}, factories, map[string]string{"OTEL_POLICY_ROOT": "${env:OTEL_POLICY_NESTED}"}, CaptureLimits{}, EnvironmentPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	// The unauthorized nested name stands in; the host is never read.
	raw, _ := json.Marshal(resolved.Snapshot)
	effective, _ := Effective(resolved.Config)
	if strings.Contains(string(raw), "private") || strings.Contains(fmt.Sprint(effective), "private") {
		t.Fatal("nested host import bypassed policy")
	}
	want := Reference{Scheme: "env", Target: "OTEL_POLICY_NESTED", Source: SourceStandIn, Reason: ReasonUnbound, StandIn: standIn("env:OTEL_POLICY_NESTED")}
	if !slices.ContainsFunc(resolved.References, func(r Reference) bool { return reflect.DeepEqual(r, want) }) {
		t.Fatalf("references %+v", resolved.References)
	}
	if names := resolved.Snapshot.RequiredEnvironment(); len(names) != 0 {
		t.Fatal("a stand-in requires no host binding", names)
	}
}

func TestCaptureRecordsPolicyOriginsAndChecksPreparedPermissions(t *testing.T) {
	const name = "OTEL_POLICY_CAPTURE"
	t.Setenv(name, "accepted:4317")
	factories, _ := registry.Factories()
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: '${env:" + name + "}'}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	policy := EnvironmentPolicy{Host: []string{name}}
	resolved, err := CaptureSourceWithEnvironment(context.Background(), Source{YAML: yaml}, factories, nil, CaptureLimits{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	policy.Host[0] = "changed"
	raw, _ := json.Marshal(resolved.Snapshot)
	snapshot, err := DecodeSnapshot(raw, CaptureLimits{})
	if err != nil || snapshot.EnvironmentMode() != "allowlisted_host" || snapshot.data.Values["env:"+name].Binding != "host" {
		t.Fatal("policy/origin lost", err)
	}
	if err := snapshot.CheckEnvironment(EnvironmentPolicy{}); !errors.Is(err, ErrEnvironmentBinding) {
		t.Fatal("existing capture bypassed policy", err)
	}
	t.Setenv(name, "changed:4317")
	if err := snapshot.CheckEnvironment(EnvironmentPolicy{Host: []string{name}}); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Resolve(context.Background(), factories); err != nil {
		t.Fatal("captured binding consulted the host", err)
	}
	for _, kind := range []string{"unknown origin", "unauthorized host", "missing policy", "file origin"} {
		t.Run(kind, func(t *testing.T) {
			var data snapshotData
			_ = json.Unmarshal(raw, &data)
			value := data.Values["env:"+name]
			switch kind {
			case "unknown origin":
				value.Binding = "unknown"
			case "unauthorized host":
				data.Environment = &EnvironmentPolicy{}
			case "missing policy":
				data.Environment = nil
			case "file origin":
				data.Values["file:extra"] = value
			}
			data.Values["env:"+name] = value
			changed, _ := json.Marshal(data)
			if _, err := DecodeSnapshot(changed, CaptureLimits{}); !errors.Is(err, ErrSnapshot) {
				t.Fatal("invalid binding archive accepted", err)
			}
		})
	}
	// Legacy archives remain readable. Their env origins cannot be inferred.
	var legacy snapshotData
	_ = json.Unmarshal(raw, &legacy)
	legacy.Schema, legacy.Environment = 1, nil
	for uri, value := range legacy.Values {
		value.Binding = ""
		legacy.Values[uri] = value
	}
	oldRaw, _ := json.Marshal(legacy)
	old, err := DecodeSnapshot(oldRaw, CaptureLimits{})
	if err != nil || old.EnvironmentMode() != "" {
		t.Fatal("legacy snapshot became unreadable", err)
	}
	if err := old.CheckEnvironment(EnvironmentPolicy{}); !errors.Is(err, ErrEnvironmentBinding) {
		t.Fatal("legacy origin silently trusted", err)
	}
	if err := old.CheckEnvironment(EnvironmentPolicy{LegacyHost: true}); err != nil {
		t.Fatal(err)
	}
}
