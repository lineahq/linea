package catalog

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/build"
	"github.com/lineahq/linea/internal/debugger/manifest"
)

var liveCapture = []Custom{
	{Module: "github.com/lineahq/linea/collector/livecapture", Component: manifest.Component{Kind: manifest.Processors, Import: "github.com/lineahq/linea/collector/livecapture/captureprocessor", Type: "capture"}},
	{Module: "github.com/lineahq/linea/collector/livecapture", Component: manifest.Component{Kind: manifest.Extensions, Import: "github.com/lineahq/linea/collector/livecapture/livecaptureextension", Type: "livecapture"}},
}

func TestComposeAddsCustomComponents(t *testing.T) {
	cat := release(t)
	for _, req := range []Request{
		{Components: []string{"receivers/otlp", "processors/batch", "exporters/debug"}, Custom: liveCapture},
		{Preset: PresetAll, Custom: liveCapture},
		{Preset: PresetBuiltIn, Custom: liveCapture},
		{Custom: liveCapture},
	} {
		out, err := Compose(cat, build.Manifest, req)
		if err != nil {
			t.Fatalf("%+v: %v", req, err)
		}
		m, err := manifest.Parse(out.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		last := func(list []manifest.Module) manifest.Module { return list[len(list)-1] }
		if p := last(m.Processors); p.GoMod != "github.com/lineahq/linea/collector/livecapture v0.0.0" || p.Import != liveCapture[0].Import || p.Path != "" {
			t.Fatalf("preset %q: last processor %+v", req.Preset, p)
		}
		if e := last(m.Extensions); e.Import != liveCapture[1].Import {
			t.Fatalf("preset %q: last extension %+v", req.Preset, e)
		}
		if req.Preset == PresetBuiltIn && !bytes.HasPrefix(out.Manifest, build.Manifest[:bytes.Index(build.Manifest, []byte("\nprocessors:"))]) {
			t.Fatal("the built-in manifest's text changed before its processors")
		}
	}
}

func TestAddCustomKeepsTheText(t *testing.T) {
	text := "# mine\ndist:\n  name: x\n  otelcol_version: 0.162.0\nprocessors:\n    - gomod: go.opentelemetry.io/collector/processor/batchprocessor v0.162.0 # batch\n\n# exporters next\nexporters:\n- gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.162.0"
	out, err := AddCustom([]byte(text), liveCapture)
	if err != nil {
		t.Fatal(err)
	}
	want := "# mine\ndist:\n  name: x\n  otelcol_version: 0.162.0\nprocessors:\n    - gomod: go.opentelemetry.io/collector/processor/batchprocessor v0.162.0 # batch\n" +
		"    - gomod: github.com/lineahq/linea/collector/livecapture v0.0.0\n      import: github.com/lineahq/linea/collector/livecapture/captureprocessor\n" +
		"\n# exporters next\nexporters:\n- gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.162.0\n" +
		"\nextensions:\n  - gomod: github.com/lineahq/linea/collector/livecapture v0.0.0\n    import: github.com/lineahq/linea/collector/livecapture/livecaptureextension\n"
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	again, err := AddCustom(out, liveCapture)
	if err != nil || string(again) != want {
		t.Fatalf("adding again changed the manifest: %v\n%s", err, again)
	}
	// A missing section goes before the next one in manifest order.
	out, err = AddCustom([]byte("dist:\n  otelcol_version: 0.162.0\nreceivers:\n  - gomod: r v1.0.0\n\n# exporters\nexporters:\n  - gomod: e v1.0.0\n"), liveCapture[:1])
	if want := "dist:\n  otelcol_version: 0.162.0\nreceivers:\n  - gomod: r v1.0.0\n\n" +
		"processors:\n  - gomod: github.com/lineahq/linea/collector/livecapture v0.0.0\n    import: github.com/lineahq/linea/collector/livecapture/captureprocessor\n\n" +
		"# exporters\nexporters:\n  - gomod: e v1.0.0\n"; err != nil || string(out) != want {
		t.Fatalf("got %v:\n%s\nwant:\n%s", err, out, want)
	}
	if _, err := AddCustom([]byte("dist:\n  otelcol_version: 0.162.0\nprocessors: []\n"), liveCapture); err == nil || !strings.Contains(err.Error(), "not a block list") {
		t.Fatalf("a flow list: %v", err)
	}
}

func TestComposeMatchesCustomTypesInTheConfiguration(t *testing.T) {
	config := []byte(`extensions:
  livecapture:
receivers:
  otlp:
processors:
  capture/ingress:
  mystery:
exporters:
  debug:
service:
  extensions: [livecapture]
  pipelines:
    traces:
      receivers: [otlp]
      processors: [capture/ingress, mystery]
      exporters: [debug]
`)
	out, err := Compose(release(t), build.Manifest, Request{Config: config, Custom: liveCapture})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"processors/mystery"}; !slices.Equal(out.Unknown, want) {
		t.Fatalf("unknown = %v, want %v", out.Unknown, want)
	}
}
