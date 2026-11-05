package bisect

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/lineahq/linea/internal/debugger/assertion"
)

func TestComposeMergesLikeTheSupervisor(t *testing.T) {
	local := []Part{{Name: "local.yaml", Body: []byte(`
receivers: {otlp: {protocols: {grpc: {}}}}
exporters: {debug: {}}
extensions: {health_check: {}}
service:
  extensions: [health_check]
  telemetry: {resource: {attributes: [{name: a, value: local}, {name: b, value: local}]}}
  pipelines: {logs: {receivers: [otlp], exporters: [debug]}}
`)}}
	remote := []Part{
		// Out of order on purpose: entries merge in name order.
		{Name: "20-override", Body: []byte(`
processors: {transform: {log_statements: ["set(log.body, \"20\")"]}}
service:
  extensions: [pprof, health_check]
  telemetry: {resource: {attributes: [{name: b, value: remote}]}}
  pipelines: {logs: {processors: [transform]}}
`)},
		{Name: "10-base", Body: []byte(`
processors: {transform: {log_statements: ["set(log.body, \"10\")"]}, batch: {}}
exporters: {otlp: {endpoint: "${env:BACKEND}"}}
extensions: {pprof: {}}
service:
  pipelines: {logs: {processors: [batch, transform], exporters: [otlp]}}
`)},
	}
	raw, err := Compose(local, remote)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	service := got["service"].(map[string]any)
	logs := service["pipelines"].(map[string]any)["logs"].(map[string]any)
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"lists are replaced by the later entry", logs["processors"], []any{"transform"}},
		{"keys merge from every document", logs["receivers"], []any{"otlp"}},
		{"the later entry wins a list", logs["exporters"], []any{"otlp"}},
		{"the higher entry wins a scalar", got["processors"].(map[string]any)["transform"].(map[string]any)["log_statements"], []any{`set(log.body, "20")`}},
		{"references stay unexpanded", got["exporters"].(map[string]any)["otlp"].(map[string]any)["endpoint"], "${env:BACKEND}"},
		{"extension lists concatenate without duplicates", service["extensions"], []any{"health_check", "pprof"}},
		{"resource attributes merge by name", service["telemetry"].(map[string]any)["resource"].(map[string]any)["attributes"],
			[]any{map[string]any{"name": "a", "value": "local"}, map[string]any{"name": "b", "value": "remote"}}},
	} {
		if !equal(c.got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, c.got, c.want)
		}
	}
}

func TestComposeNamesTheInvalidDocument(t *testing.T) {
	_, err := Compose(nil, []Part{{Name: "10-base", Body: []byte("receivers: [")}})
	if err == nil || !strings.HasPrefix(err.Error(), "10-base: ") {
		t.Fatalf("got %v, want an error naming 10-base", err)
	}
}

func equal(a, b any) bool {
	x, _ := yaml.Marshal(a)
	y, _ := yaml.Marshal(b)
	return string(x) == string(y)
}

// statuses builds a probe from a status per candidate and records the
// order it was asked in.
func statuses(s string, asked *[]int) Probe {
	return func(_ context.Context, i int) (Status, error) {
		*asked = append(*asked, i)
		switch s[i] {
		case 'g':
			return Good, nil
		case 'b':
			return Bad, nil
		}
		return Skipped, nil
	}
}

func TestSearchFindsTheFirstBadVersion(t *testing.T) {
	for _, c := range []struct {
		name, candidates   string
		verdict            Verdict
		firstBad, lastGood int
		maxProbes          int
	}{
		{"first of two", "gb", Found, 1, 0, 2},
		{"in the middle", "gggggbbbbb", Found, 5, 4, 6},
		{"right after good", "gbbbbbbbbbbbbbbb", Found, 1, 0, 6},
		{"last", "gggggggggggggggb", Found, 15, 14, 6},
		{"around a skipped version", "ggsgbbb", Found, 4, 3, 5},
		{"only skipped versions between", "ggssbb", Inconclusive, 4, 1, 6},
		{"bad bound passes", "gggg", NoRegression, -1, 3, 1},
		{"good bound fails", "bbbb", AlreadyBad, 0, -1, 2},
		{"single bad version", "b", AlreadyBad, 0, -1, 1},
		{"bad bound not judged", "gggs", Inconclusive, -1, -1, 1},
		{"good bound not judged", "sggb", Inconclusive, 3, -1, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			var asked []int
			out, err := Search(context.Background(), len(c.candidates), statuses(c.candidates, &asked))
			if err != nil {
				t.Fatal(err)
			}
			if out.Verdict != c.verdict || out.FirstBad != c.firstBad || out.LastGood != c.lastGood {
				t.Fatalf("got %s first bad %d last good %d, want %s %d %d", out.Verdict, out.FirstBad, out.LastGood, c.verdict, c.firstBad, c.lastGood)
			}
			if out.Probes != len(asked) || out.Probes > c.maxProbes {
				t.Fatalf("%d probes (%v), want at most %d", out.Probes, asked, c.maxProbes)
			}
			if len(asked) > 1 && (asked[0] != len(c.candidates)-1 || asked[1] != 0) {
				t.Fatalf("probed %v, want the bad bound, then the good bound first", asked)
			}
			for i, s := range out.Statuses {
				if (s == Untested) == slices.Contains(asked, i) {
					t.Fatalf("status %d is %s, asked %v", i, s, asked)
				}
			}
		})
	}
}

func TestSearchStopsWhenTheProbeFails(t *testing.T) {
	stop := errors.New("caller left")
	_, err := Search(context.Background(), 8, func(_ context.Context, i int) (Status, error) {
		if i == 0 {
			return "", stop
		}
		return Bad, nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("got %v, want the probe's error", err)
	}
}

func TestJudge(t *testing.T) {
	for _, c := range []struct {
		outcomes []assertion.Outcome
		want     Status
	}{
		{[]assertion.Outcome{assertion.Pass, assertion.Pass}, Good},
		{[]assertion.Outcome{assertion.Pass, assertion.Fail}, Bad},
		{[]assertion.Outcome{assertion.Error, assertion.Fail}, Bad},
		{[]assertion.Outcome{assertion.Pass, assertion.Inconclusive}, Skipped},
		{[]assertion.Outcome{assertion.Error}, Skipped},
		{nil, Skipped},
	} {
		var results []ScenarioResult
		for _, o := range c.outcomes {
			results = append(results, ScenarioResult{Outcome: o})
		}
		if got := Judge(results); got != c.want {
			t.Errorf("Judge(%v) = %s, want %s", c.outcomes, got, c.want)
		}
	}
}

func TestAssembleExplainsAnInconclusiveSearch(t *testing.T) {
	versions := make([]Version, 6)
	for i := range versions {
		versions[i] = Version{Version: i + 10, CreatedAt: time.Unix(0, 0)}
	}
	out, err := Search(context.Background(), 6, statuses("ggssbb", new([]int)))
	if err != nil {
		t.Fatal(err)
	}
	b := Assemble(Target{AgentID: "a", ConfigID: "c", Entry: "10-base"}, versions, out)
	if b.Good != 10 || b.Bad != 15 || b.LastGood != 11 || b.FirstBad != 14 {
		t.Fatalf("bounds %d-%d, last good %d, first bad %d", b.Good, b.Bad, b.LastGood, b.FirstBad)
	}
	if len(b.Reasons) != 1 || b.Reasons[0].Code != ReasonSkippedBetween {
		t.Fatalf("reasons %+v", b.Reasons)
	}
	if b.Versions[2].Status != Skipped || b.Versions[0].Status != Good {
		t.Fatalf("statuses %+v", b.Versions)
	}
	md := b.Markdown()
	for _, want := range []string{"# Config bisect: inconclusive", "| First bad | between 12 and 14 |", "`skipped_versions_between`"} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestMarkdownKeepsNamesInCodeSpans(t *testing.T) {
	b := &Bisection{Verdict: Found, Target: Target{ConfigName: "a|b`c"}, Culprit: &Culprit{Version: 2, LastGood: 1, Diff: "-x\n+```\n"}}
	md := b.Markdown()
	if !strings.Contains(md, "``a\\|b`c``") {
		t.Errorf("config name not escaped:\n%s", md)
	}
	if !strings.Contains(md, "````diff\n") {
		t.Errorf("diff fence not longer than the diff's backticks:\n%s", md)
	}
}
