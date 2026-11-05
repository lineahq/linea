package comparison

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/evidence"
)

var update = flag.Bool("update", false, "rewrite the Markdown golden file")

func sample(t *testing.T, id, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "samples", id, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// execute runs a configuration on a sample's quick input or scenario.
func execute(t *testing.T, id, config string, scenario bool) *engine.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := filepath.Join("..", "..", "..", "samples", id)
	req := engine.Request{ConfigYAML: config, Fixture: filepath.Join(dir, "input.json")}
	if scenario {
		req = engine.Request{ConfigYAML: config, Scenario: filepath.Join(dir, "scenario.yaml")}
	}
	rep, err := engine.Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func run(id string, rep *engine.Report, digest string) Side {
	return Side{Run: evidence.Run{ID: id, End: evidence.EndDone, Report: rep}, InputsDigest: digest}
}

func compare(t *testing.T, before, after Side, ignore ...string) *Comparison {
	t.Helper()
	c, err := Compare(Request{Before: before, After: after, Ignore: ignore})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func exporter(t *testing.T, c *Comparison, name string) Exporter {
	t.Helper()
	for _, e := range c.Exporters {
		if e.Exporter == name {
			return e
		}
	}
	t.Fatalf("no exporter %s in %+v", name, c.Exporters)
	return Exporter{}
}

func TestIdenticalRunsAreTheSame(t *testing.T) {
	for _, tc := range []struct {
		id       string
		scenario bool
	}{
		{"logs-cleanup", false},
		{"logs-cleanup", true},
		{"log-routing", true},
		// Mocked components order attributes arbitrarily; normalization
		// removes that.
		{"container-logs", true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			config := sample(t, tc.id, "config.yaml")
			c := compare(t, run("a", execute(t, tc.id, config, tc.scenario), "d"), run("b", execute(t, tc.id, config, tc.scenario), "d"))
			if c.Outcome != Same || !c.InputsIdentical || len(c.Exporters) == 0 || c.Records == nil {
				t.Fatalf("comparison = %+v", c)
			}
			for _, e := range c.Exporters {
				if !e.Equal {
					t.Fatalf("exporter %+v is not equal", e)
				}
			}
			if c.Records.Changed != 0 || c.Records.Aligned == 0 && !tc.scenario {
				t.Fatalf("records = %+v", c.Records)
			}
		})
	}
}

func TestChangedConfigurationsAreDifferent(t *testing.T) {
	cleanup := sample(t, "logs-cleanup", "config.yaml")
	base := execute(t, "logs-cleanup", cleanup, false)

	t.Run("removed attribute", func(t *testing.T) {
		// The after configuration no longer deletes the password.
		config := strings.Replace(cleanup, "      - key: user.password\n        action: delete\n", "", 1)
		c := compare(t, run("a", base, "d"), run("b", execute(t, "logs-cleanup", config, false), "d"))
		e := exporter(t, c, "otlp/backend")
		if c.Outcome != Different || e.Equal || e.Before != e.After {
			t.Fatalf("comparison = %+v", c)
		}
		if !slices.ContainsFunc(e.Changes, func(g evidence.ChangeGroup) bool {
			return g.Path == `log.attributes["user.password"]` && g.Op == analysis.OpAdded && g.Count == 1
		}) {
			t.Fatalf("changes = %+v", e.Changes)
		}
		if c.Records.Changed != 1 || c.Records.Items[0].FateChanged {
			t.Fatalf("records = %+v", c.Records)
		}
	})

	t.Run("dropped record", func(t *testing.T) {
		// The after configuration has no health-check filter, so the record
		// the before run dropped is delivered.
		config := strings.Replace(cleanup, "filter/health, ", "", 1)
		c := compare(t, run("a", base, "d"), run("b", execute(t, "logs-cleanup", config, false), "d"))
		e := exporter(t, c, "otlp/backend")
		if c.Outcome != Different || e.Before+1 != e.After {
			t.Fatalf("exporter = %+v", e)
		}
		var moved []Record
		for _, r := range c.Records.Items {
			if r.FateChanged {
				moved = append(moved, r)
			}
		}
		if len(moved) != 1 || len(moved[0].Before.Delivered) != 0 || !slices.Contains(moved[0].Before.Stopped, "dropped at filter/health (logs)") ||
			!slices.Contains(moved[0].After.Delivered, "otlp/backend (logs)") {
			t.Fatalf("moved = %+v", moved)
		}
	})

	t.Run("same counts, different values", func(t *testing.T) {
		config := strings.Replace(cleanup, `"upper"`, `"lower"`, 1)
		c := compare(t, run("a", base, "d"), run("b", execute(t, "logs-cleanup", config, false), "d"))
		e := exporter(t, c, "otlp/backend")
		if c.Outcome != Different || e.Equal || e.Before != e.After || e.Before == 0 {
			t.Fatalf("equal counts must not make the runs the same: %+v", e)
		}
		if len(e.Changes) != 1 || e.Changes[0].Path != "log.severity_text" || e.Changes[0].Op != analysis.OpModified {
			t.Fatalf("changes = %+v", e.Changes)
		}
	})

	t.Run("changed route", func(t *testing.T) {
		routing := sample(t, "log-routing", "config.yaml")
		config := strings.Replace(routing, `== "audit"`, `== "nobody"`, 1)
		c := compare(t, run("a", execute(t, "log-routing", routing, true), "d"), run("b", execute(t, "log-routing", config, true), "d"))
		if c.Outcome != Different {
			t.Fatalf("comparison = %+v", c)
		}
		var changed []Route
		for _, r := range c.Routes {
			if r.Changed {
				changed = append(changed, r)
			}
		}
		if !slices.ContainsFunc(changed, func(r Route) bool { return r.Pipeline == "logs/audit" && r.Before > 0 && r.After == 0 }) {
			t.Fatalf("routes = %+v", c.Routes)
		}
		if a := exporter(t, c, "otlp/audit"); a.OnlyIn != "before" {
			t.Fatalf("audit exporter = %+v", a)
		}
		if !slices.ContainsFunc(c.Assertions, func(a Assertion) bool { return a.Changed }) {
			t.Fatalf("assertions = %+v", c.Assertions)
		}
	})
}

func TestReceiverRecordsArePairedByContent(t *testing.T) {
	// The filelog receiver makes its records from mocked files, so no
	// injected record leads to the exporter.
	logs := sample(t, "container-logs", "config.yaml")
	config := strings.Replace(logs, "        id: container-parser\n",
		"        id: container-parser\n      - type: add\n        field: attributes.env\n        value: prod\n", 1)
	c := compare(t, run("a", execute(t, "container-logs", logs, true), "d"), run("b", execute(t, "container-logs", config, true), "d"))
	e := exporter(t, c, "otlp/backend")
	if c.Outcome != Different || e.Equal || e.Before != e.After || e.Before == 0 || e.Note != pairedNote {
		t.Fatalf("exporter = %+v", e)
	}
	want := evidence.ChangeGroup{Path: `log.attributes["env"]`, Op: analysis.OpAdded, Count: e.Before, Confidence: analysis.ConfidenceMedium}
	if len(e.Changes) != 1 || e.Changes[0].Path != want.Path || e.Changes[0].Op != want.Op || e.Changes[0].Count != want.Count ||
		e.Changes[0].Confidence != want.Confidence {
		t.Fatalf("changes = %+v, want %+v", e.Changes, want)
	}

	// An ignore rule covering the difference leaves no change.
	c = compare(t, run("a", execute(t, "container-logs", logs, true), "d"), run("b", execute(t, "container-logs", config, true), "d"), `log.attributes["env"]`)
	if e := exporter(t, c, "otlp/backend"); !e.EqualAfterIgnore || len(e.Changes) != 0 {
		t.Fatalf("exporter = %+v", e)
	}
}

func TestIgnoreRulesAreCountedNotDropped(t *testing.T) {
	cleanup := sample(t, "logs-cleanup", "config.yaml")
	base := execute(t, "logs-cleanup", cleanup, false)
	lower := execute(t, "logs-cleanup", strings.Replace(cleanup, `"upper"`, `"lower"`, 1), false)
	c := compare(t, run("a", base, "d"), run("b", lower, "d"), "log.severity_text", `resource.attributes["absent"]`)
	e := exporter(t, c, "otlp/backend")
	if c.Outcome != Same || e.Equal || !e.EqualAfterIgnore || len(e.Changes) != 0 || c.Records.Changed != 0 {
		t.Fatalf("comparison = %+v", c)
	}
	// Every delivered record changed its severity text; the debug exporter
	// is not observed.
	want := 0
	for _, e := range c.Exporters {
		want += e.After
	}
	if c.Ignored[0] != (Ignored{Rule: "log.severity_text", Count: want}) || c.Ignored[1].Count != 0 {
		t.Fatalf("ignored = %+v, want %d for the first rule", c.Ignored, want)
	}
}

func TestIgnoreRuleGrammar(t *testing.T) {
	for text, want := range map[string]string{
		"log.body":                          "log.body",
		`log.attributes["request.id"]`:      `log.attributes["request.id"]`,
		`scope.attributes["a"]["b"]`:        `instrumentation_scope.attributes["a"]["b"]`,
		"instrumentation_scope.version":     "instrumentation_scope.version",
		"datapoint.value_double":            "datapoint.value_double",
		`span.attributes["quote\"]"]`:       `span.attributes["quote\"]"]`,
		"metric.metadata.x":                 "metric.metadata.x",
		`resource.attributes["host.name"]`:  `resource.attributes["host.name"]`,
		"span.start_time_unix_nano":         "span.start_time_unix_nano",
		`log.attributes["k"].nested_field`:  `log.attributes["k"].nested_field`,
		`resource.attributes["é"]`:          `resource.attributes["é"]`,
		`log.attributes["tab\there"]`:       `log.attributes["tab\there"]`,
		`datapoint.attributes["collector"]`: `datapoint.attributes["collector"]`,
	} {
		got, err := parsePath(text)
		if err != nil || got != want {
			t.Errorf("parsePath(%s) = %q, %v; want %q", text, got, err, want)
		}
	}
	for _, text := range []string{"", "body", "record.body", "log.", "log", "log.attributes[k]", `log.attributes["k"`, `log.attributes["k"]x`, "log.Body", "log..body", `log.attributes['k']`} {
		if _, err := parseRules([]string{text}); !errors.Is(err, ErrInvalidRule) {
			t.Errorf("rule %q: %v, want ErrInvalidRule", text, err)
		}
	}
	rules, _ := parseRules([]string{`log.attributes["k"]`})
	for path, want := range map[string]bool{
		`log.attributes["k"]`:       true,
		`log.attributes["k"]["n"]`:  true,
		`log.attributes["k"].x`:     true,
		`log.attributes["kk"]`:      false,
		`log.attributes`:            false,
		`span.attributes["k"]`:      false,
		`log.attributes["k"]suffix`: false,
	} {
		if got := matchRules(rules, path) != nil; got != want {
			t.Errorf("match %s = %v, want %v", path, got, want)
		}
	}
	if _, err := Compare(Request{Ignore: []string{"nope"}}); !errors.Is(err, ErrInvalidRule) {
		t.Fatalf("Compare with an invalid rule: %v", err)
	}
}

// Runs whose inputs are not proven identical cannot be compared: even equal
// exporter outputs are inconclusive, listed for information only.
func TestUnequalInputsAreInconclusive(t *testing.T) {
	cleanup := sample(t, "logs-cleanup", "config.yaml")
	rep := execute(t, "logs-cleanup", cleanup, false)
	for _, digests := range [][2]string{{"d1", "d2"}, {"", ""}, {"d1", ""}} {
		c := compare(t, run("a", rep, digests[0]), run("b", rep, digests[1]))
		if c.Outcome != Inconclusive || c.InputsIdentical || c.Records != nil || len(c.Reasons) != 1 || c.Reasons[0].Code != ReasonInputsDiffer ||
			len(c.Exporters) == 0 || !c.Exporters[0].Equal {
			t.Fatalf("digests %v: comparison = %+v", digests, c)
		}
	}
	// Unequal or unknown identities prove nothing either.
	for _, ids := range [][2]string{{"i1", "i2"}, {"", ""}} {
		a, b := run("a", rep, "d1"), run("b", rep, "d2")
		a.InputIdentity, b.InputIdentity = ids[0], ids[1]
		if c := compare(t, a, b); c.InputsIdentical || c.Outcome != Inconclusive {
			t.Fatalf("identities %v: comparison = %+v", ids, c)
		}
	}
}

// Runs whose inputs differ as written but inline to one scenario, such as a
// statement test of a run, compare record by record and say why.
func TestInputsInlinedToOneScenarioAreIdentical(t *testing.T) {
	rep := execute(t, "logs-cleanup", sample(t, "logs-cleanup", "config.yaml"), false)
	a, b := run("a", rep, "d1"), run("b", rep, "d2")
	a.InputIdentity, b.InputIdentity = "i", "i"
	c := compare(t, a, b)
	if c.Outcome != Same || !c.InputsIdentical || c.Records == nil || len(c.Notes) == 0 || !strings.Contains(strings.Join(c.Notes, " "), "inline to one scenario") {
		t.Fatalf("comparison = %+v", c)
	}
	a.InputsDigest, b.InputsDigest = "d", "d"
	if c := compare(t, a, b); slices.ContainsFunc(c.Notes, func(n string) bool { return strings.Contains(n, "inline to one scenario") }) {
		t.Fatalf("equal digests noted the identity: %v", c.Notes)
	}
}

func TestIncompleteRunsAreInconclusive(t *testing.T) {
	rep := execute(t, "logs-cleanup", sample(t, "logs-cleanup", "config.yaml"), false)
	with := func(f func(r *runresult.Result)) *engine.Report {
		copied := *rep
		result := *rep.Result
		f(&result)
		copied.Result = &result
		return &copied
	}
	for name, tc := range map[string]struct {
		after Side
		code  string
	}{
		"partial": {run("b", with(func(r *runresult.Result) { r.Status = runresult.StatusPartial }), "d"), ReasonAfterIncomplete},
		"cancelled": {Side{Run: evidence.Run{ID: "b", End: evidence.EndCancelled, Report: with(func(r *runresult.Result) { r.Status = runresult.StatusCancelled })},
			InputsDigest: "d"}, ReasonAfterIncomplete},
		"truncated": {run("b", with(func(r *runresult.Result) {
			r.Truncation = &runresult.Truncation{Reason: runresult.TruncationEvidenceBudget}
		}), "d"), ReasonAfterTruncated},
		"other build": {Side{Run: run("b", rep, "d").Run, InputsDigest: "d", Build: "other"}, ReasonBuildsDiffer},
	} {
		t.Run(name, func(t *testing.T) {
			before := run("a", rep, "d")
			if tc.code == ReasonBuildsDiffer {
				before.Build = "this"
			}
			c := compare(t, before, tc.after)
			if c.Outcome != Inconclusive || !slices.ContainsFunc(c.Reasons, func(r Reason) bool { return r.Code == tc.code }) {
				t.Fatalf("comparison = %+v", c)
			}
		})
	}
	if rep.Result.Status != runresult.StatusCompleted || rep.Result.Truncation != nil {
		t.Fatal("the compared report was changed")
	}
}

// TestRunsOnTwoRuntimesShowBoth compares a built-in run with one on a built
// runtime: both runtimes are shown, and the runtime alone is no reason to be
// inconclusive.
func TestRunsOnTwoRuntimesShowBoth(t *testing.T) {
	config := sample(t, "logs-cleanup", "config.yaml")
	before := execute(t, "logs-cleanup", config, false)
	after := execute(t, "logs-cleanup", config, false)
	upgraded := *after.Runtime
	upgraded.Kind, upgraded.Name, upgraded.CollectorVersion, upgraded.Verified = engine.RuntimeContainer, "next", "0.163.0", false
	after.Runtime = &upgraded
	c := compare(t, run("a", before, "d"), run("b", after, "d"))
	if c.Outcome != Same || len(c.Reasons) != 0 || c.Before.Runtime.Kind != engine.RuntimeBuiltIn || c.After.Runtime.Name != "next" {
		t.Fatalf("comparison = %+v", c)
	}
	md := c.Markdown()
	if !strings.Contains(md, "| Runtime | built-in | `next` |") || !strings.Contains(md, "| Collector | `"+before.Runtime.CollectorVersion+"` | `0.163.0` (unverified) |") {
		t.Fatalf("markdown does not show both runtimes:\n%s", md)
	}
	if strings.Contains(compare(t, run("a", before, "d"), run("b", before, "d")).Markdown(), "| Runtime |") {
		t.Fatal("a built-in comparison shows runtime rows")
	}
}

func TestComparisonDoesNotChangeTheReports(t *testing.T) {
	cleanup := sample(t, "logs-cleanup", "config.yaml")
	before := execute(t, "logs-cleanup", cleanup, false)
	after := execute(t, "logs-cleanup", strings.Replace(cleanup, `"upper"`, `"lower"`, 1), false)
	encode := func(r *engine.Report) string {
		var b strings.Builder
		if err := engine.Encode(&b, r); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	b, a := encode(before), encode(after)
	compare(t, run("a", before, "d"), run("b", after, "d"))
	if encode(before) != b || encode(after) != a {
		t.Fatal("Compare changed a report")
	}
}

func TestMarkdownGolden(t *testing.T) {
	cleanup := sample(t, "logs-cleanup", "config.yaml")
	before := execute(t, "logs-cleanup", cleanup, true)
	after := execute(t, "logs-cleanup", strings.Replace(strings.Replace(cleanup, "filter/health, ", "", 1), `"upper"`, `"lower"`, 1), true)
	c := compare(t, run("before-run", before, "d"), run("after-run", after, "d"), `log.attributes["absent"]`)
	got := c.Markdown()
	path := filepath.Join("testdata", "different.md")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Fatalf("Markdown differs from %s (run with -update):\n%s", path, got)
	}
}

func TestMarkdownKeepsValuesInCodeSpans(t *testing.T) {
	for in, want := range map[string]string{
		"plain":         "`plain`",
		"a|b":           "`a\\|b`",
		"line\nbreak":   "`line break`",
		"has `tick`":    "`` has `tick` ``",
		"`edge":         "`` `edge ``",
		"":              "—",
		"# not heading": "`# not heading`",
	} {
		if got := code(in); got != want {
			t.Errorf("code(%q) = %q, want %q", in, got, want)
		}
	}
}

// Only processors record their pipeline: a record that reaches the exporter
// straight from the receiver is delivered by the exporter's one pipeline, so
// adding a processor changes its fields, not where it ended.
func TestDeliveriesWithoutProcessorsNameTheirPipeline(t *testing.T) {
	const plain = `receivers: {otlp: {protocols: {grpc: {}}}}
exporters: {otlp/backend: {endpoint: localhost:4317, tls: {insecure: true}}}
service:
  pipelines:
    logs: {receivers: [otlp], exporters: [otlp/backend]}
`
	const edited = `receivers: {otlp: {protocols: {grpc: {}}}}
processors:
  transform:
    log_statements:
      - context: log
        statements: ['set(attributes["added"], "yes")']
exporters: {otlp/backend: {endpoint: localhost:4317, tls: {insecure: true}}}
service:
  pipelines:
    logs: {receivers: [otlp], processors: [transform], exporters: [otlp/backend]}
`
	c := compare(t, run("a", execute(t, "logs-cleanup", plain, false), "d"), run("b", execute(t, "logs-cleanup", edited, false), "d"))
	if c.Records == nil || c.Records.Aligned != 4 || c.Records.Changed != 4 {
		t.Fatalf("records = %+v", c.Records)
	}
	for _, r := range c.Records.Items {
		if r.FateChanged || !slices.Equal(r.Before.Delivered, []string{"otlp/backend (logs)"}) || len(r.Changes) != 1 ||
			r.Changes[0].Path != `log.attributes["added"]` || r.Changes[0].Exporter != "otlp/backend (logs)" {
			t.Fatalf("record %+v", r)
		}
	}
}
