package runtime

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

const (
	checkoutLog = "/var/log/pods/shop_checkout-7d9f8b6c4-x2x9q_0b7c3f52-1d2e-4f6a-9b8c-7d6e5f4a3b2c/checkout/0.log"
	excludedLog = "/var/log/pods/observability_otel-collector-abc12_11111111-2222-3333-4444-555555555555/otel-collector/0.log"
)

var filelogConfig = filepath.Join("..", "..", "..", "testdata", "receivers", "filelog", "config.yaml")

func runCompleted(t *testing.T, opts Options) *runresult.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := Run(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runresult.StatusCompleted {
		t.Fatalf("status %s: %s", r.Status, r.StatusReason)
	}
	return r
}

func logBodies(t *testing.T, r *runresult.Result) []string {
	t.Helper()
	var out []string
	for _, o := range r.Outputs {
		p, err := telemetry.Decode(o.Data)
		if err != nil {
			t.Fatal(err)
		}
		rls := p.Logs.ResourceLogs()
		for i := range rls.Len() {
			sls := rls.At(i).ScopeLogs()
			for j := range sls.Len() {
				lrs := sls.At(j).LogRecords()
				for k := range lrs.Len() {
					out = append(out, lrs.At(k).Body().AsString())
				}
			}
		}
	}
	return out
}

// TestFilelogReadsMockedFiles runs the unchanged container-log config: the
// real receiver skips existing content (start_at end), applies the exclude
// pattern, and its container parser merges partial lines and derives the
// Kubernetes attributes from the restored deployment path.
func TestFilelogReadsMockedFiles(t *testing.T) {
	r := runCompleted(t, Options{ConfigPath: filelogConfig, Mocks: mock.Spec{Files: []mock.File{
		{
			Path:     checkoutLog,
			Existing: []byte("2026-10-10T09:59:59.000000000Z stdout F written before the collector started\n"),
			Content: []byte("2026-10-10T10:00:00.000000000Z stdout F payment accepted order=1042\n" +
				"2026-10-10T10:00:01.000000000Z stderr P card declined, retry\n" +
				"2026-10-10T10:00:01.100000000Z stderr F ing with backup card\n"),
		},
		{Path: excludedLog, Content: []byte("2026-10-10T10:00:02.000000000Z stdout F excluded\n")},
	}}})

	want := []string{"payment accepted order=1042", "card declined, retrying with backup card"}
	if got := logBodies(t, r); !slices.Equal(got, want) {
		t.Fatalf("exported bodies %q, want %q", got, want)
	}
	data := outputData(r)
	for _, s := range []string{`"k8s.namespace.name"`, `"shop"`, `"k8s.container.name"`, `"checkout"`, checkoutLog} {
		if !strings.Contains(data, s) {
			t.Errorf("output lacks %s: %s", s, data)
		}
	}
	if strings.Contains(data, "linea-files") {
		t.Errorf("output leaks the sandbox path: %s", data)
	}

	if len(r.Inputs) != 1 || r.Inputs[0].Mock != "filelog" || r.Inputs[0].Counts.Items != 2 {
		t.Fatalf("inputs %+v, want one filelog input of 2 records", r.Inputs)
	}
	injected := slices.ContainsFunc(r.Events, func(e instrument.Event) bool {
		return e.Kind == instrument.KindInput && e.Source == instrument.SourceMockedReceiver && e.Mode == instrument.ModeDependencyMocked
	})
	if !injected {
		t.Error("no input event from the mocked receiver")
	}

	if len(r.Mocks) != 1 {
		t.Fatalf("mocks %+v, want one declaration", r.Mocks)
	}
	d := r.Mocks[0]
	if d.Ref != "receiver/filelog" || d.Mode != topology.ModeDependencyMocked || !slices.Equal(d.Supplied, []string{checkoutLog}) {
		t.Errorf("declaration %+v, want dependency_mocked receiver/filelog supplying only %s", d, checkoutLog)
	}
	if !slices.ContainsFunc(d.Limitations, func(s string) bool { return strings.Contains(s, "start_at is end") }) {
		t.Errorf("limitations %q lack the start_at note", d.Limitations)
	}
	if !slices.ContainsFunc(d.Warnings, func(s string) bool { return strings.Contains(s, excludedLog) }) {
		t.Errorf("warnings %q do not name the unread file", d.Warnings)
	}
}

// TestFilelogTypeNames covers the receiver's type and its deprecated alias,
// with start_at beginning reading existing content too.
func TestFilelogTypeNames(t *testing.T) {
	for _, name := range []string{"file_log", "filelog/app"} {
		t.Run(name, func(t *testing.T) {
			cfg := "receivers:\n  " + name + ":\n    include: [/var/log/app/*.log]\n    start_at: beginning\n" +
				"exporters:\n  otlp/backend:\n    endpoint: backend:4317\n" +
				"service:\n  pipelines:\n    logs:\n      receivers: [" + name + "]\n      exporters: [otlp/backend]\n"
			r := runCompleted(t, Options{ConfigYAML: cfg, Mocks: mock.Spec{Files: []mock.File{
				{Path: "/var/log/app/a.log", Existing: []byte("zero\n"), Content: []byte("one\ntwo\n")},
			}}})
			if got, want := logBodies(t, r), []string{"zero", "one", "two"}; !slices.Equal(got, want) {
				t.Fatalf("exported bodies %q, want %q", got, want)
			}
			if len(r.Mocks) != 1 || r.Mocks[0].Ref != "receiver/"+name {
				t.Errorf("mocks %+v, want one declaration for receiver/%s", r.Mocks, name)
			}
		})
	}
}

// TestFilelogWithoutFilesWarns runs a config whose filelog receiver has no
// scenario files beside an OTLP input: the run completes and says so.
func TestFilelogWithoutFilesWarns(t *testing.T) {
	cfg := "receivers:\n  otlp:\n    protocols: {grpc: {}}\n  filelog:\n    include: [/var/log/*.log]\n" +
		"exporters:\n  otlp/backend:\n    endpoint: backend:4317\n" +
		"service:\n  pipelines:\n    traces:\n      receivers: [otlp]\n      exporters: [otlp/backend]\n" +
		"    logs:\n      receivers: [filelog]\n      exporters: [otlp/backend]\n"
	r := runCompleted(t, Options{ConfigYAML: cfg, Inputs: fixtureInputs(t)})
	if len(r.Mocks) != 1 || !slices.ContainsFunc(r.Mocks[0].Warnings, func(s string) bool { return strings.Contains(s, "no mocked files are declared") }) {
		t.Fatalf("mocks %+v, want a no-files warning", r.Mocks)
	}
}

// TestReferenceRunReadsMockedFiles: the reference Collector reads the
// scenario's files from its own sandbox, never the host's.
func TestReferenceRunReadsMockedFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := RunReference(ctx, Options{ConfigPath: filelogConfig, Mocks: mock.Spec{Files: []mock.File{
		{Path: checkoutLog, Content: []byte("2026-10-10T10:00:00.000000000Z stdout F payment accepted order=1042\n")},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runresult.StatusCompleted {
		t.Fatalf("status %s: %s", r.Status, r.StatusReason)
	}
	if got, want := logBodies(t, r), []string{"payment accepted order=1042"}; !slices.Equal(got, want) {
		t.Fatalf("exported bodies %q, want %q", got, want)
	}
	if len(r.Inputs) != 1 || r.Inputs[0].Mock != "filelog" {
		t.Errorf("inputs %+v, want one filelog input", r.Inputs)
	}
	if len(r.Mocks) != 1 || r.Mocks[0].Ref != "receiver/filelog" {
		t.Errorf("mocks %+v, want the filelog declaration", r.Mocks)
	}
}
