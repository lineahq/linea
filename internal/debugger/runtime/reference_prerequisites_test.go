package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/registry"
)

func TestReferenceTransportPrerequisites(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, receivers, exporter, settings, want string
	}{
		{"plaintext grpc", "{grpc: {}}", "otlp", "", ""},
		{"plaintext http", "{http: {}}", "otlphttp", "", ""},
		{"grpc receiver TLS", "{grpc: {tls: {cert_file: private-cert, key_file: private-key}}}", "otlp", "", "protocols.grpc.tls"},
		{"http receiver TLS", "{grpc: {}, http: {tls: {cert_file: private-cert, key_file: private-key}}}", "otlp", "", "protocols.http.tls"},
		{"grpc alias CA", "{grpc: {}}", "otlp", ", tls: {ca_file: private-ca}", "tls.ca_file"},
		{"grpc canonical CA", "{grpc: {}}", "otlp_grpc", ", tls: {ca_file: private-ca}", "tls.ca_file"},
		{"http alias CA", "{grpc: {}}", "otlphttp", ", tls: {ca_file: private-ca}", "tls.ca_file"},
		{"http canonical CA", "{grpc: {}}", "otlp_http", ", tls: {ca_file: private-ca}", "tls.ca_file"},
		{"client certificate files", "{grpc: {}}", "otlphttp", ", tls: {cert_file: private-cert, key_file: private-key}", "tls.cert_file"},
		{"client key file", "{grpc: {}}", "otlphttp", ", tls: {cert_pem: private-cert, key_file: private-key}", "tls.key_file"},
		{"grpc inline CA", "{grpc: {}}", "otlp", ", tls: {ca_pem: private-ca}", "tls.ca_pem"},
		{"http inline CA is captured", "{grpc: {}}", "otlphttp", ", tls: {ca_pem: private-ca}", ""},
		{"http inline certificate is captured", "{grpc: {}}", "otlphttp", ", tls: {cert_pem: private-cert, key_pem: private-key}", ""},
		{"system trust", "{grpc: {}}", "otlphttp", ", tls: {include_system_ca_certs_pool: true}", "tls.include_system_ca_certs_pool"},
		{"TPM", "{grpc: {}}", "otlphttp", ", tls: {tpm: {enabled: true, path: private-device}}", "tls.tpm"},
		{"explicit proxy", "{grpc: {}}", "otlphttp", ", proxy_url: http://private-proxy:8888", "proxy_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := fmt.Sprintf("receivers: {otlp: {protocols: %s}}\nexporters: {%s: {endpoint: example.invalid:4317%s}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [%s]}}}\n", tc.receivers, tc.exporter, tc.settings, tc.exporter)
			resolved, err := config.ResolveSource(context.Background(), config.Source{YAML: yaml}, factories, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = CheckReferencePrerequisites(resolved.Config)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var prerequisite *runresult.ReferencePrerequisiteError
			if !errors.Is(err, runresult.ErrReferencePrerequisite) || !errors.As(err, &prerequisite) || prerequisite.Setting != tc.want || strings.Contains(err.Error(), "private-") {
				t.Fatalf("unexpected prerequisite: %v", err)
			}
			// Direct runtime callers must fail at the same boundary, before any
			// certificate/device access, listeners or reference sinks start.
			_, err = RunReference(context.Background(), Options{ConfigYAML: yaml})
			if !errors.Is(err, runresult.ErrReferencePrerequisite) {
				t.Fatalf("direct reference bypassed preflight: %v", err)
			}
		})
	}
}

func TestReferencePrerequisitesIgnoreUnusedTransportsAndChooseStableError(t *testing.T) {
	const yaml = `receivers:
  otlp: {protocols: {grpc: {}}}
  otlp/unused: {protocols: {grpc: {tls: {cert_file: private-cert, key_file: private-key}}}}
exporters:
  otlp: {endpoint: example.invalid:4317}
  otlp/unused: {endpoint: example.invalid:4317, tls: {ca_file: private-ca}}
  otlp/a: {endpoint: example.invalid:4317, tls: {ca_file: private-ca}}
  otlp/z: {endpoint: example.invalid:4317, tls: {ca_file: private-ca}}
service: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}
`
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	for _, used := range []bool{false, true} {
		source := yaml
		if used {
			source = strings.Replace(source, "exporters: [otlp]", "exporters: [otlp/z, otlp/a]", 1)
		}
		resolved, err := config.ResolveSource(context.Background(), config.Source{YAML: source}, factories, nil)
		if err != nil {
			t.Fatal(err)
		}
		for range 20 {
			err := CheckReferencePrerequisites(resolved.Config)
			if !used && err != nil {
				t.Fatal("unused transport blocked reference", err)
			}
			var prerequisite *runresult.ReferencePrerequisiteError
			if used && (!errors.As(err, &prerequisite) || prerequisite.Component.String() != "otlp/a") {
				t.Fatal("unstable error selection", err)
			}
		}
	}
}
