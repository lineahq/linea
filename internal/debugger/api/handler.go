// Package api exposes the platform over a local HTTP API for the web
// client and scripts. Runs are asynchronous: POST /api/v1/runs queues a run
// and returns its ID; its events stream over SSE and its report, snapshots
// and diffs are retrieved independently of the stream. The complete report
// is the engine's report document, byte-identical to the CLI's JSON output.
// openapi.json is the contract; see openapi.go.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/samples"
)

// workersConnectPath is where workers dial the server, and
// buildersConnectPath where builders do.
const (
	workersConnectPath  = "/v1/workers/connect"
	buildersConnectPath = "/v1/builders/connect"
)

// maxRequestBytes bounds a request document. Configurations and scenarios
// may be sent as text with inline telemetry.
const maxRequestBytes = 16 << 20

// ConfigGraph is the response of POST /api/v1/config/graph.
type ConfigGraph struct {
	Valid       bool                  `json:"valid"`
	Diagnostics []platform.Diagnostic `json:"diagnostics"`
	Topology    *platform.Graph       `json:"topology,omitempty"`
}

// Server is the API adapter. Its platform may be owned or shared.
type Server struct {
	mux   *http.ServeMux
	runs  *platform.Service
	owned bool
	extra http.Handler
	auth  platform.Authenticator
	// remote is Options.RemoteAccess.
	remote bool
	logger *zap.Logger
}

// Options configure a Server.
type Options struct {
	// Platform is shared with other adapters. The caller owns its lifecycle.
	// When nil, New creates a private platform and Close shuts it down.
	Platform *platform.Service
	// Logger records each refused authentication, and configures an
	// internally created platform; a supplied Platform's owner configures
	// its logging. Nil discards logs.
	Logger *zap.Logger
	// Fallback serves requests outside /api/ (the web client); nil answers
	// them with 404.
	Fallback http.Handler
	// Authenticator identifies each API request's principal.
	// Nil makes every request the local owner's, for tests and embedding;
	// linea-server always sets it.
	Authenticator platform.Authenticator
	// RemoteAccess accepts requests for any Host: the server is reached
	// from other machines, and authentication rather than a loopback Host
	// keeps it private. linea-server always sets it; false, for tests and
	// embedding, accepts only a loopback Host.
	RemoteAccess bool
}

// NewHandler returns the API handler with default options and a logger.
func NewHandler(logger *zap.Logger) *Server { return New(Options{Logger: logger}) }

// New returns the API handler.
func New(opts Options) *Server {
	s := &Server{mux: http.NewServeMux(), runs: opts.Platform, extra: opts.Fallback, auth: opts.Authenticator, remote: opts.RemoteAccess, logger: opts.Logger}
	if s.logger == nil {
		s.logger = zap.NewNop()
	}
	if s.auth == nil {
		s.auth = platform.LocalAuthenticator{}
	}
	if s.runs == nil {
		s.runs, s.owned = platform.New(platform.Options{Logger: opts.Logger, Environment: platform.EnvironmentPolicy{LegacyHost: true}, Paths: platform.PathPolicy{Unrestricted: true}}), true
	}
	s.mux.HandleFunc("GET /api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAPIDocument)
	})
	s.mux.HandleFunc("GET /api/v1/audit-events", s.listAuditEvents)
	s.mux.HandleFunc("GET /api/v1/session", s.session)
	s.mux.HandleFunc("POST /api/v1/tokens", s.createToken)
	s.mux.HandleFunc("GET /api/v1/tokens", s.listTokens)
	s.mux.HandleFunc("DELETE /api/v1/tokens/{id}", s.revokeToken)
	s.mux.HandleFunc("GET /api/v1/role-bindings", s.listRoleBindings)
	s.mux.HandleFunc("POST /api/v1/role-bindings", s.addRoleBinding)
	s.mux.HandleFunc("DELETE /api/v1/role-bindings/{id}", s.removeRoleBinding)
	s.mux.HandleFunc("GET /api/v1/samples", s.listSamples)
	s.mux.HandleFunc("GET /api/v1/collector-groups", s.listCollectorGroups)
	s.mux.HandleFunc("GET /api/v1/collector-groups/{id}", s.getCollectorGroup)
	s.mux.HandleFunc("GET /api/v1/collector-groups/{id}/members", s.listCollectorGroupMembers)
	s.mux.HandleFunc("PATCH /api/v1/captures/{id}", s.updateCaptureMetadata)
	s.mux.HandleFunc("GET /api/v1/collectors", s.listCollectors)
	s.mux.HandleFunc("GET /api/v1/collectors/{id}", s.getCollector)
	s.mux.HandleFunc("GET /api/v1/collectors/{id}/capture-points", s.listCapturePoints)
	s.mux.HandleFunc("POST /api/v1/captures/validate", s.validateCapture)
	s.mux.HandleFunc("POST /api/v1/captures", s.createCapture)
	s.mux.HandleFunc("GET /api/v1/captures", s.listCaptures)
	s.mux.HandleFunc("GET /api/v1/captures/{id}", s.getCapture)
	s.mux.HandleFunc("GET /api/v1/captures/{id}/records", s.captureRecords)
	s.mux.HandleFunc("POST /api/v1/captures/{id}/fixtures", s.captureFixture)
	s.mux.HandleFunc("POST /api/v1/captures/{id}/scenario", s.captureScenario)
	s.mux.HandleFunc("POST /api/v1/captures/{id}/export", s.captureExport)
	s.mux.HandleFunc("DELETE /api/v1/captures/{id}", s.deleteCapture)
	s.mux.HandleFunc("POST /api/v1/captures/{id}/stop", s.stopCapture)
	s.mux.HandleFunc("POST /api/v1/artifacts", s.saveArtifact)
	s.mux.HandleFunc("GET /api/v1/artifacts", s.listArtifacts)
	s.mux.HandleFunc("GET /api/v1/artifacts/{artifact}", s.getArtifact)
	s.mux.HandleFunc("GET /api/v1/artifacts/{artifact}/revisions/{revision}", s.getArtifactRevision)
	s.mux.HandleFunc("GET /api/v1/artifacts/{artifact}/revisions", s.listArtifactRevisions)
	s.mux.HandleFunc("POST /api/v1/artifacts/export", s.exportArtifacts)
	s.mux.HandleFunc("POST /api/v1/artifacts/import", s.importArtifacts)
	s.mux.HandleFunc("POST /api/v1/config/validate", s.validate)
	s.mux.HandleFunc("POST /api/v1/config/graph", s.graph)
	s.mux.HandleFunc("POST /api/v1/ottl/evaluate", s.evaluateOTTL)
	s.mux.HandleFunc("POST /api/v1/candidates", s.createCandidate)
	s.mux.HandleFunc("POST /api/v1/runs", s.createRun)
	s.mux.HandleFunc("GET /api/v1/runs", s.listRuns)
	s.mux.HandleFunc("GET /api/v1/runs/{id}", s.getRun)
	s.mux.HandleFunc("DELETE /api/v1/runs/{id}", s.deleteRun)
	s.routeRunRetention()
	s.mux.HandleFunc("GET /api/v1/runs/{id}/report", s.getReport)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/inputs", s.getRunInputs)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/log", s.getRunLog)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/summary", s.getRunSummary)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/evidence", s.queryEvidence)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/evaluations", s.listEvaluations)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/evaluations/{evaluation}", s.getEvaluation)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/statements/test", s.testStatementOnRun)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/cancel", s.cancelRun)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/events", s.streamEvents)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/snapshots/{ref}", s.getSnapshot)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/diffs", s.getDiffs)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/assertions", s.evaluateAssertions)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/scenario", s.createScenarioFromRun)
	s.mux.HandleFunc("POST /api/v1/runs/{id}/export", s.exportScenario)
	s.mux.HandleFunc("GET /api/v1/runs/{id}/compare/{after}", s.compareRuns)
	s.mux.HandleFunc("POST /api/v1/comparisons", s.createComparison)
	s.mux.HandleFunc("POST /api/v1/suites", s.createSuite)
	s.mux.HandleFunc("GET /api/v1/suites", s.listSuites)
	s.mux.HandleFunc("GET /api/v1/suites/{id}", s.getSuite)
	s.mux.HandleFunc("DELETE /api/v1/suites/{id}", s.deleteSuite)
	s.mux.HandleFunc("GET /api/v1/suites/{id}/report", s.getSuiteReport)
	s.mux.HandleFunc("POST /api/v1/suites/{id}/rerun", s.rerunSuite)
	s.mux.HandleFunc("GET /api/v1/suites/{id}/junit", s.getSuiteJUnit)
	s.routeRuntimes()
	s.routeProjects()
	s.routeRuntimeBuilds()
	s.routeUpgrades()
	s.routeBisects()
	s.routeFleet()
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, errors.New("no such endpoint"))
	})
	return s
}

// Close shuts down an internally created platform. An injected platform belongs
// to the composing application; closing this adapter cannot cancel shared work.
func (s *Server) Close() {
	if s.owned {
		s.runs.Close()
	}
}

// ServeHTTP applies the browser checks, authenticates, then routes the
// request.
//
// The API must not be reachable from web pages the browser visits:
//   - Without RemoteAccess (tests, embedding), Host must name a loopback
//     host, defeating DNS rebinding. With it, as linea-server composes it,
//     authentication protects it.
//   - POST/PUT/PATCH bodies must be application/json, which a cross-origin page can
//     only send after a CORS preflight that is never answered; DELETE needs
//     that preflight too. The one exception is the runtime build upload, a
//     multipart/form-data body that must carry a custom header, which needs
//     the same preflight (runtime_builds.go).
//
// Workers dial /v1/workers/connect with the registration
// token, and builders /v1/builders/connect with the build
// token, instead of a principal's credential; a browser cannot open either:
// the platform refuses an upgrade that carries an Origin.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case workersConnectPath:
		s.runs.ConnectWorker(w, r)
		return
	case buildersConnectPath:
		s.runs.ConnectBuilder(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/artifacts") {
		w.Header().Set("Cache-Control", "no-store")
	}
	if !s.remote && !loopbackHost(r.Host) {
		writeError(w, http.StatusForbidden, fmt.Errorf("host %q is not a loopback host", r.Host))
		return
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || (mt != "application/json" && !uploadBuild(r)) {
			writeError(w, http.StatusUnsupportedMediaType, errors.New("content type must be application/json"))
			return
		}
	}
	if s.extra != nil && !strings.HasPrefix(r.URL.Path, "/api/") {
		s.extra.ServeHTTP(w, r)
		return
	}
	id := newRequestID()
	w.Header().Set("X-Request-Id", id)
	principal, err := s.auth.Authenticate(r)
	if err != nil {
		// The reason never carries the credential.
		s.logger.Warn("authentication refused", zap.String("request_id", id), zap.String("client", r.RemoteAddr),
			zap.String("interface", "http"), zap.String("reason", err.Error()))
		writeFailure(w, platform.Unauthenticated(err))
		return
	}
	project, err := callerProject(r)
	if err != nil {
		writeFailure(w, badQuery(err.Error()))
		return
	}
	caller := platform.Caller{Principal: principal, Client: "http", RequestID: id, Project: project}
	s.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller)))
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// decode reads one strict, bounded request document; whitespace may follow
// it, anything else is refused.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New("expected one request document"))
		return false
	}
	return true
}

// callerKey keys a request's authenticated caller in its context.
type callerKey struct{}

// callerOf is the caller ServeHTTP authenticated for r. A request that did
// not pass ServeHTTP has none, and the platform refuses the zero caller.
func callerOf(r *http.Request) platform.Caller {
	c, _ := r.Context().Value(callerKey{}).(platform.Caller)
	return c
}

// newRequestID names one request for audit and correlation.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req-" + hex.EncodeToString(b[:])
}

func (s *Server) inspect(w http.ResponseWriter, r *http.Request) (*platform.Inspection, bool) {
	var req InspectRequest
	if !decode(w, r, &req) {
		return nil, false
	}
	ins, err := s.runs.InspectConfiguration(r.Context(), callerOf(r), req.inspect())
	if err != nil {
		writeSourceFailure(w, err)
		return nil, false
	}
	return ins, true
}

// validate reports diagnostics, topology and the redacted effective
// configuration. An invalid configuration is a 200 with valid false.
func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	if ins, ok := s.inspect(w, r); ok {
		writeJSON(w, http.StatusOK, ins)
	}
}

func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	if ins, ok := s.inspect(w, r); ok {
		writeJSON(w, http.StatusOK, ConfigGraph{Valid: ins.Valid, Diagnostics: ins.Diagnostics, Topology: ins.Topology})
	}
}

// listSamples serves the embedded sample configurations, inputs and
// scenarios.
func (s *Server) listSamples(w http.ResponseWriter, r *http.Request) {
	list, err := samples.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// evaluateOTTL is the OTTL playground: it replays a transform processor
// on a payload in the request, without queueing a run.
func (s *Server) evaluateOTTL(w http.ResponseWriter, r *http.Request) {
	var req OTTLRequest
	if !decode(w, r, &req) {
		return
	}
	ev, err := s.runs.PreviewTransformation(r.Context(), callerOf(r), req.preview())
	if err != nil {
		writeSourceFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req Request
	if !decode(w, r, &req) {
		return
	}
	run, err := s.runs.SubmitRun(r.Context(), callerOf(r), req.run())
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/runs/"+run.ID)
	writeJSON(w, http.StatusAccepted, runOf(run))
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := platform.RunQuery{RunFilter: platform.RunFilter{Scenario: q.Get("scenario"), Runtime: q.Get("runtime"), Result: q.Get("result")}, Cursor: q.Get("cursor")}
	if v := q.Get("limit"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil || limit < 1 {
			writeFailure(w, &platform.Error{Code: platform.CodeInvalidRequest, Message: fmt.Sprintf("invalid request: limit must be between 1 and %d", platform.MaxRunPage)})
			return
		}
		query.Limit = limit
	}
	page, err := s.runs.ListRuns(r.Context(), callerOf(r), query)
	if err != nil {
		writeFailure(w, err)
		return
	}
	out := RunPage{Runs: make([]Run, len(page.Runs)), NextCursor: page.NextCursor}
	for i, run := range page.Runs {
		out.Runs[i] = runOf(run)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.runs.GetRun(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runOf(run))
}

// getRunInputs returns the sources a run was admitted with. They are the
// user's own inputs, so no cache keeps them.
func (s *Server) getRunInputs(w http.ResponseWriter, r *http.Request) {
	in, err := s.runs.GetRunInputs(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, runInputsOf(in))
}

// getRunLog returns a failed or partial run's redacted Collector log tail.
// It is run evidence, so no cache keeps it.
func (s *Server) getRunLog(w http.ResponseWriter, r *http.Request) {
	log, err := s.runs.GetRunLog(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, log)
}

// getReport returns the complete engine report. With ?wait=true it blocks
// until the run finishes; otherwise an unfinished run is a 409.
func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	rep, err := s.runs.GetRunReport(r.Context(), callerOf(r), r.PathValue("id"), r.URL.Query().Get("wait") == "true")
	if err != nil {
		if r.Context().Err() == nil {
			writeFailure(w, err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = platform.Encode(w, rep)
}

// cancelRun cancels a queued or running run. A running run still produces a
// report, with result status cancelled.
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	c, err := s.runs.CancelRun(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, runOf(c.Run))
}

// deleteRun and deleteSuite delete finished history; failures carry
// the platform code, so a client can tell a conflict from a deleted run.
func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) {
	if err := s.runs.DeleteRun(r.Context(), callerOf(r), r.PathValue("id")); err != nil {
		writeOperationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSuite(w http.ResponseWriter, r *http.Request) {
	if err := s.runs.DeleteSuite(r.Context(), callerOf(r), r.PathValue("id")); err != nil {
		writeOperationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) {
	data, err := s.runs.GetSnapshot(r.Context(), callerOf(r), r.PathValue("id"), r.PathValue("ref"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

func (s *Server) getDiffs(w http.ResponseWriter, r *http.Request) {
	changes, err := s.runs.GetStageChanges(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Diffs{Stages: changes.Stages, OTTL: changes.OTTL})
}

// evaluateAssertions re-evaluates edited assertions against a done run: the
// scenario editor's quick check, which needs no rerun while the inputs are
// unchanged.
func (s *Server) evaluateAssertions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.runs.GetRun(r.Context(), callerOf(r), id); err != nil {
		writeFailure(w, err) // An unknown run is reported before a malformed body.
		return
	}
	var req AssertionsRequest
	if !decode(w, r, &req) {
		return
	}
	rep, err := s.runs.EvaluateAssertions(r.Context(), callerOf(r), id, platform.AssertionsRequest{ScenarioYAML: req.ScenarioYAML})
	if err != nil {
		if platform.ErrorCode(err) == platform.CodeConflict {
			writeFailure(w, err)
		} else {
			writeSourceFailure(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, rep.Result)
}

// streamEvents streams a run's events as server-sent events: the recorded
// events first, then live ones, each with "<attempt>:<seq>" as the SSE id so
// that a reconnecting client resumes after Last-Event-ID. When the run was
// requeued and executes again, a "restart" event (data Restart) tells the
// client to drop the events it holds; the new attempt's follow. A final
// "end" event carries the run's state and status.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.runs.GetRun(r.Context(), callerOf(r), id); err != nil {
		writeFailure(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	var after platform.EventCursor
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		c, err := parseEventCursor(last)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("Last-Event-ID %q: %v", last, err))
			return
		}
		after = c
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for {
		watch, err := s.runs.WatchRun(r.Context(), callerOf(r), id, after)
		if err != nil {
			return
		}
		if watch.Restarted {
			data, _ := json.Marshal(Restart{Attempt: watch.Attempt})
			if _, err := fmt.Fprintf(w, "id: %d:0\nevent: restart\ndata: %s\n\n", watch.Attempt, data); err != nil {
				return
			}
			after = platform.EventCursor{Attempt: watch.Attempt}
		}
		for _, e := range watch.Events {
			data, err := json.Marshal(e)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d:%d\nevent: event\ndata: %s\n\n", watch.Attempt, e.Seq, data); err != nil {
				return
			}
			after = platform.EventCursor{Attempt: watch.Attempt, Seq: e.Seq}
		}
		if watch.Terminal {
			data, _ := json.Marshal(End{State: stateOf(watch.End.State), Status: watch.End.Status, Error: watch.End.Error})
			fmt.Fprintf(w, "event: end\ndata: %s\n\n", data)
			flusher.Flush()
			return
		}
		flusher.Flush()
		select {
		case <-watch.Changed:
		case <-r.Context().Done():
			return
		}
	}
}

// parseEventCursor reads an SSE id, "<attempt>:<seq>". A bare seq is an id
// from before attempts were part of it, which only a first attempt sent.
func parseEventCursor(id string) (platform.EventCursor, error) {
	attempt, seq, found := strings.Cut(id, ":")
	if !found {
		attempt, seq = "1", id
	}
	a, err := strconv.Atoi(attempt)
	if err != nil {
		return platform.EventCursor{}, err
	}
	n, err := strconv.Atoi(seq)
	if err != nil {
		return platform.EventCursor{}, err
	}
	if a < 1 || n < 0 {
		return platform.EventCursor{}, errors.New("out of range")
	}
	return platform.EventCursor{Attempt: a, Seq: n}, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = platform.Encode(w, v)
}
