package mcp

import (
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// inputSchema infers a tool's input schema as the SDK would, then spells each
// nullable type (inferred for pointers and slices as "type": ["null", T]) as
// anyOf branches with a single type each. Clients that map tool schemas onto
// a single-type dialect reject or drop type arrays; the accepted arguments,
// including an explicit null, stay the same.
func inputSchema[In any]() (*jsonschema.Schema, error) {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		return nil, err
	}
	splitNullableTypes(schema)
	return schema, nil
}

// optional removes properties from the required lists, following dotted paths
// into nested objects. It is for request fields the platform defaults when
// zero but the shared type always encodes.
func optional(s *jsonschema.Schema, paths ...string) {
	for _, path := range paths {
		target, name := s, path
		for {
			parent, rest, nested := strings.Cut(name, ".")
			if !nested {
				break
			}
			if target = target.Properties[parent]; target == nil {
				panic("mcp: no schema property " + path)
			}
			name = rest
		}
		if target.Properties[name] == nil {
			panic("mcp: no schema property " + path)
		}
		target.Required = slices.DeleteFunc(target.Required, func(required string) bool { return required == name })
	}
}

func splitNullableTypes(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	for _, child := range s.Properties {
		splitNullableTypes(child)
	}
	for _, child := range s.Defs {
		splitNullableTypes(child)
	}
	for _, child := range s.AnyOf {
		splitNullableTypes(child)
	}
	splitNullableTypes(s.Items)
	splitNullableTypes(s.AdditionalProperties)

	if len(s.Types) != 2 || !slices.Contains(s.Types, "null") {
		return
	}
	value := *s
	value.Types = nil
	value.Type = s.Types[0]
	if value.Type == "null" {
		value.Type = s.Types[1]
	}
	// Annotations describe the property, so they stay on the outer schema.
	value.Title, value.Description = "", ""
	*s = jsonschema.Schema{
		Title:       s.Title,
		Description: s.Description,
		AnyOf:       []*jsonschema.Schema{{Type: "null"}, &value},
	}
}
