package comparison

import (
	"fmt"
	"strconv"
	"strings"
)

// rule is one ignore rule: an OTTL path, matched exactly or as a
// prefix ending at a "[" or "." boundary, so a rule on a map key covers its
// nested keys.
type rule struct {
	text  string
	path  string
	count int
}

// contexts maps a rule's OTTL context to the context paths are named with.
var contexts = map[string]string{
	"resource":              "resource",
	"scope":                 "instrumentation_scope",
	"instrumentation_scope": "instrumentation_scope",
	"span":                  "span",
	"log":                   "log",
	"metric":                "metric",
	"datapoint":             "datapoint",
}

func parseRules(texts []string) ([]*rule, error) {
	var out []*rule
	for _, t := range texts {
		path, err := parsePath(t)
		if err != nil {
			return nil, fmt.Errorf("%w %q: %v", ErrInvalidRule, t, err)
		}
		out = append(out, &rule{text: t, path: path})
	}
	return out, nil
}

// parsePath checks a rule's grammar, context "." field followed by ".field"
// or `["key"]` segments, and returns it in the spelling compared paths use.
func parsePath(t string) (string, error) {
	ctx, rest, ok := strings.Cut(t, ".")
	if !ok {
		return "", fmt.Errorf("want <context>.<path>, for example log.attributes[\"request.id\"]")
	}
	name, ok := contexts[ctx]
	if !ok {
		return "", fmt.Errorf("unknown context %q (resource, scope, span, log, metric, datapoint)", ctx)
	}
	var b strings.Builder
	b.WriteString(name)
	field := true // A field must follow the context.
	for rest != "" {
		switch {
		case field || rest[0] == '.':
			if !field {
				rest = rest[1:]
			}
			n := 0
			for n < len(rest) && (rest[n] == '_' || rest[n] >= 'a' && rest[n] <= 'z' || rest[n] >= '0' && rest[n] <= '9') {
				n++
			}
			if n == 0 {
				return "", fmt.Errorf("want a field name at %q", rest)
			}
			b.WriteString("." + rest[:n])
			rest, field = rest[n:], false
		case rest[0] == '[':
			quoted, err := strconv.QuotedPrefix(rest[1:])
			if err != nil || quoted[0] != '"' || !strings.HasPrefix(rest[1+len(quoted):], "]") {
				return "", fmt.Errorf("want [\"key\"] at %q", rest)
			}
			key, _ := strconv.Unquote(quoted)
			b.WriteString("[" + strconv.Quote(key) + "]")
			rest = rest[len(quoted)+2:]
		default:
			return "", fmt.Errorf("unexpected %q", rest)
		}
	}
	if field {
		return "", fmt.Errorf("want a field after %q", ctx)
	}
	return b.String(), nil
}

// matchRules returns the first rule matching an OTTL path, or nil.
func matchRules(rules []*rule, path string) *rule {
	for _, r := range rules {
		if path == r.path || strings.HasPrefix(path, r.path) && (path[len(r.path)] == '[' || path[len(r.path)] == '.') {
			return r
		}
	}
	return nil
}
