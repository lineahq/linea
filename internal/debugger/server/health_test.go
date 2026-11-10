package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHealthAnswersReadinessUntilDraining: /healthz answers 200 while the
// server serves, /readyz answers 503 once it drains, both only to GET or
// HEAD, and every other path reaches the API.
func TestHealthAnswersReadinessUntilDraining(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, tc := range []struct {
		name     string
		method   string
		path     string
		draining bool
		want     int
	}{
		{"healthy", http.MethodGet, healthPath, false, http.StatusOK},
		{"healthy while draining", http.MethodGet, healthPath, true, http.StatusOK},
		{"ready", http.MethodGet, readyPath, false, http.StatusOK},
		{"head ready", http.MethodHead, readyPath, false, http.StatusOK},
		{"not ready while draining", http.MethodGet, readyPath, true, http.StatusServiceUnavailable},
		{"post refused", http.MethodPost, readyPath, false, http.StatusMethodNotAllowed},
		{"api", http.MethodGet, "/api/v1/runs", false, http.StatusTeapot},
		{"api while draining", http.MethodGet, "/api/v1/runs", true, http.StatusTeapot},
		{"prefix is not health", http.MethodGet, healthPath + "/x", false, http.StatusTeapot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var draining atomic.Bool
			draining.Store(tc.draining)
			rec := httptest.NewRecorder()
			health(api, &draining).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
			}
		})
	}
}

// TestProbeExitsZeroOnlyWhenReady: linea-server probe exits 0 on 200 and 1
// on any other answer, on a refused connection and on a stray argument.
func TestProbeExitsZeroOnlyWhenReady(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for _, tc := range []struct {
		name   string
		status int
		args   []string
		want   int
	}{
		{"ready", http.StatusOK, []string{"--url", srv.URL + readyPath}, exitOK},
		{"draining", http.StatusServiceUnavailable, []string{"--url", srv.URL + readyPath}, exitError},
		{"redirect is not ready", http.StatusFound, []string{"--url", srv.URL + readyPath}, exitError},
		{"refused", http.StatusOK, []string{"--url", closed.URL + readyPath}, exitError},
		{"stray argument", http.StatusOK, []string{"--url", srv.URL, "now"}, exitError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status = tc.status
			var stderr bytes.Buffer
			if code := Main(context.Background(), append([]string{"probe"}, tc.args...), io.Discard, &stderr); code != tc.want {
				t.Errorf("probe exit = %d, want %d: %s", code, tc.want, stderr.String())
			}
		})
	}
}

// TestServeAnswersHealthWithoutCredential: the running server answers
// /healthz and /readyz without a credential, and probe accepts it.
func TestServeAnswersHealthWithoutCredential(t *testing.T) {
	path := writeTeam(t, teamYAML())
	ctx, cancel := context.WithCancel(context.Background())
	notices := make(serveNotices, 16)
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"--config", path, "--addr", "127.0.0.1:0"}, io.Discard, notices)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != exitOK {
				t.Errorf("serve exit = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop")
		}
	})
	var server string
	for server == "" {
		select {
		case notice := <-notices:
			if rest, ok := strings.CutPrefix(notice, "serving API on "); ok {
				server = strings.TrimSuffix(strings.Fields(rest)[0], "/api/v1")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not start")
		}
	}
	go func() {
		for range notices {
		}
	}()
	for _, p := range []string{healthPath, readyPath} {
		var stderr bytes.Buffer
		if code := Main(context.Background(), []string{"probe", "--url", server + p}, io.Discard, &stderr); code != exitOK {
			t.Errorf("probe %s exit = %d: %s", p, code, stderr.String())
		}
	}
	resp, err := http.Get(server + "/api/v1/runs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/runs without a credential = %d, want 401", resp.StatusCode)
	}
}
