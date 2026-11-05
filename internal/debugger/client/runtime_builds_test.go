package client

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

const (
	buildManifest = "dist:\n  otelcol_version: 0.162.0\nprocessors:\n  - gomod: example.com/proc v0.0.0\n    path: ./proc\n"
	plainManifest = "dist:\n  otelcol_version: 0.162.0\n"
)

func moduleArchive(t *testing.T, module string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for name, content := range map[string]string{"go.mod": "module " + module + "\n", "proc.go": "package proc\n"} {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestRuntimeBuildMethodsSpeakTheBuildRoutes queues builds as JSON and as an
// upload of module archives, reads, pages and cancels them, and keeps the
// server's refusals' codes and diagnostics.
func TestRuntimeBuildMethodsSpeakTheBuildRoutes(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{Paths: platform.PathPolicy{Unrestricted: true}})
	if err != nil {
		t.Fatal(err)
	}
	s := api.New(api.Options{Platform: service})
	srv := httptest.NewServer(s)
	t.Cleanup(func() {
		srv.Close()
		s.Close()
		service.Close()
	})
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()

	plain, err := c.BuildRuntime(ctx, api.RuntimeBuildRequest{Name: "plain", ManifestYAML: plainManifest}, nil)
	if err != nil || plain.ID == "" || plain.State != api.BuildQueued {
		t.Fatalf("BuildRuntime = %+v, %v", plain, err)
	}
	// The request's own sources give way to the archives', which upload.
	edge, err := c.BuildRuntime(ctx, api.RuntimeBuildRequest{Name: "edge", ManifestYAML: buildManifest},
		[]BuildArchive{{Module: "example.com/proc", Data: moduleArchive(t, "example.com/proc")}})
	if err != nil || edge.Name != "edge" || edge.State != api.BuildQueued {
		t.Fatalf("BuildRuntime with an archive = %+v, %v", edge, err)
	}
	got, err := c.GetRuntimeBuild(ctx, edge.ID, 0)
	if err != nil || got.ID != edge.ID || got.Log == nil {
		t.Fatalf("GetRuntimeBuild = %+v, %v", got, err)
	}
	if rest, err := c.GetRuntimeBuild(ctx, edge.ID, got.LogLines); err != nil || len(rest.Log) != 0 {
		t.Fatalf("log past its end = %+v, %v", rest, err)
	}
	cancelled, err := c.CancelRuntimeBuild(ctx, edge.ID)
	if err != nil || cancelled.State != api.BuildCancelled {
		t.Fatalf("CancelRuntimeBuild = %+v, %v", cancelled, err)
	}
	if _, err := c.CancelRuntimeBuild(ctx, plain.ID); err != nil {
		t.Fatal(err)
	}

	first, err := c.ListRuntimeBuilds(ctx, 1, "")
	if err != nil || len(first.Builds) != 1 || first.Builds[0].ID != edge.ID || first.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	second, err := c.ListRuntimeBuilds(ctx, 1, first.NextCursor)
	if err != nil || len(second.Builds) != 1 || second.Builds[0].ID != plain.ID || second.NextCursor != "" {
		t.Fatalf("second page = %+v, %v", second, err)
	}

	var refused *Error
	if _, err := c.BuildRuntime(ctx, api.RuntimeBuildRequest{Name: "edge", ManifestYAML: buildManifest}, nil); !errors.As(err, &refused) ||
		refused.Code != string(platform.CodeValidationFailed) || len(refused.Diagnostics) != 1 || refused.Diagnostics[0].Code != "manifest_sources" {
		t.Fatalf("a manifest without its source = %#v", err)
	}
	bad := moduleArchive(t, "example.com/other")
	if _, err := c.BuildRuntime(ctx, api.RuntimeBuildRequest{Name: "edge", ManifestYAML: buildManifest},
		[]BuildArchive{{Module: "example.com/proc", Data: bad}}); !errors.As(err, &refused) || refused.Status != 422 {
		t.Fatalf("an archive of another module = %#v", err)
	}
	if _, err := c.GetRuntimeBuild(ctx, "nope", 0); !errors.As(err, &refused) || refused.Status != 404 {
		t.Fatalf("an unknown build = %v", err)
	}
}
