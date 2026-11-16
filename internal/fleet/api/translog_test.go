package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/translog"
)

func TestManagementAuthenticationFailuresAreCountedForTheSummary(t *testing.T) {
	token := strings.Repeat("m", 32)
	credential, _ := auth.New(token)
	h := New(fleet.NewStore(3), credential, ServerInfo{})
	var out bytes.Buffer
	log := translog.New(&out)
	h.SetLogger(log)
	for _, tc := range []struct {
		token string
		want  int
	}{{"", 401}, {strings.Repeat("x", 32), 401}, {token, 200}, {strings.Repeat("y", 32), 401}} {
		r := httptest.NewRequest("GET", "/api/v1/server", nil)
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("token %q: status %d, want %d", tc.token, w.Code, tc.want)
		}
	}
	// Health probes are not authenticated and never count.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	if out.Len() != 0 {
		t.Fatalf("requests wrote %q", out.String())
	}
	log.Flush()
	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("summary: %q: %v", out.String(), err)
	}
	if line["msg"] != "auth_failures" || line["listener"] != "management" || line["count"] != float64(3) || bytes.Count(out.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("summary = %v", line)
	}
}
