package runtime

import (
	"bytes"
	"context"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

var (
	tracesConfig  = filepath.Join("..", "..", "..", "testdata", "traces", "config.yaml")
	tracesFixture = filepath.Join("..", "..", "..", "testdata", "traces", "traces.json")
)

func loadFixture(t *testing.T, path string) telemetry.Payload {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := telemetry.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func fixtureInputs(t *testing.T) []Input {
	t.Helper()
	return []Input{{Payload: loadFixture(t, tracesFixture)}}
}

// TestInstrumentedRunMatchesReferenceCollector is the fidelity proof:
// the unchanged config runs instrumented and isolated, and its final telemetry
// equals an uninstrumented Collector receiving and exporting over real OTLP.
func TestInstrumentedRunMatchesReferenceCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	configBefore, err := os.ReadFile(tracesConfig)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: tracesConfig, Inputs: fixtureInputs(t)}

	got, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("instrumented run: %v", err)
	}
	ref, err := RunReference(ctx, opts)
	if err != nil {
		t.Fatalf("reference run: %v", err)
	}

	if cmp := runresult.Compare(got, ref); !cmp.Equal {
		t.Fatalf("instrumented output differs from reference: %v\ninstrumented: %s\nreference:    %s",
			cmp.Differences, outputData(got), outputData(ref))
	}
	if len(got.Outputs) != 1 || got.Outputs[0].Exporter != "otlp" {
		t.Fatalf("outputs = %+v, want one capture for exporter otlp", got.Outputs)
	}
	if c := got.Outputs[0].Counts; c != (telemetry.Counts{Resources: 2, Scopes: 2, Items: 2}) {
		t.Fatalf("final counts = %+v", c)
	}

	// The transform processor really ran.
	final := string(got.Outputs[0].Data)
	for _, want := range []string{`"failed POST /checkout"`, `"deployment.environment"`, `"http.route"`} {
		if !strings.Contains(final, want) {
			t.Errorf("final telemetry lacks %s", want)
		}
	}
	if strings.Contains(final, "user.email") {
		t.Error("final telemetry still contains user.email")
	}
	// The filter processor really ran.
	if strings.Contains(final, "SELECT orders") {
		t.Error("final telemetry still contains the filtered span")
	}

	configAfter, err := os.ReadFile(tracesConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configBefore, configAfter) {
		t.Fatal("run modified the input configuration")
	}
}

func TestInstrumentedRunCapturesStageEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	got, err := Run(ctx, Options{ConfigPath: tracesConfig, Inputs: fixtureInputs(t)})
	if err != nil {
		t.Fatal(err)
	}

	if got.Status != runresult.StatusCompleted || got.RunID == "" {
		t.Fatalf("status %q run %q", got.Status, got.RunID)
	}
	all := got.Events
	if all[0].Kind != instrument.KindRunStarted || all[len(all)-1].Kind != instrument.KindRunCompleted {
		t.Fatalf("run lifecycle events missing: first %s, last %s", all[0].Kind, all[len(all)-1].Kind)
	}

	type stage struct {
		kind      instrument.Kind
		component string
		pipeline  string
		inputSeq  int
	}
	// transform and filter emit synchronously inside the consume call, so their
	// output is correlated with the input. batch emits from its own goroutine
	// after the timeout and cannot be correlated.
	events := telemetryEvents(got)
	in := events[0].Seq
	want := []stage{
		{instrument.KindInput, "otlp", "", 0},
		{instrument.KindIngress, "transform", "traces", 0},
		{instrument.KindEgress, "transform", "traces", in + 1},
		{instrument.KindIngress, "filter", "traces", 0},
		{instrument.KindEgress, "filter", "traces", in + 3},
		{instrument.KindIngress, "batch", "traces", 0},
		{instrument.KindEgress, "batch", "traces", 0},
		{instrument.KindExport, "otlp", "", 0},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d telemetry events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		e := events[i]
		if e.Kind != w.kind || e.Component != w.component || e.Pipeline != w.pipeline || e.Signal != "traces" || e.InputSeq != w.inputSeq {
			t.Errorf("event %d = {%s %s %q input=%d %s}, want %+v",
				i, e.Kind, e.Component, e.Pipeline, e.InputSeq, e.Signal, w)
		}
	}
	for i := 1; i < 3; i++ {
		if events[i].Seq != in+i {
			t.Errorf("transform events are not consecutive: %+v", events[:3])
		}
	}

	snap := func(e instrument.Event) string { return string(got.Snapshots[e.Snapshot]) }
	ingress, egress := snap(events[1]), snap(events[2])
	if !strings.Contains(ingress, "user.email") || strings.Contains(ingress, "deployment.environment") {
		t.Error("ingress snapshot is not the pre-transform telemetry")
	}
	if strings.Contains(egress, "user.email") || !strings.Contains(egress, "deployment.environment") {
		t.Error("egress snapshot is not the post-transform telemetry")
	}
	if c := events[4].Counts.Items; c != 2 {
		t.Errorf("filter egress has %d spans, want 2", c)
	}
	// batch only regroups, so the export equals the filter egress: the
	// content-addressed store holds that snapshot once.
	if events[4].Snapshot != events[7].Snapshot {
		t.Error("export differs from filter egress although batch only regroups")
	}

	// Every decorated processor was started and stopped exactly once.
	lifecycle := map[string][]instrument.Kind{}
	for _, e := range all {
		if e.Kind == instrument.KindComponentStarted || e.Kind == instrument.KindComponentStopped {
			lifecycle[e.Component] = append(lifecycle[e.Component], e.Kind)
		}
	}
	for _, c := range []string{"transform", "filter", "batch"} {
		if !slices.Equal(lifecycle[c], []instrument.Kind{instrument.KindComponentStarted, instrument.KindComponentStopped}) {
			t.Errorf("%s lifecycle = %v", c, lifecycle[c])
		}
	}
}

// telemetryEvents returns the events that carry a snapshot.
func telemetryEvents(r *runresult.Result) []instrument.Event {
	var out []instrument.Event
	for _, e := range r.Events {
		if e.Snapshot != "" {
			out = append(out, e)
		}
	}
	return out
}

// TestSharedProcessorIsAttributedPerPipeline checks that one processor ID in two
// pipelines of the same signal runs as two instances, and each observation
// names its pipeline.
func TestSharedProcessorIsAttributedPerPipeline(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `receivers: {otlp: {protocols: {grpc: {}}}}
processors:
  attributes/tag: {actions: [{key: tagged, value: true, action: insert}]}
  filter/drop-db:
    traces: {span: ['attributes["db.system"] != nil']}
exporters: {nop: {}, otlp: {endpoint: "collector.example.invalid:4317"}}
service:
  pipelines:
    traces/all: {receivers: [otlp], processors: [attributes/tag], exporters: [otlp]}
    traces/web: {receivers: [otlp], processors: [filter/drop-db, attributes/tag], exporters: [nop]}
`
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Run(context.Background(), Options{ConfigPath: cfg, Inputs: fixtureInputs(t)})
	if err != nil {
		t.Fatal(err)
	}
	items := map[string]int{}
	for _, e := range got.Events {
		if e.Kind == instrument.KindIngress && e.Component == "attributes/tag" {
			items[e.Pipeline] += e.Counts.Items
		}
	}
	// The fixture has 3 spans, one of them a database span.
	want := map[string]int{"traces/all": 3, "traces/web": 2}
	if !maps.Equal(items, want) {
		t.Fatalf("attributes/tag input items per pipeline = %v, want %v", items, want)
	}
}

// TestCancelledRunShutsDownCleanly checks that cancelling during injection
// stops the run, every started component is stopped, the remaining input
// is not injected and the run is not reported as completed.
func TestCancelledRunShutsDownCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputs := append(fixtureInputs(t), fixtureInputs(t)...)
	got, err := Run(ctx, Options{
		ConfigPath: tracesConfig,
		Inputs:     inputs,
		Observer: func(e instrument.Event) {
			if e.Kind == instrument.KindEgress && e.Component == "transform" {
				cancel()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runresult.StatusCancelled || got.Completed() {
		t.Fatalf("status = %q", got.Status)
	}
	if !got.Inputs[0].Injected || got.Inputs[1].Injected {
		t.Fatalf("inputs = %+v, want only the first injected", got.Inputs)
	}
	running := map[string]int{}
	for _, e := range got.Events {
		switch e.Kind {
		case instrument.KindComponentStarted:
			running[e.Component]++
		case instrument.KindComponentStopped:
			running[e.Component]--
		}
	}
	for c, n := range running {
		if n != 0 {
			t.Errorf("%s: started %d more time(s) than stopped", c, n)
		}
	}
	if last := got.Events[len(got.Events)-1]; last.Kind != instrument.KindRunCancelled {
		t.Fatalf("last event = %s", last.Kind)
	}
}

// TestRunCancelledBeforeStartupStopsEveryComponent cancels runs as they
// start the Collector: each is cancelled, injects nothing and stops every
// component it started. Under the race detector it also guards against
// calling Collector.Shutdown before Collector.Run has begun, whose WaitGroup
// use races in otelcol v0.162.0; the race depends on scheduling, hence the
// repetitions.
func TestRunCancelledBeforeStartupStopsEveryComponent(t *testing.T) {
	for range 10 {
		ctx, cancel := context.WithCancel(context.Background())
		got, err := Run(ctx, Options{
			ConfigPath: tracesConfig,
			Inputs:     fixtureInputs(t),
			Observer: func(e instrument.Event) {
				if e.Kind == instrument.KindRunStarted {
					cancel()
				}
			},
		})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != runresult.StatusCancelled || got.Inputs[0].Injected {
			t.Fatalf("status %q, inputs %+v", got.Status, got.Inputs)
		}
		running := map[string]int{}
		for _, e := range got.Events {
			switch e.Kind {
			case instrument.KindComponentStarted:
				running[e.Component]++
			case instrument.KindComponentStopped:
				running[e.Component]--
			}
		}
		for c, n := range running {
			if n != 0 {
				t.Fatalf("%s: started %d more time(s) than stopped", c, n)
			}
		}
	}
}

// TestExpiredRunIsPartial checks that a run cut short by its deadline is
// partial, never completed.
func TestExpiredRunIsPartial(t *testing.T) {
	// The run's deadline "expires" once the first input is injected.
	ctx, expire := context.WithCancelCause(context.Background())
	defer expire(nil)
	got, err := Run(ctx, Options{
		ConfigPath: tracesConfig,
		Inputs:     append(fixtureInputs(t), fixtureInputs(t)...),
		Observer: func(e instrument.Event) {
			if e.Kind == instrument.KindInput {
				expire(context.DeadlineExceeded)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != runresult.StatusPartial || got.Completed() || got.Inputs[1].Injected {
		t.Fatalf("status %q, inputs %+v", got.Status, got.Inputs)
	}
	if last := got.Events[len(got.Events)-1]; last.Kind != instrument.KindRunFailed || last.Error == "" {
		t.Fatalf("last event = %+v", last)
	}
}

// TestCompareDetectsSemanticDifference guards against a vacuous fidelity
// proof: a reference run of a config with one OTTL statement removed must not
// compare equal.
func TestCompareDetectsSemanticDifference(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	raw, err := os.ReadFile(tracesConfig)
	if err != nil {
		t.Fatal(err)
	}
	variant := strings.Replace(string(raw), `- delete_key(attributes, "user.email")`, "", 1)
	if variant == string(raw) {
		t.Fatal("fixture config no longer contains the statement this test removes")
	}
	variantPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(variantPath, []byte(variant), 0o600); err != nil {
		t.Fatal(err)
	}

	fixture := fixtureInputs(t)
	got, err := Run(ctx, Options{ConfigPath: tracesConfig, Inputs: fixture})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := RunReference(ctx, Options{ConfigPath: variantPath, Inputs: fixture})
	if err != nil {
		t.Fatal(err)
	}
	if cmp := runresult.Compare(got, ref); cmp.Equal {
		t.Fatal("Compare reported equal output for semantically different configs")
	}
}

// TestInstrumentedRunIsNetworkIsolated points the exporter at a live OTLP
// sink and the receiver at a port that is already taken. An isolated run must
// neither deliver to the sink nor attempt to listen.
func TestInstrumentedRunIsNetworkIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	s, err := startSink()
	if err != nil {
		t.Fatal(err)
	}
	defer s.stop()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "receivers: {otlp: {protocols: {grpc: {endpoint: " + occupied.Addr().String() + "}}}}\n" +
		"exporters: {otlp: {endpoint: " + s.addr + ", tls: {insecure: true}}}\n" +
		"service: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Run(ctx, Options{ConfigPath: cfg, Inputs: fixtureInputs(t)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got.Outputs) != 1 {
		t.Fatalf("outputs = %+v, want local capture", got.Outputs)
	}
	if n := len(s.payloads()); n != 0 {
		t.Fatalf("external OTLP endpoint received %d requests", n)
	}
}

func TestRunRejectsAmbiguousReceiver(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "receivers: {otlp/a: {protocols: {grpc: {}}}, otlp/b: {protocols: {grpc: {}}}}\n" +
		"exporters: {nop: {}}\n" +
		"service: {pipelines: {traces: {receivers: [otlp/a, otlp/b], exporters: [nop]}}}\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{ConfigPath: cfg, Inputs: fixtureInputs(t)})
	if err == nil || !strings.Contains(err.Error(), "several receivers") {
		t.Fatalf("err = %v, want ambiguity error", err)
	}
}

func outputData(r *runresult.Result) string {
	var b strings.Builder
	for _, o := range r.Outputs {
		b.WriteString(o.Exporter + "=" + string(o.Data) + " ")
	}
	return b.String()
}
