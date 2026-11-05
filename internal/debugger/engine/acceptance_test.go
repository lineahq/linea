package engine

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
)

// The acceptance tests that need no web UI and are not covered by a
// dedicated test elsewhere.
// They run testdata/acceptance/config.yaml unchanged.
var (
	acceptance       = filepath.Join("..", "..", "..", "testdata", "acceptance")
	acceptanceConfig = filepath.Join(acceptance, "config.yaml")
)

const ordersSpan = "span:5b8efff798038103d269b633813fc60c:eee19b7ec3c1b174"

func stageOf(t *testing.T, rep *Report, component, pipeline string) analysis.Stage {
	t.Helper()
	for _, s := range rep.Stages {
		if s.Component == component && s.Pipeline == pipeline {
			return s
		}
	}
	t.Fatalf("no stage %s in %s", component, pipeline)
	return analysis.Stage{}
}

// changeAt returns the change to path on the orders span, failing when the
// diff has none.
func changeAt(t *testing.T, d *analysis.Diff, path string) analysis.Change {
	t.Helper()
	for _, c := range d.Changes {
		if c.Entity == ordersSpan && c.Path == path {
			return c
		}
	}
	t.Fatalf("no change to %s on the orders span: %+v", path, d.Changes)
	return analysis.Change{}
}

func paths(d *analysis.Diff) []string {
	var out []string
	for _, c := range d.Changes {
		out = append(out, c.Path)
	}
	return out
}

func ottlOf(t *testing.T, rep *Report, processor string) *ottldebug.Report {
	t.Helper()
	for _, r := range rep.OTTL {
		if r.Processor == processor {
			return r
		}
	}
	t.Fatalf("no OTTL replay for %s", processor)
	return nil
}

func TestAcceptanceSuite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{Config: acceptanceConfig, Scenario: filepath.Join(acceptance, "scenario.yaml"), Compare: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Result.Status != runresult.StatusCompleted || !rep.Comparison.Equal {
		t.Fatalf("run %s, comparison %+v", rep.Result.Status, rep.Comparison)
	}

	t.Run("one log record is visible at each boundary", func(t *testing.T) {
		var injected, in, out, exported int
		stages := map[string]bool{}
		for _, e := range rep.Result.Events {
			if e.Signal != "logs" && e.Kind != instrument.KindInput && e.Kind != instrument.KindExport {
				continue
			}
			items := 0
			if e.Counts != nil {
				items = e.Counts.Items
			}
			switch {
			case e.Kind == instrument.KindInput && items == 1:
				injected++
			case e.Kind == instrument.KindIngress && e.Signal == "logs":
				in++
				stages[e.Component+" in"] = items == 1
			case e.Kind == instrument.KindEgress && e.Signal == "logs":
				out++
				stages[e.Component+" out"] = items == 1
			case e.Kind == instrument.KindExport && items == 1:
				exported++
			}
		}
		for _, b := range []string{"attributes/logs in", "attributes/logs out", "batch in", "batch out"} {
			if !stages[b] {
				t.Errorf("record not visible at %s: %v", b, stages)
			}
		}
		if injected != 1 || in != 2 || out != 2 || exported < 1 {
			t.Errorf("injected %d, stage inputs %d, stage outputs %d, exports %d", injected, in, out, exported)
		}
		var logs *runresult.Output
		for i, o := range rep.Result.Outputs {
			if o.Signal == "logs" {
				logs = &rep.Result.Outputs[i]
			}
		}
		if logs == nil || logs.Counts.Items != 1 || !strings.Contains(string(logs.Data), "order 42 accepted") {
			t.Errorf("exporter output = %+v", logs)
		}
	})

	add := stageOf(t, rep, "transform/add", "traces").Inputs[0]
	change := stageOf(t, rep, "transform/change", "traces").Inputs[0]

	t.Run("transform adds an attribute", func(t *testing.T) {
		c := changeAt(t, add.Diff, `attributes["stage"]`)
		if c.Op != analysis.OpAdded || c.After != "add" || c.Before != nil {
			t.Fatalf("change = %+v", c)
		}
	})

	t.Run("transform changes an attribute", func(t *testing.T) {
		c := changeAt(t, change.Diff, `attributes["http.method"]`)
		if c.Op != analysis.OpModified || c.Before != "GET" || c.After != "POST" {
			t.Fatalf("change = %+v", c)
		}
	})

	t.Run("transform deletes an attribute", func(t *testing.T) {
		c := changeAt(t, change.Diff, `attributes["user.password"]`)
		if c.Op != analysis.OpRemoved || c.Before != "hunter2" || c.After != nil {
			t.Fatalf("change = %+v", c)
		}
	})

	t.Run("filter drop is classified", func(t *testing.T) {
		filter := stageOf(t, rep, "filter/health", "traces")
		in := filter.Inputs[0]
		// The filter emitted the surviving span synchronously, so the dropped
		// span is a removal from that emission, not missing output.
		if filter.Emission != analysis.EmissionSynchronous || in.Outcome != analysis.OutcomeEmitted ||
			len(in.Diff.Removed) != 1 || in.Diff.Removed[0].Name != "GET /health" || len(in.Diff.Changes) != 0 {
			t.Fatalf("filter = %+v diff %+v", filter, in.Diff)
		}
	})

	replay := ottlOf(t, rep, "transform/add")

	t.Run("false OTTL condition is shown as unmatched", func(t *testing.T) {
		st := replay.Groups[0].Statements[1]
		if !strings.Contains(st.Text, `"gold"`) || st.Matched != 0 || st.NotMatched != 2 || len(st.Changes) != 0 || len(st.Errors) != 0 {
			t.Fatalf("statement = %+v", st)
		}
		if replay.Verification != ottldebug.VerificationReplayVerified {
			t.Fatalf("verification = %s", replay.Verification)
		}
	})

	t.Run("OTTL error and its handling are visible", func(t *testing.T) {
		g := replay.Groups[0]
		st := g.Statements[2]
		if g.ErrorMode != "ignore" || len(st.Errors) != 2 || st.Errors[0].Phase != "function" || st.Errors[0].Message == "" {
			t.Fatalf("group %s, statement = %+v", g.ErrorMode, st)
		}
		// error_mode ignore: processing continued and the stage emitted.
		if replay.Error != "" || add.Outcome != analysis.OutcomeEmitted || add.Error != "" {
			t.Fatalf("replay error %q, stage input %+v", replay.Error, add)
		}
	})

	t.Run("sequential processors are attributed to their stages", func(t *testing.T) {
		if got := strings.Join(paths(add.Diff), ","); got != `attributes["stage"],attributes["stage"]` {
			t.Errorf("transform/add changes = %s", got)
		}
		if got := strings.Join(paths(change.Diff), ","); got != `attributes["http.method"],attributes["user.password"]` {
			t.Errorf("transform/change changes = %s", got)
		}
		// Each OTTL statement's changes belong to its own processor.
		if st := ottlOf(t, rep, "transform/change").Groups[0].Statements; len(st[0].Changes) != 1 || len(st[1].Changes) != 1 {
			t.Errorf("transform/change statements = %+v", st)
		}
	})

	t.Run("batch output is recorded after asynchronous emission", func(t *testing.T) {
		batch := stageOf(t, rep, "batch", "traces")
		in := batch.Inputs[0]
		if batch.Emission != analysis.EmissionAsynchronous || in.Outcome != analysis.OutcomeDeferred ||
			len(batch.Emissions) != 1 || batch.Emissions[0] <= in.Seq ||
			batch.Reconciliation == nil || !batch.Reconciliation.Empty() {
			t.Fatalf("batch = %+v", batch)
		}
		// With a one-hour timeout the flush comes from shutdown.
		stopped := 0
		for _, e := range rep.Result.Events {
			if e.Kind == instrument.KindComponentStopped && stopped == 0 {
				stopped = e.Seq
			}
		}
		if batch.Emissions[0] < stopped {
			t.Fatalf("batch emitted at seq %d, before shutdown began at %d", batch.Emissions[0], stopped)
		}
	})
}

// listener counts TCP connections to a local address.
type listener struct {
	net.Listener
	conns atomic.Int32
}

func listen(t *testing.T) *listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &listener{Listener: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			l.conns.Add(1)
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return l
}

// TestAcceptanceNetworkIsolation checks that the original OTLP
// exporter stays configured, pointed at a reachable local endpoint, and a
// fixture carrying credentials and a card number is replayed. Nothing may
// connect to that endpoint, and the receiver must not bind its port.
func TestAcceptanceNetworkIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	backend, receiver := listen(t), listen(t)

	abs, err := filepath.Abs(acceptance)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.ToSlash(abs)
	yaml := "version: 1\nname: isolation\ninputs:\n" +
		"  - {receiver: otlp, file: '" + dir + "/traces.json'}\n" +
		"  - {receiver: otlp, file: '" + dir + "/logs.json'}\n" +
		"environment:\n  BACKEND_ENDPOINT: " + backend.Addr().String() + "\n  RECEIVER_ENDPOINT: " + receiver.Addr().String() + "\n"
	path := filepath.Join(t.TempDir(), "isolation.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := Execute(ctx, Request{Config: acceptanceConfig, Scenario: path})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Result.Status != runresult.StatusCompleted {
		t.Fatalf("status %s: %s", rep.Result.Status, rep.Result.StatusReason)
	}
	c, ok := rep.Topology.Component("exporter/otlp/backend")
	if !ok || c.Mode != "captured" {
		t.Fatalf("exporter = %+v", c)
	}
	captured := ""
	for _, o := range rep.Result.Outputs {
		captured += string(o.Data)
	}
	if !strings.Contains(captured, "4111111111111111") {
		t.Fatal("sensitive span was not captured locally")
	}
	if n := backend.conns.Load(); n != 0 {
		t.Fatalf("the configured exporter endpoint received %d connection(s)", n)
	}
	if n := receiver.conns.Load(); n != 0 {
		t.Fatalf("the receiver endpoint received %d connection(s)", n)
	}
}
