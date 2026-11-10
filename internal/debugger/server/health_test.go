package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/store/pg"
)

// TestHealthAnswersReadinessWhileServing: /healthz answers 200 from the
// start, /readyz answers 503 while the server waits for its storage and once
// it drains, both only to GET or HEAD, and every other path reaches the API,
// or is answered 503 with Retry-After while the server starts.
func TestHealthAnswersReadinessWhileServing(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, tc := range []struct {
		name     string
		method   string
		path     string
		starting bool
		draining bool
		want     int
	}{
		{"healthy", http.MethodGet, healthPath, false, false, http.StatusOK},
		{"healthy while starting", http.MethodGet, healthPath, true, false, http.StatusOK},
		{"healthy while draining", http.MethodGet, healthPath, false, true, http.StatusOK},
		{"ready", http.MethodGet, readyPath, false, false, http.StatusOK},
		{"head ready", http.MethodHead, readyPath, false, false, http.StatusOK},
		{"not ready while starting", http.MethodGet, readyPath, true, false, http.StatusServiceUnavailable},
		{"not ready while draining", http.MethodGet, readyPath, false, true, http.StatusServiceUnavailable},
		{"post refused", http.MethodPost, readyPath, false, false, http.StatusMethodNotAllowed},
		{"api", http.MethodGet, "/api/v1/runs", false, false, http.StatusTeapot},
		{"api while starting", http.MethodGet, "/api/v1/runs", true, false, http.StatusServiceUnavailable},
		{"api while draining", http.MethodGet, "/api/v1/runs", false, true, http.StatusTeapot},
		{"prefix is not health", http.MethodGet, healthPath + "/x", false, false, http.StatusTeapot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state serveState
			if !tc.starting {
				state.serve(api)
			}
			state.draining.Store(tc.draining)
			rec := httptest.NewRecorder()
			health(&state).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
			}
			if tc.starting && tc.path != healthPath && tc.path != readyPath && rec.Header().Get("Retry-After") == "" {
				t.Error("a request refused while starting has no Retry-After")
			}
		})
	}
}

// TestOpenStoreRetriesStorageErrors: a storage error is retried with a
// doubling delay capped at storeRetryMax, and said on stderr; any other
// error fails at once; a cancelled start stops retrying.
func TestOpenStoreRetriesStorageErrors(t *testing.T) {
	storageErr := fmt.Errorf("%w: connect: refused", artifact.ErrStorage)
	configErr := errors.New("database URL is invalid")
	for _, tc := range []struct {
		name       string
		errs       []error
		cancelAt   int // cancel during this wait, 0 for never
		wantErr    error
		wantDelays []time.Duration
	}{
		{"opens at once", nil, 0, nil, nil},
		{"retries until open", []error{storageErr, storageErr, storageErr}, 0, nil, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}},
		{"delay is capped", repeatErr(storageErr, 7), 0, nil,
			[]time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}},
		{"config error fails at once", []error{configErr}, 0, configErr, nil},
		{"cancelled start stops", repeatErr(storageErr, 5), 2, context.Canceled, []time.Duration{time.Second, 2 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			restoreOpen, restoreWait := OpenStore, storeRetryWait
			t.Cleanup(func() { OpenStore, storeRetryWait = restoreOpen, restoreWait })
			OpenStore = func(_ context.Context, c pg.Config) (artifact.Repository, error) {
				calls++
				if calls <= len(tc.errs) {
					return nil, tc.errs[calls-1]
				}
				return memory.New(c.Limits), nil
			}
			var delays []time.Duration
			storeRetryWait = func(ctx context.Context, d time.Duration) error {
				delays = append(delays, d)
				if len(delays) == tc.cancelAt {
					cancel()
				}
				return ctx.Err()
			}
			var stderr bytes.Buffer
			repo, err := openStoreRetrying(ctx, pg.Config{}, &stderr)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (repo != nil) {
				t.Fatalf("openStoreRetrying = %v, %v; want error %v", repo, err, tc.wantErr)
			}
			if !slices.Equal(delays, tc.wantDelays) {
				t.Errorf("delays = %v, want %v", delays, tc.wantDelays)
			}
			if got := strings.Count(stderr.String(), "storage not available"); got != len(tc.wantDelays) {
				t.Errorf("%d retry notices, want %d: %s", got, len(tc.wantDelays), stderr.String())
			}
		})
	}
}

func repeatErr(err error, n int) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = err
	}
	return errs
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

// TestServeAnswersHealthWhileStorageStarts: a server whose storage does not
// answer yet keeps running: /healthz answers 200, /readyz and the API 503,
// and once the storage answers the server becomes ready.
func TestServeAnswersHealthWhileStorageStarts(t *testing.T) {
	up := make(chan struct{})
	waiting := make(chan struct{}, 1)
	release := make(chan struct{})
	restoreOpen, restoreWait := OpenStore, storeRetryWait
	t.Cleanup(func() { OpenStore, storeRetryWait = restoreOpen, restoreWait })
	OpenStore = func(_ context.Context, c pg.Config) (artifact.Repository, error) {
		select {
		case <-up:
			return memory.New(c.Limits), nil
		default:
			return nil, fmt.Errorf("%w: connect: refused", artifact.ErrStorage)
		}
	}
	storeRetryWait = func(ctx context.Context, _ time.Duration) error {
		select {
		case waiting <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	path := writeTeam(t, teamYAML())
	ctx, cancel := context.WithCancel(context.Background())
	notices := make(serveNotices, 64)
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
	// next returns the rest of the first notice with prefix.
	next := func(prefix string) string {
		t.Helper()
		for {
			select {
			case notice := <-notices:
				if rest, ok := strings.CutPrefix(notice, prefix); ok {
					return rest
				}
			case code := <-done:
				t.Fatalf("serve exited with %d while waiting for %q", code, prefix)
			case <-time.After(10 * time.Second):
				t.Fatalf("no notice %q", prefix)
			}
		}
	}
	server := strings.TrimSuffix(strings.Fields(next("listening on "))[0], ",")
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not retry its storage")
	}
	probe := func(p string) int {
		var stderr bytes.Buffer
		return Main(context.Background(), []string{"probe", "--url", server + p}, io.Discard, &stderr)
	}
	if code := probe(healthPath); code != exitOK {
		t.Errorf("healthz while starting: exit %d", code)
	}
	if code := probe(readyPath); code != exitError {
		t.Errorf("readyz while starting: exit %d, want %d", code, exitError)
	}
	resp, err := http.Get(server + "/api/v1/runs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Errorf("API while starting = %d, Retry-After %q; want 503 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	close(up)
	close(release)
	next("serving API on ")
	go func() {
		for range notices {
		}
	}()
	if code := probe(readyPath); code != exitOK {
		t.Errorf("readyz once the storage answers: exit %d", code)
	}
}
