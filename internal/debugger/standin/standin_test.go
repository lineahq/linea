package standin

import (
	"reflect"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/processor"
)

func TestSettingsKeepTheirShapeWithValuesRedacted(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Unmarshal(confmap.NewFromStringMap(map[string]any{
		"api":     map[string]any{"key": "secret", "site": "eu"},
		"brokers": []any{"kafka:9092"},
		"empty":   nil,
	})); err != nil {
		t.Fatal(err)
	}
	conf := confmap.New()
	if err := conf.Marshal(map[string]any{"datadog": cfg}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"datadog": map[string]any{
		"api":     map[string]any{"key": Redacted, "site": Redacted},
		"brokers": []any{Redacted},
		"empty":   nil,
	}}
	if got := conf.ToStringMap(); !reflect.DeepEqual(got, want) {
		t.Fatalf("marshaled %v, want %v", got, want)
	}
}

func TestAddCoversOnlyMissingTypes(t *testing.T) {
	known := component.MustNewType("known")
	base := otelcol.Factories{Processors: map[component.Type]processor.Factory{known: Processor(known)}}
	unknown := component.MustNewType("unknown")
	m := Find(base, map[string][]component.ID{"processors": {component.NewID(known), component.NewIDWithName(unknown, "a"), component.NewID(unknown)}})
	if !reflect.DeepEqual(m, Missing{Processors: []component.Type{unknown}}) {
		t.Fatalf("missing %+v", m)
	}
	out := Add(base, m)
	if len(out.Processors) != 2 || len(base.Processors) != 1 || !Is(out.Processors[unknown].CreateDefaultConfig()) {
		t.Fatal("stand-in not added, or base changed")
	}
}
