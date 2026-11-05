package platform

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

const captureDestination = `receivers:
  otlp:
    protocols:
      grpc: {}
exporters:
  debug: {}
service:
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [debug]
    traces:
      receivers: [otlp]
      exporters: [debug]
    metrics:
      receivers: [otlp]
      exporters: [debug]
`

func TestCaptureDataAdmissionCursorDerivationAndDeletion(t *testing.T) {
	f, _ := dataFixture(t, true, false)
	ctx := context.Background()
	v := f.start(t)
	ingestLog(t, f, v.ID, "one") // before the target confirmed: fleet decides delivery
	ingestLog(t, f, v.ID, "two")
	page, err := f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{Limit: 1})
	if err != nil || page.Watermark != 2 || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("%+v %v", page, err)
	}
	text := string(page.Items[0].Payload)
	for _, wanted := range []string{"9223372036854775806", "1791980000123456789", "resource-schema", "scope-schema", "scope"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("lost %s: %s", wanted, text)
		}
	}
	if page.Items[0].Stream != (CaptureStream{CollectorID: f.fleet.peers[0].ID, Pipeline: "logs", Point: "egress", Signal: "logs"}) {
		t.Fatalf("stream %+v", page.Items[0].Stream)
	}
	ingestLog(t, f, v.ID, "later")
	next, err := f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{Limit: 1, Cursor: page.NextCursor})
	if err != nil || next.Watermark != 2 || next.Items[0].Sequence != 2 || next.NextCursor != "" {
		t.Fatalf("live cursor changed %+v %v", next, err)
	}
	_, err = f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{Limit: 1, Cursor: page.NextCursor, Stream: CaptureStream{Signal: "logs"}})
	wantCode(t, err, CodeInvalidRequest)
	sel := CaptureInputSelection{Stream: page.Items[0].Stream, Watermark: page.Watermark, Sequences: []int{1}, Receiver: "otlp"}
	fixture, err := f.s.CreateFixtureFromCapture(ctx, testCaller, v.ID, CaptureDeriveRequest{ConfigYAML: captureDestination, Selection: sel, Save: true})
	if err != nil || fixture.Saved == nil || fixture.Derivation.Records != 1 {
		t.Fatalf("%+v %v", fixture, err)
	}
	if strings.Contains(string(fixture.Payload), "capture.session.id") || !strings.Contains(string(fixture.Payload), "9223372036854775806") {
		t.Fatal(string(fixture.Payload))
	}
	scenario, err := f.s.CreateScenarioFromCapture(ctx, testCaller, v.ID, CaptureScenarioRequest{ConfigYAML: captureDestination, Inputs: []CaptureInputSelection{sel}, Save: true})
	if err != nil || scenario.Saved == nil || !strings.Contains(scenario.YAML, "one") {
		t.Fatalf("%+v %v", scenario, err)
	}
	report, err := f.s.ExecuteRun(ctx, testCaller, RunRequest{Mode: ModeScenario, Config: Source{Text: captureDestination}, Scenario: &ScenarioInput{Scenario: Source{Text: scenario.YAML}}})
	if err != nil || report == nil {
		t.Fatalf("ordinary scenario replay %v", err)
	}
	exported, err := f.s.ExportCaptureSelection(ctx, testCaller, v.ID, CaptureExportRequest{Fixture: &CaptureDeriveRequest{ConfigYAML: captureDestination, Selection: sel}})
	if err != nil || exported.Content != string(fixture.Payload) || exported.Digest != fixture.Derivation.Digest {
		t.Fatalf("export %+v %v", exported, err)
	}
	wantCode(t, f.s.DeleteCaptureSession(ctx, testCaller, v.ID), CodeConflict)
	if _, err := f.s.StopCaptureSession(ctx, testCaller, v.ID); err != nil {
		t.Fatal(err)
	}
	ingestLog(t, f, v.ID, "draining") // delivered after the stop, before fleet confirmed it
	current := f.finish(t, v.ID, "stopped")
	if current.Data.State != "frozen" || current.Data.ReceivedRecords != 4 {
		t.Fatalf("%+v", current)
	}
	if err := f.s.DeleteCaptureSession(ctx, testCaller, v.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{})
	wantCode(t, err, CodeNotFound)
	revision, err := f.s.repository.Revision(ctx, fixture.Saved.Revision.Ref)
	if err != nil || string(revision.Content) != string(fixture.Payload) || revision.Revision.Origin.Kind != "capture" {
		t.Fatalf("derived copy changed %+v %v", revision, err)
	}
}

func TestCaptureDataDurableRestartIncludingRaw(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "redacted", true: "raw"}[raw], func(t *testing.T) {
			f, data := dataFixture(t, true, raw)
			v := dispatchData(t, f)
			ingestLog(t, f, v.ID, "sensitive-original-payload")
			f.s.Close()
			s, err := openMemory(data, Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			view, err := s.GetCaptureSession(context.Background(), testCaller, v.ID)
			if err != nil || !view.Terminal || view.State != "uncertain" || view.Reason != "platform_restarted; capture_not_resumed" || view.Targets[0].State != "unknown" {
				t.Fatalf("%+v %v", view, err)
			}
			page, err := s.GetCaptureRecords(context.Background(), testCaller, v.ID, CaptureRecordsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			// Raw records are stored like redacted ones, without the Collector's routing tags.
			if view.Data.ContainsRaw != raw || view.Data.MemoryOnly || view.Data.State != "frozen" ||
				len(page.Items) != 1 || page.Items[0].Sensitivity != map[bool]string{false: "redacted", true: "raw"}[raw] ||
				!strings.Contains(string(page.Items[0].Payload), "sensitive-original-payload") ||
				strings.Contains(string(page.Items[0].Payload), "capture.") {
				t.Fatalf("%+v %+v", view.Data, page.Items)
			}
		})
	}
}

func TestCaptureDataRefusesForeignBatchesAndBoundsPages(t *testing.T) {
	f, _ := dataFixture(t, false, false)
	v := dispatchData(t, f)
	ctx := context.Background()
	raw := captureLogBytes(t, "valid")
	peer := f.fleet.peers[0]
	retag := func(change func(pcommon.Map)) []byte {
		p, _ := telemetry.DecodeProto("logs", raw)
		resource, _ := p.ResourceAt(0)
		change(resource.Attributes())
		out, _ := p.Proto()
		return out
	}
	for name, e := range map[string]fleetcapture.Envelope{
		"unknown peer":     {PeerID: strings.Repeat("e", 64), Generation: 1, Data: raw},
		"old generation":   {PeerID: peer.ID, Generation: 2, Data: raw},
		"other capture":    {PeerID: peer.ID, Generation: 1, CaptureID: strings.Repeat("c", 32), Data: raw},
		"other encoding":   {PeerID: peer.ID, Generation: 1, Encoding: "otlp-json", Data: raw},
		"unselected":       {PeerID: peer.ID, Generation: 1, Signal: "traces", Data: raw},
		"not protobuf":     {PeerID: peer.ID, Generation: 1, Data: []byte("not otlp")},
		"missing tag":      {PeerID: peer.ID, Generation: 1, Data: retag(func(a pcommon.Map) { a.Remove("capture.point") })},
		"wrong tag type":   {PeerID: peer.ID, Generation: 1, Data: retag(func(a pcommon.Map) { a.PutInt("capture.pipeline", 1) })},
		"unselected point": {PeerID: peer.ID, Generation: 1, Data: retag(func(a pcommon.Map) { a.PutStr("capture.point", "ingress") })},
	} {
		if accepted := f.fleet.acked(t, v.ID, f.fleet.enqueue(v.ID, e)); accepted != 0 {
			t.Fatalf("%s: accepted %d", name, accepted)
		}
	}
	view, err := f.s.GetCaptureSession(ctx, testCaller, v.ID)
	if err != nil || view.Data.ReceivedRecords != 0 || view.Data.RejectedRecords != 9 {
		t.Fatalf("%+v %v", view.Data, err)
	}
	// A redelivered batch is acknowledged again but stored once.
	batch := f.fleet.enqueue(v.ID, fleetcapture.Envelope{PeerID: peer.ID, Generation: 1, Data: raw})
	f.fleet.acked(t, v.ID, batch)
	if again := f.fleet.acked(t, v.ID, f.fleet.enqueue(v.ID, fleetcapture.Envelope{BatchID: batch, PeerID: peer.ID, Generation: 1, Data: raw})); again != 1 {
		t.Fatalf("redelivery acknowledged %d", again)
	}
	ingestLog(t, f, v.ID, strings.Repeat("x", 64000))
	f.s.budgets.PageBytes = 1024
	page, err := f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{})
	if err != nil || page.Watermark != 2 {
		t.Fatalf("%+v %v", page, err)
	}
	for _, item := range page.Items {
		if item.Sequence == 2 && (!item.Oversize || item.Payload != nil) {
			t.Fatalf("oversize record inlined: %+v", item)
		}
	}
	if accepted := f.deliver(t, v.ID, 0, captureLogBytes(t, strings.Repeat("x", 66000)), 1); accepted != 0 {
		t.Fatal("oversize record accepted")
	}
	encoded, _ := json.Marshal(page)
	if len(encoded) > 1024 {
		t.Fatal("page exceeded budget")
	}
}

func TestCaptureDataNormalBatchAndSharedContextExpansion(t *testing.T) {
	for _, expanded := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal_exporter_batch", true: "shared_context_budget"}[expanded], func(t *testing.T) {
			f, _ := dataFixture(t, false, false)
			v := dispatchData(t, f)
			p, _ := telemetry.DecodeProto("logs", captureLogBytes(t, "record"))
			data := plog.NewLogs()
			if expanded {
				resource := data.ResourceLogs().AppendEmpty()
				p.Logs.ResourceLogs().At(0).CopyTo(resource)
				resource.Resource().Attributes().PutStr("shared", strings.Repeat("x", 16<<10))
				logs := resource.ScopeLogs().At(0).LogRecords()
				for i := 1; i < 1000; i++ {
					logs.At(0).CopyTo(logs.AppendEmpty())
				}
			} else {
				// The Collector may batch copies that each retain their own context.
				for i := 0; i < 1000; i++ {
					p.Logs.ResourceLogs().At(0).CopyTo(data.ResourceLogs().AppendEmpty())
				}
			}
			raw, _ := telemetry.FromLogs(data).Proto()
			if len(raw) > 1<<20 {
				t.Fatal("test batch exceeds transport limit")
			}
			accepted := f.deliver(t, v.ID, 0, raw, 1000)
			view, err := f.s.GetCaptureSession(context.Background(), testCaller, v.ID)
			if err != nil || view.Data.ReceivedBytes > captureSessionBytes || view.Data.ReceivedRecords != accepted || accepted+view.Data.RejectedRecords != 1000 {
				t.Fatalf("%+v %v", view, err)
			}
			if expanded {
				if accepted == 0 || view.Data.RejectedRecords == 0 || view.StopReason == "" {
					t.Fatalf("expansion not bounded: %d %+v", accepted, view)
				}
				f.eventually(t, v.ID, func(v *CaptureSession) bool { return v.State == "stopping" })
			} else if accepted != 1000 {
				t.Fatalf("ordinary batch rejected: %d", accepted)
			}
		})
	}
}

func TestCaptureDataDestinationSetupAndCatalogDrift(t *testing.T) {
	f, _ := dataFixture(t, false, false)
	v := dispatchData(t, f)
	ingestLog(t, f, v.ID, "before drift")
	page, err := f.s.GetCaptureRecords(context.Background(), testCaller, v.ID, CaptureRecordsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	req := CaptureDeriveRequest{
		ConfigYAML: strings.ReplaceAll(captureDestination, "receivers: [otlp]", "receivers: ['${env:INPUT_RECEIVER}']"),
		SetupYAML:  "environment:\n  INPUT_RECEIVER: otlp\n",
		Selection:  CaptureInputSelection{Stream: page.Items[0].Stream, Watermark: page.Watermark, Receiver: "otlp"},
	}
	if _, err := f.s.CreateFixtureFromCapture(context.Background(), testCaller, v.ID, req); err != nil {
		t.Fatal(err)
	}
	// A catalog that now labels the stream raw stops the capture; batches the
	// target still sends are refused.
	f.fleet.setPeer(0, func(p *fleetcapture.Peer) { p.Identity.CapturePoints[0].Sensitivity = "raw" })
	f.advance(peerFreshness)
	view := f.eventually(t, v.ID, func(v *CaptureSession) bool {
		return v.StopReason == "capture_catalog_changed" && v.State == "stopping"
	})
	if view.Data.ContainsRaw {
		t.Fatal("drift relabelled stored records")
	}
	if accepted := f.deliver(t, v.ID, 0, captureLogBytes(t, "after drift"), 1); accepted != 0 {
		t.Fatal("changed sensitivity admitted")
	}
}

func TestCaptureDataUnsavedProvenanceSurvivesQuickScenarioAndRestart(t *testing.T) {
	f, data := dataFixture(t, true, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v := dispatchData(t, f)
	ingestLog(t, f, v.ID, "unsaved capture input")
	page, err := f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sel := CaptureInputSelection{Stream: page.Items[0].Stream, Watermark: page.Watermark, Sequences: []int{1}, Receiver: "otlp"}
	const setup = "environment: {CAPTURE_DECLARED: preserved}\n"
	fixture, err := f.s.CreateFixtureFromCapture(ctx, testCaller, v.ID, CaptureDeriveRequest{ConfigYAML: captureDestination, SetupYAML: setup, Selection: sel})
	if err != nil || fixture.Saved != nil || fixture.Derivation.SourceDigest == "" {
		t.Fatalf("%+v %v", fixture, err)
	}
	identity := f.fleet.peers[0].Identity
	if fixture.Derivation.Source == nil || fixture.Derivation.Source.Pod != identity.Pod || fixture.Derivation.Source.CollectorVersion != identity.CollectorVersion || fixture.Derivation.SourceSensitivity != "redacted" {
		t.Fatal("admission identity/version/sensitivity snapshot lost")
	}
	derived, err := f.s.CreateScenarioFromCapture(ctx, testCaller, v.ID, CaptureScenarioRequest{ConfigYAML: captureDestination, SetupYAML: setup, Inputs: []CaptureInputSelection{sel}})
	if err != nil || derived.Saved != nil {
		t.Fatalf("%+v %v", derived, err)
	}
	requests := []RunRequest{
		{Mode: ModeQuick, Config: Source{Text: captureDestination}, Quick: &QuickInput{Fixture: Source{Text: fixture.FixtureJSON}, Receiver: fixture.Receiver, Setup: fixture.SetupYAML}},
		{Mode: ModeScenario, Config: Source{Text: captureDestination}, Scenario: &ScenarioInput{Scenario: Source{Text: derived.YAML}}},
	}
	ids := []string{}
	for _, req := range requests {
		run, err := f.s.SubmitRun(ctx, testCaller, req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.GetRunReport(ctx, testCaller, run.ID, true); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	f.finish(t, v.ID, "completed")
	if err := f.s.DeleteCaptureSession(ctx, testCaller, v.ID); err != nil {
		t.Fatal(err)
	}
	f.s.Close()
	reopened, err := openMemory(data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	for i, id := range ids {
		inputs, err := reopened.GetRunInputs(ctx, testCaller, id)
		if err != nil {
			t.Fatal(err)
		}
		var doc *scenario.Scenario
		if i == 0 {
			if inputs.Fixture == nil || inputs.Fixture.Text != fixture.FixtureJSON || inputs.Setup == nil {
				t.Fatal("quick input bytes lost")
			}
			doc, err = scenario.ParseSetup([]byte(inputs.Setup.Text))
		} else {
			if inputs.Scenario == nil {
				t.Fatal("scenario input bytes lost")
			}
			doc, err = scenario.Parse([]byte(inputs.Scenario.Text), "")
		}
		if err != nil || !reflect.DeepEqual(doc.CaptureOrigins, []CaptureDerivation{fixture.Derivation}) || doc.Environment["CAPTURE_DECLARED"] != "preserved" {
			t.Fatalf("provenance not pinned: %+v %v", doc, err)
		}
		exported, err := reopened.ExportScenario(ctx, testCaller, id, ScenarioFromRunRequest{})
		if err != nil {
			t.Fatal(err)
		}
		doc, err = scenario.Parse([]byte(exported.YAML), "")
		if err != nil || !reflect.DeepEqual(doc.CaptureOrigins, []CaptureDerivation{fixture.Derivation}) {
			t.Fatalf("export dropped lineage: %+v %v", doc, err)
		}
	}
}

func TestCaptureDataConcurrentDeliveryReadAndStop(t *testing.T) {
	f, _ := dataFixture(t, false, false)
	v := dispatchData(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw := captureLogBytes(t, "concurrent record")
	peer := f.fleet.peers[0]
	batches := make(chan string, 80)
	start := make(chan struct{})
	errs := make(chan error, 128)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			<-start
			for range 20 {
				batches <- f.fleet.enqueue(v.ID, fleetcapture.Envelope{PeerID: peer.ID, Generation: 1, Data: raw})
			}
		})
	}
	workers.Go(func() {
		<-start
		for range 20 {
			if _, err := f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{Limit: 5}); err != nil {
				errs <- err
			}
		}
	})
	workers.Go(func() {
		<-start
		if _, err := f.s.StopCaptureSession(ctx, testCaller, v.ID); err != nil {
			errs <- err
		}
	})
	close(start)
	workers.Wait()
	close(errs)
	close(batches)
	for err := range errs {
		t.Error(err)
	}
	for batch := range batches {
		if accepted := f.fleet.acked(t, v.ID, batch); accepted != 1 {
			t.Fatalf("batch accepted %d", accepted)
		}
	}
	view := f.finish(t, v.ID, "stopped")
	if view.Data.State != "frozen" || view.Data.ReceivedRecords != 80 {
		t.Fatalf("%+v", view)
	}
	// Captures have no retention period: a day later the records are still there.
	f.advance(24 * time.Hour)
	page, err := f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{})
	if err != nil || page.Watermark != 80 {
		t.Fatalf("%+v %v", page, err)
	}
}

func TestCaptureDataSavedFixtureProvenanceInheritedByQuick(t *testing.T) {
	f, _ := dataFixture(t, true, false)
	v := dispatchData(t, f)
	ctx := context.Background()
	ingestLog(t, f, v.ID, "saved fixture input")
	page, err := f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := f.s.CreateFixtureFromCapture(ctx, testCaller, v.ID, CaptureDeriveRequest{ConfigYAML: captureDestination, Selection: CaptureInputSelection{Stream: page.Items[0].Stream, Watermark: page.Watermark, Receiver: "otlp"}, Save: true})
	if err != nil {
		t.Fatal(err)
	}
	ref := &ArtifactRef{ArtifactID: fixture.Saved.Revision.Ref.ArtifactID, RevisionID: fixture.Saved.Revision.Ref.RevisionID}
	run, err := f.s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Text: captureDestination}, Quick: &QuickInput{Fixture: Source{Artifact: ref}, Receiver: "otlp", Setup: "environment: {MY_DECLARED: preserved}\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetRunReport(ctx, testCaller, run.ID, true); err != nil {
		t.Fatal(err)
	}
	inputs, err := f.s.GetRunInputs(ctx, testCaller, run.ID)
	if err != nil || inputs.Setup == nil || inputs.Fixture == nil || inputs.Fixture.Artifact == nil {
		t.Fatalf("%+v %v", inputs, err)
	}
	doc, err := scenario.ParseSetup([]byte(inputs.Setup.Text))
	if err != nil || !reflect.DeepEqual(doc.CaptureOrigins, []CaptureDerivation{fixture.Derivation}) || doc.Environment["MY_DECLARED"] != "preserved" {
		t.Fatalf("saved origin lost: %+v %v", doc, err)
	}
}

func TestCaptureDataTargetSourcesRemainSeparate(t *testing.T) {
	ctx := context.Background()
	f := newFleetFixture(t, nil, fakePeer(0, logsEgress), fakePeer(1, logsEgress))
	f.req.CollectorIDs = []string{f.fleet.peers[0].ID, f.fleet.peers[1].ID}
	v := dispatchData(t, f)
	for peer := range 2 {
		if accepted := f.deliver(t, v.ID, peer, captureLogBytes(t, "same body"), 1); accepted != 1 {
			t.Fatalf("peer %d accepted %d", peer, accepted)
		}
	}
	page, err := f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{})
	if err != nil || len(page.Items) != 2 || page.Items[0].Stream.CollectorID == page.Items[1].Stream.CollectorID {
		t.Fatalf("target sources merged: %+v %v", page, err)
	}
	selections := []CaptureInputSelection{}
	for _, item := range page.Items {
		selections = append(selections, CaptureInputSelection{Stream: item.Stream, Watermark: page.Watermark, Sequences: []int{item.Sequence}, Receiver: "otlp"})
	}
	derived, err := f.s.CreateScenarioFromCapture(ctx, testCaller, v.ID, CaptureScenarioRequest{ConfigYAML: captureDestination, Inputs: selections})
	if err != nil || len(derived.Derivations) != 2 || derived.Derivations[0].SourceDigest == derived.Derivations[1].SourceDigest {
		t.Fatalf("target provenance lost: %+v %v", derived, err)
	}
}

func TestCaptureDataDerivationCapacityAndScenarioProgress(t *testing.T) {
	f, _ := dataFixture(t, false, false)
	v := dispatchData(t, f)
	ctx := context.Background()
	ingestLog(t, f, v.ID, "bounded derivation")
	page, err := f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sel := CaptureInputSelection{Stream: page.Items[0].Stream, Watermark: page.Watermark, Receiver: "otlp"}
	releaseFirst, err := f.s.beginCaptureDerivation(ctx, testCaller, authz.CaptureRead)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	releaseSecond, err := f.s.beginCaptureDerivation(ctx, testCaller, authz.CaptureRead)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.CreateFixtureFromCapture(ctx, testCaller, v.ID, CaptureDeriveRequest{ConfigYAML: captureDestination, Selection: sel})
	wantCode(t, err, CodeResourceExhausted)
	_, err = f.s.CreateScenarioFromCapture(ctx, testCaller, v.ID, CaptureScenarioRequest{ConfigYAML: captureDestination, Inputs: []CaptureInputSelection{sel}})
	wantCode(t, err, CodeResourceExhausted)
	releaseSecond()
	// One free slot must be enough for a scenario and all of its input derivations.
	derived, err := f.s.CreateScenarioFromCapture(ctx, testCaller, v.ID, CaptureScenarioRequest{ConfigYAML: captureDestination, Inputs: []CaptureInputSelection{sel}})
	if err != nil || len(derived.Derivations) != 1 {
		t.Fatalf("nested derivation needs extra capacity: %+v %v", derived, err)
	}
}

func TestCaptureScenarioRejectsIncompatibleReceiverWithSignalHint(t *testing.T) {
	f, _ := dataFixture(t, false, false)
	v := dispatchData(t, f)
	ingestLog(t, f, v.ID, "capture remains available")
	page, err := f.s.GetCaptureRecords(context.Background(), testCaller, v.ID, CaptureRecordsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sel := CaptureInputSelection{Stream: page.Items[0].Stream, Watermark: page.Watermark, Sequences: []int{1}, Receiver: "otlp"}
	req := CaptureScenarioRequest{ConfigYAML: strings.Replace(captureDestination, "    logs:\n      receivers: [otlp]\n      exporters: [debug]\n", "", 1), Inputs: []CaptureInputSelection{sel}}
	_, err = f.s.CreateScenarioFromCapture(context.Background(), testCaller, v.ID, req)
	if ErrorCode(err) != CodeInvalidRequest || !strings.Contains(err.Error(), `receiver "otlp" does not accept logs`) || !strings.Contains(err.Error(), "logs pipeline") {
		t.Fatalf("actionable validation: %v", err)
	}
	req.ConfigYAML = captureDestination
	result, err := f.s.CreateScenarioFromCapture(context.Background(), testCaller, v.ID, req)
	if err != nil || len(result.Derivations) != 1 || !strings.Contains(result.YAML, "capture remains available") {
		t.Fatalf("compatible retry: %+v %v", result, err)
	}
}

func TestCaptureDataFinishedCaptureIsReadFromWorkspace(t *testing.T) {
	f, _ := dataFixture(t, true, true)
	v := dispatchData(t, f)
	ctx := context.Background()
	ingestLog(t, f, v.ID, "first")
	ingestLog(t, f, v.ID, "second")
	f.finish(t, v.ID, "completed")
	r := f.s.collectors
	r.mu.Lock()
	memory, onDisk := r.memoryBytes, r.sessions[v.ID].onDisk
	r.mu.Unlock()
	if memory != 0 || !onDisk {
		t.Fatalf("memory=%d onDisk=%v", memory, onDisk)
	}
	page, err := f.s.GetCaptureRecords(ctx, testCaller, v.ID, CaptureRecordsRequest{})
	if err != nil || page.Watermark != 2 || !strings.Contains(string(page.Items[1].Payload), "second") {
		t.Fatalf("%+v %v", page, err)
	}
	if err := f.s.DeleteCaptureSession(ctx, testCaller, v.ID); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	cached := r.loaded.id
	r.mu.Unlock()
	if cached != "" {
		t.Fatal("deleted capture stayed cached")
	}
}
