package config

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/registry"
)

func TestCaptureSnapshotMatchesCaptureSourceWith(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	token := filepath.ToSlash(filepath.Join(dir, "token.txt"))
	writeConfigFile(t, token, "secret-token")
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\n" +
		"exporters:\n  otlp:\n    endpoint: '${env:CAPTURE_ENDPOINT}'\n    headers:\n" +
		"      a: '${file:" + token + "}'\n" +
		"      b: '${env:CAPTURE_UNBOUND}'\n" +
		"      c: '${vault:secret/x}'\n" +
		"service: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	env := map[string]string{"CAPTURE_ENDPOINT": "localhost:4317"}
	opts := CaptureOptions{Paths: hostfs.Policy{Unrestricted: true}}
	ctx := context.Background()

	want, err := CaptureSourceWith(ctx, Source{YAML: yaml}, factories, env, opts)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := CaptureSnapshot(ctx, Source{YAML: yaml}, env, opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := snap.Resolve(ctx, factories)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := want.Snapshot.MarshalJSON()
	b, _ := got.Snapshot.MarshalJSON()
	if string(a) != string(b) {
		t.Fatalf("snapshots differ\n%s\n%s", a, b)
	}
	if !reflect.DeepEqual(want.References, got.References) || len(got.References) != 4 {
		t.Fatalf("references\n got %+v\nwant %+v", got.References, want.References)
	}
}

func TestCaptureSnapshotNeedsNoFactories(t *testing.T) {
	yaml := "receivers: {nosuchreceiver: {}}\nexporters: {nosuchexporter: {k: '${env:CAPTURE_X:-d}'}}\n" +
		"service: {pipelines: {traces: {receivers: [nosuchreceiver], exporters: [nosuchexporter]}}}\n"
	snap, err := CaptureSnapshot(context.Background(), Source{YAML: yaml}, nil, CaptureOptions{Paths: hostfs.Policy{Unrestricted: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.References()) != 1 {
		t.Fatalf("references %+v", snap.References())
	}
}
