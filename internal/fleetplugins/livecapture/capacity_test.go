package livecapture

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCapacityRefusalsPromiseNoRetryAfter(t *testing.T) {
	// No refusal of this kind clears on a schedule the plugin can state, so none
	// may carry a Retry-After.
	s, r, _ := fixture(t)
	w := httptest.NewRecorder()
	fail(w, ErrCapacity)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "" {
		t.Fatalf("status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	for i := len(s.channels); i < 32; i++ {
		if _, err := s.NewChannel(context.Background(), "owner", time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.NewChannel(context.Background(), "owner", time.Now().Add(time.Minute))
	w = httptest.NewRecorder()
	fail(w, err)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "" {
		t.Fatalf("full channel table: status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	for i := 0; i < 32; i++ {
		s.operations[string(rune('a'+i))] = &activeOperation{}
	}
	_, err = s.Create(context.Background(), "owner", r)
	w = httptest.NewRecorder()
	fail(w, err)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "" {
		t.Fatalf("full operation table: status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
}
