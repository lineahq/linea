// Package samples embeds the sample configurations, inputs and scenarios
// that the web client offers to try the debugger without files of one's
// own. Each sample directory holds config.yaml, input.json and
// scenario.yaml; index.yaml lists them in display order.
package samples

import (
	"bytes"
	"embed"
	"fmt"
	"path"

	"go.yaml.in/yaml/v3"
)

//go:embed index.yaml */config.yaml */input.json */scenario.yaml
var files embed.FS

// Sample is one ready-to-run example.
type Sample struct {
	ID          string `yaml:"id" json:"id"`
	Title       string `yaml:"title" json:"title"`
	Description string `yaml:"description" json:"description"`
	// Receiver is the receiver InputJSON is injected at in a quick run.
	Receiver string `yaml:"receiver" json:"receiver"`
	// Expect is the outcome of the scenario's assertions as shipped: pass,
	// or fail for a sample with a bug to find.
	Expect     string `yaml:"expect" json:"expect"`
	ConfigYAML string `yaml:"-" json:"config_yaml"`
	// ScenarioYAML is the scenario with its input files inlined as data,
	// because a scenario held by the web client has no directory.
	ScenarioYAML string `yaml:"-" json:"scenario_yaml"`
	// InputJSON is the sample's OTLP JSON input, for a run without a
	// scenario.
	InputJSON string `yaml:"-" json:"input_json"`
}

// List returns the samples in index order.
func List() ([]Sample, error) {
	raw, err := files.ReadFile("index.yaml")
	if err != nil {
		return nil, err
	}
	var out []Sample
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("samples index: %w", err)
	}
	for i := range out {
		s := &out[i]
		cfg, err := files.ReadFile(path.Join(s.ID, "config.yaml"))
		if err != nil {
			return nil, err
		}
		input, err := files.ReadFile(path.Join(s.ID, "input.json"))
		if err != nil {
			return nil, err
		}
		scenario, err := files.ReadFile(path.Join(s.ID, "scenario.yaml"))
		if err != nil {
			return nil, err
		}
		inlined, err := inlineInputs(scenario, s.ID)
		if err != nil {
			return nil, fmt.Errorf("sample %s: %w", s.ID, err)
		}
		s.ConfigYAML, s.InputJSON, s.ScenarioYAML = string(cfg), string(input), inlined
	}
	return out, nil
}

// FileNames are the files of every sample, in the order Files returns them.
var FileNames = []string{"config.yaml", "input.json", "scenario.yaml"}

// Files returns one sample and its files as shipped, in FileNames order, so
// they can be written out and run from disk: the scenario reads input.json
// beside it. ok is false when no sample has the ID.
func Files(id string) (s Sample, contents [][]byte, ok bool, err error) {
	list, err := List()
	if err != nil {
		return Sample{}, nil, false, err
	}
	for _, s := range list {
		if s.ID != id {
			continue
		}
		for _, name := range FileNames {
			raw, err := files.ReadFile(path.Join(id, name))
			if err != nil {
				return Sample{}, nil, false, err
			}
			contents = append(contents, raw)
		}
		return s, contents, true, nil
	}
	return Sample{}, nil, false, nil
}

// inlineInputs replaces each input's file with its content as a literal
// block scalar under data. Comments and the style of every other node are
// kept by editing the YAML node tree.
func inlineInputs(scenario []byte, dir string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(scenario, &doc); err != nil {
		return "", err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("scenario is not a mapping")
	}
	// A scenario may read only mocked dependencies (mocks.files) and have no
	// inputs. Mocks keep their fixtures inline: the web client cannot read
	// files next to a sample.
	inputs := value(doc.Content[0], "inputs")
	var items []*yaml.Node
	if inputs != nil {
		if inputs.Kind != yaml.SequenceNode {
			return "", fmt.Errorf("scenario inputs are not a list")
		}
		items = inputs.Content
	}
	for _, in := range items {
		if in.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(in.Content); i += 2 {
			k, v := in.Content[i], in.Content[i+1]
			if k.Value != "file" {
				continue
			}
			data, err := files.ReadFile(path.Join(dir, v.Value))
			if err != nil {
				return "", err
			}
			k.Value = "data"
			*v = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.LiteralStyle, Value: string(data)}
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func value(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
