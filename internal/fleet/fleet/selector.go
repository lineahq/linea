package fleet

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Selector bounds keep queries indexable and cheap to validate.
const (
	MaxSelectorBytes = 4096
	MaxSelectorTerms = 8
	MaxSelectorIn    = 32
	MaxValueBytes    = 256
	MaxKeyBytes      = 128
)

// Promoted maps the attribute keys that have their own indexed column.
// PromotedRule is the reason for a group_by or sort that is not promoted.
const PromotedRule = "must be one of service.name, service.version, host.name, k8s.namespace.name, k8s.pod.name"

var Promoted = map[string]string{
	"service.name":       "service_name",
	"service.version":    "service_version",
	"host.name":          "host_name",
	"k8s.namespace.name": "k8s_namespace",
	"k8s.pod.name":       "k8s_pod",
}

// Term is one condition: key = value, key in (values) or key exists.
type Term struct {
	Key    string
	Op     string
	Values []string
}

// Selector is an AND of terms over agent attributes. The zero value matches
// every agent.
type Selector struct{ Terms []Term }

func (s Selector) Empty() bool { return len(s.Terms) == 0 }

// ValidKey accepts attribute keys such as k8s.namespace.name.
func ValidKey(key string) bool {
	if key == "" || len(key) > MaxKeyBytes {
		return false
	}
	for _, c := range key {
		if !keyChar(c) {
			return false
		}
	}
	return true
}

func keyChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' || c == '/'
}

// ValidValue accepts bounded printable UTF-8 attribute values.
func ValidValue(v string) bool {
	if v == "" || len(v) > MaxValueBytes || !utf8.ValidString(v) {
		return false
	}
	for _, c := range v {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// Matches evaluates the selector against an attribute map.
func (s Selector) Matches(attributes map[string]string) bool {
	for _, t := range s.Terms {
		v, ok := attributes[t.Key]
		if !ok || (t.Op != "exists" && !slices.Contains(t.Values, v)) {
			return false
		}
	}
	return true
}

// String renders the canonical form accepted by ParseSelector.
func (s Selector) String() string {
	var b strings.Builder
	for i, t := range s.Terms {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(t.Key)
		switch t.Op {
		case "=":
			b.WriteByte('=')
			b.WriteString(quote(t.Values[0]))
		case "in":
			b.WriteString(" in (")
			for j, v := range t.Values {
				if j > 0 {
					b.WriteByte(',')
				}
				b.WriteString(quote(v))
			}
			b.WriteByte(')')
		}
	}
	return b.String()
}

func quote(v string) string {
	if strings.ContainsAny(v, `,()"=`) || strings.TrimSpace(v) != v {
		q, _ := json.Marshal(v)
		return string(q)
	}
	return v
}

// ParseSelector reads terms separated by commas:
//
//	key=value
//	key in (value, value)
//	key
//
// Values containing , ( ) " = or edge spaces are written as JSON strings.
func ParseSelector(text string) (Selector, error) {
	if text == "" || len(text) > MaxSelectorBytes {
		return Selector{}, Invalid(fmt.Sprintf("selector must be 1 to %d bytes", MaxSelectorBytes))
	}
	p := selectorParser{s: text}
	var s Selector
	for {
		if len(s.Terms) == MaxSelectorTerms {
			return Selector{}, Invalid(fmt.Sprintf("selector has more than %d terms", MaxSelectorTerms))
		}
		t, ok := p.term()
		if !ok {
			return Selector{}, Invalid(fmt.Sprintf("selector term %d must be key, key=value or key in (value, ...), with keys of letters, digits and . _ - / up to %d bytes and values up to %d bytes", len(s.Terms)+1, MaxKeyBytes, MaxValueBytes))
		}
		s.Terms = append(s.Terms, t)
		p.space()
		if p.i == len(p.s) {
			return s, nil
		}
		if !p.take(',') {
			return Selector{}, Invalid(fmt.Sprintf("selector term %d must be followed by a comma or the end", len(s.Terms)))
		}
	}
}

type selectorParser struct {
	s string
	i int
}

func (p *selectorParser) space() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *selectorParser) take(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *selectorParser) term() (Term, bool) {
	p.space()
	start := p.i
	for p.i < len(p.s) && keyChar(rune(p.s[p.i])) {
		p.i++
	}
	key := p.s[start:p.i]
	if !ValidKey(key) {
		return Term{}, false
	}
	p.space()
	if p.take('=') {
		v, ok := p.value()
		return Term{Key: key, Op: "=", Values: []string{v}}, ok
	}
	if strings.HasPrefix(p.s[p.i:], "in") && p.i+2 < len(p.s) && (p.s[p.i+2] == ' ' || p.s[p.i+2] == '(') {
		p.i += 2
		p.space()
		if !p.take('(') {
			return Term{}, false
		}
		t := Term{Key: key, Op: "in"}
		for {
			v, ok := p.value()
			if !ok || len(t.Values) == MaxSelectorIn {
				return Term{}, false
			}
			if !slices.Contains(t.Values, v) {
				t.Values = append(t.Values, v)
			}
			p.space()
			if p.take(')') {
				return t, true
			}
			if !p.take(',') {
				return Term{}, false
			}
		}
	}
	return Term{Key: key, Op: "exists"}, true
}

func (p *selectorParser) value() (string, bool) {
	p.space()
	if p.i < len(p.s) && p.s[p.i] == '"' {
		end := p.i + 1
		for end < len(p.s) && p.s[end] != '"' {
			if p.s[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(p.s) {
			return "", false
		}
		var v string
		if json.Unmarshal([]byte(p.s[p.i:end+1]), &v) != nil {
			return "", false
		}
		p.i = end + 1
		return v, ValidValue(v)
	}
	start := p.i
	for p.i < len(p.s) && !strings.ContainsRune(`,()"=`, rune(p.s[p.i])) {
		p.i++
	}
	v := strings.TrimRight(p.s[start:p.i], " ")
	return v, ValidValue(v)
}

// AgentQuery selects one inventory page. After is an agent ID for the default
// order, or an opaque position when Sort names a promoted attribute.
type AgentQuery struct {
	After     string
	Limit     int
	Connected *bool
	Healthy   *bool
	Selector  Selector
	// Group restricts the page to the explicit members of a static group.
	Group string
	// Credential restricts the page to agents connected with one credential.
	Credential string
	Sort       string
}

// errAfter refuses a keyset position this server did not issue.
var errAfter = Invalid("after must be a position from a previous page")

// EncodeAfter and DecodeAfter carry a (sort value, id) keyset position.
func EncodeAfter(value, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Quote(value) + id))
}

func DecodeAfter(after string) (string, string, error) {
	if len(after) > 1024 {
		return "", "", errAfter
	}
	b, err := base64.RawURLEncoding.DecodeString(after)
	if err != nil || len(b) < 34 {
		return "", "", errAfter
	}
	id := string(b[len(b)-32:])
	value, err := strconv.Unquote(string(b[:len(b)-32]))
	if err != nil || !ValidID(id) || len(value) > MaxValueBytes {
		return "", "", errAfter
	}
	return value, id, nil
}
