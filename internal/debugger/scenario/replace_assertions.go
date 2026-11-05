package scenario

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// ReplaceAssertions replaces the document's assertions with validated assertion
// source nodes. It reads no files and leaves other scenario nodes intact.
func ReplaceAssertions(raw, replacement []byte) ([]byte, error) {
	if _, err := ParseAssertions(replacement); err != nil {
		return nil, fmt.Errorf("assertions: %w", err)
	}
	var doc, assertions yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("scenario must be a YAML mapping")
	}
	if err := yaml.Unmarshal(replacement, &assertions); err != nil {
		return nil, fmt.Errorf("assertions: %w", err)
	}
	// Materialize replacement aliases, including references to its optional
	// version node. Its anchors must not rebind retained scenario aliases.
	var copyNode func(*yaml.Node) *yaml.Node
	copyNode = func(n *yaml.Node) *yaml.Node {
		copy := *resolvedNode(n)
		copy.Anchor = ""
		copy.Content = make([]*yaml.Node, len(copy.Content))
		for i, child := range resolvedNode(n).Content {
			copy.Content[i] = copyNode(child)
		}
		return &copy
	}
	setField(doc.Content[0], "assertions", copyNode(field(assertions.Content[0], "assertions")))
	return yaml.Marshal(&doc)
}
