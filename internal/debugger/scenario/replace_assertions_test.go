package scenario

import (
	"reflect"
	"strings"
	"testing"
)

func TestReplaceAssertionsPreservesScenarioNodes(t *testing.T) {
	for _, own := range []string{
		"",
		"assertions: []\n",
		"assertions: null\n",
		fromRunAssertion,
		"assertions: &old [{target: {exporter: debug, signal: logs}, count: {equals: 99}}]\n",
		// Replacement can repair invalid original assertions, just as replacing
		// an invalid statement can repair a configuration.
		"assertions: [{target: {unknown: invalid}}]\n",
	} {
		t.Run(own, func(t *testing.T) {
			base := "# preserve document\nversion: 1\nname: 'customer scenario'\ninputs: [{receiver: otlp, file: never-read.json, client_address: 10.0.0.9, after: 1ms}]\nmocks: {files: [{path: /logs/app.log, file: never-read.log}]}\nenvironment: {TOKEN: 'declared'}\nexecution: {timestamps: rebase, timeout: 5s}\n"
			raw, err := ReplaceAssertions([]byte(base+own), []byte(fromRunExtra))
			if err != nil {
				t.Fatal(err)
			}
			before, err := Parse([]byte(base), "relative directory")
			if err != nil {
				t.Fatal(err)
			}
			after, err := Parse(raw, "relative directory")
			if err != nil || len(after.Assertions) != 1 || after.Assertions[0].Name != "appended" {
				t.Fatalf("replacement = %+v, %v", after, err)
			}
			before.Assertions, err = ParseAssertions([]byte(fromRunExtra))
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("replacement changed other scenario fields", err)
			}
			if !strings.Contains(string(raw), "# preserve document") || !strings.Contains(string(raw), "'customer scenario'") {
				t.Fatal("retained node comments or scalar style changed")
			}
		})
	}
}

func TestReplaceAssertionsResolvesReplacementAliases(t *testing.T) {
	replacement := "version: &one 1\nassertions:\n  - &assertion {name: replacement, target: {exporter: debug, signal: logs}, count: {equals: *one}}\n  - *assertion\n"
	base := "version: 1\ninputs: [{file: never-read.json}]\n" + fromRunAssertion
	raw, err := ReplaceAssertions([]byte(base), []byte(replacement))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw, "")
	if err != nil || len(parsed.Assertions) != 2 || parsed.Assertions[0].Name != "replacement" || parsed.Assertions[1].Name != "replacement" {
		t.Fatalf("replacement = %+v, %v", parsed, err)
	}
}

func TestReplaceAssertionsRejectsInvalidSources(t *testing.T) {
	for _, replacement := range []string{"", "assertions: []", "version: 99\n" + fromRunExtra, "inputs: []\n" + fromRunExtra, "assertions: [{target: {exporter: debug}}]", "assertions: ["} {
		if _, err := ReplaceAssertions([]byte("version: 1\ninputs: [{file: never-read}]\n"), []byte(replacement)); err == nil || !strings.HasPrefix(err.Error(), "assertions: ") {
			t.Fatalf("replacement %q: %v", replacement, err)
		}
	}
	for _, raw := range []string{"", "- list", "version: ["} {
		if _, err := ReplaceAssertions([]byte(raw), []byte(fromRunExtra)); err == nil {
			t.Fatalf("invalid scenario %q accepted", raw)
		}
	}
}

func TestReplaceAssertionsAvoidsAnchorCollisionsWithRetainedNodes(t *testing.T) {
	base := "version: 1\nname: &shared kept\n" + fromRunAssertion + "inputs: [{file: *shared}]\n"
	replacement := "assertions:\n  - {name: &shared replacement, target: {exporter: debug, signal: logs}, count: {equals: 1}}\n  - {name: *shared, target: {exporter: debug, signal: logs}, count: {equals: 2}}\n"
	raw, err := ReplaceAssertions([]byte(base), []byte(replacement))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw, "")
	if err != nil || parsed.Name != "kept" || parsed.Inputs[0].File != "kept" || len(parsed.Assertions) != 2 || parsed.Assertions[0].Name != "replacement" || parsed.Assertions[1].Name != "replacement" {
		t.Fatalf("anchor collision changed nodes: %+v, %v", parsed, err)
	}
}
