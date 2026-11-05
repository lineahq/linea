package configmgmt

import (
	"encoding/json"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Component set bounds. A contrib build provides a few hundred types.
const (
	// MaxComponentTypes bounds the types of one build across all kinds.
	MaxComponentTypes = 4096
	// MaxComponentsHash bounds an AvailableComponents hash, in bytes.
	MaxComponentsHash = 64
	// MaxMissing bounds the missing types a check reports.
	MaxMissing = 32
)

// Components lists component types by Collector section ("receivers",
// "processors", ...), each sorted and unique. As a build's available set,
// it is what the agent reported; as a version's use, it is what the body
// configures. Its JSON form is what fleet.component_sets stores, so set
// containment can be checked in SQL.
type Components map[string][]string

// NewComponents normalizes kind → types from a report. It returns nil when
// the report exceeds the bounds or names no known section, so an unusable
// report skips the check rather than blocking delivery.
func NewComponents(byKind map[string][]string) Components {
	out := Components{}
	total := 0
	for kind, types := range byKind {
		if _, ok := sections[kind]; !ok {
			continue
		}
		var keep []string
		for _, t := range types {
			if componentType.MatchString(t) {
				keep = append(keep, t)
			}
		}
		slices.Sort(keep)
		keep = slices.Compact(keep)
		if total += len(keep); total > MaxComponentTypes {
			return nil
		}
		if len(keep) > 0 {
			out[kind] = keep
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Uses returns the component types a version body configures. Every entry
// of a section counts, used in a pipeline or not: the Collector refuses
// unknown types when it loads the config. Bodies were validated when
// stored; one that no longer parses uses nothing.
func Uses(body []byte) Components {
	var doc yaml.Node
	if yaml.Unmarshal(body, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return Components{}
	}
	root := doc.Content[0]
	nodes := 0
	if bounded(root, 0, &nodes) != nil {
		return Components{}
	}
	byKind := map[string][]string{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i].Value, root.Content[i+1]
		if _, ok := sections[key]; !ok || value.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j < len(value.Content); j += 2 {
			typ, _, _ := strings.Cut(value.Content[j].Value, "/")
			byKind[key] = append(byKind[key], typ)
		}
	}
	if out := NewComponents(byKind); out != nil {
		return out
	}
	return Components{}
}

// Merge adds the types of other.
func (c Components) Merge(other Components) Components {
	out := Components{}
	for _, src := range []Components{c, other} {
		for kind, types := range src {
			out[kind] = append(out[kind], types...)
		}
	}
	for kind, types := range out {
		slices.Sort(types)
		out[kind] = slices.Compact(types)
	}
	return out
}

// Missing returns the types in uses that the available set lacks, as
// "receiver:filelog", sorted and at most MaxMissing.
func (c Components) Missing(uses Components) []string {
	var out []string
	for kind, types := range uses {
		for _, t := range types {
			if _, found := slices.BinarySearch(c[kind], t); !found {
				out = append(out, sections[kind]+":"+t)
			}
		}
	}
	slices.Sort(out)
	if len(out) > MaxMissing {
		out = out[:MaxMissing]
	}
	return out
}

// JSON is the stored form.
func (c Components) JSON() []byte {
	b, _ := json.Marshal(c)
	return b
}

// ParseComponents reads the stored form; it returns nil when it is unusable.
func ParseComponents(b []byte) Components {
	var byKind map[string][]string
	if json.Unmarshal(b, &byKind) != nil {
		return nil
	}
	return NewComponents(byKind)
}
