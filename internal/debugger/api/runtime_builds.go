package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Server runtime builds: a build is a durable job a builder
// claims. A request carries the manifest as text or as a saved revision and,
// for each local module directory the manifest names, an uploaded archive.

const (
	// BuildUploadHeader must accompany a multipart upload. A browser sends
	// multipart/form-data cross-origin without a preflight, so the header,
	// which makes the request need one that is never answered, keeps a
	// page from another origin from posting to a server that signs its
	// requests in without a credential.
	BuildUploadHeader = "X-Linea-Upload"
	// BuildRequestPart is the multipart part holding the JSON request; every
	// other part is a module archive the request's sources name.
	BuildRequestPart = "request"
	// maxBuildRequestPart bounds the JSON request part: a manifest of at
	// most 1 MiB, escaped.
	maxBuildRequestPart = 2 << 20
	// maxBuildUploadBytes bounds a whole upload: the platform's bound on
	// the manifest and archives of one build (artifact.MaxBuildInputBytes,
	// 64 MiB of sources and 1 MiB of manifest) and the request part's
	// framing. The platform checks its own bounds again at admission.
	maxBuildUploadBytes = 64<<20 + 1<<20 + maxBuildRequestPart
	// maxBuildSources bounds the module archives of one build
	// (artifact.MaxBuildSources).
	maxBuildSources = 32
)

// RuntimeBuildRequest builds a runtime from an OCB manifest, sent as text
// (manifest_yaml) or as a saved manifest revision (manifest_ref). Its bytes
// are never changed. Each local module directory the manifest names
// (path: entries and local replaces) needs a source whose part is the
// archive sent as a file part of a multipart request.
type RuntimeBuildRequest struct {
	Name         string                `json:"name"`
	ManifestYAML string                `json:"manifest_yaml,omitempty"`
	ManifestRef  *platform.ArtifactRef `json:"manifest_ref,omitempty"`
	Sources      []RuntimeBuildSource  `json:"sources,omitempty"`
}

// RuntimeBuildSource names one module's source archive: an uncompressed tar
// of the module's directory with go.mod at its root, regular files and
// directories only. Part is the name of the multipart file part holding it.
type RuntimeBuildSource struct {
	Module string `json:"module"`
	Part   string `json:"part,omitempty"`
}

// RuntimeBuildState is a build's lifecycle state.
type RuntimeBuildState string

const (
	BuildQueued    RuntimeBuildState = "queued"
	BuildRunning   RuntimeBuildState = "running"
	BuildDone      RuntimeBuildState = "done"
	BuildFailed    RuntimeBuildState = "failed"
	BuildCancelled RuntimeBuildState = "cancelled"
)

// RuntimeBuild is a build with the tail of its log: log holds lines from
// log_from on (at most the last 2000), log_lines counts every line.
type RuntimeBuild struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	State    RuntimeBuildState `json:"state"`
	Created  time.Time         `json:"created"`
	Started  *time.Time        `json:"started,omitempty"`
	Finished *time.Time        `json:"finished,omitempty"`
	Log      []string          `json:"log"`
	LogFrom  int               `json:"log_from"`
	LogLines int               `json:"log_lines"`
	Runtime  *Runtime          `json:"runtime,omitempty"`
	Error    string            `json:"error,omitempty"`
	// CollectorVersion is the manifest's release; verified is false for a
	// release outside the support table.
	CollectorVersion string `json:"collector_version"`
	Verified         bool   `json:"verified"`
	// Warnings name the manifest fields the builder ignores and the optional
	// features the runtime will lack.
	Warnings []string `json:"warnings"`
	// Reused is true when the server took the binary of an earlier build of
	// identical inputs instead of building again.
	Reused bool `json:"reused"`
	// Image is the runtime's exported image by digest when the server
	// exports builds, tagged with the name of the build that made the
	// binary; export_error is why that export failed. Neither fails a build.
	Image       string `json:"image,omitempty"`
	ExportError string `json:"export_error,omitempty"`
	// ManifestYAML is the manifest the build was requested with, as sent;
	// only getRuntimeBuild returns it, never the sources.
	ManifestYAML string `json:"manifest_yaml,omitempty"`
}

// RuntimeBuildPage is a page of builds without their logs, newest first;
// next_cursor continues it and is absent on the last page.
type RuntimeBuildPage struct {
	Builds     []RuntimeBuild `json:"builds"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

func runtimeBuildOf(v *platform.RuntimeBuild) RuntimeBuild {
	log := v.Log
	if log == nil {
		log = []string{}
	}
	return RuntimeBuild{ID: v.ID, Name: v.Name, State: RuntimeBuildState(v.State), Created: v.Created, Started: timeOrNil(v.Started), Finished: timeOrNil(v.Finished),
		Log: log, LogFrom: v.LogFrom, LogLines: v.LogLines, Runtime: runtimeOf(v.Runtime), Error: v.Error,
		CollectorVersion: v.CollectorVersion, Verified: v.Verified, Warnings: orEmpty(v.Warnings), Reused: v.Reused,
		Image: v.Image, ExportError: v.ExportError, ManifestYAML: v.Manifest}
}

// invalidBuild is a malformed build request.
func invalidBuild(format string, args ...any) error {
	return &platform.Error{Code: platform.CodeInvalidRequest, Message: "invalid request: " + fmt.Sprintf(format, args...)}
}

func oversizedBuild(message string) error {
	return &platform.Error{Code: platform.CodeResourceExhausted, Message: message}
}

// uploadBuild reports whether r is a multipart build upload, which must
// carry BuildUploadHeader.
func uploadBuild(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "multipart/form-data" && r.Method == http.MethodPost &&
		r.URL.Path == "/api/v1/runtime-builds" && r.Header.Get(BuildUploadHeader) != ""
}

// readBuildUpload reads a multipart build request: the "request" part, a
// JSON RuntimeBuildRequest, and one file part per module archive, whose form
// name the request's sources name. Parts may come in any order. Every
// archive part must be named by exactly one source.
func readBuildUpload(w http.ResponseWriter, r *http.Request) (RuntimeBuildRequest, map[string][]byte, error) {
	var req RuntimeBuildRequest
	_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if params["boundary"] == "" {
		return req, nil, invalidBuild("the multipart content type has no boundary")
	}
	mr := multipart.NewReader(http.MaxBytesReader(w, r.Body, maxBuildUploadBytes), params["boundary"])
	archives := map[string][]byte{}
	var haveRequest bool
	for parts := 0; ; parts++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return req, nil, uploadError(err)
		}
		if parts > maxBuildSources {
			return req, nil, oversizedBuild(fmt.Sprintf("more than %d module archives", maxBuildSources))
		}
		name := part.FormName()
		switch {
		case name == "":
			return req, nil, invalidBuild("a part has no form name")
		case name == BuildRequestPart:
			if haveRequest {
				return req, nil, invalidBuild("more than one %q part", BuildRequestPart)
			}
			haveRequest = true
			raw, err := io.ReadAll(io.LimitReader(part, maxBuildRequestPart+1))
			if err != nil {
				return req, nil, uploadError(err)
			}
			if len(raw) > maxBuildRequestPart {
				return req, nil, oversizedBuild(fmt.Sprintf("the %q part is larger than %d bytes", BuildRequestPart, maxBuildRequestPart))
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				return req, nil, invalidBuild("the %q part: %v", BuildRequestPart, err)
			}
			if err := dec.Decode(new(any)); err != io.EOF {
				return req, nil, invalidBuild("the %q part holds more than one JSON document", BuildRequestPart)
			}
		default:
			if _, dup := archives[name]; dup {
				return req, nil, invalidBuild("more than one part named %q", name)
			}
			data, err := io.ReadAll(part)
			if err != nil {
				return req, nil, uploadError(err)
			}
			archives[name] = data
		}
	}
	if !haveRequest {
		return req, nil, invalidBuild("the upload has no %q part", BuildRequestPart)
	}
	return req, archives, nil
}

// uploadError maps a failed body read: a body past its bound is a size
// refusal, anything else a malformed upload.
func uploadError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return oversizedBuild(fmt.Sprintf("the upload is larger than %d bytes", maxBuildUploadBytes))
	}
	return invalidBuild("the multipart body: %v", err)
}

// build maps the request and its archives onto the platform's. Every
// archive part must be named by a source, and a source's part must exist.
func (q RuntimeBuildRequest) build(archives map[string][]byte) (platform.BuildRuntimeRequest, error) {
	req := platform.BuildRuntimeRequest{Name: q.Name, Manifest: platform.Source{Text: q.ManifestYAML, Artifact: q.ManifestRef}}
	used := map[string]bool{}
	for _, s := range q.Sources {
		src := platform.SourceOverride{Module: s.Module}
		if s.Part != "" {
			data, ok := archives[s.Part]
			switch {
			case !ok:
				return req, invalidBuild("the source of %s names part %q, which the upload does not have", s.Module, s.Part)
			case used[s.Part]:
				return req, invalidBuild("part %q is the source of more than one module", s.Part)
			}
			used[s.Part] = true
			src.Archive = data
		}
		req.Sources = append(req.Sources, src)
	}
	for part := range archives {
		if !used[part] {
			return req, invalidBuild("part %q is not named by any source", part)
		}
	}
	return req, nil
}

func (s *Server) createRuntimeBuild(w http.ResponseWriter, r *http.Request) {
	var q RuntimeBuildRequest
	var archives map[string][]byte
	if uploadBuild(r) {
		var err error
		if q, archives, err = readBuildUpload(w, r); err != nil {
			writeOperationError(w, err)
			return
		}
	} else if !decode(w, r, &q) {
		return
	}
	req, err := q.build(archives)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	b, err := s.runs.BuildRuntime(r.Context(), callerOf(r), req)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/runtime-builds/"+b.ID)
	writeJSON(w, http.StatusAccepted, runtimeBuildOf(b))
}

func (s *Server) listRuntimeBuilds(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := platform.BuildListRequest{Cursor: q.Get("cursor")}
	if v := q.Get("limit"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil {
			writeOperationError(w, invalidBuild("limit must be an integer"))
			return
		}
		req.Limit = limit
	}
	page, err := s.runs.ListRuntimeBuilds(r.Context(), callerOf(r), req)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	out := RuntimeBuildPage{Builds: make([]RuntimeBuild, 0, len(page.Builds)), NextCursor: page.NextCursor}
	for i := range page.Builds {
		out.Builds = append(out.Builds, runtimeBuildOf(&page.Builds[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// logFrom reads a non-negative line number from the query.
func logFrom(r *http.Request) (int, error) {
	v := r.URL.Query().Get("log_from")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, invalidBuild("log_from %q: use a line number from 0", v)
	}
	return n, nil
}

// getRuntimeBuild returns a build with its log from ?log_from=N on. With
// ?wait=true it blocks until the build ends; ending the request never
// cancels the build.
func (s *Server) getRuntimeBuild(w http.ResponseWriter, r *http.Request) {
	from, err := logFrom(r)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	var b *platform.RuntimeBuild
	if r.URL.Query().Get("wait") == "true" {
		b, err = s.runs.WaitRuntimeBuild(r.Context(), callerOf(r), r.PathValue("id"), from)
		if r.Context().Err() != nil {
			return
		}
	} else {
		b, err = s.runs.GetRuntimeBuild(r.Context(), callerOf(r), r.PathValue("id"), from)
	}
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeBuildOf(b))
}

// streamRuntimeBuild follows a build as server-sent events: event "build"
// carries the build (data RuntimeBuild) with the log lines not yet sent, on
// every change of its state or log, and its id is the log line to continue
// from, so a reconnecting client resumes after Last-Event-ID. The build's
// last "build" event is followed by one "end" event, the build without its
// log, and the stream closes. The first event starts at ?log_from=N.
func (s *Server) streamRuntimeBuild(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	from, err := logFrom(r)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if from, err = strconv.Atoi(last); err != nil || from < 0 {
			writeOperationError(w, invalidBuild("Last-Event-ID %q: use a log line number from 0", last))
			return
		}
	}
	// An unknown build is refused before the stream opens.
	b, changed, err := s.runs.WatchRuntimeBuild(r.Context(), callerOf(r), id, from)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	var state platform.BuildState
	first := true
	for {
		if next := b.LogFrom + len(b.Log); first || b.State != state || next > from {
			data, err := json.Marshal(runtimeBuildOf(b))
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: build\ndata: %s\n\n", b.LogLines, data); err != nil {
				return
			}
			from, state, first = max(next, from), b.State, false
		}
		if b.Terminal() {
			b.Log, b.LogFrom = nil, b.LogLines
			data, _ := json.Marshal(runtimeBuildOf(b))
			fmt.Fprintf(w, "event: end\ndata: %s\n\n", data)
			flusher.Flush()
			return
		}
		flusher.Flush()
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
		if b, changed, err = s.runs.WatchRuntimeBuild(r.Context(), callerOf(r), id, from); err != nil {
			return
		}
	}
}

func (s *Server) cancelRuntimeBuild(w http.ResponseWriter, r *http.Request) {
	b, err := s.runs.CancelRuntimeBuild(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, runtimeBuildOf(b))
}

func (s *Server) routeRuntimeBuilds() {
	s.mux.HandleFunc("POST /api/v1/runtime-builds", s.createRuntimeBuild)
	s.mux.HandleFunc("GET /api/v1/runtime-builds", s.listRuntimeBuilds)
	s.mux.HandleFunc("GET /api/v1/runtime-builds/{id}", s.getRuntimeBuild)
	s.mux.HandleFunc("GET /api/v1/runtime-builds/{id}/events", s.streamRuntimeBuild)
	s.mux.HandleFunc("POST /api/v1/runtime-builds/{id}/cancel", s.cancelRuntimeBuild)
}

const (
	buildsNeedBuilder = "The server cannot build: it has no build queue or runtime store."
	forbiddenBuild    = "The caller lacks runtime.build (create), runtime.read (read) or, for another principal's build, runtime.manage."
)

func runtimeBuildPaths() map[string]any {
	buildID := []any{idParam}
	logFromParam := map[string]any{"name": "log_from", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 0},
		"description": "First log line to return; the last 2000 lines at most come back."}
	query := func(name, description string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": "query", "description": description, "schema": schema}
	}
	multipartBody := map[string]any{
		"required": true,
		"description": "A build with local module sources: a multipart/form-data body with the request header " + BuildUploadHeader + " set to any value. " +
			"The part named \"" + BuildRequestPart + "\" is the JSON RuntimeBuildRequest. Every other part is a file part: the uncompressed tar archive of one module's directory (go.mod at the archive root; regular files and directories only; relative paths; at most 64 MiB and 20,000 files unpacked), named by the part of exactly one entry of the request's sources, which names the module. " +
			"Parts may come in any order; at most 32 archives; at most 65 MiB of manifest and archives in all. A request without local sources may be plain application/json.",
		"content": map[string]any{
			"application/json": map[string]any{"schema": ref("RuntimeBuildRequest")},
			"multipart/form-data": map[string]any{
				"schema": map[string]any{"type": "object", "required": []string{BuildRequestPart},
					"properties":           map[string]any{BuildRequestPart: ref("RuntimeBuildRequest")},
					"additionalProperties": map[string]any{"type": "string", "format": "binary"}},
				"encoding": map[string]any{BuildRequestPart: map[string]any{"contentType": "application/json"}},
			},
		},
	}
	create := op("createRuntimeBuild",
		"Queue a runtime build from an OCB manifest. The manifest, its Collector release and the uploaded sources are checked first: a release before the oldest supported one, modules off the release, a local directory without an uploaded archive and an unsafe or oversized archive are refused; a release newer than the support table builds unverified. Builds of identical inputs reuse the earlier build's binary (reused). Returns without waiting; follow the build with getRuntimeBuild or streamRuntimeBuildEvents. A build runs on a builder, never in this server.",
		nil, "", map[string]any{
			"202": reply("The queued build.", ref("RuntimeBuild")),
			"400": operationFailure("Malformed request or upload, invalid runtime name, or a Go image, module proxy or other setting a team server's builders own."),
			"403": operationFailure(forbiddenBuild),
			"404": operationFailure("The saved manifest does not exist."),
			"413": operationFailure("The upload or an archive exceeds its bound."),
			"415": failure("Not application/json, or a multipart body without the " + BuildUploadHeader + " header."),
			"422": operationFailure("The manifest or a source archive was refused before building; diagnostics name each problem."),
			"503": operationFailure("Too many unfinished builds: at most 2 per principal and 20 on the server (retryable), or " + buildsNeedBuilder),
		})
	create["requestBody"] = multipartBody
	return map[string]any{
		"/api/v1/runtime-builds": map[string]any{
			"post": create,
			"get": op("listRuntimeBuilds", "The retained builds, newest first, without their logs: the caller's own, or every build with runtime.manage.",
				[]any{
					query("limit", "Page size, 1 to 500; default 50.", map[string]any{"type": "integer"}),
					query("cursor", "next_cursor of the previous page.", map[string]any{"type": "string"}),
				}, "", map[string]any{
					"200": reply("A page of builds.", ref("RuntimeBuildPage")),
					"400": operationFailure("Invalid limit or cursor."),
					"403": operationFailure(forbiddenBuild),
				}),
		},
		"/api/v1/runtime-builds/{id}": map[string]any{"get": op("getRuntimeBuild",
			"A build with its log lines from log_from on. With ?wait=true the request blocks until the build ends; ending the request never cancels it.",
			append(buildID, logFromParam, map[string]any{"name": "wait", "in": "query", "schema": map[string]any{"type": "boolean"}}), "", map[string]any{
				"200": reply("The build.", ref("RuntimeBuild")),
				"400": operationFailure("Invalid log_from."),
				"403": operationFailure(forbiddenBuild),
				"404": operationFailure("Unknown build, or another principal's."),
			})},
		"/api/v1/runtime-builds/{id}/events": map[string]any{"get": op("streamRuntimeBuildEvents",
			"Server-sent events following a build: event \"build\" (data RuntimeBuild, id the log line to continue from) on every change of its state or log, carrying only the log lines not yet sent, then one \"end\" (data RuntimeBuild without its log) after the build ended. Last-Event-ID, or log_from for the first event, starts the log at a line.",
			append(buildID, logFromParam, map[string]any{"name": "Last-Event-ID", "in": "header", "schema": map[string]any{"type": "string", "pattern": "^[0-9]+$"}}),
			"", map[string]any{
				"200": map[string]any{"description": "The event stream.", "content": map[string]any{"text/event-stream": map[string]any{"schema": map[string]any{"type": "string"}}}},
				"400": operationFailure("Invalid log_from or Last-Event-ID."),
				"403": operationFailure(forbiddenBuild),
				"404": operationFailure("Unknown build, or another principal's."),
			})},
		"/api/v1/runtime-builds/{id}/cancel": map[string]any{"post": op("cancelRuntimeBuild",
			"Cancel a queued or running build; a finished build is unchanged. The owner may cancel their build, runtime.manage any. Send an empty JSON body.",
			buildID, "", map[string]any{
				"202": reply("The build.", ref("RuntimeBuild")),
				"403": operationFailure(forbiddenBuild),
				"404": operationFailure("Unknown build, or another principal's."),
			})},
	}
}
