package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/lineahq/linea/internal/debugger/recommend"
)

// ErrNotVerifiable rejects a recommendation whose change cannot be expressed
// as provider overrides.
var ErrNotVerifiable = errors.New("recommendation cannot be verified")

func notVerifiable(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrNotVerifiable}, args...)...)
}

// Verifiable reports whether a recommendation's change can be expressed as
// provider overrides, from the recommendation alone: a swap, a removed
// transform statement or filter condition, or an option with a value.
// CandidateOverrides may still refuse it against the run's configuration.
func Verifiable(rec recommend.Recommendation) error {
	c := rec.Change
	if c == nil {
		return notVerifiable("%s suggests no change", rec.Rule)
	}
	switch c.Op {
	case recommend.OpSwap:
		if rec.Pipeline == "" || len(c.Processors) == 0 {
			return notVerifiable("the swap names no pipeline or processors")
		}
		// confmap keys are separated by "::"; IDs never contain it.
		if strings.Contains(rec.Pipeline, "::") {
			return notVerifiable("pipeline %q", rec.Pipeline)
		}
		for _, id := range c.Processors {
			if id == "" || strings.Contains(id, "::") {
				return notVerifiable("processor %q", id)
			}
		}
	case recommend.OpRemoveStatement:
		if err := checkComponentID(c.Component); err != nil {
			return err
		}
		at, err := itemPath(c.Path)
		if err != nil {
			return err
		}
		if t := componentType(c.Component); t != "transform" && t != "filter" || at.statement != (t == "transform") {
			return notVerifiable("%s has no %s to remove", c.Component, c.Path)
		}
		if c.Text == "" {
			return notVerifiable("the statement has no text")
		}
	case recommend.OpSetOption:
		if err := checkComponentID(c.Component); err != nil {
			return err
		}
		if !optionKey.MatchString(c.Option) {
			return notVerifiable("option %q", c.Option)
		}
		if c.Value == "" {
			return notVerifiable("the recommendation leaves the value of %s to you", c.Option)
		}
		if _, err := optionValue(c.Value); err != nil {
			return err
		}
	default:
		return notVerifiable("a %s change is not verified", c.Op)
	}
	return nil
}

// CandidateOverrides are the provider overrides that apply a recommendation's
// change on top of the user's unchanged configuration, read from the run's
// bundle: yaml: URIs layered after its root. A list value replaces the list,
// so a swap is one override of the pipeline's processors, and a removed
// statement or condition replaces the whole list that holds it, rebuilt from
// the root configuration's text: references in it stay references and
// resolve from the captured values as in the run. An option is one override
// of its key.
func CandidateOverrides(rec recommend.Recommendation, raw []byte) ([]string, error) {
	if err := Verifiable(rec); err != nil {
		return nil, err
	}
	c := rec.Change
	switch c.Op {
	case recommend.OpSwap:
		return []string{"yaml:service::pipelines::" + rec.Pipeline + "::processors: " + jsonValue(c.Processors)}, nil
	case recommend.OpSetOption:
		v, _ := optionValue(c.Value)
		return []string{"yaml:processors::" + c.Component + "::" + c.Option + ": " + jsonValue(v)}, nil
	}
	_, snapshot, err := readBundle(raw)
	if err != nil {
		return nil, err
	}
	// An earlier override of this processor would hide the list the
	// recommendation was made on.
	for _, o := range snapshot.Overrides() {
		if strings.HasPrefix(o, "yaml:processors::"+c.Component+":") || !strings.HasPrefix(o, "yaml:processors::") && strings.HasPrefix(o, "yaml:processors:") {
			return nil, notVerifiable("the run already overrides %s", c.Component)
		}
	}
	root, err := yamlDocument(snapshot.SourceBytes())
	if err != nil {
		return nil, err
	}
	p := mapField(mapField(root, "processors"), c.Component)
	if p == nil || special(p) {
		return nil, notVerifiable("%s is not written out in the root configuration", c.Component)
	}
	key, list, err := removeItem(p, rec)
	if err != nil {
		return nil, err
	}
	var value any
	if err := list.Decode(&value); err != nil {
		return nil, notVerifiable("the list holding the statement cannot be read")
	}
	if value == nil {
		value = []any{}
	}
	return []string{"yaml:processors::" + c.Component + "::" + key + ": " + jsonValue(value)}, nil
}

// removeItem removes the recommendation's statement or condition from a copy
// of the processor's list and returns the list's confmap key and the copy:
// the outermost list, since confmap replaces a list as a whole. A group the
// removal empties goes with it.
func removeItem(p *yaml.Node, rec recommend.Recommendation) (string, *yaml.Node, error) {
	c := rec.Change
	at, _ := itemPath(c.Path)
	key, group, index, grouped := at.key, at.group, at.index, at.grouped
	inner := "conditions"
	top := p
	for _, k := range strings.Split(key, "::") {
		if special(top) {
			break
		}
		top = mapField(top, k)
	}
	if at.statement {
		// A flat statement list is group 0.
		inner = "statements"
		grouped = top != nil && top.Kind == yaml.SequenceNode && len(top.Content) > 0 && top.Content[0].Kind == yaml.MappingNode
		if !grouped && group != 0 {
			return "", nil, notVerifiable("%s holds no group %d", key, group)
		}
	}
	if top == nil || special(top) || top.Kind != yaml.SequenceNode {
		return "", nil, notVerifiable("%s of %s is not a list written out in the root configuration", key, c.Component)
	}
	top = cloneNode(top)
	list := top
	if grouped {
		if group < 0 || group >= len(top.Content) {
			return "", nil, notVerifiable("%s holds no group %d", key, group)
		}
		list = mapField(top.Content[group], inner)
		if special(top.Content[group]) || list == nil || special(list) || list.Kind != yaml.SequenceNode {
			return "", nil, notVerifiable("group %d of %s has no %s list", group, key, inner)
		}
	}
	if index < 0 || index >= len(list.Content) {
		return "", nil, notVerifiable("%s holds no statement %d", key, index)
	}
	item := list.Content[index]
	if item.Kind != yaml.ScalarNode || item.Value != c.Text {
		return "", nil, notVerifiable("the configuration's text differs from the statement the run evaluated (it may hold a reference); remove it by hand")
	}
	list.Content = slices.Delete(list.Content, index, index+1)
	if grouped && len(list.Content) == 0 {
		top.Content = slices.Delete(top.Content, group, group+1)
	}
	return key, top, nil
}

// itemAt locates a transform statement or filter condition: its list's
// confmap key, and the group and index in it.
type itemAt struct {
	key          string
	group, index int
	grouped      bool
	statement    bool
}

var (
	groupedItem     = regexp.MustCompile(`^([a-z]+)_(statements|conditions)\[(\d+)\]\.(statements|conditions)\[(\d+)\]$`)
	legacyCondition = regexp.MustCompile(`^(logs|traces|metrics)\.([a-z_]+)\[(\d+)\]$`)
	optionKey       = regexp.MustCompile(`^[a-z0-9_]+(::[a-z0-9_]+)*$`)
	componentID     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]*$`)
)

// itemPath parses a change's Path: a transform statement's
// "log_statements[g].statements[i]", a filter condition's
// "log_conditions[g].conditions[i]" or deprecated "logs.log_record[i]".
func itemPath(path string) (itemAt, error) {
	if m := groupedItem.FindStringSubmatch(path); m != nil && m[2] == m[4] {
		g, err1 := strconv.Atoi(m[3])
		i, err2 := strconv.Atoi(m[5])
		if err1 == nil && err2 == nil {
			return itemAt{key: m[1] + "_" + m[2], group: g, index: i, grouped: true, statement: m[2] == "statements"}, nil
		}
	}
	if m := legacyCondition.FindStringSubmatch(path); m != nil {
		if i, err := strconv.Atoi(m[3]); err == nil {
			return itemAt{key: m[1] + "::" + m[2], index: i}, nil
		}
	}
	return itemAt{}, notVerifiable("the location %q is not known", path)
}

// checkComponentID accepts a processor ID that is a plain YAML key without
// the confmap separator.
func checkComponentID(id string) error {
	if !componentID.MatchString(id) || strings.Contains(id, "::") {
		return notVerifiable("component %q", id)
	}
	return nil
}

func componentType(id string) string {
	t, _, _ := strings.Cut(id, "/")
	return t
}

// optionValue reads a recommended option value as YAML.
func optionValue(v string) (any, error) {
	var out any
	if err := yaml.Unmarshal([]byte(v), &out); err != nil || out == nil {
		return nil, notVerifiable("the recommended value is not one YAML value")
	}
	return out, nil
}

// jsonValue writes a value as JSON, which is YAML whatever the strings hold.
func jsonValue(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func yamlDocument(text []byte) (*yaml.Node, error) {
	d := yaml.NewDecoder(bytes.NewReader(text))
	var n yaml.Node
	if err := d.Decode(&n); err != nil || len(n.Content) != 1 {
		return nil, notVerifiable("the root configuration is not one YAML document")
	}
	if d.Decode(new(yaml.Node)) != io.EOF {
		return nil, notVerifiable("the root configuration is not one YAML document")
	}
	return n.Content[0], nil
}

func mapField(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// special nodes (anchors, aliases, tags, merge keys) are not rewritten.
func special(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	if n.Anchor != "" || n.Kind == yaml.AliasNode || n.Style&yaml.TaggedStyle != 0 {
		return true
	}
	return n.Kind == yaml.MappingNode && mapField(n, "<<") != nil
}

func cloneNode(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = cloneNode(child)
	}
	return &c
}

// CandidateRequest prepares a new execution from a run's captured bundle with
// the given provider overrides layered on its configuration snapshot: the
// same inputs, setup, limits and runtime, read from the bundle and never from
// the host. The user's configuration is unchanged; the candidate's snapshot
// records the overrides (snapshot schema 4). A bundle for an out-of-process
// runtime keeps its target, so the candidate runs on the same Collector
// build; the caller binds it again. The returned request has no run ID.
func CandidateRequest(ctx context.Context, raw []byte, overrides []string) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if len(overrides) == 0 {
		return Request{}, fmt.Errorf("%w: a candidate needs overrides", ErrInvalidRequest)
	}
	a, snapshot, err := readBundle(raw)
	if err != nil {
		return Request{}, err
	}
	if snapshot, err = snapshot.WithOverrides(overrides); err != nil {
		return Request{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	runtime := ""
	if a.Runtime != nil {
		runtime = a.Runtime.ID
	} else if err := a.Distribution.compatible(); err != nil {
		return Request{}, err
	}
	return decodedRequest(ctx, a, snapshot, runtime)
}
