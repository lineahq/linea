package upgrade

import (
	"fmt"
	"strings"
)

// Markdown renders the check for a pull request or a CI log. Headings and
// prose are fixed text and counts; names, versions, codes and messages
// appear only in code spans inside tables and lists.
func (c *Check) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Upgrade check: %s\n\n", c.Verdict)
	b.WriteString("| | From | To |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| Runtime | %s | %s |\n", code(c.From.Name), code(c.To.Name))
	fmt.Fprintf(&b, "| Collector | %s | %s |\n", runtimeRelease(c.From), runtimeRelease(c.To))
	fmt.Fprintf(&b, "| Configuration | %s | %s |\n", valid(c.Configuration.FromValid), valid(c.Configuration.ToValid))
	if len(c.Reasons) > 0 {
		b.WriteString("\n## Why the check is inconclusive\n\n")
		for _, r := range c.Reasons {
			fmt.Fprintf(&b, "- %s: %s\n", code(r.Code), code(r.Message))
		}
	}

	b.WriteString("\n## Configuration\n\n")
	if len(c.Configuration.Added)+len(c.Configuration.Removed) == 0 {
		b.WriteString("Both runtimes report the same diagnostics.\n")
	} else {
		b.WriteString("| Change | Severity | Code | Component | Message |\n|---|---|---|---|---|\n")
		for _, d := range c.Configuration.Added {
			fmt.Fprintf(&b, "| added | %s | %s | %s | %s |\n", code(string(d.Severity)), code(d.Code), code(d.Component), code(d.Message))
		}
		for _, d := range c.Configuration.Removed {
			fmt.Fprintf(&b, "| removed | %s | %s | %s | %s |\n", code(string(d.Severity)), code(d.Code), code(d.Component), code(d.Message))
		}
	}

	b.WriteString("\n## Feature gates\n\n")
	switch {
	case len(c.FeatureGates.Unknown) > 0:
		fmt.Fprintf(&b, "Not compared: the %s runtime does not list its feature gates.\n", strings.Join(c.FeatureGates.Unknown, " and "))
	case len(c.FeatureGates.Changes) == 0:
		b.WriteString("No feature gate of the configuration's components, or of the Collector itself, changed.\n")
	default:
		b.WriteString("| Gate | Component | From | To |\n|---|---|---|---|\n")
		for _, g := range c.FeatureGates.Changes {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", code(g.ID), code(g.Component), gate(g.From), gate(g.To))
		}
	}

	b.WriteString("\n## Scenarios\n\n")
	if len(c.Scenarios) == 0 {
		b.WriteString("No scenario was compared.\n")
	} else {
		b.WriteString("| Scenario | Result | Assertions (from) | Assertions (to) | Inputs identical | Runs |\n|---|---|---|---|---|---|\n")
		for _, s := range c.Scenarios {
			result := code(string(s.Outcome))
			if s.Error != "" {
				result = "not compared: " + code(s.Error)
			}
			identical := "no"
			if s.InputsIdentical {
				identical = "yes"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s %s |\n", code(s.Name), result,
				code(string(s.BeforeAssertions)), code(string(s.AfterAssertions)), identical, code(s.Before), code(s.After))
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

func runtimeRelease(r Runtime) string {
	if r.Verified {
		return code(r.CollectorVersion)
	}
	return code(r.CollectorVersion) + " (unverified)"
}

func valid(v bool) string {
	if v {
		return "valid"
	}
	return "invalid"
}

func gate(g *GateState) string {
	if g == nil {
		return "absent"
	}
	on := "off"
	if g.Enabled {
		on = "on"
	}
	return code(g.Stage) + " " + on
}

// code renders s as a code span that its own backticks or a table's pipes
// cannot break; empty is empty.
func code(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "|", "\\|")
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}
