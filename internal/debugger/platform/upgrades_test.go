package platform

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/build"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// upgradeService has two built runtimes standing for Collector 0.160 and
// 0.162 and the built-in one.
func upgradeService(t *testing.T) (*Service, *fakeRuntimes) {
	t.Helper()
	store := newFakeRuntimes("c160", "c162")
	old := store.images["c160"]
	old.CollectorVersion = "0.160.0"
	store.images["c160"] = old
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Runtimes: store})
	t.Cleanup(s.Close)
	return s, store
}

func upgradeRequest() UpgradeCheckRequest {
	return UpgradeCheckRequest{From: "c160", To: "c162", Config: Source{Path: logsCleanup("config.yaml")},
		Scenarios: []ScenarioInput{{Scenario: Source{Path: logsCleanup("scenario.yaml")}}}}
}

func TestAnUpgradeWithTheSameBehaviorIsCompatible(t *testing.T) {
	s, store := upgradeService(t)
	store.gates = map[string][]release.Gate{
		"c160": {{ID: "processor.transform.Flag", Stage: "alpha"}, {ID: "receiver.kafka.Unused", Stage: "alpha"}},
		"c162": {{ID: "processor.transform.Flag", Stage: "beta", Enabled: true}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := s.CheckUpgrade(ctx, testCaller, upgradeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if c.Verdict != UpgradeCompatible || c.From.CollectorVersion != "0.160.0" || c.To.CollectorVersion != "0.162.0" || len(c.Reasons) != 0 {
		t.Fatalf("check = %+v", c)
	}
	if len(c.Scenarios) != 1 {
		t.Fatalf("scenarios = %+v", c.Scenarios)
	}
	sc := c.Scenarios[0]
	if sc.Outcome != ComparisonSame || !sc.InputsIdentical || !strings.HasPrefix(sc.InputsDigest, "sha256:") ||
		sc.Name != logsCleanup("scenario.yaml") || sc.BeforeAssertions != "pass" || sc.AfterAssertions != "pass" {
		t.Fatalf("scenario = %+v", sc)
	}
	if sc.Comparison.Before.InputsDigest != sc.Comparison.After.InputsDigest {
		t.Fatal("the runs' inputs differ")
	}
	for id, name := range map[string]string{sc.Before: "c160", sc.After: "c162"} {
		rep, err := s.GetRunReport(ctx, testCaller, id, false)
		if err != nil || rep.Runtime == nil || rep.Runtime.Name != name {
			t.Fatalf("run %s: %v, runtime %+v, want %s", id, err, rep.Runtime, name)
		}
	}
	if g := c.FeatureGates.Changes; len(g) != 1 || g[0].ID != "processor.transform.Flag" || g[0].Component != "processors/transform" ||
		g[0].From.Stage != "alpha" || g[0].To.Stage != "beta" {
		t.Fatalf("gates = %+v", c.FeatureGates)
	}
	if !strings.Contains(c.Markdown(), "# Upgrade check: compatible") {
		t.Fatalf("markdown:\n%s", c.Markdown())
	}
}

func TestAnUpgradeThatChangesTheOutputChangesBehavior(t *testing.T) {
	s, store := upgradeService(t)
	store.mutate = map[string]func(*engine.Report){"c162": func(rep *engine.Report) {
		for k, raw := range rep.Result.Snapshots {
			rep.Result.Snapshots[k] = bytes.ReplaceAll(raw, []byte(`"WARNING"`), []byte(`"Warning"`))
		}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := s.CheckUpgrade(ctx, testCaller, upgradeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if c.Verdict != UpgradeBehaviorChanges || c.Scenarios[0].Outcome != ComparisonDifferent || !c.Scenarios[0].InputsIdentical {
		t.Fatalf("check = %+v, scenario %+v", c, c.Scenarios[0])
	}
	if c.FeatureGates.Unknown == nil {
		t.Fatal("runtimes listing no gates were compared")
	}
}

func TestAnUpgradeTheTargetRefusesBreaksTheConfiguration(t *testing.T) {
	s, store := upgradeService(t)
	removed := config.Diagnostic{Severity: config.SeverityError, Code: "invalid_config", Component: "processors::transform/severity",
		Message: "'' has invalid keys: error_mode"}
	store.inspect = map[string]func(*engine.Inspection){"c162": func(ins *engine.Inspection) {
		ins.Valid = false
		ins.Diagnostics = append(ins.Diagnostics, removed)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := s.CheckUpgrade(ctx, testCaller, upgradeRequest())
	if err != nil {
		t.Fatal(err)
	}
	if c.Verdict != UpgradeConfigurationBreaks || c.Configuration.ToValid || !c.Configuration.FromValid ||
		len(c.Configuration.Added) != 1 || c.Configuration.Added[0] != removed || len(c.Scenarios) != 0 {
		t.Fatalf("check = %+v", c)
	}
	runs, err := allRuns(ctx, s, testCaller, RunFilter{})
	if err != nil || len(runs) != 0 {
		t.Fatalf("%d runs, %v: a refused configuration ran", len(runs), err)
	}
}

func TestAnUpgradeCheckNeedsTwoAvailableRuntimes(t *testing.T) {
	s, _ := upgradeService(t)
	ctx := context.Background()
	for _, tc := range []struct {
		from, to string
		code     Code
	}{
		{"c162", "c162", CodeInvalidRequest},
		{"", BuiltInRuntime, CodeInvalidRequest},
		{"c160", "c163", CodeUnavailable},
	} {
		req := upgradeRequest()
		req.From, req.To = tc.from, tc.to
		if _, err := s.CheckUpgrade(ctx, testCaller, req); ErrorCode(err) != tc.code {
			t.Errorf("%s to %s: %v, want %s", tc.from, tc.to, err, tc.code)
		}
	}
	// The built-in runtime is a side like any other.
	req := upgradeRequest()
	req.From = ""
	c, err := s.CheckUpgrade(ctx, testCaller, req)
	if err != nil || c.From.Name != BuiltInRuntime || c.From.CollectorVersion != release.CollectorVersion || c.Verdict != UpgradeCompatible {
		t.Fatalf("check = %+v, %v", c, err)
	}
}

func TestRetargetManifestMovesItToARelease(t *testing.T) {
	s, _ := upgradeService(t)
	ctx := context.Background()
	out, err := s.RetargetManifest(ctx, testCaller, RetargetManifestRequest{Manifest: Source{Text: string(build.Manifest)}, Version: "0.160.0"})
	if err != nil {
		t.Fatal(err)
	}
	if out.From != release.CollectorVersion || out.To != "0.160.0" || !strings.Contains(out.Manifest, "otelcol_version: 0.160.0") ||
		len(out.Missing) != 0 || len(out.Custom) != 0 || len(out.Moved) == 0 {
		t.Fatalf("retargeted = %+v", out)
	}
	path := t.TempDir() + "/manifest.yaml"
	if err := os.WriteFile(path, []byte(out.Manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	back, err := s.RetargetManifest(ctx, testCaller, RetargetManifestRequest{Manifest: Source{Path: path}, Version: release.CollectorVersion})
	if err != nil || back.Manifest != string(build.Manifest) {
		t.Fatalf("moving back to the built-in release: %v\n%s", err, back.Manifest)
	}
	if _, err := s.RetargetManifest(ctx, testCaller, RetargetManifestRequest{Manifest: Source{Text: string(build.Manifest)}, Version: "0.150.0"}); ErrorCode(err) != CodeNotFound {
		t.Fatalf("an unverified release: %v", err)
	}
	if _, err := s.RetargetManifest(ctx, testCaller, RetargetManifestRequest{Manifest: Source{Text: "receivers: [{gomod: x}]"}}); ErrorCode(err) != CodeValidationFailed {
		t.Fatalf("an invalid manifest: %v", err)
	}
}

func TestAnAbandonedUpgradeCheckCancelsItsRuns(t *testing.T) {
	s, store := upgradeService(t)
	store.execute = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.CheckUpgrade(ctx, testCaller, upgradeRequest())
		done <- err
	}()
	deadline := time.After(time.Minute)
	for {
		runs, err := allRuns(context.Background(), s, testCaller, RunFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the check admitted no runs")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned check: %v", err)
	}
	runs, err := allRuns(context.Background(), s, testCaller, RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// Executions block until store.execute closes, which it never does: the
	// waits end only because the check cancelled both runs.
	for _, r := range runs {
		_, _ = s.GetRunReport(context.Background(), testCaller, r.ID, true)
		if r, err := s.GetRun(context.Background(), testCaller, r.ID); err != nil || r.State == StateDone {
			t.Errorf("run %s: %v, state %s %s", r.ID, err, r.State, r.Error)
		}
	}
}

// TestARuntimeOnConnectedWorkersListsItsGates: a team server's runtimes run
// on connected workers, whose hello lists the gates an upgrade check
// compares, so they are not unknown there.
func TestARuntimeOnConnectedWorkersListsItsGates(t *testing.T) {
	gates := []release.Gate{{ID: "processor.transform.Flag", Stage: "Beta", Enabled: true}}
	for name, h := range map[string]worker.Hello{"listed": {Runtime: "sha256:a", Gates: gates}, "not listed": {Runtime: "sha256:b"}} {
		t.Run(name, func(t *testing.T) {
			b := pooledRuntime(nil, "up", h, nil)
			l, ok := b.worker.(gateLister)
			if !ok {
				t.Fatalf("%T lists no gates", b.worker)
			}
			if got := l.FeatureGates(); !reflect.DeepEqual(got, h.Gates) {
				t.Fatalf("gates = %+v, want %+v", got, h.Gates)
			}
		})
	}
}
