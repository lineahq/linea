package livecapture

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/lineahq/linea/pluginsdk"
)

func TestDrainWithdrawsExecutionAndManifest(t *testing.T) {
	s, request, p := fixture(t)
	op, err := s.Create(context.Background(), "owner", request)
	if err != nil {
		t.Fatal(err)
	}
	s.Drain()
	s.mu.Lock()
	state := s.operations[op.ID].op.State
	message, err := s.desiredLocked(p)
	s.mu.Unlock()
	if err != nil || state != "stopping" || len(message.GetSessions()) != 0 {
		t.Fatal("drain did not withdraw existing execution")
	}
	request.ID = pluginsdk.RandomID()
	if _, err := s.Create(context.Background(), "owner", request); !errors.Is(err, ErrUnavailable) {
		t.Fatal("draining accepted capture")
	}
	r := httptest.NewRequest("GET", pluginsdk.ManifestPath, nil)
	r.Header.Set("Authorization", "Bearer "+s.config.serviceToken)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("draining advertised ready manifest")
	}
}
