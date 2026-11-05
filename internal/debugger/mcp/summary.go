package mcp

import (
	"fmt"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// summaryFacts is the summary's part of the tool text: server-computed facts only.
func summaryFacts(s *platform.RunSummary) string {
	text := ""
	if s.ExecutionStatus != "" {
		text += fmt.Sprintf("; execution %s", s.ExecutionStatus)
	}
	if !s.Complete {
		text += "; observations incomplete"
	}
	if s.StandIns > 0 {
		text += "; " + plural(s.StandIns, "stand-in component")
	}
	if s.StandInReferences > 0 {
		text += "; " + plural(s.StandInReferences, "stand-in reference")
	}
	if s.Assertions != nil {
		text += fmt.Sprintf("; assertions %s", s.Assertions.Outcome)
	}
	text += fmt.Sprintf("; %s, %s", plural(len(s.Stages), "stage"), plural(len(s.Outputs), "output"))
	if len(s.EndpointOutputs) > 0 {
		text += ", " + plural(len(s.EndpointOutputs), "endpoint output")
	}
	if l := s.ComponentLogs; l != nil {
		text += fmt.Sprintf(", %s (%d warning(s), %d error(s))", plural(l.Total, "component log line"), l.Warnings, l.Errors)
	}
	return text + ", " + plural(len(s.Errors), "error")
}
