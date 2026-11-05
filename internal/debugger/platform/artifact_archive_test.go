package platform

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/lineahq/linea/examples/ci"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func archiveWorkspace(t *testing.T, limits artifact.Limits) *Service {
	t.Helper()
	return openPlatformLimits(t, memory.NewData(), limits, Options{})
}

func saveNew(t *testing.T, s *Service, kind ArtifactKind, label, content string) ArtifactMetadata {
	t.Helper()
	info, err := s.GetWorkspaceInfo(context.Background(), testCaller)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveArtifact(context.Background(), testCaller, SaveArtifactRequest{WorkspaceID: info.ID, Kind: kind, Label: label, Content: []byte(content)})
	if err != nil {
		t.Fatal(err)
	}
	return saved.Artifact
}

func unzip(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(b)
	}
	return out
}

// zipOf writes entries in the order given; a nil content is a directory.
func zipOf(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e[0])
		if err == nil {
			_, err = w.Write([]byte(e[1]))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func statuses(imp *ArtifactImport) string {
	var out []string
	for _, it := range imp.Items {
		out = append(out, it.Path+"="+it.Status)
	}
	return strings.Join(out, " ")
}

func TestExportForCIRoundTripsThroughImport(t *testing.T) {
	ctx := context.Background()
	s := archiveWorkspace(t, artifact.Limits{})
	const config = "receivers: {otlp: {protocols: {grpc: {endpoint: '${env:OTLP_ENDPOINT}'}}}}\n"
	cfg := saveNew(t, s, artifact.Configuration, "gateway.yaml", config)
	first := saveNew(t, s, artifact.Scenario, "tests/checkout.yaml", "version: 1\nname: first\n")
	second := saveNew(t, s, artifact.Scenario, `C:\work\checkout.yaml`, "version: 1\nname: second\n")

	if !bytes.Contains(ci.GitHubActions, []byte(ciScenarioLine)) {
		t.Fatalf("examples/ci/github-actions.yml no longer has %q", ciScenarioLine)
	}
	exp, err := s.ExportArtifactsForCI(ctx, testCaller, ExportCIRequest{Configuration: cfg.ID, Scenarios: []string{first.ID, second.ID}})
	if err != nil {
		t.Fatal(err)
	}
	files := unzip(t, exp.Archive)
	if exp.Name != "collector-ci.zip" || len(files) != 5 || files[ciConfigPath] != config ||
		files["collector/tests/checkout.yaml"] != "version: 1\nname: first\n" || files["collector/tests/checkout-2.yaml"] != "version: 1\nname: second\n" {
		t.Fatalf("archive = %v", files)
	}
	workflow := files[ciWorkflowPath]
	if !strings.Contains(workflow, "  SCENARIO: collector/tests/checkout.yaml\n") || !strings.Contains(workflow, "  CONFIG: collector/config.yaml\n") || strings.Contains(workflow, "OTLP_ENDPOINT") {
		t.Fatalf("workflow = %s", workflow)
	}
	var manifest ciManifest
	if err := json.Unmarshal([]byte(files[ciManifestPath]), &manifest); err != nil || len(manifest.Files) != 3 || manifest.Files[2].Artifact != second.ID || manifest.Files[2].Revision != second.Head {
		t.Fatalf("manifest = %+v, %v", manifest, err)
	}

	// The unedited archive changes nothing.
	imp, err := s.ImportArtifacts(ctx, testCaller, exp.Archive)
	if err != nil || statuses(imp) != "collector/config.yaml=unchanged collector/tests/checkout-2.yaml=unchanged collector/tests/checkout.yaml=unchanged" {
		t.Fatalf("import = %+v, %v", imp, err)
	}

	// An edited scenario is revised and a new one is created.
	edited := zipOf(t,
		[2]string{"collector/", ""}, [2]string{ciConfigPath, config},
		[2]string{"collector/tests/checkout.yaml", "version: 1\nname: first, edited\n"},
		[2]string{"collector/tests/checkout-2.yaml", files["collector/tests/checkout-2.yaml"]},
		[2]string{"collector/tests/new.yml", "version: 1\nname: new\n"},
		[2]string{ciWorkflowPath, workflow}, [2]string{ciManifestPath, files[ciManifestPath]})
	imp, err = s.ImportArtifacts(ctx, testCaller, edited)
	if err != nil || statuses(imp) != "collector/config.yaml=unchanged collector/tests/checkout-2.yaml=unchanged collector/tests/checkout.yaml=revised collector/tests/new.yml=created" {
		t.Fatalf("edited import = %+v, %v", imp, err)
	}
	if revised := imp.Items[2].Artifact; revised.ID != first.ID || revised.Generation != 2 || revised.Label != first.Label {
		t.Fatalf("revised = %+v", revised)
	}
	if created := imp.Items[3].Artifact; created.Kind != artifact.Scenario || created.Label != "new.yml" || created.Generation != 1 {
		t.Fatalf("created = %+v", created)
	}
	revs, err := s.ListArtifactRevisions(ctx, testCaller, ListArtifactRevisionsRequest{ArtifactID: first.ID})
	if err != nil || len(revs.Items) != 2 || revs.Items[0].Generation != 2 || revs.Items[1].Ref.RevisionID != first.Head {
		t.Fatalf("revisions = %+v, %v", revs, err)
	}

	// The same archive again would overwrite the revision it made.
	_, err = s.ImportArtifacts(ctx, testCaller, edited)
	wantCode(t, err, CodeConflict)
	if !strings.Contains(err.Error(), "revised after its export") {
		t.Fatalf("err = %v", err)
	}

	// Another workspace creates every file.
	other := archiveWorkspace(t, artifact.Limits{})
	imp, err = other.ImportArtifacts(ctx, testCaller, exp.Archive)
	if err != nil || statuses(imp) != "collector/config.yaml=created collector/tests/checkout-2.yaml=created collector/tests/checkout.yaml=created" ||
		imp.Items[0].Artifact.Kind != artifact.Configuration || imp.Items[0].Artifact.Label != "gateway.yaml" {
		t.Fatalf("other workspace import = %+v, %v", imp, err)
	}

	for _, tc := range []struct {
		name string
		req  ExportCIRequest
		code Code
	}{
		{"no scenarios", ExportCIRequest{Configuration: cfg.ID}, CodeInvalidRequest},
		{"scenario as configuration", ExportCIRequest{Configuration: first.ID, Scenarios: []string{second.ID}}, CodeInvalidRequest},
		{"duplicate scenario", ExportCIRequest{Configuration: cfg.ID, Scenarios: []string{first.ID, first.ID}}, CodeInvalidRequest},
		{"unknown artifact", ExportCIRequest{Configuration: cfg.ID, Scenarios: []string{"missing"}}, CodeNotFound},
	} {
		_, err := s.ExportArtifactsForCI(ctx, testCaller, tc.req)
		if ErrorCode(err) != tc.code {
			t.Errorf("export %s: err = %v, want %s", tc.name, err, tc.code)
		}
	}
}

func TestImportRefusesUnknownAndOversizedEntries(t *testing.T) {
	ctx := context.Background()
	s := archiveWorkspace(t, artifact.Limits{BlobBytes: 64})
	for _, tc := range []struct {
		name    string
		archive []byte
		want    string
	}{
		{"not a zip", []byte("plain"), "not a zip archive"},
		{"empty", zipOf(t, [2]string{ciWorkflowPath, "on: push\n"}), "no collector/config.yaml"},
		{"unknown entry", zipOf(t, [2]string{ciConfigPath, "a: 1\n"}, [2]string{"README.md", "hi"}), `unknown entry "README.md"`},
		{"nested test", zipOf(t, [2]string{"collector/tests/sub/x.yaml", "a: 1\n"}), "unknown entry"},
		{"path traversal", zipOf(t, [2]string{"collector/tests/../../x.yaml", "a: 1\n"}), "unknown entry"},
		{"duplicate", zipOf(t, [2]string{ciConfigPath, "a: 1\n"}, [2]string{ciConfigPath, "a: 2\n"}), "duplicate entry"},
		{"oversized", zipOf(t, [2]string{ciConfigPath, strings.Repeat("a", 65)}), "exceeds 64 bytes"},
		{"bad manifest", zipOf(t, [2]string{ciConfigPath, "a: 1\n"}, [2]string{ciManifestPath, `{"schema": 9}`}), "not a schema 1 manifest"},
	} {
		_, err := s.ImportArtifacts(ctx, testCaller, tc.archive)
		if ErrorCode(err) != CodeInvalidRequest || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	page, err := s.ListArtifacts(ctx, testCaller, ListArtifactsRequest{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("a refused import saved %+v, %v", page, err)
	}
	_, err = New(Options{}).ImportArtifacts(ctx, testCaller, zipOf(t, [2]string{ciConfigPath, "a: 1\n"}))
	wantCode(t, err, CodeUnavailable)
}
