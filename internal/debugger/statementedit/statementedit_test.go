package statementedit

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const advanced = `processors:
  # keep this comment
  transform/logs:
    log_statements:
      - context: log
        statements:
          - set(log.severity_text, "WARN") where log.severity_number == 13
          - 'set(log.attributes["a"], "x")'
      - context: resource
        statements: ["set(resource.attributes[\"b\"], 1)"]
`

const basic = `processors:
  transform:
    trace_statements:
      - set(span.name, "a")
      - set(span.name, "b")
`

func TestLocateBothStyles(t *testing.T) {
	for _, tc := range []struct {
		text, processor, signal, value string
		group, index                   int
		style                          yaml.Style
	}{
		{advanced, "transform/logs", "logs", `set(log.attributes["a"], "x")`, 0, 1, yaml.SingleQuotedStyle},
		{basic, "transform", "traces", `set(span.name, "b")`, 0, 1, 0},
	} {
		loc, err := Locate([]byte(tc.text), tc.processor, tc.signal, tc.group, tc.index)
		if err != nil || loc.Value != tc.value || loc.Style != tc.style {
			t.Fatalf("locate = %+v, %v", loc, err)
		}
	}
	if _, err := Locate([]byte(basic), "transform", "traces", 1, 0); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "flat") {
		t.Fatal(err)
	}
	if _, err := Locate([]byte(advanced), "transform/logs", "metrics", 0, 0); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no metric_statements") {
		t.Fatal(err)
	}
}

// assertSplice proves the exact unchanged prefix/suffix around a known span,
// the exact replacement bytes, and that the complete result re-parses.
func assertSplice(t *testing.T, text []byte, e Edit, start, end int, replacement string) []byte {
	t.Helper()
	got, err := Apply(text, e)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte(nil), text[:start]...), []byte(replacement)...), text[end:]...)
	if !bytes.Equal(got, want) {
		t.Fatalf("splice:\n%s\nwant:\n%s", got, want)
	}
	if !bytes.Equal(got[:start], text[:start]) || !bytes.Equal(got[start+len(replacement):], text[end:]) {
		t.Fatal("changed outside splice")
	}
	var parsed any
	if err := yaml.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReplacePortedWebCases(t *testing.T) {
	for _, tc := range []struct {
		name, text, processor, signal string
		group, index                  int
		value, written                string
	}{
		{"plain stays plain", advanced, "transform/logs", "logs", 0, 0, `set(log.severity_text, "WARN") where log.severity_number >= 13`, `set(log.severity_text, "WARN") where log.severity_number >= 13`},
		{"single quotes kept", advanced, "transform/logs", "logs", 0, 1, `set(log.attributes["a"], "it's")`, `'set(log.attributes["a"], "it''s")'`},
		{"double quotes in flow", advanced, "transform/logs", "logs", 1, 0, `set(resource.attributes["b"], 2)`, `"set(resource.attributes[\"b\"], 2)"`},
		{"plain falls back", basic, "transform", "traces", 0, 0, `set(span.name, "a") where span.name == "a #b"`, `'set(span.name, "a") where span.name == "a #b"'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := []byte(tc.text)
			loc, err := Locate(text, tc.processor, tc.signal, tc.group, tc.index)
			if err != nil {
				t.Fatal(err)
			}
			got := assertSplice(t, text, Edit{Processor: tc.processor, Signal: tc.signal, Group: tc.group, Index: tc.index, Op: Replace, Text: tc.value, Expected: loc.Value}, loc.Start, loc.End, tc.written)
			before, after := strings.Split(tc.text, "\n"), strings.Split(string(got), "\n")
			if len(before) != len(after) {
				t.Fatal("line count changed")
			}
			changed := 0
			for i := range before {
				if before[i] != after[i] {
					changed++
				}
			}
			if changed != 1 {
				t.Fatalf("%d lines changed", changed)
			}
			if strings.Contains(tc.text, "# keep this comment") && !bytes.Contains(got, []byte("# keep this comment")) {
				t.Fatal("comment lost")
			}
		})
	}
}

const three = "processors:\n  transform:\n    log_statements:\n      - 'first' # keep/remove with item\n      - 'middle'\n      - 'last'\nservice: {}\n"

func TestInsertAppendDelete(t *testing.T) {
	for _, tc := range []struct {
		name  string
		op    Op
		index int
	}{
		{"insert first", Insert, 0}, {"insert middle", Insert, 1}, {"insert end", Insert, 3},
		{"append", Append, 0}, {"delete first", Delete, 0}, {"delete middle", Delete, 1}, {"delete last", Delete, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := []byte(three)
			at := tc.index
			if tc.op == Append {
				at = 3
			}
			loc, err := Locate(text, "transform", "logs", 0, min(at, 2))
			if err != nil {
				t.Fatal(err)
			}
			start := bytes.LastIndexByte(text[:loc.Start], '\n') + 1
			end := loc.End + bytes.IndexByte(text[loc.End:], '\n') + 1
			replacement := "      - 'new'\n"
			switch {
			case tc.op == Delete:
				replacement = ""
			case at == 3:
				start = end
			default:
				end = start
			}
			assertSplice(t, text, Edit{Processor: "transform", Signal: "logs", Op: tc.op, Index: tc.index, Text: "new", Expected: loc.Value}, start, end, replacement)
		})
	}
}

func TestPositionsCommentsAndLineEndings(t *testing.T) {
	for _, text := range []string{
		strings.ReplaceAll(three, "\n", "\r\n"),
		"# Grüße 世界\n" + three,
		"processors: {transform: {log_statements: ['éé', 'old']}} # comment\n",
		"processors:\n  transform:\n    log_statements:\n      - 'old' # trailing comment\n",
		"processors:\n  transform:\n    log_statements:\n      - old # trailing comment\n",
	} {
		index := 0
		if strings.Contains(text, "'éé'") {
			index = 1
		}
		loc, err := Locate([]byte(text), "transform", "logs", 0, index)
		if err != nil {
			t.Fatal(err)
		}
		written := "new"
		if loc.Style == yaml.SingleQuotedStyle {
			written = "'new'"
		}
		assertSplice(t, []byte(text), Edit{Processor: "transform", Signal: "logs", Index: index, Op: Replace, Text: "new", Expected: loc.Value}, loc.Start, loc.End, written)
	}
	for _, op := range []Op{Insert, Append, Delete} {
		text := []byte(strings.ReplaceAll(three, "\n", "\r\n"))
		loc, _ := Locate(text, "transform", "logs", 0, 1)
		start := bytes.LastIndexByte(text[:loc.Start], '\n') + 1
		end := loc.End + bytes.IndexByte(text[loc.End:], '\n') + 1
		replacement := "      - 'new'\r\n"
		index := 1
		if op == Insert {
			end = start
		}
		if op == Delete {
			replacement = ""
		}
		if op == Append {
			index = 0
			start = bytes.Index(text, []byte("service:"))
			end = start
		}
		assertSplice(t, text, Edit{Processor: "transform", Signal: "logs", Index: index, Op: op, Text: "new", Expected: loc.Value}, start, end, replacement)
	}
}

func TestNoFinalNewlineAndJSONQuoting(t *testing.T) {
	text := []byte("processors:\n  transform:\n    log_statements:\n      - \"old\"")
	loc, _ := Locate(text, "transform", "logs", 0, 0)
	assertSplice(t, text, Edit{Processor: "transform", Signal: "logs", Op: Append, Text: "new"}, len(text), len(text), "\n      - \"new\"")
	assertSplice(t, text, Edit{Processor: "transform", Signal: "logs", Op: Replace, Expected: "old", Text: "<>&\n世界"}, loc.Start, loc.End, `"<>&\n世界"`)
	assertSplice(t, text, Edit{Processor: "transform", Signal: "logs", Op: Replace, Expected: "old", Text: `literal \u2028 and \u2029`}, loc.Start, loc.End, `"literal \\u2028 and \\u2029"`)
}

func TestGroupedInsertAppendDeleteAndDashLayout(t *testing.T) {
	text := []byte(strings.ReplaceAll(advanced, "          - ", "          -   "))
	for _, e := range []Edit{
		{Processor: "transform/logs", Signal: "logs", Group: 0, Op: Insert, Index: 1, Text: "new"},
		{Processor: "transform/logs", Signal: "logs", Group: 0, Op: Append, Text: "new"},
		{Processor: "transform/logs", Signal: "logs", Group: 0, Op: Delete, Index: 1, Expected: `set(log.attributes["a"], "x")`},
	} {
		loc, err := Locate(text, e.Processor, e.Signal, e.Group, 1)
		if err != nil {
			t.Fatal(err)
		}
		start := bytes.LastIndexByte(text[:loc.Start], '\n') + 1
		end := loc.End + bytes.IndexByte(text[loc.End:], '\n') + 1
		replacement := "          -   'new'\n"
		if e.Op == Insert {
			end = start
		}
		if e.Op == Append {
			start = end
		}
		if e.Op == Delete {
			replacement = ""
		}
		assertSplice(t, text, e, start, end, replacement)
	}
}

func TestStaleExpected(t *testing.T) {
	for _, op := range []Op{Replace, Delete} {
		_, err := Apply([]byte(basic), Edit{Processor: "transform", Signal: "traces", Op: op, Text: "x", Expected: `set(span.name, "z")`})
		var stale *StaleError
		if !errors.As(err, &stale) || stale.Current != `set(span.name, "a")` {
			t.Fatalf("stale = %v", err)
		}
	}
}

func TestRefusedShapes(t *testing.T) {
	for _, item := range []string{"|\n        old", ">\n        old", "&a old", "*a", "!!str old", "'old\n        continued'", "\"old\n        continued\"", "old\n        continued"} {
		text := []byte("anchor: &a old\nprocessors:\n  transform:\n    log_statements:\n      - " + item + "\n")
		if _, err := Apply(text, Edit{Processor: "transform", Signal: "logs", Op: Replace, Text: "new", Expected: "old"}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%q: %v", item, err)
		}
	}
	for _, list := range []string{"&list [old]", "!!seq [old]", "*list"} {
		text := []byte("anchor: &list [old]\nprocessors:\n  transform:\n    log_statements: " + list + "\n")
		if _, err := Apply(text, Edit{Processor: "transform", Signal: "logs", Op: Replace, Text: "new", Expected: "old"}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%q: %v", list, err)
		}
	}
	for _, op := range []Op{Insert, Append, Delete} {
		text := []byte("processors: {transform: {log_statements: [old, another]}}")
		if _, err := Apply(text, Edit{Processor: "transform", Signal: "logs", Op: op, Text: "new", Expected: "old"}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("flow %s: %v", op, err)
		}
	}
	text := []byte("processors:\n  transform:\n    log_statements:\n      - old\n")
	if _, err := Apply(text, Edit{Processor: "transform", Signal: "logs", Op: Delete, Expected: "old"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("only statement: %v", err)
	}
}

func TestInvalidAddressesAndRequiredFields(t *testing.T) {
	valid := Edit{Processor: "transform", Signal: "traces", Op: Replace, Text: "new", Expected: `set(span.name, "a")`}
	for _, mutate := range []func(*Edit){
		func(e *Edit) { e.Processor = "missing" }, func(e *Edit) { e.Processor = "transform/unknown" }, func(e *Edit) { e.Signal = "unknown" },
		func(e *Edit) { e.Group = -1 }, func(e *Edit) { e.Group = 1 }, func(e *Edit) { e.Index = -1 }, func(e *Edit) { e.Index = 2 },
		func(e *Edit) { e.Expected = "" }, func(e *Edit) { e.Text = "" }, func(e *Edit) { e.Op = "unknown" },
		func(e *Edit) { e.Op = Append; e.Index = 1 }, func(e *Edit) { e.Op = Insert; e.Index = 3 },
	} {
		e := valid
		mutate(&e)
		if _, err := Apply([]byte(basic), e); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v: %v", e, err)
		}
	}
	if _, err := Locate([]byte(advanced), "transform/logs", "logs", 9, 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
