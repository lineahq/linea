package platform

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// Config management in the fake fleet: layers per agent, versions per
// config and the retained rollouts.
type fakeConfigs struct {
	layers   map[string][]fleetcapture.DesiredLayer
	versions map[string][]fleetcapture.ConfigVersion // oldest first
	rollouts []fleetcapture.Rollout
}

func (f *fakeFleet) AgentConfig(_ context.Context, agentID string) (*fleetcapture.AgentConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("AgentConfig"); err != nil {
		return nil, err
	}
	layers, ok := f.configs.layers[agentID]
	if !ok {
		return nil, refusal(404, "agent not found")
	}
	return &fleetcapture.AgentConfig{AgentID: agentID, Desired: &fleetcapture.DesiredConfig{Layers: layers, Hash: "h"}}, nil
}

func (f *fakeFleet) StoredConfig(_ context.Context, id string) (*fleetcapture.StoredConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vs, ok := f.configs.versions[id]
	if !ok {
		return nil, refusal(404, "config not found")
	}
	return &fleetcapture.StoredConfig{ID: id, Name: id + "-name", LatestVersion: vs[len(vs)-1].Version}, nil
}

// ConfigVersions pages newest first, as fleet does.
func (f *fakeFleet) ConfigVersions(_ context.Context, id string, after, limit int) (*fleetcapture.ConfigVersionPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	page := &fleetcapture.ConfigVersionPage{Items: []fleetcapture.ConfigVersion{}}
	vs := f.configs.versions[id]
	for i := len(vs) - 1; i >= 0; i-- {
		if after > 0 && vs[i].Version >= after {
			continue
		}
		if len(page.Items) == limit {
			page.NextAfter = strconv.Itoa(page.Items[limit-1].Version)
			break
		}
		v := vs[i]
		v.Body = ""
		page.Items = append(page.Items, v)
	}
	return page, nil
}

func (f *fakeFleet) ConfigVersion(_ context.Context, id string, version int) (*fleetcapture.ConfigVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("ConfigVersion"); err != nil {
		return nil, err
	}
	for _, v := range f.configs.versions[id] {
		if v.Version == version {
			return &v, nil
		}
	}
	return nil, refusal(404, "version not found")
}

func (f *fakeFleet) Rollouts(_ context.Context, _ string, _ int) (*fleetcapture.RolloutPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("Rollouts"); err != nil {
		return nil, err
	}
	return &fleetcapture.RolloutPage{Items: f.configs.rollouts}, nil
}

const bisectAgent = "0123456789abcdef0123456789abcdef"

// bisectFixture serves the logs-cleanup sample as two fleet layers: 10-base
// with everything but the processors and 20-processors, whose versions are
// built by processors. The searched config is "processors".
func bisectFixture(t *testing.T, processors func(version int) string, versions int) (*Service, *fakeFleet) {
	t.Helper()
	raw, err := os.ReadFile(logsCleanup("config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	full := string(raw)
	cut := strings.Index(full, "processors:\n")
	end := strings.Index(full, "exporters:\n")
	base := full[:cut] + full[end:]
	base = strings.Replace(base, "      processors: [resource, attributes/scrub, filter/health, transform/severity, batch]\n", "", 1)
	fleet := newFakeFleet()
	fleet.configs.layers = map[string][]fleetcapture.DesiredLayer{bisectAgent: {
		{Name: "20-processors", AssignmentID: "as-p", ConfigID: "processors", Version: versions},
		{Name: "10-base", AssignmentID: "as-b", ConfigID: "base", Version: 1},
	}}
	fleet.configs.versions = map[string][]fleetcapture.ConfigVersion{"base": {{ConfigID: "base", Version: 1, Body: base}}}
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for v := 1; v <= versions; v++ {
		fleet.configs.versions["processors"] = append(fleet.configs.versions["processors"], fleetcapture.ConfigVersion{
			ConfigID: "processors", Version: v, Message: "change " + strconv.Itoa(v), CreatedAt: created.Add(time.Duration(v) * time.Hour), Body: processors(v)})
	}
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Capture: CaptureOptions{fleet: fleet}})
	t.Cleanup(s.Close)
	return s, fleet
}

// sampleProcessors is the sample's processors section with the pipeline's
// processor list; drop removes a processor from the list.
func sampleProcessors(t *testing.T, drop string) string {
	t.Helper()
	raw, err := os.ReadFile(logsCleanup("config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	full := string(raw)
	section := full[strings.Index(full, "processors:\n"):strings.Index(full, "exporters:\n")]
	list := "resource, attributes/scrub, filter/health, transform/severity, batch"
	if drop != "" {
		list = strings.Replace(list, drop+", ", "", 1)
	}
	return section + "service:\n  pipelines:\n    logs:\n      processors: [" + list + "]\n"
}

func bisectRequest() BisectRequest {
	return BisectRequest{AgentID: bisectAgent, ConfigID: "processors", Scenarios: []ScenarioInput{{Scenario: Source{Path: logsCleanup("scenario.yaml")}}}}
}

func TestABisectFindsTheVersionThatBrokeTheScenario(t *testing.T) {
	good, bad := sampleProcessors(t, ""), sampleProcessors(t, "filter/health")
	s, fleet := bisectFixture(t, func(v int) string {
		if v >= 5 {
			return bad
		}
		return good
	}, 7)
	fleet.configs.rollouts = []fleetcapture.Rollout{
		{ID: "r5", AssignmentID: "as-p", ConfigID: "processors", FromVersion: 4, ToVersion: 5, State: "completed"},
		{ID: "other", AssignmentID: "as-b", ConfigID: "base", ToVersion: 5, State: "completed"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := s.BisectFleetConfig(ctx, testCaller, bisectRequest())
	if err != nil {
		t.Fatal(err)
	}
	if b.Verdict != BisectFound || b.FirstBad != 5 || b.LastGood != 4 || b.Good != 1 || b.Bad != 7 || len(b.Reasons) != 0 {
		t.Fatalf("bisection = %+v", b)
	}
	if b.Probes > 5 || b.Target.ConfigName != "processors-name" || b.Target.Entry != "20-processors" ||
		len(b.Target.Layers) != 2 || b.Target.Layers[0].Entry != "10-base" || !b.Target.Layers[1].Searched {
		t.Fatalf("probes %d, target %+v", b.Probes, b.Target)
	}
	if r := b.Versions[4].Rollouts; len(r) != 1 || r[0].ID != "r5" {
		t.Fatalf("rollouts of version 5 = %+v", r)
	}
	c := b.Culprit
	if c == nil || c.Version != 5 || c.LastGood != 4 || !strings.Contains(c.Diff, "-      processors: [resource, attributes/scrub, filter/health,") {
		t.Fatalf("culprit = %+v", c)
	}
	if len(c.Scenarios) != 1 || c.Scenarios[0].GoodAssertions != "pass" || c.Scenarios[0].BadAssertions != "fail" ||
		!c.Scenarios[0].InputsIdentical || c.Scenarios[0].Comparison == nil {
		t.Fatalf("culprit scenarios = %+v", c.Scenarios)
	}
	if !strings.Contains(b.Markdown(), "# Config bisect: found") {
		t.Fatalf("markdown:\n%s", b.Markdown())
	}
}

func TestABisectSkipsAVersionThatDoesNotLoad(t *testing.T) {
	good, bad := sampleProcessors(t, ""), sampleProcessors(t, "filter/health")
	s, _ := bisectFixture(t, func(v int) string {
		switch {
		case v == 2:
			return strings.Replace(good, "batch]", "batch, missing]", 1)
		case v >= 3:
			return bad
		}
		return good
	}, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := s.BisectFleetConfig(ctx, testCaller, bisectRequest())
	if err != nil {
		t.Fatal(err)
	}
	if b.Verdict != BisectInconclusive || b.LastGood != 1 || b.FirstBad != 3 || b.Versions[1].Status != BisectSkipped ||
		!strings.Contains(b.Versions[1].Reason, "invalid") || b.Culprit == nil || b.Culprit.LastGood != 1 || b.Culprit.Version != 3 {
		t.Fatalf("bisection = %+v, versions %+v", b, b.Versions)
	}
}

func TestABisectWhoseBadBoundPassesFindsNoRegression(t *testing.T) {
	good := sampleProcessors(t, "")
	s, fleet := bisectFixture(t, func(int) string { return good }, 3)
	fleet.fail("Rollouts", refusal(503, "unavailable"))
	b, err := s.BisectFleetConfig(context.Background(), testCaller, bisectRequest())
	if err != nil {
		t.Fatal(err)
	}
	if b.Verdict != BisectNoRegression || b.Probes != 1 || b.Culprit != nil || !strings.Contains(strings.Join(b.Notes, "\n"), "rollouts could not be read") {
		t.Fatalf("bisection = %+v", b)
	}
}

func TestABisectRefusesWhatItCannotSearch(t *testing.T) {
	good := sampleProcessors(t, "")
	s, fleet := bisectFixture(t, func(int) string { return good }, 3)
	unconnected := New(Options{})
	t.Cleanup(unconnected.Close)
	if _, err := unconnected.BisectFleetConfig(context.Background(), testCaller, bisectRequest()); ErrorCode(err) != CodeUnavailable ||
		!strings.Contains(err.Error(), "--fleet-url") {
		t.Fatalf("without fleet: %v", err)
	}
	for name, c := range map[string]struct {
		req  func(*BisectRequest)
		code Code
	}{
		"no scenarios":           {func(r *BisectRequest) { r.Scenarios = nil }, CodeInvalidRequest},
		"reversed range":         {func(r *BisectRequest) { r.Good, r.Bad = 3, 2 }, CodeInvalidRequest},
		"an unnamed config":      {func(r *BisectRequest) { r.ConfigID = "" }, CodeInvalidRequest},
		"a config not sent":      {func(r *BisectRequest) { r.ConfigID = "elsewhere" }, CodeInvalidRequest},
		"an unknown agent":       {func(r *BisectRequest) { r.AgentID = strings.Repeat("f", 32) }, CodeNotFound},
		"a pruned bound":         {func(r *BisectRequest) { r.Good = 1; r.Bad = 9 }, CodeNotFound},
		"a single version range": {func(r *BisectRequest) { r.Good = 3 }, CodeInvalidRequest},
	} {
		req := bisectRequest()
		c.req(&req)
		if _, err := s.BisectFleetConfig(context.Background(), testCaller, req); ErrorCode(err) != c.code {
			t.Errorf("%s: got %v, want %s", name, err, c.code)
		}
	}
	fleet.fail("ConfigVersion", refusal(403, "admin only"))
	if _, err := s.BisectFleetConfig(context.Background(), testCaller, bisectRequest()); ErrorCode(err) != CodeForbidden {
		t.Fatalf("body refused: %v", err)
	}
}
