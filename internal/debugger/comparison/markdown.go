package comparison

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/evidence"
)

// Markdown renders the comparison for a review or a CI log. Headings
// and prose are fixed text and counts; component names, paths and values
// appear only in code spans inside tables and lists.
func (c *Comparison) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Run comparison: %s\n\n", c.Outcome)
	b.WriteString("| | Before | After |\n|---|---|---|\n")
	row := func(name string, before, after string) {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", name, before, after)
	}
	row("Run", code(c.Before.ID), code(c.After.ID))
	row("Mode", code(c.Before.Mode), code(c.After.Mode))
	row("End", code(string(c.Before.End)), code(string(c.After.End)))
	row("Execution", code(string(c.Before.ExecutionStatus)), code(string(c.After.ExecutionStatus)))
	row("Complete", yes(c.Before.Complete), yes(c.After.Complete))
	if built(c.Before.Runtime) || built(c.After.Runtime) {
		row("Runtime", runtimeName(c.Before.Runtime), runtimeName(c.After.Runtime))
		row("Collector", collectorVersion(c.Before.Runtime), collectorVersion(c.After.Runtime))
	}
	if c.Before.Assertions != "" || c.After.Assertions != "" {
		row("Assertions", code(string(c.Before.Assertions)), code(string(c.After.Assertions)))
	}
	fmt.Fprintf(&b, "\nInputs identical: %s.\n", yes(c.InputsIdentical))
	if len(c.Reasons) > 0 {
		b.WriteString("\n## Why the comparison is inconclusive\n\n")
		for _, r := range c.Reasons {
			fmt.Fprintf(&b, "- %s: %s\n", code(r.Code), r.Message)
		}
	}

	b.WriteString("\n## Exporters\n\n")
	if len(c.Exporters) == 0 {
		b.WriteString("Neither run delivered output.\n")
	} else {
		b.WriteString("| Exporter | Signal | Before | After | Result |\n|---|---|---|---|---|\n")
		for _, e := range c.Exporters {
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %s |\n", code(e.Exporter), code(e.Signal), e.Before, e.After, exporterResult(e))
		}
		for _, e := range c.Exporters {
			if len(e.Changes) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n### Field changes at %s (%s)\n\n", code(e.Exporter), code(e.Signal))
			b.WriteString("| Path | Op | Records | Before | After |\n|---|---|---|---|---|\n")
			for _, g := range e.Changes {
				fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n", code(g.Path), g.Op, g.Count, value(g.Before), value(g.After))
			}
			if e.ChangesTruncated {
				fmt.Fprintf(&b, "\nOnly the first %d change groups are listed.\n", maxGroups)
			}
		}
	}

	if r := c.Records; r != nil {
		b.WriteString("\n## Injected records\n\n")
		fmt.Fprintf(&b, "%d aligned, %d changed, %d unaligned, %d uncertain.\n", r.Aligned, r.Changed, r.Unaligned, r.Uncertain)
		if len(r.Items) > 0 {
			b.WriteString("\n| Input | Record | Before | After | Diverges at | Field changes |\n|---|---|---|---|---|---|\n")
			for _, it := range r.Items {
				diverges := "-"
				if it.DivergesAt != "" {
					diverges = code(it.DivergesAt)
				}
				fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %d |\n", it.Input, code(it.Ref), fateText(it.Before), fateText(it.After), diverges, len(it.Changes))
			}
			if r.Truncated {
				fmt.Fprintf(&b, "\nOnly the first %d changed records are listed.\n", maxRecords)
			}
		}
	}

	var routes []Route
	for _, r := range c.Routes {
		if r.Changed {
			routes = append(routes, r)
		}
	}
	if len(routes) > 0 {
		b.WriteString("\n## Routes\n\n| Connector | Pipeline | Before | After |\n|---|---|---|---|\n")
		for _, r := range routes {
			fmt.Fprintf(&b, "| %s | %s | %d | %d |\n", code(r.Connector), code(r.Pipeline), r.Before, r.After)
		}
	}
	if len(c.Errors) > 0 {
		b.WriteString("\n## Errors\n\n| Source | Component | Message | Before | After |\n|---|---|---|---|---|\n")
		for _, e := range c.Errors {
			fmt.Fprintf(&b, "| %s | %s | %s | %d | %d |\n", e.Source, code(where(e.Component, e.Pipeline)), code(e.Message), e.Before, e.After)
		}
	}
	var assertions []Assertion
	for _, a := range c.Assertions {
		if a.Changed || a.Before == "" || a.After == "" {
			assertions = append(assertions, a)
		}
	}
	if len(assertions) > 0 {
		b.WriteString("\n## Assertions\n\n| Assertion | Before | After |\n|---|---|---|\n")
		for _, a := range assertions {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", code(a.Name), code(string(a.Before)), code(string(a.After)))
		}
	}
	if r := c.Recommendations; r != nil {
		fmt.Fprintf(&b, "\n## Recommendations\n\n%s.\n\n| Status | Rule | Pipeline | Components | Subject | Benefit before | Benefit after |\n|---|---|---|---|---|---|---|\n", r.Counts())
		for _, d := range r.Items {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", d.Status, code(d.Rule), code(d.Pipeline), code(strings.Join(d.Components, ", ")),
				code(d.Subject), benefitCell(d.Before), benefitCell(d.After))
		}
	}
	if len(c.Ignored) > 0 {
		b.WriteString("\n## Ignored\n\n| Rule | Differences |\n|---|---|\n")
		for _, i := range c.Ignored {
			fmt.Fprintf(&b, "| %s | %d |\n", code(i.Rule), i.Count)
		}
	}
	if len(c.Notes) > 0 {
		b.WriteString("\n## Notes\n\n")
		for _, n := range c.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	return b.String()
}

// benefitCell is one side's benefit count and metric, or a dash.
func benefitCell(s *RecommendationSide) string {
	if s == nil || s.Benefit == nil {
		return "—"
	}
	return fmt.Sprintf("%d %s", s.Benefit.Count, code(s.Benefit.Metric))
}

func exporterResult(e Exporter) string {
	switch {
	case e.OnlyIn != "":
		return "output only in the " + e.OnlyIn + " run"
	case e.Equal && e.Note != "":
		return "same records, different order"
	case e.Equal:
		return "same records"
	case e.EqualAfterIgnore:
		return "same apart from ignored paths"
	case e.Before == e.After:
		return "different records, same count"
	}
	return "different records"
}

func fateText(f Fate) string {
	var parts []string
	for _, d := range f.Delivered {
		parts = append(parts, "delivered to "+code(d))
	}
	for _, s := range f.Stopped {
		parts = append(parts, code(s))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "<br>")
}

func value(v *evidence.SummaryValue) string {
	if v == nil {
		return "—"
	}
	var s string
	if str, ok := v.Value.(string); ok {
		s = str
	} else {
		raw, _ := json.Marshal(v.Value)
		s = string(raw)
	}
	if v.Truncated {
		s += "…"
	}
	return code(s)
}

func yes(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// code renders text as a code span inside a table cell: pipes and line
// breaks cannot end the cell, and a backtick inside lengthens the fence.
func code(s string) string {
	if s == "" {
		return "—"
	}
	s = strings.NewReplacer("|", `\|`, "\r", " ", "\n", " ").Replace(s)
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// built reports whether a run executed on a built runtime; the Runtime rows
// appear only then, so comparisons on the built-in runtime keep their text.
func built(rt *engine.ReportRuntime) bool {
	return rt != nil && (rt.Kind == engine.RuntimeContainer || rt.Kind == engine.RuntimeImage)
}

func runtimeName(rt *engine.ReportRuntime) string {
	switch {
	case rt == nil:
		return "unknown"
	case built(rt):
		return code(rt.Name)
	default:
		return "built-in"
	}
}

// collectorVersion is the run's Collector release, marked when it is
// outside the support table.
func collectorVersion(rt *engine.ReportRuntime) string {
	switch {
	case rt == nil:
		return ""
	case !rt.Verified:
		return code(rt.CollectorVersion) + " (unverified)"
	}
	return code(rt.CollectorVersion)
}
