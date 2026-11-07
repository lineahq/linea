package engine

import (
	"fmt"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/features"
)

// configuredWrites lists the fields a processor of type typ may write on a
// record of signal, from its configuration alone: what any input could
// see changed, not what this input did. An empty Key is any key of the
// field. reason is set when the fields cannot be listed; the caller then
// keeps the filter where it is.
func configuredWrites(typ string, cfg component.Config, signal pipeline.Signal) (writes []features.FieldRead, reason string) {
	rec, res, scope := features.LevelRecord, features.LevelResource, features.LevelScope
	switch typ {
	case "filter":
		// A filter removes records and changes none.
		return nil, ""
	case "transform":
		w := features.TransformWrites()
		if w == nil {
			return nil, "this runtime does not list transform statements"
		}
		return w(cfg, signal)
	case "resource":
		return actionWrites(cfg, "attributes", res)
	case "attributes":
		return actionWrites(cfg, "actions", rec)
	case "span":
		// Renames from or to attributes and sets the status.
		return []features.FieldRead{{Level: rec, Field: "name"}, {Level: rec, Field: "attributes"}, {Level: rec, Field: "status"}}, ""
	case "redaction":
		// Masks or removes attribute values at every level, and with its
		// sanitizers rewrites names and bodies.
		return []features.FieldRead{{Level: res, Field: "attributes"}, {Level: scope, Field: "attributes"}, {Level: rec, Field: "attributes"},
			{Level: rec, Field: "body"}, {Level: rec, Field: "name"}}, ""
	case "probabilistic_sampler":
		// May record its sampling decision on the record.
		return []features.FieldRead{{Level: rec, Field: "attributes"}, {Level: rec, Field: "trace_state"}}, ""
	}
	return nil, fmt.Sprintf("the fields a %s processor writes are not listed", typ)
}

// actionWrites lists the attribute keys of a resource or attributes
// processor's actions, read from its configuration under field. Every
// action but extract writes its own key; an action with a pattern writes
// the keys it matches, and extract writes the keys its rule names, so both
// count as any key.
func actionWrites(cfg component.Config, field string, level features.Level) ([]features.FieldRead, string) {
	conf := confmap.New()
	if err := conf.Marshal(cfg); err != nil {
		return nil, "the configuration could not be read: " + err.Error()
	}
	actions, _ := conf.Get(field).([]any)
	var out []features.FieldRead
	for i, a := range actions {
		m, ok := a.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("action %d could not be read", i)
		}
		key, _ := m["key"].(string)
		pattern, _ := m["pattern"].(string)
		action, _ := m["action"].(string)
		if pattern != "" || strings.EqualFold(action, "extract") || key == "" {
			key = ""
		}
		out = append(out, features.FieldRead{Level: level, Field: "attributes", Key: key})
	}
	return out, ""
}

// overlaps reports whether a write may change what a read sees: the same
// level and field, the same span event field when both name one, and the
// same key unless either is any key.
func overlaps(r, w features.FieldRead) bool {
	if r.Level != w.Level || r.Field != w.Field {
		return false
	}
	if r.Event != "" && w.Event != "" && r.Event != w.Event {
		return false
	}
	return r.Key == "" || w.Key == "" || r.Key == w.Key
}

// writeConflict names the first field the filter reads that processor's
// configuration may write; "" when there is none.
func writeConflict(processor, filter string, reads, writes []features.FieldRead) string {
	for _, r := range reads {
		for _, w := range writes {
			if overlaps(r, w) {
				return fmt.Sprintf("%s reads %s, which %s's configuration may set; placed before %s, it would decide on the value before %s sets it, on any input where it does.",
					filter, readName(r), processor, processor, processor)
			}
		}
	}
	return ""
}
