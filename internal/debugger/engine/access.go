package engine

import (
	"fmt"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/features"
)

// configuredAccess lists the fields a processor of type typ may read and
// write on a record of signal, from its configuration alone: what any
// input could exercise, not what this input did. An empty Key is any key of
// the field. reason is set when the fields cannot be listed; callers then
// treat the processor as reading and writing everything.
func configuredAccess(typ string, cfg component.Config, signal pipeline.Signal) (reads, writes []features.FieldRead, reason string) {
	rec, res, scope := features.LevelRecord, features.LevelResource, features.LevelScope
	switch typ {
	case "batch", "queue_batch", "memory_limiter":
		// They pass records on unchanged and look at none of their fields
		// (the batchers' metadata_keys read the request's client metadata).
		return nil, nil, ""
	case "filter":
		// A filter removes records and changes none.
		r := features.FilterReads()
		if r == nil {
			return nil, nil, "this runtime does not list filter conditions"
		}
		reads, reason = r(cfg, signal)
		return reads, nil, reason
	case "transform":
		a := features.TransformAccess()
		if a == nil {
			return nil, nil, "this runtime does not list transform statements"
		}
		return a(cfg, signal)
	case "resource":
		return actionAccess(cfg, "attributes", res)
	case "attributes":
		return actionAccess(cfg, "actions", rec)
	case "span":
		// Renames from or to attributes and sets the status.
		return []features.FieldRead{{Level: rec, Field: "name"}, {Level: rec, Field: "attributes"}},
			[]features.FieldRead{{Level: rec, Field: "name"}, {Level: rec, Field: "attributes"}, {Level: rec, Field: "status"}}, ""
	case "redaction":
		// Masks or removes attribute values at every level, and with its
		// sanitizers rewrites names and bodies.
		all := []features.FieldRead{{Level: res, Field: "attributes"}, {Level: scope, Field: "attributes"}, {Level: rec, Field: "attributes"},
			{Level: rec, Field: "body"}, {Level: rec, Field: "name"}}
		return all, all, ""
	case "probabilistic_sampler":
		// May hash an attribute to decide and record its decision on the
		// record.
		return []features.FieldRead{{Level: rec, Field: "attributes"}, {Level: rec, Field: "trace_id"}, {Level: rec, Field: "trace_state"}},
			[]features.FieldRead{{Level: rec, Field: "attributes"}, {Level: rec, Field: "trace_state"}}, ""
	}
	return nil, nil, fmt.Sprintf("the fields a %s processor accesses are not listed", typ)
}

// configuredWrites is configuredAccess's writes.
func configuredWrites(typ string, cfg component.Config, signal pipeline.Signal) ([]features.FieldRead, string) {
	_, writes, reason := configuredAccess(typ, cfg, signal)
	return writes, reason
}

// actionAccess lists the attribute keys a resource or attributes
// processor's actions read and write, from its configuration under field.
// Every action but extract writes its own key; a pattern action writes the
// keys it matches and extract the keys its rule names, so both write any
// key. insert and update act on whether the key is present, and hash,
// extract and convert on its value, so they read it too; from_attribute
// reads its source key. include and exclude match on any field, which
// cannot be listed here.
func actionAccess(cfg component.Config, field string, level features.Level) (reads, writes []features.FieldRead, reason string) {
	conf := confmap.New()
	if err := conf.Marshal(cfg); err != nil {
		return nil, nil, "the configuration could not be read: " + err.Error()
	}
	if conf.IsSet("include") || conf.IsSet("exclude") {
		if m, _ := conf.Get("include").(map[string]any); len(m) > 0 {
			return nil, nil, "include matches on fields that are not listed"
		}
		if m, _ := conf.Get("exclude").(map[string]any); len(m) > 0 {
			return nil, nil, "exclude matches on fields that are not listed"
		}
	}
	actions, _ := conf.Get(field).([]any)
	attr := func(key string) features.FieldRead {
		return features.FieldRead{Level: level, Field: "attributes", Key: key}
	}
	for i, a := range actions {
		m, ok := a.(map[string]any)
		if !ok {
			return nil, nil, fmt.Sprintf("action %d could not be read", i)
		}
		key, _ := m["key"].(string)
		pattern, _ := m["pattern"].(string)
		action, _ := m["action"].(string)
		from, _ := m["from_attribute"].(string)
		action = strings.ToLower(action)
		target := key
		if pattern != "" || action == "extract" {
			target = ""
		}
		switch action {
		case "insert", "update", "hash", "extract", "convert":
			reads = append(reads, attr(key))
		}
		if from != "" {
			reads = append(reads, attr(from))
		}
		writes = append(writes, attr(target))
	}
	return reads, writes, ""
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
