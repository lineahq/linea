package engine

import (
	"slices"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/registry"
)

// TestConfiguredWritesListsActionKeys: a resource or attributes action
// writes its key; a pattern or extract action writes any key. Overlap
// needs the same level and field, and the same key unless either is any.
func TestConfiguredWritesListsActionKeys(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	configOf := func(typ string, raw map[string]any) component.Config {
		t.Helper()
		f, ok := factories.Processors[component.MustNewType(typ)]
		if !ok {
			t.Fatalf("no %s processor", typ)
		}
		cfg := f.CreateDefaultConfig()
		if err := confmap.NewFromStringMap(raw).Unmarshal(cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	rec, res := features.LevelRecord, features.LevelResource
	cases := []struct {
		typ  string
		raw  map[string]any
		want []features.FieldRead
	}{
		{"resource", map[string]any{"attributes": []any{
			map[string]any{"key": "env", "value": "x", "action": "insert"},
			map[string]any{"key": "tmp", "action": "delete"},
		}}, []features.FieldRead{{Level: res, Field: "attributes", Key: "env"}, {Level: res, Field: "attributes", Key: "tmp"}}},
		{"attributes", map[string]any{"actions": []any{
			map[string]any{"key": "user.password", "action": "delete"},
			map[string]any{"pattern": "^secret\\.", "action": "delete"},
			map[string]any{"key": "url", "pattern": "^/(?P<route>\\w+)", "action": "extract"},
		}}, []features.FieldRead{{Level: rec, Field: "attributes", Key: "user.password"}, {Level: rec, Field: "attributes"}, {Level: rec, Field: "attributes"}}},
	}
	for _, c := range cases {
		got, reason := configuredWrites(c.typ, configOf(c.typ, c.raw), pipeline.SignalLogs)
		if reason != "" || !slices.Equal(got, c.want) {
			t.Fatalf("%s writes = %+v (reason %q), want %+v", c.typ, got, reason, c.want)
		}
	}

	if _, reason := configuredWrites("groupbyattrs", nil, pipeline.SignalLogs); reason == "" {
		t.Fatal("an unlisted type must give a reason")
	}

	env := features.FieldRead{Level: res, Field: "attributes", Key: "env"}
	overlapCases := []struct {
		w    features.FieldRead
		want bool
	}{
		{features.FieldRead{Level: res, Field: "attributes", Key: "env"}, true},
		{features.FieldRead{Level: res, Field: "attributes"}, true},
		{features.FieldRead{Level: res, Field: "attributes", Key: "other"}, false},
		{features.FieldRead{Level: rec, Field: "attributes", Key: "env"}, false},
		{features.FieldRead{Level: res, Field: "dropped_attributes_count"}, false},
	}
	for _, c := range overlapCases {
		if got := overlaps(env, c.w); got != c.want {
			t.Fatalf("overlaps(%+v, %+v) = %t", env, c.w, got)
		}
	}
	if !overlaps(features.FieldRead{Level: rec, Field: "attributes"}, features.FieldRead{Level: rec, Field: "attributes", Key: "x"}) {
		t.Fatal("a read of the whole map overlaps a write of one key")
	}
}
