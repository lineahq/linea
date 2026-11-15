package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/translog"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(s.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func events(lines []map[string]any) []string {
	var names []string
	for _, l := range lines {
		names = append(names, l["msg"].(string))
	}
	return names
}

func TestRunLogsStartReadyFailedAuthenticationsAndStop(t *testing.T) {
	var out syncBuffer
	c := testConfig()
	c.Log = translog.New(&out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addresses := make(chan [2]string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, c, "9.8.7", func(m, a net.Addr) { addresses <- [2]string{m.String(), a.String()} })
	}()
	var addrs [2]string
	select {
	case addrs = <-addresses:
	case err := <-done:
		t.Fatalf("startup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup timeout")
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	const wrong = "wrong-credential-that-must-not-be-logged-0123456789"
	for _, target := range []string{addrs[0] + "/api/v1/server", addrs[1] + "/v1/opamp"} {
		for range 2 {
			r, _ := http.NewRequest("GET", "http://"+target, nil)
			r.Header.Set("Authorization", "Bearer "+wrong)
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Fatalf("%s: status %d", target, resp.StatusCode)
			}
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown timeout")
	}
	lines := out.lines(t)
	if got := events(lines); len(got) < 4 || got[0] != "process_started" || got[1] != "process_ready" || got[len(got)-1] != "process_stopped" {
		t.Fatalf("events = %v", got)
	}
	if lines[0]["version"] != "9.8.7" {
		t.Errorf("start line = %v", lines[0])
	}
	failures := map[any]any{}
	for _, l := range lines {
		if l["msg"] == "auth_failures" {
			failures[l["listener"]] = l["count"]
		}
	}
	if failures["management"] != float64(2) || failures["agent"] != float64(2) || len(failures) != 2 {
		t.Errorf("auth_failures = %v", failures)
	}
	stop := lines[len(lines)-1]
	if stop["reason"] != "signal" || stop["level"] != "INFO" || stop["version"] != "9.8.7" {
		t.Errorf("stop line = %v", stop)
	}
	if strings.Contains(out.String(), wrong) || strings.Contains(out.String(), c.APIToken) || strings.Contains(out.String(), c.AgentToken) {
		t.Error("a credential reached the log")
	}
}

func TestRunLogsWhyItCouldNotStart(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	var out syncBuffer
	c := testConfig()
	c.Log = translog.New(&out)
	c.ManagementAddr = occupied.Addr().String()
	if err := Run(context.Background(), c, "1.0.0", nil); err == nil {
		t.Fatal("expected bind failure")
	}
	lines := out.lines(t)
	stop := lines[len(lines)-1]
	if stop["msg"] != "process_stopped" || stop["reason"] != "listen_failed" || stop["level"] != "ERROR" {
		t.Errorf("stop line = %v", stop)
	}
	if strings.Contains(out.String(), occupied.Addr().String()) {
		t.Error("the bind error text reached the log")
	}

	var second syncBuffer
	c.Log = translog.New(&second)
	c.ServerID = ""
	if err := Run(context.Background(), c, "1.0.0", nil); err == nil {
		t.Fatal("expected a validation error")
	}
	if lines := second.lines(t); len(lines) != 1 || lines[0]["reason"] != "config_invalid" {
		t.Errorf("lines = %v", lines)
	}
}
