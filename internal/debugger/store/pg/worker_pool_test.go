package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// largeLogs is an OTLP logs document of about 2 MiB.
func largeLogs() string {
	var b strings.Builder
	b.WriteString(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"accounts"}}]},"scopeLogs":[{"scope":{"name":"accounts.log"},"logRecords":[`)
	filler := strings.Repeat("x", 400)
	for i := range 4000 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"timeUnixNano":"%d","severityText":"info","body":{"stringValue":"record %d %s"}}`, 1760000000000000000+int64(i), i, filler)
	}
	b.WriteString(`]}]}]}`)
	return b.String()
}

// TestAConnectedWorkerMovesLargePayloadsThroughObjectStorage runs a run on
// a connected worker of a service on PostgreSQL and object storage: its
// bundle and its report are above worker.InlineLimit, so the worker
// downloads the one and uploads the other through presigned URLs, and the
// staged report is gone once the run is stored.
func TestAConnectedWorkerMovesLargePayloadsThroughObjectStorage(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const token = "registration-token-for-the-store-tests"
	store := f.open(t, artifact.Limits{RunBytes: 64 << 20, StorageBytes: 256 << 20})
	s, err := platform.Open(store, platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, WorkerToken: token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	caller := platform.Local("test")
	hello, err := worker.Identity()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRuntime(ctx, caller, platform.RegisterRuntimeRequest{
		Name: "team", Image: "registry.example/linea-runtime:team", Hello: raw}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(s.ConnectWorker))
	t.Cleanup(srv.Close)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Connect(workerCtx, worker.ConnectOptions{URL: srv.URL, Token: token, Name: "team", Capacity: 2})
	}()
	t.Cleanup(func() {
		stopWorker()
		if err := <-done; err != nil {
			t.Errorf("Connect = %v", err)
		}
	})

	config := filepath.Join("..", "..", "..", "..", "samples", "logs-cleanup", "config.yaml")
	req := platform.RunRequest{Mode: platform.ModeQuick, Runtime: "team", Config: platform.Source{Path: config},
		Quick: &platform.QuickInput{Fixture: platform.Source{Text: largeLogs()}}}
	// A registered runtime queues the run until its worker connects.
	run, err := s.SubmitRun(ctx, caller, req)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := store.RunBundle(ctx, run.ID)
	if err != nil || len(bundle) <= worker.InlineLimit {
		t.Fatalf("bundle of %d bytes, %v: want one above the inline limit", len(bundle), err)
	}
	rep, err := s.GetRunReport(ctx, caller, run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runtime == nil || rep.Runtime.Name != "team" || rep.Result == nil || rep.Result.Status != "completed" {
		t.Fatalf("report runtime %+v, result %+v", rep.Runtime, rep.Result)
	}
	var encoded bytes.Buffer
	if err := engine.Encode(&encoded, rep); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() <= worker.InlineLimit {
		t.Fatalf("report of %d bytes: want one above the inline limit", encoded.Len())
	}
	if !json.Valid(encoded.Bytes()) {
		t.Fatal("the stored report is not JSON")
	}
	var staged []string
	if err := store.objects.List(ctx, stagingPrefix(store.prefix), func(o ObjectInfo) error { staged = append(staged, o.Key); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(staged) != 0 {
		t.Fatalf("staged objects left behind: %v", staged)
	}
}
