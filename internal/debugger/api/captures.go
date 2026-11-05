package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// CaptureStopRequest is the empty body of a stop request.
type CaptureStopRequest struct{}

// Capture mutations use one bounded strict document, including empty stop bodies.
func decodeCapture(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeCaptureSized(w, r, v, 64<<10)
}

func decodeCaptureSized(w http.ResponseWriter, r *http.Request, v any, size int64) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, size))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeOperationError(w, &platform.Error{Code: platform.CodeInvalidRequest, Message: "invalid capture request JSON"})
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		writeOperationError(w, &platform.Error{Code: platform.CodeInvalidRequest, Message: "expected one capture request document"})
		return false
	}
	return true
}

func captureReply(w http.ResponseWriter, result any, err error) {
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) validateCapture(w http.ResponseWriter, r *http.Request) {
	var req platform.CaptureSelection
	if !decodeCapture(w, r, &req) {
		return
	}
	result, err := s.runs.ValidateCaptureRequest(r.Context(), callerOf(r), req)
	captureReply(w, result, err)
}

func (s *Server) createCapture(w http.ResponseWriter, r *http.Request) {
	var req platform.CaptureSelection
	if !decodeCapture(w, r, &req) {
		return
	}
	result, err := s.runs.CreateCaptureSession(r.Context(), callerOf(r), req)
	captureReply(w, result, err)
}

func (s *Server) listCaptures(w http.ResponseWriter, r *http.Request) {
	req, err := collectorPageRequest(r)
	if err != nil {
		captureReply(w, nil, err)
		return
	}
	result, err := s.runs.ListCaptureSessions(r.Context(), callerOf(r), req)
	captureReply(w, result, err)
}

func (s *Server) getCapture(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("wait") == "true" {
		// A bounded observation timeout never cancels a session.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		result, err := s.runs.WaitCaptureSession(ctx, callerOf(r), r.PathValue("id"))
		if ctx.Err() != nil && r.Context().Err() == nil {
			result, err = s.runs.GetCaptureSession(r.Context(), callerOf(r), r.PathValue("id"))
		}
		captureReply(w, result, err)
		return
	}
	result, err := s.runs.GetCaptureSession(r.Context(), callerOf(r), r.PathValue("id"))
	captureReply(w, result, err)
}

func (s *Server) stopCapture(w http.ResponseWriter, r *http.Request) {
	var empty CaptureStopRequest
	if !decodeCapture(w, r, &empty) {
		return
	}
	result, err := s.runs.StopCaptureSession(r.Context(), callerOf(r), r.PathValue("id"))
	captureReply(w, result, err)
}

func capturePaths() map[string]any {
	responses := func(schema string) map[string]any {
		return map[string]any{"200": reply("Capture control result; target counters are fleet's last report, not received telemetry.", ref(schema)), "400": operationFailure("Invalid capture request."), "403": operationFailure("Fleet refused the capture, such as raw points without permission."), "404": operationFailure("Collector, group or session not found."), "409": operationFailure("Target busy, reconnected or unable to serve the selection."), "413": operationFailure("Capture capacity exceeded."), "503": operationFailure("Fleet not connected or platform closed.")}
	}
	return map[string]any{
		"/api/v1/captures/validate":  map[string]any{"post": op("validateCapture", "Preflight a selection against fleet's Collectors; the Collector compiles filters on activation.", nil, "CaptureSelection", responses("CaptureValidation"))},
		"/api/v1/captures":           map[string]any{"post": op("createCapture", "Ask fleet to admit a bounded capture; targets activate asynchronously.", nil, "CaptureSelection", responses("CaptureSession")), "get": op("listCaptures", "List ephemeral capture sessions in bounded pages.", []any{map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer"}}, map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}}}, "", responses("CaptureSessionPage"))},
		"/api/v1/captures/{id}":      map[string]any{"get": op("getCapture", "Read a capture, or wait up to 30 seconds for terminal state without stopping it.", []any{idParam, map[string]any{"name": "wait", "in": "query", "schema": map[string]any{"type": "boolean"}}}, "", responses("CaptureSession"))},
		"/api/v1/captures/{id}/stop": map[string]any{"post": op("stopCapture", "Ask fleet to stop the capture; targets confirm asynchronously.", []any{idParam}, "CaptureStopRequest", responses("CaptureSession"))},
	}
}
