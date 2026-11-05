package fleetcapture

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestActingHeadersFitFleetBounds: the context's acting-for value is sent on
// every call, fitted to fleet's bounds so attribution never fails a call.
func TestActingHeadersFitFleetBounds(t *testing.T) {
	seen := make(chan http.Header, 1)
	c := newFake(t, map[string]func(http.ResponseWriter, *http.Request, []byte){
		"GET /api/v1/groups/" + strings.Repeat("e", 32): func(w http.ResponseWriter, r *http.Request, _ []byte) {
			seen <- r.Header.Clone()
			reply(w, 200, Group{ID: strings.Repeat("e", 32), Kind: "static"})
		},
	})
	call := func(ctx context.Context) http.Header {
		t.Helper()
		if _, err := c.Group(ctx, strings.Repeat("e", 32)); err != nil {
			t.Fatal(err)
		}
		return <-seen
	}
	if h := call(context.Background()); h.Get(actingIDHeader) != "" {
		t.Fatalf("headers without a value: %v", h)
	}
	long := strings.Repeat("é", 100) // 200 bytes
	h := call(WithActing(context.Background(), Acting{ID: "user:alice", Name: " " + long, RequestID: "req-1"}))
	name := h.Get(actingNameHeader)
	if h.Get(actingIDHeader) != "user:alice" || h.Get(actingRequestHeader) != "req-1" || len(name) > maxActingName || !strings.HasPrefix(long, name) || name == "" {
		t.Fatalf("fitted headers: %v", h)
	}
	h = call(WithActing(context.Background(), Acting{ID: "user:bob", Name: "\x01"}))
	if h.Get(actingIDHeader) != "user:bob" || h.Values(actingNameHeader) != nil || h.Values(actingRequestHeader) != nil {
		t.Fatalf("unusable name: %v", h)
	}
	h = call(WithActing(context.Background(), Acting{ID: " padded", Name: "Alice"}))
	if h.Values(actingIDHeader) != nil || h.Values(actingNameHeader) != nil {
		t.Fatalf("unusable id: %v", h)
	}
}
