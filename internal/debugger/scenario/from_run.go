package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// ErrMissingCapture means a referenced dependency is absent from private evidence.
var ErrMissingCapture = errors.New("execution input dependency was not captured")

// RunSources is private captured source data. FromRun never reads the host.
type RunSources struct {
	Mode, Receiver, ClientAddress        string
	Fixture, Setup, Assertions, Scenario []byte
	Dir                                  string
	Files                                map[string][]byte
}

// FromRun produces scenario v1 YAML and verifies it against the captured inputs,
// mocks and settings. Only caller-provided assertion errors expose parse details.
func FromRun(src RunSources, name string, appended []byte) ([]byte, error) {
	var extra []Assertion
	var extraDoc yaml.Node
	if len(appended) != 0 {
		var err error
		extra, err = ParseAssertions(appended)
		if err != nil {
			return nil, fmt.Errorf("assertions: %w", err)
		}
		if err := yaml.Unmarshal(appended, &extraDoc); err != nil {
			return nil, errors.New("cannot decode appended assertions")
		}
	}
	var original *Scenario
	var doc yaml.Node
	changed := false
	switch src.Mode {
	case "quick":
		original = &Scenario{Version: Version}
		if len(src.Setup) != 0 {
			setup, err := ParseSetup(src.Setup)
			if err != nil {
				return nil, errors.New("cannot express captured setup as a scenario")
			}
			original = setup
			original.Version = Version
			original.Name = ""
		}
		if len(src.Assertions) != 0 {
			var err error
			original.Assertions, err = ParseAssertions(src.Assertions)
			if err != nil {
				return nil, errors.New("cannot decode captured assertions")
			}
		}
		data, err := capturedScalar(src.Fixture, "inputs[0]")
		if err != nil {
			return nil, err
		}
		original.Inputs = []Input{{Receiver: src.Receiver, ClientAddress: src.ClientAddress, Data: *data}}
		root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
		setField(root, "version", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"})
		input := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if src.Receiver != "" {
			setField(input, "receiver", stringNode(src.Receiver))
		}
		if src.ClientAddress != "" {
			setField(input, "client_address", stringNode(src.ClientAddress))
		}
		setField(input, "data", data)
		setField(root, "inputs", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{input}})
		if len(src.Setup) != 0 {
			var setupDoc yaml.Node
			if err := yaml.Unmarshal(src.Setup, &setupDoc); err != nil {
				return nil, errors.New("cannot decode captured setup")
			}
			for _, key := range []string{"mocks", "environment", "execution", "capture_origins"} {
				if node := field(setupDoc.Content[0], key); node != nil {
					setField(root, key, node)
				}
			}
		}
		if len(src.Assertions) != 0 {
			var assertionDoc yaml.Node
			if err := yaml.Unmarshal(src.Assertions, &assertionDoc); err != nil {
				return nil, errors.New("cannot decode captured assertions")
			}
			setField(root, "assertions", field(assertionDoc.Content[0], "assertions"))
		}
		changed = true
	case "scenario":
		var err error
		original, err = Parse(src.Scenario, src.Dir)
		if err != nil {
			return nil, errors.New("cannot decode captured scenario")
		}
		if err := yaml.Unmarshal(src.Scenario, &doc); err != nil {
			return nil, errors.New("cannot decode captured scenario")
		}
	default:
		return nil, errors.New("unknown captured run mode")
	}
	original = original.WithFileReader(func(path string) ([]byte, error) {
		raw, ok := src.Files[path]
		if !ok {
			return nil, ErrMissingCapture
		}
		return bytes.Clone(raw), nil
	})
	root := doc.Content[0]
	if inputs := field(root, "inputs"); inputs != nil {
		for i, in := range resolvedNode(inputs).Content {
			if ref := original.Inputs[i].File; ref != "" {
				raw, err := original.readFile(ref)
				if err != nil {
					return nil, ErrMissingCapture
				}
				node, err := capturedScalar(raw, fmt.Sprintf("inputs[%d]", i))
				if err != nil {
					return nil, err
				}
				replaceField(in, "file", "data", node)
				changed = true
			}
		}
	}
	if mocks := field(root, "mocks"); mocks != nil {
		if files := field(mocks, "files"); files != nil {
			for i, f := range resolvedNode(files).Content {
				if ref := original.Mocks.Files[i].File; ref != "" {
					raw, err := original.readFile(ref)
					if err != nil {
						return nil, ErrMissingCapture
					}
					node, err := capturedScalar(raw, fmt.Sprintf("mocks.files[%d]", i))
					if err != nil {
						return nil, err
					}
					replaceField(f, "file", "content", node)
					changed = true
				}
			}
		}
		if kubernetes := field(mocks, "kubernetes"); kubernetes != nil {
			if len(original.Mocks.Kubernetes.Files) != 0 {
				materializeMapping(resolvedNode(kubernetes))
				objects := field(kubernetes, "objects")
				if objects == nil || resolvedNode(objects).Tag == "!!null" {
					objects = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
					setField(kubernetes, "objects", objects)
				}
				for _, ref := range original.Mocks.Kubernetes.Files {
					raw, err := original.readFile(ref)
					if err != nil {
						return nil, ErrMissingCapture
					}
					manifests, err := decodeManifests(raw)
					if err != nil {
						return nil, errors.New("cannot decode captured Kubernetes manifests")
					}
					for _, manifest := range manifests {
						var node yaml.Node
						if err := node.Encode(manifest); err != nil {
							return nil, errors.New("cannot inline captured Kubernetes objects")
						}
						resolvedNode(objects).Content = append(resolvedNode(objects).Content, &node)
					}
				}
				removeField(kubernetes, "files")
				changed = true
			}
		}
	}
	if name != "" {
		setField(root, "name", stringNode(name))
		changed = true
	}
	if len(extra) != 0 {
		assertions := field(root, "assertions")
		if assertions == nil || resolvedNode(assertions).Tag == "!!null" {
			assertions = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			setField(root, "assertions", assertions)
		}
		resolvedNode(assertions).Content = append(resolvedNode(assertions).Content, resolvedNode(field(extraDoc.Content[0], "assertions")).Content...)
		changed = true
	}
	raw := bytes.Clone(src.Scenario)
	if changed {
		var err error
		raw, err = yaml.Marshal(&doc)
		if err != nil {
			return nil, errors.New("cannot encode scenario from captured sources")
		}
	}
	if err := verifyFromRun(original, raw, name, extra); err != nil {
		return nil, err
	}
	return raw, nil
}

func verifyFromRun(original *Scenario, raw []byte, name string, extra []Assertion) error {
	invalid := errors.New("scenario from captured sources does not round-trip exactly")
	result, err := Parse(raw, "")
	if err != nil {
		return invalid
	}
	// Verification must also be closed to host reads, even if a transform missed
	// an inherited or otherwise unusual reference. Empty file keys are harmless.
	result = result.WithFileReader(func(string) ([]byte, error) { return nil, invalid })
	if len(result.Mocks.Kubernetes.Files) != 0 {
		return invalid
	}
	for _, in := range result.Inputs {
		if in.File != "" {
			return invalid
		}
	}
	for _, f := range result.Mocks.Files {
		if f.File != "" {
			return invalid
		}
	}
	if name == "" {
		name = original.Name
	}
	assertions := append(append([]Assertion(nil), original.Assertions...), extra...)
	if result.Name != name || !reflect.DeepEqual(result.Environment, original.Environment) || result.Execution != original.Execution ||
		!reflect.DeepEqual(result.CaptureOrigins, original.CaptureOrigins) ||
		len(result.Assertions) != len(assertions) || result.Mocks.Exporters != original.Mocks.Exporters || len(result.Inputs) != len(original.Inputs) {
		return invalid
	}
	for i := range assertions {
		if !reflect.DeepEqual(result.Assertions[i], assertions[i]) {
			return invalid
		}
	}
	before, err := original.Fixtures()
	if err != nil {
		if errors.Is(err, ErrMissingCapture) {
			return ErrMissingCapture
		}
		return invalid
	}
	after, err := result.Fixtures()
	if err != nil {
		return invalid
	}
	for i := range before {
		a, b := before[i], after[i]
		aj, ae := a.Payload.JSON()
		bj, be := b.Payload.JSON()
		if ae != nil || be != nil || !bytes.Equal(aj, bj) || a.Receiver != b.Receiver || a.ClientAddress != b.ClientAddress || a.After != b.After || original.Inputs[i].Format != result.Inputs[i].Format || original.Inputs[i].Signal != result.Inputs[i].Signal {
			return invalid
		}
		ar, ae := original.read(original.Inputs[i])
		br, be := result.read(result.Inputs[i])
		if ae != nil || be != nil || !bytes.Equal(ar, br) {
			return invalid
		}
	}
	a, err := original.MockSpec()
	if err != nil {
		if errors.Is(err, ErrMissingCapture) {
			return ErrMissingCapture
		}
		return invalid
	}
	b, err := result.MockSpec()
	if err != nil || a.Drain != b.Drain || !a.Clock.Equal(b.Clock) || !reflect.DeepEqual(a.Objects, b.Objects) || len(a.Files) != len(b.Files) {
		return invalid
	}
	for i, f := range a.Files {
		g := b.Files[i]
		if f.Path != g.Path || !f.Modified.Equal(g.Modified) || !bytes.Equal(f.Content, g.Content) || !bytes.Equal(f.Existing, g.Existing) {
			return invalid
		}
	}
	return nil
}

func capturedScalar(raw []byte, location string) (*yaml.Node, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%s: the captured file is not UTF-8 text and cannot be inlined", location)
	}
	n := stringNode(string(raw))
	n.Style = yaml.LiteralStyle
	for _, style := range []yaml.Style{yaml.LiteralStyle, yaml.DoubleQuotedStyle} {
		n.Style = style
		encoded, err := yaml.Marshal(n)
		if err != nil {
			continue
		}
		var decoded string
		if yaml.Unmarshal(encoded, &decoded) == nil && decoded == string(raw) {
			return n, nil
		}
	}
	return nil, fmt.Errorf("%s: the captured text cannot be inlined exactly", location)
}

func stringNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}
func resolvedNode(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}
func field(n *yaml.Node, key string) *yaml.Node {
	n = resolvedNode(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Tag == "!!merge" {
			merged := resolvedNode(n.Content[i+1])
			if merged.Kind == yaml.SequenceNode {
				for _, item := range merged.Content {
					if value := field(item, key); value != nil {
						return value
					}
				}
			} else if value := field(merged, key); value != nil {
				return value
			}
		}
	}
	return nil
}
func setField(n *yaml.Node, key string, value *yaml.Node) {
	n = resolvedNode(n)
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			previous := n.Content[i+1]
			value.HeadComment, value.LineComment, value.FootComment = previous.HeadComment, previous.LineComment, previous.FootComment
			n.Content[i+1] = value
			return
		}
	}
	n.Content = append(n.Content, stringNode(key), value)
}
func replaceField(n *yaml.Node, old, next string, value *yaml.Node) {
	n = resolvedNode(n)
	materializeMapping(n)
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == old {
			n.Content[i].Value = next
			previous := n.Content[i+1]
			value.HeadComment, value.LineComment, value.FootComment = previous.HeadComment, previous.LineComment, previous.FootComment
			n.Content[i+1] = value
			return
		}
	}
}

// Materialize inherited keys only in a mapping whose file reference is edited.
// Explicit keys win, and earlier merge-sequence entries win over later ones,
// matching yaml.v3. Leave all other captured mappings in their original form.
func materializeMapping(n *yaml.Node) {
	var inherited []*yaml.Node
	present := map[string]bool{}
	var explicit []*yaml.Node
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Tag != "!!merge" {
			explicit = append(explicit, n.Content[i], n.Content[i+1])
			present[n.Content[i].Value] = true
		}
	}
	var collect func(*yaml.Node)
	collect = func(node *yaml.Node) {
		node = resolvedNode(node)
		if node.Kind == yaml.SequenceNode {
			for _, item := range node.Content {
				collect(item)
			}
			return
		}
		copy := *node
		materializeMapping(&copy)
		for i := 0; i < len(copy.Content); i += 2 {
			if key := copy.Content[i].Value; !present[key] {
				inherited = append(inherited, copy.Content[i], copy.Content[i+1])
				present[key] = true
			}
		}
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Tag == "!!merge" {
			collect(n.Content[i+1])
		}
	}
	n.Content = append(explicit, inherited...)
}
func removeField(n *yaml.Node, key string) {
	n = resolvedNode(n)
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content = append(n.Content[:i], n.Content[i+2:]...)
			return
		}
	}
}
