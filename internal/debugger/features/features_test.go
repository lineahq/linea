package features

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/component"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

func TestFeaturesAreMissingUntilProvided(t *testing.T) {
	// This test binary imports no feature package.
	if got := Missing(); len(got) != len(All) {
		t.Fatalf("Missing() = %v, want every feature", got)
	}
	ProvideCount(func(context.Context, string, component.Config, int, telemetry.Payload, int, *telemetry.Payload) (*analysis.Aggregation, error) {
		return nil, nil
	})
	defer ProvideCount(nil)
	if !Has(CountAggregation) || Count() == nil || slices.Contains(Missing(), CountAggregation) {
		t.Fatalf("count was provided, Missing() = %v", Missing())
	}
	if Has(OTTLReplay) || Replay() != nil {
		t.Fatal("the OTTL replay was never provided")
	}
}

func TestLackingNamesTheFeaturesAManifestDoesNotBring(t *testing.T) {
	got := Lacking([]string{
		"github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor",
		"go.opentelemetry.io/collector/processor/batchprocessor",
	})
	var names []Name
	for _, f := range got {
		names = append(names, f.Name)
	}
	if want := []Name{CountAggregation, RoutingExplanation, SamplingExplanation, FilterExplanation}; !slices.Equal(names, want) {
		t.Fatalf("Lacking = %v, want %v", names, want)
	}
	if p := Packages(); p["github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor"] != "github.com/lineahq/linea/internal/debugger/ottlreplay" {
		t.Fatalf("Packages() = %v", p)
	}
}

// TestPathFieldNamesLevelAndField: a context prefix names its own level or
// another one; otelcol paths read the client context, not a record field.
func TestPathFieldNamesLevelAndField(t *testing.T) {
	seg := func(names ...string) []PathSegment {
		var out []PathSegment
		for _, n := range names {
			name, key, _ := strings.Cut(n, ":")
			out = append(out, PathSegment{Name: name, Key: key})
		}
		return out
	}
	cases := []struct {
		ctx, pathContext string
		segs             []PathSegment
		want             FieldRead
		ok               bool
	}{
		{"log", "", seg("log", "attributes:a"), FieldRead{Level: LevelRecord, Field: "attributes", Key: "a"}, true},
		{"log", "", seg("resource", "attributes:r"), FieldRead{Level: LevelResource, Field: "attributes", Key: "r"}, true},
		{"spanevent", "", seg("name"), FieldRead{Level: LevelRecord, Field: "events", Event: "name"}, true},
		{"log", "", seg("cache:x"), FieldRead{}, false},
		{"log", "", seg("otelcol", "client", "addr"), FieldRead{Level: LevelClient, Field: "address"}, true},
		{"log", "", seg("otelcol", "client", "metadata:tenant"), FieldRead{Level: LevelClient, Field: "metadata", Key: "tenant"}, true},
		{"log", "", seg("otelcol", "client", "auth", "attributes:sub"), FieldRead{Level: LevelClient, Field: "auth", Key: "sub"}, true},
		{"log", "otelcol", seg("grpc", "metadata:x-id"), FieldRead{Level: LevelClient, Field: "grpc_metadata", Key: "x-id"}, true},
		{"log", "", seg("otelcol", "other", "thing"), FieldRead{}, false},
	}
	for _, c := range cases {
		got, ok := PathField(c.ctx, c.pathContext, c.segs)
		if ok != c.ok || got != c.want {
			t.Errorf("PathField(%q, %q, %+v) = %+v, %v; want %+v, %v", c.ctx, c.pathContext, c.segs, got, ok, c.want, c.ok)
		}
	}
}
