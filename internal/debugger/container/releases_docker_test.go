package container

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/catalog"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/manifest"
	"github.com/lineahq/linea/samples"
)

// releasesEnv narrows TestEveryReleaseRunsTheSamples to a comma-separated
// list of releases; empty means every release of the support table.
const releasesEnv = "LINEA_RELEASES"

// releaseManifest composes the built-in components at a release from its
// catalog: the release's own module versions, never rewritten ones.
func releaseManifest(t *testing.T, table *versions.Table, version string) []byte {
	t.Helper()
	builtIn := parse(t, builtInManifest(t))
	cat := table.Release(builtIn.CollectorVersion()).Catalog
	var picked []string
	for _, kind := range manifest.Kinds {
		for _, c := range builtIn.Components(kind) {
			i := slices.IndexFunc(cat.Components, func(e versions.Component) bool { return e.Module() == c.ModulePath() })
			if i < 0 {
				t.Fatalf("built-in module %s is not in the %s catalog", c.ModulePath(), builtIn.CollectorVersion())
			}
			picked = append(picked, cat.Components[i].Kind+"/"+cat.Components[i].Type)
		}
	}
	res, err := catalog.Compose(table.Release(version).Catalog, builtInManifest(t), catalog.Request{Components: picked})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unknown) > 0 {
		t.Fatalf("Collector %s lacks built-in components %v", version, res.Unknown)
	}
	return res.Manifest
}

// TestEveryReleaseRunsTheSamples builds the built-in components at every
// verified release and runs, on each worker, every sample's scenario
// with the outcome samples/index.yaml declares and the testdata/components
// configurations with an equal reference comparison. A manifest without the
// transform processor builds without the OTTL replay and its worker says so.
func TestEveryReleaseRunsTheSamples(t *testing.T) {
	d := requireDocker(t)
	src := packedSource(t)
	table := versions.Embedded()
	releases := table.Versions()
	if env := os.Getenv(releasesEnv); env != "" {
		releases = strings.Split(env, ",")
	}
	list, err := samples.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range releases {
		t.Run(version, func(t *testing.T) {
			r := table.Release(version)
			if r == nil {
				t.Fatalf("Collector %s is not in the support table", version)
			}
			name := "test-release-" + strings.ReplaceAll(version, ".", "-")
			rt, err := build(t, d, BuildRequest{Name: name, Manifest: releaseManifest(t, table, version), Source: src, Set: &DependencySet{GoMod: r.GoMod, GoSum: r.GoSum}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			w, err := d.Start(ctx, rt.Ref, rt.Name, RunOptions{Memory: 1 << 30})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if h := w.Hello(); h.CollectorVersion != version || len(h.MissingFeatures) != 0 {
				t.Fatalf("hello %+v", h)
			}
			for _, s := range list {
				t.Run("sample/"+s.ID, func(t *testing.T) {
					rep := executeOn(ctx, t, w, engine.Request{ConfigYAML: s.ConfigYAML, ScenarioYAML: s.ScenarioYAML})
					if got := rep.Assertions.Outcome; got != assertion.Outcome(s.Expect) {
						t.Fatalf("assertions = %s, want %s: %+v", got, s.Expect, rep.Assertions.Results)
					}
				})
			}
			for _, ref := range componentReferences() {
				t.Run("reference/"+ref.config, func(t *testing.T) {
					rep := executeOn(ctx, t, w, engine.Request{Config: filepath.Join("..", "..", "..", "testdata", "components", ref.config+".yaml"), Fixture: ref.fixture, Compare: true})
					if rep.Comparison == nil || !rep.Comparison.Equal {
						t.Fatalf("comparison: %+v", rep.Comparison)
					}
				})
			}
		})
	}

	t.Run("without the transform processor", func(t *testing.T) {
		version := table.Newest()
		res, err := catalog.Compose(table.Release(version).Catalog, builtInManifest(t), catalog.Request{
			Components: []string{"receivers/otlp", "processors/batch", "exporters/debug"}})
		if err != nil {
			t.Fatal(err)
		}
		r := table.Release(version)
		rt, err := build(t, d, BuildRequest{Name: "test-release-lean", Manifest: res.Manifest, Source: src, Set: &DependencySet{GoMod: r.GoMod, GoSum: r.GoSum}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		w, err := d.Start(ctx, rt.Ref, rt.Name, RunOptions{Memory: 1 << 30})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		want := []string{string(features.OTTLReplay), string(features.CountAggregation), string(features.RoutingExplanation), string(features.SamplingExplanation), string(features.FilterExplanation)}
		if got := w.Hello().MissingFeatures; !slices.Equal(got, want) {
			t.Fatalf("missing features %v, want %v", got, want)
		}
		cfg := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {batch: {}}\nexporters: {debug: {}}\n" +
			"service: {pipelines: {logs: {receivers: [otlp], processors: [batch], exporters: [debug]}}}\n"
		rep := executeOn(ctx, t, w, engine.Request{ConfigYAML: cfg, Fixture: sample("logs-cleanup", "input.json")})
		if rep.Result.Status != "completed" {
			t.Fatalf("status %s: %s", rep.Result.Status, rep.Result.StatusReason)
		}
	})
}

func executeOn(ctx context.Context, t *testing.T, w *Worker, req engine.Request) *engine.Report {
	t.Helper()
	prepared, err := engine.Prepare(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := w.Execute(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

type componentReference struct{ config, fixture string }

// componentReferences are TestExtendedComponentsMatchTheReference's inputs.
func componentReferences() []componentReference {
	var (
		root    = filepath.Join("..", "..", "..")
		traces  = filepath.Join(root, "testdata", "traces", "traces.json")
		logs    = filepath.Join(root, "testdata", "multisignal", "logs.json")
		metrics = filepath.Join(root, "samples", "metrics-rename", "input.json")
		deltas  = filepath.Join(root, "testdata", "components", "delta_sums.json")
	)
	return []componentReference{
		{"redaction", traces}, {"groupbyattrs", traces}, {"probabilistic_sampler", traces}, {"span", traces},
		{"metricstransform", metrics}, {"cumulativetodelta", metrics}, {"deltatocumulative", deltas},
		{"memory_limiter", logs}, {"forward", logs},
	}
}
