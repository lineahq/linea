package engine

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/lineahq/linea/internal/debugger/assertion"
)

// JUnit XML follows the widely consumed Surefire/Jenkins layout: one
// testsuite per scenario and one testcase per assertion. Inconclusive is
// reported as a failure of type "inconclusive", never as skipped, because
// CI systems treat skipped tests as passing.
type JUnitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []JUnitSuite `xml:"testsuite"`
}

type JUnitSuite struct {
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Errors     int             `xml:"errors,attr"`
	Time       string          `xml:"time,attr"`
	Properties []JUnitProperty `xml:"properties>property"`
	Cases      []JUnitCase     `xml:"testcase"`
}

type JUnitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type JUnitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *JUnitProblem `xml:"failure,omitempty"`
	Error     *JUnitProblem `xml:"error,omitempty"`
}

type JUnitProblem struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// WriteJUnit writes the test report as JUnit XML. It is a method so adapters
// can encode a platform result without importing this package.
func (rep *TestReport) WriteJUnit(w io.Writer) error {
	root := JUnitSuites{Name: "linea"}
	total := 0.0
	for _, s := range rep.Scenarios {
		name := s.Name
		if name == "" {
			name = s.Path
		}
		suite := JUnitSuite{
			Name: name,
			Time: seconds(s.Duration),
			Properties: []JUnitProperty{
				{Name: "config", Value: rep.Config},
				{Name: "scenario", Value: s.Path},
				{Name: "outcome", Value: string(s.Outcome)},
			},
		}
		total += s.Duration
		if s.Error != "" {
			suite.Cases = append(suite.Cases, JUnitCase{
				Name: "setup", Classname: name,
				Error: &JUnitProblem{Message: s.Error, Type: string(assertion.Error), Text: s.Error},
			})
		} else {
			suite.Properties = append(suite.Properties,
				JUnitProperty{Name: "run_id", Value: s.RunID},
				JUnitProperty{Name: "run_status", Value: string(s.Assertions.RunStatus)})
			for _, r := range s.Assertions.Results {
				suite.Cases = append(suite.Cases, junitCaseOf(name, r))
			}
		}
		for _, c := range suite.Cases {
			switch {
			case c.Error != nil:
				suite.Errors++
			case c.Failure != nil:
				suite.Failures++
			}
		}
		suite.Tests = len(suite.Cases)
		root.Tests += suite.Tests
		root.Failures += suite.Failures
		root.Errors += suite.Errors
		root.Suites = append(root.Suites, suite)
	}
	root.Time = seconds(total)
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(root); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func junitCaseOf(suite string, r assertion.Result) JUnitCase {
	c := JUnitCase{Name: r.Name, Classname: suite}
	if r.Outcome == assertion.Pass {
		return c
	}
	var text strings.Builder
	var first string
	fmt.Fprintf(&text, "target: %s\n", r.Target)
	for _, ch := range r.Checks {
		if ch.Outcome == assertion.Pass {
			continue
		}
		line := ch.Line()
		if first == "" {
			first = line
		}
		fmt.Fprintln(&text, line)
		if ch.Message != "" && ch.Expected != "" {
			fmt.Fprintf(&text, "  %s\n", ch.Message)
		}
		for _, v := range ch.Violations {
			fmt.Fprintf(&text, "  at %s: %s\n", v.Location, v.Observed)
		}
	}
	p := &JUnitProblem{Message: first, Type: string(r.Outcome), Text: text.String()}
	if r.Outcome == assertion.Error {
		c.Error = p
	} else {
		c.Failure = p
	}
	return c
}

func seconds(s float64) string { return fmt.Sprintf("%.3f", s) }
