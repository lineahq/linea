package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/instrument"
)

func TestExecuteUsesOneCapturedConfigurationAcrossPlanningStartupAndReference(t *testing.T) {
	dir := t.TempDir()
	root, child := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "resource.yaml")
	text := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {resource: '${file:" + child + "}'}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [resource], exporters: [otlp]}}}\n"
	if err := os.WriteFile(root, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(child, []byte("attributes: [{key: captured, value: '${env:OTEL_EXECUTE_CAPTURE}', action: upsert}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXECUTE_CAPTURE", "original")
	var mutationErr error
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rep, err := Execute(ctx, Request{Config: root, Fixture: tracesFixture, Compare: true, Observer: func(e instrument.Event) {
		if e.Kind != instrument.KindRunStarted {
			return
		}
		if err := os.WriteFile(root, []byte("invalid: ["), 0o600); err != nil {
			mutationErr = err
		}
		if err := os.Remove(child); err != nil {
			mutationErr = err
		}
		if err := os.Setenv("OTEL_EXECUTE_CAPTURE", "mutated"); err != nil {
			mutationErr = err
		}
	}})
	if err != nil || mutationErr != nil {
		t.Fatalf("execute = %v; mutation = %v", err, mutationErr)
	}
	if !rep.Comparison.Equal {
		t.Fatalf("reference configuration drifted: %+v", rep.Comparison)
	}
	for _, output := range rep.Result.Outputs {
		if !bytes.Contains(output.Data, []byte("original")) || bytes.Contains(output.Data, []byte("mutated")) {
			t.Fatalf("startup reread providers: %s", output.Data)
		}
	}
	sum := sha256.Sum256([]byte(text))
	if rep.Provenance.Config.Path != root || rep.Provenance.Config.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("provenance changed: %+v", rep.Provenance.Config)
	}
}
