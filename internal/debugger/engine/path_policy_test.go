package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/hostfs"
)

const (
	policyConfig    = "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {nop: {}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [nop]}}}\n"
	policyTransform = "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {transform: {trace_statements: [{context: span, statements: ['set(span.attributes[\"x\"], \"y\")']}]}}\nexporters: {nop: {}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [transform], exporters: [nop]}}}\n"
	policyTraces    = `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"s","startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}`
	policyManifest  = "apiVersion: v1\nkind: Pod\nmetadata: {name: p, namespace: n, uid: u}\nstatus: {podIP: 10.0.0.1}\n"
)

// policyLayout is an allowed root and an outside directory holding, each, a
// complete set of sources that would make any request succeed if read.
type policyLayout struct{ root, outside string }

func newPolicyLayout(t *testing.T) policyLayout {
	t.Helper()
	base := t.TempDir()
	l := policyLayout{root: filepath.Join(base, "root"), outside: filepath.Join(base, "outside")}
	for _, dir := range []string{l.root, l.outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{
			"config.yaml":    policyConfig,
			"transform.yaml": policyTransform,
			"exporters.yaml": "nop: {}\n",
			"traces.json":    policyTraces,
			"scenario.yaml":  "version: 1\ninputs: [{file: traces.json}]\n",
			"pods.yaml":      policyManifest,
			"app.log":        "line\n",
			"k8s-in.yaml":    "version: 1\ninputs: [{file: traces.json}]\nmocks: {kubernetes: {files: [pods.yaml]}}\n",
			"k8s-out.yaml":   "version: 1\ninputs: [{file: traces.json}]\nmocks: {kubernetes: {files: [../outside/pods.yaml]}}\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return l
}

func (l policyLayout) in(name string) string  { return filepath.Join(l.root, name) }
func (l policyLayout) out(name string) string { return filepath.Join(l.outside, name) }

// fileConfig is policyConfig with its exporters read from uri.
func fileConfig(uri string) string {
	return strings.Replace(policyConfig, "exporters: {nop: {}}", "exporters: ${file:"+uri+"}", 1)
}

func quotedPath(p string) string { return `"` + filepath.ToSlash(p) + `"` }

// TestPathPolicyCoversEveryReadSite covers each row of docs/path-policy.md
// for runs, inspection and preview: a host file outside the roots that would
// make the request succeed if read is refused with ErrOutsideRoots, as an
// error and never as a diagnostic. A ${file:} reference outside the roots is
// never read either: it stands in, which here leaves the exporters
// section a string, so the request fails naming the reference and its reason.
// The same requests succeed inside the root.
func TestPathPolicyCoversEveryReadSite(t *testing.T) {
	l := newPolicyLayout(t)
	pol := Policy{Paths: hostfs.Policy{Roots: []string{l.root}}}
	ctx := context.Background()
	fixture := policyTraces

	type request = policyCall
	cases := []struct {
		name    string
		row     string
		refused func(l policyLayout) request
		allowed func(l policyLayout) request
	}{
		{"config path", "2",
			func(l policyLayout) request {
				return request{run: &Request{Config: l.out("config.yaml"), FixtureJSON: fixture}}
			},
			func(l policyLayout) request {
				return request{run: &Request{Config: l.in("config.yaml"), FixtureJSON: fixture}}
			}},
		{"config path hashed by inspection", "3",
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{Config: l.out("config.yaml")}}
			},
			func(l policyLayout) request { return request{inspect: &InspectRequest{Config: l.in("config.yaml")}} }},
		{"config path in preview", "2",
			func(l policyLayout) request {
				return request{preview: &OTTLRequest{Config: l.out("transform.yaml"), Processor: "transform", Input: []byte(fixture)}}
			},
			func(l policyLayout) request {
				return request{preview: &OTTLRequest{Config: l.in("transform.yaml"), Processor: "transform", Input: []byte(fixture)}}
			}},
		{"${file:} in a run", "2",
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: fileConfig(l.out("exporters.yaml")), FixtureJSON: fixture}}
			},
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: fileConfig(l.in("exporters.yaml")), FixtureJSON: fixture}}
			}},
		{"${file:} in inspection", "2",
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{ConfigYAML: fileConfig(l.out("exporters.yaml"))}}
			},
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{ConfigYAML: fileConfig(l.in("exporters.yaml"))}}
			}},
		{"${file:} in preview", "2",
			func(l policyLayout) request {
				return request{preview: &OTTLRequest{ConfigYAML: strings.Replace(policyTransform, "exporters: {nop: {}}", "exporters: ${file:"+l.out("exporters.yaml")+"}", 1), Processor: "transform", Input: []byte(fixture)}}
			},
			func(l policyLayout) request {
				return request{preview: &OTTLRequest{ConfigYAML: strings.Replace(policyTransform, "exporters: {nop: {}}", "exporters: ${file:"+l.in("exporters.yaml")+"}", 1), Processor: "transform", Input: []byte(fixture)}}
			}},
		{"nested ${file:${env:X}}", "2",
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: fileConfig("${env:POLICY_EXPORTERS}"), FixtureJSON: fixture,
					SetupYAML: "environment: {POLICY_EXPORTERS: " + quotedPath(l.out("exporters.yaml")) + "}\n"}}
			},
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: fileConfig("${env:POLICY_EXPORTERS}"), FixtureJSON: fixture,
					SetupYAML: "environment: {POLICY_EXPORTERS: " + quotedPath(l.in("exporters.yaml")) + "}\n"}}
			}},
		{"nested ${file:${env:X}} in inspection", "2",
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{ConfigYAML: fileConfig("${env:POLICY_EXPORTERS}"),
					SetupYAML: "environment: {POLICY_EXPORTERS: " + quotedPath(l.out("exporters.yaml")) + "}\n"}}
			},
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{ConfigYAML: fileConfig("${env:POLICY_EXPORTERS}"),
					SetupYAML: "environment: {POLICY_EXPORTERS: " + quotedPath(l.in("exporters.yaml")) + "}\n"}}
			}},
		{"fixture path", "1",
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, Fixture: l.out("traces.json")}}
			},
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, Fixture: l.in("traces.json")}}
			}},
		{"scenario path", "1",
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, Scenario: l.out("scenario.yaml")}}
			},
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, Scenario: l.in("scenario.yaml")}}
			}},
		{"scenario input escaping with ..", "1",
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, ScenarioDir: l.root,
					ScenarioYAML: "version: 1\ninputs: [{file: ../outside/traces.json}]\n"}}
			},
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, ScenarioDir: l.root,
					ScenarioYAML: "version: 1\ninputs: [{file: traces.json}]\n"}}
			}},
		{"absolute scenario input", "1",
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, ScenarioYAML: "version: 1\ninputs: [{file: " + quotedPath(l.out("traces.json")) + "}]\n"}}
			},
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, ScenarioYAML: "version: 1\ninputs: [{file: " + quotedPath(l.in("traces.json")) + "}]\n"}}
			}},
		{"quick-run mock file", "1",
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, FixtureJSON: fixture,
					SetupYAML: "mocks: {files: [{path: /var/log/app.log, file: " + quotedPath(l.out("app.log")) + "}]}\n"}}
			},
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, FixtureJSON: fixture,
					SetupYAML: "mocks: {files: [{path: /var/log/app.log, file: " + quotedPath(l.in("app.log")) + "}]}\n"}}
			}},
		{"scenario Kubernetes file", "1",
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, Scenario: l.in("k8s-out.yaml")}}
			},
			func(l policyLayout) request {
				return request{run: &Request{ConfigYAML: policyConfig, Scenario: l.in("k8s-in.yaml")}}
			}},
		{"scenario path in inspection", "4",
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{ConfigYAML: policyConfig, Scenario: l.out("scenario.yaml")}}
			},
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{ConfigYAML: policyConfig, Scenario: l.in("scenario.yaml")}}
			}},
		{"scenario path in preview", "4",
			func(l policyLayout) request {
				return request{preview: &OTTLRequest{ConfigYAML: policyTransform, Scenario: l.out("scenario.yaml"), Processor: "transform", Input: []byte(fixture)}}
			},
			func(l policyLayout) request {
				return request{preview: &OTTLRequest{ConfigYAML: policyTransform, Scenario: l.in("scenario.yaml"), Processor: "transform", Input: []byte(fixture)}}
			}},
		{"mock file read by inspection", "5",
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{ConfigYAML: policyConfig, ScenarioDir: l.root,
					ScenarioYAML: "version: 1\ninputs: [{file: traces.json}]\nmocks: {files: [{path: /var/log/app.log, file: ../outside/app.log}]}\n"}}
			},
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{ConfigYAML: policyConfig, ScenarioDir: l.root,
					ScenarioYAML: "version: 1\ninputs: [{file: traces.json}]\nmocks: {files: [{path: /var/log/app.log, file: app.log}]}\n"}}
			}},
		{"Kubernetes file read by inspection", "5",
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{ConfigYAML: policyConfig, ScenarioDir: l.root,
					ScenarioYAML: "version: 1\ninputs: [{file: traces.json}]\nmocks: {kubernetes: {files: [" + quotedPath(l.out("pods.yaml")) + "]}}\n"}}
			},
			func(l policyLayout) request {
				return request{inspect: &InspectRequest{ConfigYAML: policyConfig, ScenarioDir: l.root,
					ScenarioYAML: "version: 1\ninputs: [{file: traces.json}]\nmocks: {kubernetes: {files: [pods.yaml]}}\n"}}
			}},
	}
	for _, tt := range cases {
		t.Run("row "+tt.row+"/"+tt.name, func(t *testing.T) {
			standIn := strings.Contains(tt.name, "${file:")
			err := tt.refused(l).call(ctx, pol)
			if standIn && (err == nil || errors.Is(err, hostfs.ErrOutsideRoots) || !strings.Contains(err.Error(), "(outside_roots)")) {
				t.Fatalf("outside the roots: %v; want a stand-in", err)
			}
			if !standIn && !errors.Is(err, hostfs.ErrOutsideRoots) {
				t.Fatalf("outside the roots: %v; want ErrOutsideRoots", err)
			}
			if err := tt.allowed(l).call(ctx, pol); err != nil {
				t.Fatalf("inside the root: %v", err)
			}
			if err := tt.refused(l).call(ctx, Policy{Paths: hostfs.Policy{Unrestricted: true}}); err != nil {
				t.Fatalf("unrestricted: %v", err)
			}
		})
	}
}

// policyCall is one request to a run (preparation), an inspection or a preview.
type policyCall struct {
	run     *Request
	inspect *InspectRequest
	preview *OTTLRequest
}

// call returns the operation's error, or an error naming the diagnostics of an
// invalid inspection or preview.
func (r policyCall) call(ctx context.Context, pol Policy) error {
	switch {
	case r.run != nil:
		_, err := PrepareWithPolicy(ctx, *r.run, pol)
		return err
	case r.inspect != nil:
		ins, err := InspectWithPolicy(ctx, *r.inspect, pol)
		if err == nil && !ins.Valid {
			return errors.New("inspection reported diagnostics: " + diagnosticText(ins.Diagnostics))
		}
		return err
	default:
		ev, err := EvaluateOTTLWithPolicy(ctx, *r.preview, pol)
		if err == nil && !ev.Valid {
			return errors.New("preview reported diagnostics: " + diagnosticText(ev.Diagnostics))
		}
		return err
	}
}

func diagnosticText(diags []config.Diagnostic) string {
	var parts []string
	for _, d := range diags {
		part := d.Code + ": " + d.Message
		if d.Hint != "" {
			part += " (" + d.Hint + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// TestRelativePathsAreRefusedUnderRoots: under roots a relative host path is
// refused, including a relative scenario_dir, and a relative
// ${file:./x} stands in unread; unrestricted, the same requests read
// from the working directory as the Collector does.
func TestRelativePathsAreRefusedUnderRoots(t *testing.T) {
	l := newPolicyLayout(t)
	t.Chdir(l.root)
	ctx := context.Background()
	rooted := Policy{Paths: hostfs.Policy{Roots: []string{l.root}}}
	unrestrictedPolicy := Policy{Paths: hostfs.Policy{Unrestricted: true}}
	relativeFile := fileConfig("./exporters.yaml")
	transform := strings.Replace(policyTransform, "exporters: {nop: {}}", "exporters: ${file:./exporters.yaml}", 1)
	for name, r := range map[string]policyCall{
		"${file:./x} in a run":        {run: &Request{ConfigYAML: relativeFile, FixtureJSON: policyTraces}},
		"${file:./x} in inspection":   {inspect: &InspectRequest{ConfigYAML: relativeFile}},
		"${file:./x} in preview":      {preview: &OTTLRequest{ConfigYAML: transform, Processor: "transform", Input: []byte(policyTraces)}},
		"config path":                 {run: &Request{Config: "config.yaml", FixtureJSON: policyTraces}},
		"config path in inspection":   {inspect: &InspectRequest{Config: "config.yaml"}},
		"fixture path":                {run: &Request{ConfigYAML: policyConfig, Fixture: "traces.json"}},
		"scenario path":               {run: &Request{ConfigYAML: policyConfig, Scenario: "scenario.yaml"}},
		"scenario_dir":                {run: &Request{ConfigYAML: policyConfig, ScenarioDir: ".", ScenarioYAML: "version: 1\ninputs: [{file: traces.json}]\n"}},
		"scenario_dir in inspection":  {inspect: &InspectRequest{ConfigYAML: policyConfig, ScenarioDir: ".", ScenarioYAML: "version: 1\ninputs: [{file: traces.json}]\nmocks: {files: [{path: /var/log/a.log, file: app.log}]}\n"}},
		"scenario path in preview":    {preview: &OTTLRequest{ConfigYAML: policyTransform, Scenario: "scenario.yaml", Processor: "transform", Input: []byte(policyTraces)}},
		"scenario path in inspection": {inspect: &InspectRequest{ConfigYAML: policyConfig, Scenario: "scenario.yaml"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := r.call(ctx, rooted)
			if strings.HasPrefix(name, "${file:") {
				// The failure names the reference and its reason.
				if err == nil || !strings.Contains(err.Error(), "${file:./exporters.yaml} (outside_roots)") {
					t.Fatalf("under roots: %v", err)
				}
			} else if !errors.Is(err, hostfs.ErrOutsideRoots) || !strings.Contains(err.Error(), "use an absolute path under an allowed root") {
				t.Fatalf("under roots: %v", err)
			}
			if err := r.call(ctx, unrestrictedPolicy); err != nil {
				t.Fatalf("unrestricted: %v", err)
			}
		})
	}
}

// TestFilelogPatternsCannotLeaveTheSandbox: an include pattern climbing out
// of the filelog sandbox with .. reads nothing from the host, in every
// composition; the mocked files are still read.
func TestFilelogPatternsCannotLeaveTheSandbox(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.log"), []byte("host-secret-line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := "/var/log/" + strings.Repeat("../", 40) + strings.TrimPrefix(filepath.ToSlash(strings.TrimPrefix(outside, filepath.VolumeName(outside))), "/") + "/*.log"
	cfg := "receivers:\n  filelog:\n    include: [/var/log/app/*.log, " + quotedPath(escape) + "]\n    start_at: beginning\nexporters:\n  otlp/backend: {endpoint: backend.example.com:4317}\nservice:\n  pipelines:\n    logs: {receivers: [filelog], exporters: [otlp/backend]}\n"
	scen := "version: 1\nmocks:\n  files:\n    - path: /var/log/app/a.log\n      content: |\n        mocked-line\n"
	rep, err := Execute(context.Background(), Request{ConfigYAML: cfg, ScenarioYAML: scen})
	if err != nil {
		t.Fatal(err)
	}
	var data strings.Builder
	for _, o := range rep.Result.Outputs {
		data.Write(o.Data)
	}
	if strings.Contains(data.String(), "host-secret-line") {
		t.Fatalf("the filelog receiver read a host file outside its sandbox: %s", data.String())
	}
	if !strings.Contains(data.String(), "mocked-line") {
		t.Fatalf("the mocked file was not read: %s", data.String())
	}
}
