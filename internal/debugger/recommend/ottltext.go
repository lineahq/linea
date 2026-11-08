package recommend

import (
	"regexp"
	"strings"
)

// ottlCall is an OTTL editor statement split into its function, its
// top-level arguments and its where clause. ok is false when the text is
// not a plain `name(args) [where cond]` statement.
type ottlCall struct {
	name  string
	args  []string
	where string
	ok    bool
}

func parseCall(text string) ottlCall {
	text = strings.TrimSpace(text)
	open := strings.IndexByte(text, '(')
	if open <= 0 || !identifier.MatchString(text[:open]) {
		return ottlCall{}
	}
	c := ottlCall{name: text[:open]}
	depth, quoted, start := 0, false, open+1
	for i := open; i < len(text); i++ {
		switch ch := text[i]; {
		case quoted && ch == '\\':
			i++
		case ch == '"':
			quoted = !quoted
		case quoted:
		case ch == '(' || ch == '[' || ch == '{':
			depth++
		case ch == ')' || ch == ']' || ch == '}':
			depth--
			if depth == 0 {
				if arg := strings.TrimSpace(text[start:i]); arg != "" || len(c.args) > 0 {
					c.args = append(c.args, arg)
				}
				rest := strings.TrimSpace(text[i+1:])
				if rest != "" {
					w, found := strings.CutPrefix(rest, "where ")
					if !found {
						return ottlCall{}
					}
					c.where = strings.TrimSpace(w)
				}
				c.ok = true
				return c
			}
		case ch == ',' && depth == 1:
			c.args = append(c.args, strings.TrimSpace(text[start:i]))
			start = i + 1
		}
	}
	return ottlCall{}
}

var (
	identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	literal    = regexp.MustCompile(`^(?:"(?:[^"\\]|\\.)*"|-?[0-9]+(?:\.[0-9]+)?|true|false|nil)$`)
	stringList = regexp.MustCompile(`^\[\s*(?:"(?:[^"\\]|\\.)*"\s*(?:,\s*"(?:[^"\\]|\\.)*"\s*)*)?\]$`)
)

// selfAssignment: `set(P, P)` writes every item's value back unchanged,
// whatever the input and the where clause.
func selfAssignment(text string) bool {
	c := parseCall(text)
	return c.ok && c.name == "set" && len(c.args) == 2 && c.args[0] == c.args[1]
}

// idempotent: running the statement a second time right after itself
// changes nothing on any input. That holds for setting a literal or the
// path's own value, and for deleting or keeping keys named by literals:
// the second run finds the state the first left, whatever its where clause
// decides.
func idempotent(text string) bool {
	c := parseCall(text)
	if !c.ok {
		return false
	}
	switch c.name {
	case "set":
		return len(c.args) == 2 && (literal.MatchString(c.args[1]) || c.args[0] == c.args[1])
	case "delete_key", "delete_matching_keys":
		return len(c.args) == 2 && literal.MatchString(c.args[1])
	case "keep_keys":
		return len(c.args) == 2 && stringList.MatchString(c.args[1])
	}
	return false
}
