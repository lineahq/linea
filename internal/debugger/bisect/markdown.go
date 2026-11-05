package bisect

import (
	"fmt"
	"strings"
)

// Markdown renders the bisection for an incident note or a pull request.
// Headings and prose are fixed text and numbers; IDs, messages and names
// appear only in code spans inside tables and lists.
func (b *Bisection) Markdown() string {
	var s strings.Builder
	fmt.Fprintf(&s, "# Config bisect: %s\n\n", b.Verdict)
	s.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&s, "| Agent | %s |\n", code(b.Target.AgentID))
	fmt.Fprintf(&s, "| Config | %s %s |\n", code(b.Target.ConfigName), code(b.Target.ConfigID))
	fmt.Fprintf(&s, "| Entry | %s |\n", code(b.Target.Entry))
	fmt.Fprintf(&s, "| Range | %d to %d |\n", b.Good, b.Bad)
	switch b.Verdict {
	case Found:
		fmt.Fprintf(&s, "| First bad | %d |\n| Last good | %d |\n", b.FirstBad, b.LastGood)
	case Inconclusive:
		if b.FirstBad > 0 && b.LastGood > 0 {
			fmt.Fprintf(&s, "| First bad | between %d and %d |\n", b.LastGood+1, b.FirstBad)
		}
	}
	fmt.Fprintf(&s, "| Probes | %d |\n", b.Probes)
	if len(b.Reasons) > 0 {
		s.WriteString("\n## Why the bisection is inconclusive\n\n")
		for _, r := range b.Reasons {
			fmt.Fprintf(&s, "- %s: %s\n", code(r.Code), code(r.Message))
		}
	}

	s.WriteString("\n## Layers\n\n| Entry | Config | Version |\n|---|---|---|\n")
	for _, l := range b.Target.Layers {
		version := fmt.Sprint(l.Version)
		if l.Searched {
			version = "searched"
		}
		fmt.Fprintf(&s, "| %s | %s | %s |\n", code(l.Entry), code(l.ConfigID), version)
	}
	switch {
	case b.Target.LocalFiles == 1:
		s.WriteString("\nMerged over 1 local supervisor file.\n")
	case b.Target.LocalFiles > 1:
		fmt.Fprintf(&s, "\nMerged over %d local supervisor files.\n", b.Target.LocalFiles)
	}

	s.WriteString("\n## Versions\n\n| Version | Status | Created | Message | Rollouts | Runs |\n|---|---|---|---|---|---|\n")
	for _, v := range b.Versions {
		status := string(v.Status)
		if v.Reason != "" {
			status += ": " + code(v.Reason)
		}
		var rollouts, runs []string
		for _, r := range v.Rollouts {
			rollouts = append(rollouts, code(r.ID)+" "+r.State)
		}
		for _, r := range v.Scenarios {
			runs = append(runs, code(r.Name)+" "+string(r.Outcome)+" "+code(r.Run))
		}
		fmt.Fprintf(&s, "| %d | %s | %s | %s | %s | %s |\n", v.Version, status, v.CreatedAt.UTC().Format("2006-01-02 15:04"),
			code(v.Message), strings.Join(rollouts, ", "), strings.Join(runs, "<br>"))
	}

	if c := b.Culprit; c != nil {
		fmt.Fprintf(&s, "\n## Version %d against version %d\n\n", c.Version, c.LastGood)
		for _, sc := range c.Scenarios {
			fmt.Fprintf(&s, "- %s: assertions %s, then %s; runs %s and %s", code(sc.Name), sc.GoodAssertions, sc.BadAssertions, code(sc.Good), code(sc.Bad))
			switch {
			case sc.Error != "":
				fmt.Fprintf(&s, "; not compared: %s", code(sc.Error))
			case sc.Comparison != nil:
				fmt.Fprintf(&s, "; comparison %s", sc.Comparison.Outcome)
				if !sc.InputsIdentical {
					s.WriteString(", inputs not proven identical")
				}
			}
			s.WriteString("\n")
		}
		if c.Diff != "" {
			fence := "```"
			for strings.Contains(c.Diff, fence) {
				fence += "`"
			}
			fmt.Fprintf(&s, "\n%sdiff\n%s%s\n", fence, c.Diff, fence)
		}
	}
	for i, n := range b.Notes {
		if i == 0 {
			s.WriteString("\n## Notes\n\n")
		}
		fmt.Fprintf(&s, "- %s\n", n)
	}
	return s.String()
}

// code puts text in a code span that its own backticks cannot end, and
// keeps table cells on one line.
func code(text string) string {
	if text == "" {
		return ""
	}
	text = strings.NewReplacer("\r", " ", "\n", " ", "|", "\\|").Replace(text)
	fence := "`"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		text = " " + text + " "
	}
	return fence + text + fence
}
