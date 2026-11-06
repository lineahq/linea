package features

import (
	"context"
	"slices"
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
