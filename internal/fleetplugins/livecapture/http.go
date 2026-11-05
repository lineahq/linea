package livecapture

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lineahq/linea/pluginsdk"
)

func strictJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		return ErrInvalid
	}
	return nil
}

type authorityContextKey struct{}

// actingContextKey carries the validated acting-for headers into Create.
type actingContextKey struct{}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, err error) {
	code := 503
	switch {
	case errors.Is(err, ErrInvalid):
		code = 400
	case errors.Is(err, ErrForbidden):
		code = 403
	case errors.Is(err, ErrNotFound):
		code = 404
	case errors.Is(err, ErrConflict):
		code = 409
	case errors.Is(err, ErrPrecondition):
		code = 412
	case errors.Is(err, ErrCapacity):
		code = 429
	case errors.Is(err, ErrCursorExpired):
		code = 410
	}
	if code == 429 {
		w.Header().Set("Retry-After", "1")
	}
	reason := "capture_request_refused"
	if errors.Is(err, ErrPeerBusy) {
		reason = "collector_busy"
	}
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": reason, "message": err.Error()}})
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil || strictJSON(b, out) != nil {
		fail(w, ErrInvalid)
		return false
	}
	return true
}
func revision(w http.ResponseWriter, r *http.Request) (int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		writeJSON(w, 428, map[string]string{"error": "If-Match revision required"})
		return 0, false
	}
	if len(values) != 1 || len(values[0]) < 3 || values[0][0] != '"' || values[0][len(values[0])-1] != '"' {
		fail(w, ErrInvalid)
		return 0, false
	}
	value, err := strconv.ParseInt(values[0][1:len(values[0])-1], 10, 64)
	if err != nil || value < 1 {
		fail(w, ErrInvalid)
		return 0, false
	}
	return value, true
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/metrics" {
		if s.config.metricsToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.config.metricsToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, 401, map[string]string{"error": "metrics credential required"})
			return
		}
		if r.Method != "GET" || r.URL.RawQuery != "" {
			fail(w, ErrInvalid)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.Metrics().Write(w)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.config.serviceToken)) != 1 {
		writeJSON(w, 401, map[string]string{"error": "service credential required"})
		return
	}
	if r.URL.RawQuery != "" && r.URL.Path != pluginsdk.APIPath+"v1/peers" {
		fail(w, ErrInvalid)
		return
	}
	// Metadata metrics intentionally remain nonblocking and payload-free. Every
	// other service request verifies the pinned session before using memory state.
	if r.URL.Path != pluginsdk.APIPath+"v1/metrics" && s.repo.Ready(r.Context()) != nil {
		fail(w, ErrUnavailable)
		return
	}
	if r.Method == "GET" && r.URL.Path == pluginsdk.ManifestPath {
		s.mu.Lock()
		ready := s.dbOK && !s.draining
		s.mu.Unlock()
		if !ready {
			fail(w, ErrUnavailable)
			return
		}
		writeJSON(w, 200, pluginsdk.Manifest{ID: s.config.ID, Version: Version, ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: s.instance, APIVersion: APIVersion})
		return
	}
	s.mu.Lock()
	ready := s.dbOK && s.hostOK && !s.draining
	epoch := s.epoch
	s.mu.Unlock()
	if r.URL.Path == "/readyz" && r.Method == "GET" {
		if !ready {
			fail(w, ErrUnavailable)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ready"})
		return
	}
	caller, roles := r.Header.Values(pluginsdk.CallerIDHeader), r.Header.Values(pluginsdk.CallerRoleHeader)
	if len(caller) != 1 || len(roles) != 1 || !safeName(caller[0]) || (roles[0] != "admin" && roles[0] != "reader") {
		fail(w, ErrForbidden)
		return
	}
	if !ready {
		fail(w, ErrUnavailable)
		return
	}
	if r.URL.Path == pluginsdk.StatusPath && r.Method == "GET" {
		s.mu.Lock()
		counts := map[string]any{"peers": len(s.peers), "executions": len(s.operations), "consumer_channels": len(s.channels), "control": "grpc-v1", "delivery": "otlp-grpc", "plugin_api_version": APIVersion, "host_permissions": s.permissionsOK, "single_authority": true}
		s.mu.Unlock()
		writeJSON(w, 200, counts)
		return
	}
	if !strings.HasPrefix(r.URL.Path, pluginsdk.APIPath) {
		fail(w, ErrNotFound)
		return
	}
	if r.Header.Get(pluginsdk.InstanceHeader) != s.instance || r.Header.Get(pluginsdk.EpochHeader) != epoch {
		fail(w, ErrConflict)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), authorityContextKey{}, epoch))
	path := strings.TrimPrefix(r.URL.Path, pluginsdk.APIPath)
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] != "v1" {
		fail(w, ErrNotFound)
		return
	}
	if path == "v1/openapi.json" && r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAPI)
		return
	}
	if path == "v1/metrics" && r.Method == "GET" {
		writeJSON(w, 200, s.Metrics())
		return
	}
	if path == "v1/peers" && r.Method == "GET" {
		page, err := s.PeerPage(r.URL.RawQuery)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, page)
		return
	}
	if r.Method != "GET" && roles[0] != "admin" {
		fail(w, ErrForbidden)
		return
	}
	owner := caller[0]
	if path == "v1/channels" && r.Method == "POST" {
		var request struct {
			ExpiresAt time.Time `json:"expires_at"`
		}
		if !decode(w, r, &request) {
			return
		}
		result, err := s.NewChannel(r.Context(), owner, request.ExpiresAt)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, result)
		return
	}
	if path == "v1/captures" && r.Method == "POST" {
		var request CreateRequest
		if !decode(w, r, &request) {
			return
		}
		acting, err := pluginsdk.ReadActing(r.Header)
		if err != nil {
			fail(w, ErrInvalid)
			return
		}
		ctx := r.Context()
		if acting != nil {
			ctx = context.WithValue(ctx, actingContextKey{}, *acting)
		}
		result, err := s.Create(ctx, owner, request)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, result.Revision))
		writeJSON(w, 202, result)
		return
	}
	if len(parts) >= 3 && parts[1] == "channels" && pluginsdk.ValidUID(parts[2]) {
		id := parts[2]
		if len(parts) == 3 && r.Method == "DELETE" {
			if err := s.DeleteChannel(owner, id); err != nil {
				fail(w, err)
				return
			}
			w.WriteHeader(204)
			return
		}
		if len(parts) == 4 && parts[3] == "next" && r.Method == "GET" {
			batch, err := s.Next(owner, id)
			if err != nil {
				fail(w, err)
				return
			}
			if batch == nil {
				w.WriteHeader(204)
				return
			}
			writeJSON(w, 200, batch)
			return
		}
		if len(parts) == 4 && parts[3] == "acks" && r.Method == "POST" {
			var ack Acknowledgement
			if !decode(w, r, &ack) {
				return
			}
			if err := s.Ack(owner, id, ack); err != nil {
				fail(w, err)
				return
			}
			w.WriteHeader(204)
			return
		}
	}
	if len(parts) >= 3 && parts[1] == "captures" && pluginsdk.ValidUID(parts[2]) {
		id := parts[2]
		if len(parts) == 3 && r.Method == "GET" {
			op, err := s.Get(r.Context(), owner, id)
			if err != nil {
				fail(w, err)
				return
			}
			w.Header().Set("ETag", fmt.Sprintf(`"%d"`, op.Revision))
			writeJSON(w, 200, op)
			return
		}
		if len(parts) == 4 && parts[3] == "stop" && r.Method == "POST" {
			rev, ok := revision(w, r)
			if !ok {
				return
			}
			op, err := s.Stop(r.Context(), owner, id, rev)
			if err != nil {
				fail(w, err)
				return
			}
			w.Header().Set("ETag", fmt.Sprintf(`"%d"`, op.Revision))
			writeJSON(w, 202, op)
			return
		}
	}
	fail(w, ErrNotFound)
}
