package scenario

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// maxIdentityNodes bounds alias expansion. A scenario that expands further is
// never proven identical to another, which only makes a comparison
// inconclusive.
const maxIdentityNodes = 1 << 20

// InputIdentity returns what a self-contained scenario injects, as bytes to
// digest: the document with every alias expanded, its top-level merge keys
// resolved and keys sorted, and its comments, name and assertions removed.
// The name labels a run and assertions only read it, so two scenarios with
// equal identities inject the same inputs and mocks under the same
// environment and execution settings. Everything else is kept as written, so
// a difference in style can make two equal scenarios differ, never the
// reverse. It reads no files: a scenario that still refers to one is the
// caller's to refuse.
func InputIdentity(raw []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || resolvedNode(doc.Content[0]).Kind != yaml.MappingNode {
		return nil, errors.New("scenario must be a YAML mapping")
	}
	nodes := 0
	var expand func(*yaml.Node) (*yaml.Node, error)
	expand = func(n *yaml.Node) (*yaml.Node, error) {
		if nodes++; nodes > maxIdentityNodes {
			return nil, fmt.Errorf("scenario expands to more than %d nodes", maxIdentityNodes)
		}
		copy := *resolvedNode(n)
		copy.Anchor, copy.HeadComment, copy.LineComment, copy.FootComment = "", "", "", ""
		copy.Content = make([]*yaml.Node, len(copy.Content))
		for i, child := range resolvedNode(n).Content {
			c, err := expand(child)
			if err != nil {
				return nil, err
			}
			copy.Content[i] = c
		}
		return &copy, nil
	}
	root, err := expand(doc.Content[0])
	if err != nil {
		return nil, err
	}
	materializeMapping(root)
	removeField(root, "name")
	removeField(root, "assertions")
	pairs := make([][2]*yaml.Node, 0, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		pairs = append(pairs, [2]*yaml.Node{root.Content[i], root.Content[i+1]})
	}
	slices.SortStableFunc(pairs, func(a, b [2]*yaml.Node) int { return strings.Compare(a[0].Value, b[0].Value) })
	root.Content = root.Content[:0]
	for _, p := range pairs {
		root.Content = append(root.Content, p[0], p[1])
	}
	return yaml.Marshal(root)
}
