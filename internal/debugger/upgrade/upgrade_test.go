package upgrade

import (
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/comparison"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/release"
)

var (
	from = Runtime{Name: "c160", CollectorVersion: "0.160.0", Verified: true}
	to   = Runtime{Name: "c162", CollectorVersion: "0.162.0", Verified: true}
	ok   = Inspection{Valid: true, Diagnostics: []config.Diagnostic{}}
)

func same(name string) Scenario {
	return Scenario{Name: name, Before: "r1", After: "r2", Outcome: comparison.Same, InputsIdentical: true, InputsDigest: "sha256:aa",
		BeforeAssertions: assertion.Pass, AfterAssertions: assertion.Pass}
}

func TestTheVerdictFollowsTheEvidence(t *testing.T) {
	removed := config.Diagnostic{Severity: config.SeverityError, Code: "invalid_config", Component: "processors::transform",
		Message: "'' has invalid keys: error_mode2"}
	different := same("b")
	different.Outcome = comparison.Different
	assertions := same("c")
	assertions.AfterAssertions = assertion.Fail
	inconclusive := same("d")
	inconclusive.Outcome = comparison.Inconclusive
	failed := Scenario{Name: "e", Error: "the run failed"}
	for _, tc := range []struct {
		name    string
		in      Input
		verdict Verdict
		reasons []string
	}{
		{"compatible", Input{FromConfig: ok, ToConfig: ok, Scenarios: []Scenario{same("a")}}, Compatible, nil},
		{"no scenarios", Input{FromConfig: ok, ToConfig: ok}, Compatible, nil},
		{"configuration breaks", Input{FromConfig: ok, ToConfig: Inspection{Diagnostics: []config.Diagnostic{removed}}}, ConfigurationBreaks, nil},
		{"invalid before", Input{FromConfig: Inspection{}, ToConfig: ok}, Inconclusive, []string{ReasonFromInvalid}},
		{"output changes", Input{FromConfig: ok, ToConfig: ok, Scenarios: []Scenario{same("a"), different}}, BehaviorChanges, nil},
		{"assertions change", Input{FromConfig: ok, ToConfig: ok, Scenarios: []Scenario{assertions}}, BehaviorChanges, nil},
		{"behavior beats inconclusive", Input{FromConfig: ok, ToConfig: ok, Scenarios: []Scenario{inconclusive, different}}, BehaviorChanges, []string{ReasonScenarioInconclusive}},
		{"inconclusive", Input{FromConfig: ok, ToConfig: ok, Scenarios: []Scenario{same("a"), inconclusive}}, Inconclusive, []string{ReasonScenarioInconclusive}},
		{"failed scenario", Input{FromConfig: ok, ToConfig: ok, Scenarios: []Scenario{failed}}, Inconclusive, []string{ReasonScenarioFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.From, tc.in.To = from, to
			c := Assess(tc.in)
			if c.Verdict != tc.verdict {
				t.Fatalf("verdict %s, want %s: %+v", c.Verdict, tc.verdict, c.Reasons)
			}
			var codes []string
			for _, r := range c.Reasons {
				codes = append(codes, r.Code)
			}
			if strings.Join(codes, ",") != strings.Join(tc.reasons, ",") {
				t.Fatalf("reasons %v, want %v", codes, tc.reasons)
			}
		})
	}
}

func TestTheConfigurationDiffNamesTheTargetsDiagnostics(t *testing.T) {
	gone := config.Diagnostic{Severity: config.SeverityWarning, Code: "deprecated", Message: "old"}
	added := config.Diagnostic{Severity: config.SeverityError, Code: "invalid_config", Component: "receivers::otlp", Message: "unknown field"}
	c := Assess(Input{From: from, To: to,
		FromConfig: Inspection{Valid: true, Diagnostics: []config.Diagnostic{gone}},
		ToConfig:   Inspection{Diagnostics: []config.Diagnostic{added}}})
	if c.Verdict != ConfigurationBreaks || len(c.Configuration.Added) != 1 || c.Configuration.Added[0] != added ||
		len(c.Configuration.Removed) != 1 || c.Configuration.Removed[0] != gone {
		t.Fatalf("%+v", c)
	}
	md := c.Markdown()
	for _, want := range []string{"# Upgrade check: configuration_breaks", "| added | `error` | `invalid_config` | `receivers::otlp` | `unknown field` |", "| Configuration | valid | invalid |"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestFeatureGatesOfTheConfigurationsComponentsAreListed(t *testing.T) {
	fromGates := []release.Gate{
		{ID: "processor.transform.Old", Stage: "alpha"},
		{ID: "processor.filter.Other", Stage: "alpha"},
		{ID: "receiver.prometheusreceiver.Same", Stage: "beta", Enabled: true},
		{ID: "pkg.ottl.Flip", Stage: "alpha"},
		{ID: "exporter.k8s_events.Removed", Stage: "beta", Enabled: true},
	}
	toGates := []release.Gate{
		{ID: "processor.transform.Old", Stage: "beta", Enabled: true},
		{ID: "processor.filter.Other", Stage: "stable", Enabled: true},
		{ID: "receiver.prometheusreceiver.Same", Stage: "beta", Enabled: true},
		{ID: "pkg.ottl.Flip", Stage: "alpha", Enabled: true},
		{ID: "processor.transform.New", Stage: "alpha"},
	}
	c := Assess(Input{From: from, To: to, FromConfig: ok, ToConfig: ok, FromGates: fromGates, ToGates: toGates,
		Components: []string{"receivers/prometheus", "processors/transform", "exporters/k8sevents"}})
	var got []string
	for _, g := range c.FeatureGates.Changes {
		got = append(got, g.ID+"@"+g.Component)
	}
	want := "exporter.k8s_events.Removed@exporters/k8sevents,pkg.ottl.Flip@,processor.transform.New@processors/transform,processor.transform.Old@processors/transform"
	if strings.Join(got, ",") != want {
		t.Fatalf("gates %v\nwant %s", got, want)
	}
	if c.Verdict != Compatible {
		t.Fatalf("a gate change alone changed the verdict: %s", c.Verdict)
	}
	md := c.Markdown()
	if !strings.Contains(md, "| `processor.transform.Old` | `processors/transform` | `alpha` off | `beta` on |") ||
		!strings.Contains(md, "| `exporter.k8s_events.Removed` | `exporters/k8sevents` | `beta` on | absent |") {
		t.Fatalf("markdown:\n%s", md)
	}
	unknown := Assess(Input{From: from, To: to, FromConfig: ok, ToConfig: ok, FromGates: fromGates})
	if len(unknown.FeatureGates.Changes) != 0 || strings.Join(unknown.FeatureGates.Unknown, ",") != "to" {
		t.Fatalf("unknown gates: %+v", unknown.FeatureGates)
	}
}

// TestScopedGatesFollowTheComponentsLinkingThem uses the scopes builds
// record: a gate is listed for a configured component linking it under any
// of its names, or for the Collector, never for code only the debugger
// links, whatever its ID says.
func TestScopedGatesFollowTheComponentsLinkingThem(t *testing.T) {
	component := func(id string, components ...string) release.Gate {
		return release.Gate{ID: id, Stage: "alpha", Scope: release.GateScopeComponent, Components: components}
	}
	fromGates := []release.Gate{
		component("filelog.mtimeSortType", "receivers/file_log", "receivers/filelog"),
		component("ottl.set.allowNil", "processors/filter", "processors/transform"),
		component("pkg.exporterhelper.queueBatchEnabled", "exporters/otlp", "exporters/otlp_grpc"),
		{ID: "service.profilesSupport", Stage: "alpha", Scope: release.GateScopeCollector},
		{ID: "receiver.mock.Internal", Stage: "alpha", Scope: release.GateScopeDebugger},
	}
	toGates := make([]release.Gate, len(fromGates))
	for i, g := range fromGates {
		g.Stage, g.Enabled = "beta", true
		toGates[i] = g
	}
	c := Assess(Input{From: from, To: to, FromConfig: ok, ToConfig: ok, FromGates: fromGates, ToGates: toGates,
		Components: []string{"receivers/otlp", "processors/transform", "exporters/otlp"}})
	var got []string
	for _, g := range c.FeatureGates.Changes {
		got = append(got, g.ID+"@"+g.Component)
	}
	want := "ottl.set.allowNil@processors/transform,pkg.exporterhelper.queueBatchEnabled@exporters/otlp,service.profilesSupport@"
	if strings.Join(got, ",") != want {
		t.Fatalf("gates %v\nwant %s", got, want)
	}
}

func TestTheMarkdownMarksUnverifiedReleases(t *testing.T) {
	newer := Runtime{Name: "c163", CollectorVersion: "0.163.0"}
	c := Assess(Input{From: from, To: newer, FromConfig: ok, ToConfig: ok, Scenarios: []Scenario{same("logs.yaml")}})
	md := c.Markdown()
	for _, want := range []string{"| Collector | `0.160.0` | `0.163.0` (unverified) |", "| `logs.yaml` | `same` | `pass` | `pass` | yes | `r1` `r2` |", "unverified Collector release"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}
