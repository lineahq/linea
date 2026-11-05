package api

import (
	"archive/zip"
	"bytes"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Saved artifacts list their revisions, export for CI as a zip and import
// it back.
func TestArtifactRevisionsExportAndImportRoutes(t *testing.T) {
	_, _, srv := workspaceServer(t, memory.NewData())
	url := srv.URL + "/api/v1/artifacts"
	ws := decodeBody[artifact.Page](t, get(t, url), http.StatusOK).Workspace.ID
	config := decodeBody[artifact.Saved](t, post(t, url, SaveRequest{WorkspaceID: ws, Kind: "configuration", Label: "gateway.yaml", Content: []byte("v1")}), http.StatusCreated).Artifact
	config = decodeBody[artifact.Saved](t, post(t, url, SaveRequest{WorkspaceID: ws, ArtifactID: config.ID, ExpectedGeneration: 1, ExpectedHead: config.Head, Kind: "configuration", Label: "gateway.yaml", Content: []byte("v2")}), http.StatusCreated).Artifact
	scenario := decodeBody[artifact.Saved](t, post(t, url, SaveRequest{WorkspaceID: ws, Kind: "scenario", Label: "checkout.yaml", Content: []byte("version: 1\n")}), http.StatusCreated).Artifact

	page := decodeBody[artifact.RevisionPage](t, get(t, url+"/"+config.ID+"/revisions?limit=1"), http.StatusOK)
	if len(page.Items) != 1 || page.Items[0].Ref.RevisionID != config.Head || page.NextCursor == "" {
		t.Fatalf("first page = %+v", page)
	}
	page = decodeBody[artifact.RevisionPage](t, get(t, url+"/"+config.ID+"/revisions?limit=1&cursor="+page.NextCursor), http.StatusOK)
	if len(page.Items) != 1 || page.Items[0].Generation != 1 || page.NextCursor != "" {
		t.Fatalf("second page = %+v", page)
	}
	decodeBody[ArtifactError](t, get(t, url+"/"+config.ID+"/revisions?limit=x"), http.StatusBadRequest)
	decodeBody[ArtifactError](t, get(t, url+"/missing/revisions"), http.StatusNotFound)

	resp := post(t, url+"/export", ExportForCIRequest{ConfigurationID: config.ID, ScenarioIDs: []string{scenario.ID}})
	archive, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/zip" || resp.Header.Get("Content-Disposition") != "attachment; filename=collector-ci.zip" {
		t.Fatalf("export = %d %v: %s", resp.StatusCode, resp.Header, archive)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(zr.File) != 4 || zr.File[0].Name != "collector/config.yaml" || zr.File[1].Name != "collector/tests/checkout.yaml" {
		t.Fatalf("archive: %v", err)
	}
	if e := decodeBody[ArtifactError](t, post(t, url+"/export", ExportForCIRequest{ConfigurationID: config.ID}), http.StatusBadRequest); !strings.Contains(e.Error, "1 to 32 scenarios") {
		t.Fatalf("export without scenarios = %+v", e)
	}

	imp := decodeBody[platform.ArtifactImport](t, post(t, url+"/import", ImportRequest{Archive: archive}), http.StatusOK)
	if len(imp.Items) != 2 || imp.Items[0].Status != "unchanged" || imp.Items[0].Artifact.ID != config.ID {
		t.Fatalf("import = %+v", imp)
	}
	if e := decodeBody[ArtifactError](t, post(t, url+"/import", ImportRequest{Archive: []byte("plain")}), http.StatusBadRequest); e.Code != "invalid_request" || !strings.Contains(e.Error, "not a zip") {
		t.Fatalf("import of a non-zip = %+v", e)
	}
	decodeBody[ArtifactError](t, post(t, url+"/import", map[string]any{"archive_base64": "", "extra": 1}), http.StatusBadRequest)
}
