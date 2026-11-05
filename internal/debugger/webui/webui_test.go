package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerServesClientRoutes: client-side routes get index.html when the
// client is built; otherwise every path explains how to build it.
func TestHandlerServesClientRoutes(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/some/client/route?run=x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !Built() {
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "build-web.ps1") {
			t.Fatalf("status %d: %s", resp.StatusCode, body)
		}
		return
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `<div id="root">`) {
		t.Fatalf("status %d: %.200s", resp.StatusCode, body)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index cache control %q", cc)
	}
}
