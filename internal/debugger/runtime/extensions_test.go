package runtime

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/registry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// extensionsConfig uses every extension of the distribution. health_check
// and pprof name a port the test already holds, so a real start would fail;
// filelog keeps its offsets in file_storage and the exporter authenticates.
func extensionsConfig(port string) string {
	return "extensions:\n" +
		"  health_check: {endpoint: 127.0.0.1:" + port + "}\n" +
		"  pprof: {endpoint: 127.0.0.1:" + port + "}\n" +
		"  zpages: {endpoint: 127.0.0.1:" + port + "}\n" +
		"  file_storage: {directory: /var/lib/otelcol/storage}\n" +
		"  basicauth/client: {client_auth: {username: user, password: secret}}\n" +
		"  bearertokenauth: {token: secret}\n" +
		"  oauth2client: {client_id: id, client_secret: secret, token_url: https://auth.example.com/token}\n" +
		"  headers_setter: {headers: [{key: X-Tenant, value: lab}]}\n" +
		"receivers:\n  filelog: {include: [/var/log/app.log], start_at: beginning, storage: file_storage}\n" +
		"exporters:\n  otlp: {endpoint: backend.example.com:4317, auth: {authenticator: bearertokenauth}}\n" +
		"service:\n  extensions: [health_check, pprof, zpages, file_storage, basicauth/client, bearertokenauth, oauth2client, headers_setter]\n" +
		"  pipelines: {logs: {receivers: [filelog], exporters: [otlp]}}\n"
}

// TestExtensionsDoNotRunDuringRuns: the run completes although the
// extensions' port is taken, filelog gets a storage client, and the
// topology lists every extension as simulated.
func TestExtensionsDoNotRunDuringRuns(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	yaml := extensionsConfig(port)

	r := runCompleted(t, Options{ConfigYAML: yaml, Mocks: mock.Spec{Files: []mock.File{
		{Path: "/var/log/app.log", Content: []byte("started\n")},
	}}})
	if got := logBodies(t, r); len(got) != 1 || got[0] != "started" {
		t.Fatalf("exported bodies %q", got)
	}

	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := config.ResolveSource(context.Background(), config.Source{YAML: yaml}, factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	// file_storage validates without its directory on this host, and the
	// effective configuration keeps the configured directory.
	effective, err := config.Effective(resolved.Config)
	if err != nil {
		t.Fatal(err)
	}
	if fs := effective["extensions"].(map[string]any)["file_storage"].(map[string]any); fs["directory"] != "/var/lib/otelcol/storage" {
		t.Fatalf("effective file_storage %v", fs)
	}
	g := topology.Build(resolved.Config, ExecutionModes(resolved.Config, factories))
	var extensions int
	for _, c := range g.Components {
		if c.Kind != topology.KindExtension {
			continue
		}
		extensions++
		if c.Mode != topology.ModeSimulated || c.Note != extensionNote {
			t.Errorf("%s: mode %s, note %q", c.Ref, c.Mode, c.Note)
		}
	}
	if extensions != 8 {
		t.Fatalf("topology lists %d extensions, want 8", extensions)
	}
}

// TestInvalidExtensionSettingsAreDiagnosed: inert extensions still decode and
// validate their settings with the real configuration types.
func TestInvalidExtensionSettingsAreDiagnosed(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	for name, settings := range map[string]string{
		"unknown field":   "health_check: {no_such_setting: true}",
		"failed validate": "oauth2client: {client_id: id}",
	} {
		t.Run(name, func(t *testing.T) {
			yaml := "extensions:\n  " + settings + "\nreceivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {debug: {}}\n" +
				"service: {pipelines: {traces: {receivers: [otlp], exporters: [debug]}}}\n"
			_, err := config.ResolveSource(context.Background(), config.Source{YAML: yaml}, factories, nil)
			diags := config.AsDiagnostics(err)
			if len(diags) == 0 || diags[0].Severity != config.SeverityError {
				t.Fatalf("diagnostics %+v", diags)
			}
		})
	}
}

// TestReferenceExportersCannotAuthenticate: the reference run sends over
// loopback with inert extensions, so an exporter that authenticates is a
// stated prerequisite rather than a silent difference.
func TestReferenceExportersCannotAuthenticate(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	for _, exporter := range []string{"otlp", "otlphttp"} {
		yaml := "extensions: {bearertokenauth: {token: secret}}\nreceivers: {otlp: {protocols: {grpc: {}}}}\n" +
			"exporters: {" + exporter + ": {endpoint: example.invalid:4317, auth: {authenticator: bearertokenauth}}}\n" +
			"service: {extensions: [bearertokenauth], pipelines: {traces: {receivers: [otlp], exporters: [" + exporter + "]}}}\n"
		resolved, err := config.ResolveSource(context.Background(), config.Source{YAML: yaml}, factories, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = CheckReferencePrerequisites(resolved.Config)
		var prerequisite *runresult.ReferencePrerequisiteError
		if !errors.As(err, &prerequisite) || prerequisite.Setting != "auth" || strings.Contains(err.Error(), "secret") {
			t.Fatalf("%s: prerequisite %v", exporter, err)
		}
	}
}
