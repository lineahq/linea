// Package statementedit splices transform statements without serializing YAML.
// A decoded comparison verifies every splice before it is returned.
package statementedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

type Op string

const (
	Replace Op = "replace"
	Insert  Op = "insert"
	Append  Op = "append"
	Delete  Op = "delete"
)

// Edit addresses one statement. Its JSON form is persisted as a candidate's
// revision origin, so the field names are part of the workspace format.
type Edit struct {
	Processor string `json:"processor"`
	Signal    string `json:"signal"`
	Group     int    `json:"group"`
	Index     int    `json:"index"`
	Op        Op     `json:"op"`
	Text      string `json:"text,omitempty"`
	Expected  string `json:"expected,omitempty"`
}

type Location struct {
	Value      string
	Start, End int
	Style      yaml.Style
}

var (
	ErrInvalid     = errors.New("invalid statement edit")
	ErrUnsupported = errors.New("unsupported statement shape; provide complete YAML")
)

type StaleError struct{ Current string }

func (e *StaleError) Error() string { return "the statement has changed since it was read" }

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

func document(text []byte) (*yaml.Node, error) {
	d := yaml.NewDecoder(bytes.NewReader(text))
	var n yaml.Node
	if err := d.Decode(&n); err != nil {
		return nil, invalid("the configuration has YAML errors")
	}
	if d.Decode(new(yaml.Node)) != io.EOF || len(n.Content) != 1 {
		return nil, invalid("one YAML document is required")
	}
	return n.Content[0], nil
}

func field(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func special(n *yaml.Node) bool {
	return n != nil && (n.Anchor != "" || n.Kind == yaml.AliasNode || n.Style&yaml.TaggedStyle != 0)
}

// statements resolves both upstream shapes, retaining the container for verify.
func statements(root *yaml.Node, e Edit) (*yaml.Node, error) {
	if e.Processor != "transform" && !strings.HasPrefix(e.Processor, "transform/") {
		return nil, invalid("processor must be a transform processor")
	}
	name := map[string]string{"traces": "trace_statements", "logs": "log_statements", "metrics": "metric_statements"}[e.Signal]
	if name == "" {
		return nil, invalid("unknown signal")
	}
	p := field(field(root, "processors"), e.Processor)
	list := field(p, name)
	if special(p) || special(list) {
		return nil, ErrUnsupported
	}
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil, invalid("processor has no " + name + " list")
	}
	if e.Group < 0 {
		return nil, invalid("unknown group")
	}
	if len(list.Content) > 0 && (list.Content[0].Kind == yaml.ScalarNode || list.Content[0].Kind == yaml.AliasNode) {
		if e.Group != 0 {
			return nil, invalid("statements are flat; only group 0 exists")
		}
		return list, nil
	}
	if e.Group >= len(list.Content) {
		return nil, invalid("unknown group")
	}
	group := list.Content[e.Group]
	inner := field(group, "statements")
	if special(group) || special(inner) {
		return nil, ErrUnsupported
	}
	if inner == nil || inner.Kind != yaml.SequenceNode {
		return nil, invalid("group has no statements list")
	}
	return inner, nil
}

// yaml.v3 columns count Unicode characters. Walk UTF-8 runes within the
// indicated line rather than adding the column to a byte offset. LF locates
// lines, preserving the CR byte of a CRLF line unchanged.
func position(text []byte, line, column int) (int, error) {
	i := 0
	for l := 1; l < line; l++ {
		j := bytes.IndexByte(text[i:], '\n')
		if j < 0 {
			return 0, ErrUnsupported
		}
		i += j + 1
	}
	for c := 1; c < column; c++ {
		if i >= len(text) || text[i] == '\n' {
			return 0, ErrUnsupported
		}
		_, size := utf8.DecodeRune(text[i:])
		i += size
	}
	return i, nil
}

func extent(text []byte, n *yaml.Node, flow bool) (Location, error) {
	if special(n) || n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return Location{}, ErrUnsupported
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return Location{}, invalid("statement is not a string")
	}
	start, err := position(text, n.Line, n.Column)
	if err != nil || start >= len(text) {
		return Location{}, ErrUnsupported
	}
	end := start
	if n.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle) != 0 {
		quote := text[start]
		end++
		closed := false
		for end < len(text) {
			ch := text[end]
			if ch == '\n' || ch == '\r' {
				return Location{}, ErrUnsupported
			}
			end++
			if quote == '"' && ch == '\\' {
				if end >= len(text) || text[end] == '\n' || text[end] == '\r' {
					return Location{}, ErrUnsupported
				}
				end++
			} else if ch == quote {
				if quote == '\'' && end < len(text) && text[end] == '\'' {
					end++
				} else {
					closed = true
					break
				}
			}
		}
		if !closed {
			return Location{}, ErrUnsupported
		}
	} else {
		for end < len(text) && text[end] != '\n' && text[end] != '\r' {
			ch := text[end]
			if ch == '#' && (end == start || text[end-1] == ' ' || text[end-1] == '\t') || flow && strings.ContainsRune(",]}", rune(ch)) {
				break
			}
			end++
		}
		for end > start && (text[end-1] == ' ' || text[end-1] == '\t') {
			end--
		}
	}
	// Also catches a plain scalar continued onto later lines.
	var value string
	if yaml.Unmarshal(text[start:end], &value) != nil || value != n.Value {
		return Location{}, ErrUnsupported
	}
	return Location{Value: n.Value, Start: start, End: end, Style: n.Style}, nil
}

func Locate(text []byte, processor, signal string, group, index int) (Location, error) {
	root, err := document(text)
	if err != nil {
		return Location{}, err
	}
	list, err := statements(root, Edit{Processor: processor, Signal: signal, Group: group})
	if err != nil {
		return Location{}, err
	}
	if index < 0 || index >= len(list.Content) {
		return Location{}, invalid("unknown index")
	}
	return extent(text, list.Content[index], list.Style&yaml.FlowStyle != 0)
}

func quoted(value string, style yaml.Style) []string {
	single := "'" + strings.ReplaceAll(value, "'", "''") + "'"
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	double := strings.TrimSuffix(buf.String(), "\n")
	// JSON.stringify leaves these valid JSON characters literal too.
	var js strings.Builder
	for i := 0; i < len(double); i++ {
		if double[i] == '\\' && i+1 < len(double) {
			if strings.HasPrefix(double[i:], `\u2028`) {
				js.WriteRune('\u2028')
				i += 5
				continue
			}
			if strings.HasPrefix(double[i:], `\u2029`) {
				js.WriteRune('\u2029')
				i += 5
				continue
			}
			js.WriteByte(double[i])
			i++ // Consume escaped backslashes before looking for Unicode escapes.
		}
		js.WriteByte(double[i])
	}
	double = js.String()
	switch {
	case style&yaml.DoubleQuotedStyle != 0:
		return []string{double, single}
	case style&yaml.SingleQuotedStyle != 0:
		if strings.ContainsAny(value, "\r\n") {
			return []string{double}
		}
		return []string{single, double}
	default:
		return []string{value, single, double}
	}
}

// itemLine permits only a standalone block item, and retains its exact dash
// layout and line ending. Comments stay on replacement and leave on deletion.
func itemLine(text []byte, loc Location) (start, end int, prefix, newline string, err error) {
	start = bytes.LastIndexByte(text[:loc.Start], '\n') + 1
	end = loc.End
	for end < len(text) && text[end] != '\n' {
		end++
	}
	if end < len(text) {
		end++
		newline = "\n"
		if end > 1 && text[end-2] == '\r' {
			newline = "\r\n"
		}
	}
	prefix = string(text[start:loc.Start])
	dash := strings.TrimLeft(prefix, " ")
	if !strings.HasPrefix(dash, "- ") || strings.Trim(dash[1:], " \t") != "" {
		err = ErrUnsupported
	}
	tail := strings.TrimSpace(string(text[loc.End:end]))
	if tail != "" && !strings.HasPrefix(tail, "#") {
		err = ErrUnsupported
	}
	return
}

func Apply(text []byte, e Edit) ([]byte, error) {
	if (e.Op == Replace || e.Op == Delete) && e.Expected == "" {
		return nil, invalid("Expected is required")
	}
	if e.Op != Delete && e.Text == "" {
		return nil, invalid("Text is required")
	}
	root, err := document(text)
	if err != nil {
		return nil, err
	}
	list, err := statements(root, e)
	if err != nil {
		return nil, err
	}
	n := len(list.Content)
	at := e.Index
	switch e.Op {
	case Replace, Delete:
		if at < 0 || at >= n {
			return nil, invalid("unknown index")
		}
	case Insert:
		if at < 0 || at > n {
			return nil, invalid("unknown index")
		}
	case Append:
		if at != 0 {
			return nil, invalid("append Index must be 0")
		}
		at = n
	default:
		return nil, invalid("unknown operation")
	}
	flow := list.Style&yaml.FlowStyle != 0
	if flow && e.Op != Replace || n == 0 {
		return nil, ErrUnsupported
	}
	loc, err := extent(text, list.Content[min(at, n-1)], flow)
	if err != nil {
		return nil, err
	}
	if (e.Op == Replace || e.Op == Delete) && e.Expected != loc.Value {
		return nil, &StaleError{Current: loc.Value}
	}
	// Apply the operation to the decoded base, independently of the splice.
	var want any
	if root.Decode(&want) != nil {
		return nil, invalid("the configuration has YAML errors")
	}
	name := map[string]string{"traces": "trace_statements", "logs": "log_statements", "metrics": "metric_statements"}[e.Signal]
	lookup := func(v any, key string) any {
		switch m := v.(type) {
		case map[string]any:
			return m[key]
		case map[any]any:
			return m[key]
		}
		return nil
	}
	parent := lookup(lookup(want, "processors"), e.Processor)
	key := name
	items, ok := lookup(parent, key).([]any)
	if !ok {
		return nil, ErrUnsupported
	}
	if len(items) > 0 {
		if _, flat := items[0].(string); !flat {
			parent, key = items[e.Group], "statements"
			items, ok = lookup(parent, key).([]any)
			if !ok {
				return nil, ErrUnsupported
			}
		}
	}
	switch e.Op {
	case Replace:
		items[at] = e.Text
	case Delete:
		items = append(items[:at], items[at+1:]...)
	case Insert, Append:
		items = append(items, nil)
		copy(items[at+1:], items[at:])
		items[at] = e.Text
	}
	switch m := parent.(type) {
	case map[string]any:
		m[key] = items
	case map[any]any:
		m[key] = items
	default:
		return nil, ErrUnsupported
	}
	start, end := loc.Start, loc.End
	prefix, newline, lead := "", "", ""
	if e.Op != Replace {
		lineStart, lineEnd, layout, ending, err := itemLine(text, loc)
		if err != nil {
			return nil, err
		}
		prefix, newline = layout, ending
		if newline == "" {
			newline = "\n"
			if bytes.Contains(text, []byte("\r\n")) {
				newline = "\r\n"
			}
		}
		switch {
		case e.Op == Delete:
			start, end = lineStart, lineEnd
		case at == n:
			start, end = lineEnd, lineEnd
			if ending == "" {
				lead, newline = newline, ""
			}
		default:
			start, end = lineStart, lineStart
		}
	}
	candidates := quoted(e.Text, loc.Style)
	if e.Op == Delete {
		candidates = []string{""}
	}
	for _, scalar := range candidates {
		if e.Op != Replace && e.Op != Delete {
			scalar = lead + prefix + scalar + newline
		}
		result := make([]byte, 0, len(text)+len(scalar))
		result = append(result, text[:start]...)
		result = append(result, scalar...)
		result = append(result, text[end:]...)
		gotRoot, err := document(result)
		var got any
		if err == nil && gotRoot.Decode(&got) == nil && reflect.DeepEqual(got, want) {
			return result, nil
		}
	}
	return nil, ErrUnsupported
}
