package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

func TestDrainRejectsMutationsAndReadiness(t *testing.T) {
	token := strings.Repeat("m", 32)
	bearer, _ := auth.New(token)
	h := NewAuthorized(fleet.NewStore(2), auth.Static{Bearer: bearer, Role: "admin"}, ServerInfo{}, nil, nil)
	h.SetReady(true)
	h.Drain()
	for _, c := range []struct {
		method, path string
		status       int
	}{{"GET", "/readyz", 503}, {"POST", "/api/v1/credentials", 503}, {"GET", "/api/v1/server", 200}} {
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.status {
			t.Fatal("drain readiness/admission mismatch")
		}
	}
}
