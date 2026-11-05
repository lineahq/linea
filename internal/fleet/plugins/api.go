package plugins

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

func validAPIPath(path string) bool {
	if len(path) == 0 || len(path) > 512 {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 128 {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return false
			}
		}
	}
	return true
}

// ServeAPI routes opaque bounded JSON to an operator-granted plugin API.
// It deliberately has no knowledge of any plugin's routes or domain types.
func (r *Registry) ServeAPI(w http.ResponseWriter, request *http.Request, id, path string, caller auth.Principal) {
	started := time.Now()
	defer func() { r.apiDuration.Observe(time.Since(started)) }()
	fail := func(code int) {
		r.apiErrors.Add(1)
		if code == 429 {
			w.Header().Set("Retry-After", "1")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":{"code":"plugin_request_refused","message":"plugin API request refused"}}`))
	}
	if !pluginsdk.ValidID(id) || !validAPIPath(path) || request.URL.EscapedPath() != request.URL.Path || len(request.URL.RawQuery) > 2048 {
		fail(400)
		return
	}
	if request.Method != "GET" && request.Method != "POST" && request.Method != "PUT" && request.Method != "PATCH" && request.Method != "DELETE" {
		fail(405)
		return
	}
	if caller.ID == "" || (caller.Role != "reader" && caller.Role != "admin") {
		fail(403)
		return
	}
	e := r.find(id)
	if e == nil || e.config.APIVersion == "" {
		fail(404)
		return
	}
	if !e.ready() || r.ctx.Err() != nil {
		fail(503)
		return
	}
	gate := e.apiReads
	if request.Method != "GET" {
		gate = e.apiWrites
	}
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		r.apiRefused.Add(1)
		fail(429)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, request.Body, 64<<10))
	if err != nil {
		fail(413)
		return
	}
	if len(body) != 0 && !json.Valid(body) {
		fail(400)
		return
	}
	// Body reads can outlive an authority change. Snapshot readiness and the
	// matching cancellation lease together immediately before dispatch.
	e.mu.RLock()
	before, lease := e.info, e.lease
	ready := before.Desired == "enabled" && before.State == "ready" && before.CheckedAt != nil && time.Since(*before.CheckedAt) < 3*time.Second
	e.mu.RUnlock()
	if !ready || r.ctx.Err() != nil {
		fail(503)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	stop := context.AfterFunc(lease, cancel)
	defer stop()
	stopRegistry := context.AfterFunc(r.ctx, cancel)
	defer stopRegistry()
	endpoint := e.config.Endpoint + pluginsdk.APIPath + path
	if request.URL.RawQuery != "" {
		endpoint += "?" + request.URL.RawQuery
	}
	upstream, err := http.NewRequestWithContext(ctx, request.Method, endpoint, strings.NewReader(string(body)))
	if err != nil {
		fail(400)
		return
	}
	upstream.Header.Set("Authorization", "Bearer "+e.config.serviceToken)
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set(pluginsdk.CallerIDHeader, caller.ID)
	upstream.Header.Set(pluginsdk.CallerRoleHeader, caller.Role)
	// Only the validated acting-for value is forwarded, never client copies.
	fleet.ActingFrom(request.Context()).Set(upstream.Header)
	upstream.Header.Set(pluginsdk.InstanceHeader, before.InstanceID)
	upstream.Header.Set(pluginsdk.EpochHeader, before.Epoch)
	if values := request.Header.Values("If-Match"); len(values) != 0 {
		if len(values) != 1 || len(values[0]) > 128 || strings.ContainsAny(values[0], "\r\n") {
			fail(400)
			return
		}
		upstream.Header.Set("If-Match", values[0])
	}
	response, err := e.apiClient.Do(upstream)
	if err != nil {
		fail(503)
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || e.snapshot().Epoch != before.Epoch || ctx.Err() != nil {
		fail(503)
		return
	}
	code := response.StatusCode
	if code == 204 && len(data) == 0 {
		w.WriteHeader(204)
		return
	}
	if (code < 200 || code > 299) && code != 400 && code != 403 && code != 404 && code != 409 && code != 410 && code != 412 && code != 413 && code != 428 && code != 429 && code != 503 {
		fail(503)
		return
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") || !json.Valid(data) {
		fail(503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if code >= 400 {
		r.apiErrors.Add(1)
	}
	if retry := response.Header.Get("Retry-After"); code == 429 && retry == "1" {
		w.Header().Set("Retry-After", retry)
	}
	if etag := response.Header.Get("ETag"); len(etag) > 0 && len(etag) <= 128 && !strings.ContainsAny(etag, "\r\n") {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(code)
	_, _ = w.Write(data)
}
