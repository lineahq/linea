package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// The text output is plain text that a person skims and a language model
// reads: a verdict line first, then sections under upper-case headings,
// "key: value" lines for single facts and aligned columns for lists. It
// never wraps, truncates or draws boxes. Color only marks what the words
// already say, and only on a terminal without NO_COLOR, so piped output and
// files are the same plain text.

// textOut writes one command's text output and keeps the first write error.
type textOut struct {
	w       io.Writer
	color   bool
	wrote   bool
	errs    []error
	section string
}

func newTextOut(w io.Writer) *textOut {
	return &textOut{w: w, color: colorOutput(w)}
}

// colorOutput reports whether w is a terminal that should get color.
func colorOutput(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" || !terminal(w) {
		return false
	}
	return enableColor(w)
}

// printf writes formatted text as it is.
func (t *textOut) printf(format string, a ...any) {
	if format == "" {
		return
	}
	t.wrote = true
	_, err := fmt.Fprintf(t.w, format, a...)
	t.errs = append(t.errs, err)
}

// line writes one line indented by depth levels of two spaces.
func (t *textOut) line(depth int, format string, a ...any) {
	t.printf(strings.Repeat("  ", depth)+format+"\n", a...)
}

// field writes a "key: value" line, skipping an empty value.
func (t *textOut) field(depth int, key, value string) {
	if value != "" {
		t.line(depth, "%s: %s", key, value)
	}
}

// heading starts a section: a blank line, then the title in capitals.
func (t *textOut) heading(title string) {
	t.section = title
	if t.wrote {
		t.printf("\n")
	}
	t.printf("%s\n", t.style(ansiBold, strings.ToUpper(title)))
}

// status returns an outcome word in capitals, padded to width and colored
// by what it means.
func (t *textOut) status(word string, width int) string {
	word = strings.ToUpper(word)
	pad := strings.Repeat(" ", max(0, width-len(word)))
	switch word {
	case "PASS", "VALID", "COMPLETED", "EQUAL", "OK", "SAME", "COMPATIBLE", "FOUND", "ACTIVE", "ALLOWED", "SUCCEEDED", "HEALTHY":
		return t.style(ansiGreen, word) + pad
	case "FAIL", "FAILED", "ERROR", "INVALID", "DIFFERENT", "CANCELLED", "CONFIGURATION_BREAKS", "BEHAVIOR_CHANGES", "DENIED", "REVOKED", "UNHEALTHY":
		return t.style(ansiRed, word) + pad
	case "INCONCLUSIVE", "PARTIAL", "WARNING", "INCOMPLETE", "NO_REGRESSION", "ALREADY_BAD", "EXPIRED":
		return t.style(ansiYellow, word) + pad
	}
	return word + pad
}

// none writes the line a list prints when it is empty, such as "no runs".
func (t *textOut) none(what string) {
	t.line(0, "no %s", what)
}

// nextPage writes how to read the page after this one, when there is one.
func (t *textOut) nextPage(cursor string) {
	if cursor == "" {
		return
	}
	t.printf("\n")
	t.line(0, "next page: --cursor %s", cursor)
}

// table writes rows in aligned columns two spaces apart, indented by depth
// levels; a header row, when given, comes first in capitals. Cells may hold
// status words from t.status.
func (t *textOut) table(depth int, header []string, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	all := rows
	if header != nil {
		upper := make([]string, len(header))
		for i, h := range header {
			upper[i] = strings.ToUpper(h)
		}
		all = append([][]string{upper}, rows...)
	}
	// Columns are measured by visible width, so a colored status word lines
	// up with plain ones in any column.
	var widths []int
	for _, r := range all {
		// A row's last cell is never padded, so it sets no column width.
		for i, c := range r[:max(0, len(r)-1)] {
			if i == len(widths) {
				widths = append(widths, 0)
			}
			widths[i] = max(widths[i], visibleWidth(c))
		}
	}
	indent := strings.Repeat("  ", depth)
	var b strings.Builder
	for _, r := range all {
		b.WriteString(indent)
		for i, c := range r {
			b.WriteString(c)
			if i < len(r)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-visibleWidth(c)+2))
			}
		}
		// The last column is never padded; trailing spaces carry nothing.
		b.WriteString("\n")
	}
	t.printf("%s", strings.ReplaceAll(b.String(), "%", "%%"))
}

// visibleWidth is the number of characters s shows, without color codes.
func visibleWidth(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			if j := strings.IndexByte(s[i:], 'm'); j >= 0 {
				i += j + 1
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		n++
		i += size
	}
	return n
}

// err returns the first write error.
func (t *textOut) err() error { return errors.Join(t.errs...) }

const (
	ansiBold   = "1"
	ansiRed    = "31"
	ansiGreen  = "32"
	ansiYellow = "33"
)

func (t *textOut) style(code, s string) string {
	if !t.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// statusWidth is the width of the widest outcome word, to align a list.
func statusWidth[T any](items []T, word func(T) string) int {
	w := 0
	for _, it := range items {
		w = max(w, len(word(it)))
	}
	return w
}
