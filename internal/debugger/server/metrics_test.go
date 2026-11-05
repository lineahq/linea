package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestServeMetricsListenServesBuildMetricsApart: --metrics-listen serves
// the build metrics on a listener of its own, which serves nothing else,
// and the API listener does not serve them.
func TestServeMetricsListenServesBuildMetricsApart(t *testing.T) {
	path := writeTeam(t, teamYAML())
	ctx, cancel := context.WithCancel(context.Background())
	notices := make(serveNotices, 16)
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"--config", path, "--addr", "127.0.0.1:0", "--metrics-listen", "127.0.0.1:0"}, io.Discard, notices)
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
	var server, metrics string
	for server == "" || metrics == "" {
		select {
		case notice := <-notices:
			if rest, ok := strings.CutPrefix(notice, "serving build metrics on "); ok {
				metrics = strings.TrimSpace(rest)
			}
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
	get := func(url string) (int, string) {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(metrics); code != http.StatusOK || !strings.Contains(body, `linea_runtime_builds{state="queued"} 0`) || !strings.Contains(body, "linea_runtime_builds_unfinished 0") || !strings.Contains(body, "linea_builders_connected 0") {
		t.Fatalf("GET %s = %d\n%s", metrics, code, body)
	}
	if code, _ := get(strings.TrimSuffix(metrics, "/metrics") + "/api/v1/runs"); code != http.StatusNotFound {
		t.Errorf("the metrics listener serves the API: %d", code)
	}
	if code, body := get(server + "/metrics"); strings.Contains(body, "linea_runtime_builds") {
		t.Errorf("the API listener serves the metrics: %d", code)
	}
}
