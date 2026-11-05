package engine

import (
	"testing"

	"github.com/lineahq/linea/internal/debugger/features"
)

func TestTheBuiltInRuntimeHasEveryOptionalFeature(t *testing.T) {
	// The built-in manifest names every feature's component, so the
	// generated feature set compiles them all in.
	if got := BuiltInRuntime().MissingFeatures; got != nil {
		t.Fatalf("missing features %v", got)
	}
	for _, f := range features.All {
		if !features.Has(f.Name) {
			t.Errorf("%s is not provided", f.Name)
		}
	}
	for _, v := range []string{"0.8", "0.9", "0.10", "0.11", ReportSchemaVersion} {
		if !ReadableReportSchema(v) {
			t.Errorf("report schema %s is not readable: each is the next without its new fields", v)
		}
	}
}
