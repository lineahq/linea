package api

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

const (
	// buildManifest names example.com/proc from a local directory, so a
	// build of it needs an uploaded archive.
	buildManifest = "dist:\n  otelcol_version: 0.162.0\nprocessors:\n  - gomod: example.com/proc v0.0.0\n    path: ./proc\n"
	// plainManifest needs no local source.
	plainManifest = "dist:\n  otelcol_version: 0.162.0\n"
)

// The adapter's upload bounds mirror the artifact package's, which it may not
// import.
func TestBuildUploadBoundsMatchTheArtifactBounds(t *testing.T) {
	if maxBuildSources != artifact.MaxBuildSources {
		t.Fatalf("maxBuildSources = %d, artifact.MaxBuildSources = %d", maxBuildSources, artifact.MaxBuildSources)
	}
	if maxBuildUploadBytes < artifact.MaxBuildInputBytes {
		t.Fatalf("maxBuildUploadBytes = %d, below artifact.MaxBuildInputBytes = %d", maxBuildUploadBytes, artifact.MaxBuildInputBytes)
	}
}

func moduleTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for name, content := range files {
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

func procTar(t *testing.T) []byte {
	return moduleTar(t, map[string]string{"go.mod": "module example.com/proc\n", "proc.go": "package proc\n"})
}

// buildPart is one multipart part: a file part, or the JSON request part
// when contentType is set.
type buildPart struct {
	name, contentType string
	body              []byte
}

// uploadBuildRequest posts a multipart build. A nil header map sends the
// upload header.
func uploadBuildRequest(t *testing.T, base string, header map[string]string, parts ...buildPart) *http.Response {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		if p.contentType != "" {
			h.Set("Content-Disposition", `form-data; name="`+p.name+`"`)
			h.Set("Content-Type", p.contentType)
		} else {
			h.Set("Content-Disposition", `form-data; name="`+p.name+`"; filename="`+p.name+`.tar"`)
			h.Set("Content-Type", "application/x-tar")
		}
		pw, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Write(p.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/runtime-builds", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if header == nil {
		header = map[string]string{BuildUploadHeader: "1"}
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func requestPart(t *testing.T, q RuntimeBuildRequest) buildPart {
	t.Helper()
	return buildPart{name: "request", contentType: "application/json", body: mustJSON(t, q)}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := platform.Encode(&b, v); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestRuntimeBuildRoutesMapPlatformOperations queues builds with and
// without uploaded sources, reads, lists, pages and cancels them, and maps
// refusals to their codes.
func TestRuntimeBuildRoutesMapPlatformOperations(t *testing.T) {
	_, _, srv := workspaceServer(t, memory.NewData())
	api := srv.URL + "/api/v1"

	// A manifest without local directories is plain JSON.
	b := decodeBody[RuntimeBuild](t, post(t, api+"/runtime-builds", RuntimeBuildRequest{Name: "plain", ManifestYAML: plainManifest}), http.StatusAccepted)
	if b.ID == "" || b.Name != "plain" || b.State != BuildQueued || b.Reused || b.CollectorVersion != "0.162.0" || !b.Verified || b.Log == nil || b.Warnings == nil {
		t.Fatalf("build = %+v", b)
	}
	got := decodeBody[RuntimeBuild](t, get(t, api+"/runtime-builds/"+b.ID+"?log_from=0"), http.StatusOK)
	if got.ID != b.ID || got.State != BuildQueued || got.ManifestYAML != plainManifest || b.ManifestYAML != "" {
		t.Fatalf("get = %+v", got)
	}
	cancelled := decodeBody[RuntimeBuild](t, post(t, api+"/runtime-builds/"+b.ID+"/cancel", nil), http.StatusAccepted)
	if cancelled.State != BuildCancelled || cancelled.Finished == nil {
		t.Fatalf("cancelled = %+v", cancelled)
	}
	if again := decodeBody[RuntimeBuild](t, post(t, api+"/runtime-builds/"+b.ID+"/cancel", nil), http.StatusAccepted); again.State != BuildCancelled {
		t.Fatalf("cancelling twice = %+v", again)
	}

	// A manifest with a local directory takes an archive in a multipart
	// request, whose parts come in any order.
	archive := procTar(t)
	req := RuntimeBuildRequest{Name: "edge", ManifestYAML: buildManifest, Sources: []RuntimeBuildSource{{Module: "example.com/proc", Part: "proc-archive"}}}
	resp := uploadBuildRequest(t, srv.URL, nil, buildPart{name: "proc-archive", body: archive}, requestPart(t, req))
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/api/v1/runtime-builds/") {
		t.Fatalf("Location = %q", loc)
	}
	edge := decodeBody[RuntimeBuild](t, resp, http.StatusAccepted)
	if edge.Name != "edge" || edge.State != BuildQueued {
		t.Fatalf("uploaded build = %+v", edge)
	}
	decodeBody[RuntimeBuild](t, post(t, api+"/runtime-builds/"+edge.ID+"/cancel", nil), http.StatusAccepted)

	// Newest first, paged by limit and cursor, without logs.
	last := decodeBody[RuntimeBuild](t, post(t, api+"/runtime-builds", RuntimeBuildRequest{Name: "third", ManifestYAML: plainManifest}), http.StatusAccepted)
	decodeBody[RuntimeBuild](t, post(t, api+"/runtime-builds/"+last.ID+"/cancel", nil), http.StatusAccepted)
	first := decodeBody[RuntimeBuildPage](t, get(t, api+"/runtime-builds?limit=2"), http.StatusOK)
	if len(first.Builds) != 2 || first.Builds[0].ID != last.ID || first.Builds[1].ID != edge.ID || first.NextCursor == "" || len(first.Builds[0].Log) != 0 {
		t.Fatalf("first page = %+v", first)
	}
	second := decodeBody[RuntimeBuildPage](t, get(t, api+"/runtime-builds?limit=2&cursor="+first.NextCursor), http.StatusOK)
	if len(second.Builds) != 1 || second.Builds[0].ID != b.ID || second.NextCursor != "" {
		t.Fatalf("second page = %+v", second)
	}
	decodeBody[OperationError](t, get(t, api+"/runtime-builds?limit=x"), http.StatusBadRequest)
	decodeBody[OperationError](t, get(t, api+"/runtime-builds?limit=501"), http.StatusBadRequest)

	// Refusals keep the platform's codes.
	missing := decodeBody[OperationError](t, get(t, api+"/runtime-builds/missing"), http.StatusNotFound)
	if missing.Code != string(platform.CodeNotFound) {
		t.Fatalf("missing build = %+v", missing)
	}
	decodeBody[OperationError](t, get(t, api+"/runtime-builds/"+b.ID+"?log_from=-1"), http.StatusBadRequest)
	decodeBody[OperationError](t, post(t, api+"/runtime-builds/missing/cancel", nil), http.StatusNotFound)
	decodeBody[OperationError](t, post(t, api+"/runtime-builds", RuntimeBuildRequest{Name: "Bad Name", ManifestYAML: plainManifest}), http.StatusBadRequest)
	decodeBody[OperationError](t, post(t, api+"/runtime-builds", map[string]any{"name": "x", "manifest_yaml": plainManifest, "go_image": "golang:latest"}), http.StatusBadRequest)
	// The manifest's local directory has no archive: refused with diagnostics.
	noSource := decodeBody[OperationError](t, post(t, api+"/runtime-builds", RuntimeBuildRequest{Name: "edge", ManifestYAML: buildManifest}), http.StatusUnprocessableEntity)
	if noSource.Code != string(platform.CodeValidationFailed) || len(noSource.Diagnostics) != 1 || noSource.Diagnostics[0].Code != "manifest_sources" {
		t.Fatalf("manifest without its source = %+v", noSource)
	}
	// A source names its module but sends no archive.
	decodeBody[OperationError](t, post(t, api+"/runtime-builds", RuntimeBuildRequest{Name: "edge", ManifestYAML: buildManifest, Sources: []RuntimeBuildSource{{Module: "example.com/proc"}}}), http.StatusUnprocessableEntity)
	unknownRef := &platform.ArtifactRef{ArtifactID: "missing", RevisionID: "missing"}
	decodeBody[OperationError](t, post(t, api+"/runtime-builds", RuntimeBuildRequest{Name: "edge", ManifestRef: unknownRef}), http.StatusNotFound)
}

// TestRuntimeBuildUploadsAreBoundedAndChecked refuses multipart bodies that
// lack the header, the request part or a matching archive, and unsafe or
// oversized ones, before admitting anything.
func TestRuntimeBuildUploadsAreBoundedAndChecked(t *testing.T) {
	_, _, srv := workspaceServer(t, memory.NewData())
	api := srv.URL + "/api/v1"
	archive := procTar(t)
	one := RuntimeBuildRequest{Name: "edge", ManifestYAML: buildManifest, Sources: []RuntimeBuildSource{{Module: "example.com/proc", Part: "a"}}}

	// Without the header a multipart body is not accepted: a page of another
	// origin could send it without a preflight.
	resp := uploadBuildRequest(t, srv.URL, map[string]string{}, requestPart(t, one), buildPart{name: "a", body: archive})
	decodeBody[Error](t, resp, http.StatusUnsupportedMediaType)
	// The header exception is for the build route only.
	req, err := http.NewRequest(http.MethodPost, api+"/runs", strings.NewReader("--x--"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.Header.Set(BuildUploadHeader, "1")
	other, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	decodeBody[Error](t, other, http.StatusUnsupportedMediaType)

	invalid := func(name string, parts ...buildPart) {
		t.Helper()
		e := decodeBody[OperationError](t, uploadBuildRequest(t, srv.URL, nil, parts...), http.StatusBadRequest)
		if e.Code != string(platform.CodeInvalidRequest) {
			t.Errorf("%s: %+v", name, e)
		}
	}
	invalid("no request part", buildPart{name: "a", body: archive})
	invalid("two request parts", requestPart(t, one), requestPart(t, one), buildPart{name: "a", body: archive})
	invalid("a part no source names", requestPart(t, one), buildPart{name: "a", body: archive}, buildPart{name: "b", body: archive})
	invalid("a source without its part", requestPart(t, one))
	invalid("two parts of a name", requestPart(t, one), buildPart{name: "a", body: archive}, buildPart{name: "a", body: archive})
	invalid("an unknown request field", buildPart{name: "request", contentType: "application/json", body: []byte(`{"name":"edge","go_image":"x"}`)})
	two := one
	two.Sources = []RuntimeBuildSource{{Module: "example.com/proc", Part: "a"}, {Module: "example.com/other", Part: "a"}}
	invalid("one part for two modules", requestPart(t, two), buildPart{name: "a", body: archive})

	// Platform refusals of the archive keep their code and name the module.
	for name, bad := range map[string][]byte{
		"not a tar":    []byte("not a tar archive, just some bytes that are long enough"),
		"no go.mod":    moduleTar(t, map[string]string{"proc.go": "package proc\n"}),
		"an escape":    moduleTar(t, map[string]string{"go.mod": "module example.com/proc\n", "../x.go": "package x\n"}),
		"other go.mod": moduleTar(t, map[string]string{"go.mod": "module example.com/other\n"}),
	} {
		e := decodeBody[OperationError](t, uploadBuildRequest(t, srv.URL, nil, requestPart(t, one), buildPart{name: "a", body: bad}), http.StatusUnprocessableEntity)
		if e.Code != string(platform.CodeValidationFailed) || len(e.Diagnostics) != 1 || !strings.Contains(e.Diagnostics[0].Message, "example.com/proc") {
			t.Errorf("%s: %+v", name, e)
		}
	}

	// More archives than a build takes, and a request part past its bound,
	// are sizes.
	var many []buildPart
	q := RuntimeBuildRequest{Name: "edge", ManifestYAML: buildManifest}
	for i := 0; i <= maxBuildSources; i++ {
		part := "p" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		q.Sources = append(q.Sources, RuntimeBuildSource{Module: "example.com/m" + part, Part: part})
		many = append(many, buildPart{name: part, body: []byte("x")})
	}
	e := decodeBody[OperationError](t, uploadBuildRequest(t, srv.URL, nil, append([]buildPart{requestPart(t, q)}, many...)...), http.StatusRequestEntityTooLarge)
	if e.Code != string(platform.CodeResourceExhausted) {
		t.Errorf("too many parts: %+v", e)
	}
	huge := []byte(`{"name":"edge","manifest_yaml":"` + strings.Repeat("a", maxBuildRequestPart) + `"}`)
	e = decodeBody[OperationError](t, uploadBuildRequest(t, srv.URL, nil, buildPart{name: "request", contentType: "application/json", body: huge}), http.StatusRequestEntityTooLarge)
	if e.Code != string(platform.CodeResourceExhausted) {
		t.Errorf("huge request part: %+v", e)
	}
	// A request part holds one JSON document.
	twoDocs := []byte(`{"name":"edge","manifest_yaml":"x"}{"name":"other","manifest_yaml":"y"}`)
	e = decodeBody[OperationError](t, uploadBuildRequest(t, srv.URL, nil, buildPart{name: "request", contentType: "application/json", body: twoDocs}), http.StatusBadRequest)
	if e.Code != string(platform.CodeInvalidRequest) {
		t.Errorf("two request documents: %+v", e)
	}
	// A body past the upload bound is a size refusal, not a malformed upload.
	if got := platform.ErrorCode(uploadError(&http.MaxBytesError{Limit: maxBuildUploadBytes})); got != platform.CodeResourceExhausted {
		t.Errorf("oversized upload: %s", got)
	}

	// None of the refusals admitted a build.
	if page := decodeBody[RuntimeBuildPage](t, get(t, api+"/runtime-builds"), http.StatusOK); len(page.Builds) != 0 {
		t.Fatalf("builds after refusals = %+v", page.Builds)
	}
}

// TestRuntimeBuildEventsFollowABuild streams a queued build until it is
// cancelled, resumes after an id and refuses an unknown build before it
// opens a stream.
func TestRuntimeBuildEventsFollowABuild(t *testing.T) {
	_, _, srv := workspaceServer(t, memory.NewData())
	api := srv.URL + "/api/v1"
	b := decodeBody[RuntimeBuild](t, post(t, api+"/runtime-builds", RuntimeBuildRequest{Name: "plain", ManifestYAML: plainManifest}), http.StatusAccepted)

	resp, err := http.Get(api + "/runtime-builds/" + b.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// The queued build arrives first; cancelling it then ends the stream.
	events := make(chan []sseMessage, 1)
	go func() { events <- readSSE(t, resp.Body) }()
	decodeBody[RuntimeBuild](t, post(t, api+"/runtime-builds/"+b.ID+"/cancel", nil), http.StatusAccepted)
	got := <-events
	if len(got) < 2 || got[len(got)-1].event != "end" || got[0].event != "build" {
		t.Fatalf("events = %+v", got)
	}
	var end RuntimeBuild
	if err := json.Unmarshal([]byte(got[len(got)-1].data), &end); err != nil || end.State != BuildCancelled || len(end.Log) != 0 {
		t.Fatalf("end = %+v, %v", end, err)
	}
	var seen RuntimeBuild
	if err := json.Unmarshal([]byte(got[len(got)-2].data), &seen); err != nil || seen.State != BuildCancelled {
		t.Fatalf("last build event = %+v, %v", seen, err)
	}

	// A finished build streams its final state at once, from a log line.
	again, err := http.NewRequest(http.MethodGet, api+"/runtime-builds/"+b.ID+"/events?log_from=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	again.Header.Set("Last-Event-ID", "0")
	r2, err := http.DefaultClient.Do(again)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if replay := readSSE(t, r2.Body); len(replay) != 2 || replay[0].event != "build" || replay[1].event != "end" {
		t.Fatalf("replay = %+v", replay)
	}

	decodeBody[OperationError](t, get(t, api+"/runtime-builds/"+b.ID+"/events?log_from=x"), http.StatusBadRequest)
	decodeBody[OperationError](t, get(t, api+"/runtime-builds/missing/events"), http.StatusNotFound)
}
