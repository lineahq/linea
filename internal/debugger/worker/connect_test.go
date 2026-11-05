package worker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
)

const testToken = "s3cret-token"

// connectedWorker serves a worker in this process that dials a test server
// with the registration token, and returns the server's client for it.
func connectedWorker(t *testing.T, name string, capacity int) *Client {
	t.Helper()
	accepted := make(chan *Client, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ConnectPath {
			http.NotFound(w, r)
			return
		}
		c, err := Accept(w, r, func(token string) bool { return ConstantTimeEqual(token, testToken) })
		if err != nil {
			t.Errorf("Accept = %v", err)
			return
		}
		accepted <- c
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Connect(ctx, ConnectOptions{URL: srv.URL, Token: testToken, Name: name, Capacity: capacity})
	}()
	var c *Client
	select {
	case c = <-accepted:
	case err := <-done:
		t.Fatalf("Connect = %v before it connected", err)
	case <-time.After(time.Minute):
		t.Fatal("the worker did not connect")
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Connect = %v", err)
		}
		_ = c.Close()
	})
	return c
}

// TestAConnectedWorkerAnnouncesItsNameAndCapacity checks the hello a
// worker sends a server it dialed.
func TestAConnectedWorkerAnnouncesItsNameAndCapacity(t *testing.T) {
	c := connectedWorker(t, "team-runtime", 3)
	h, want := c.Hello(), func() Hello { h, _ := Identity(); return h }()
	if h.Name != "team-runtime" || h.Capacity != 3 || h.Runtime != want.Runtime || h.Protocol != ProtocolVersion {
		t.Fatalf("hello = %+v", h)
	}
	if c.name != "team-runtime" {
		t.Fatalf("client name = %q", c.name)
	}
}

// TestAConnectedWorkerRunsEightRunsAtOnce checks that one worker with a
// capacity of 8 has 8 runs in flight together, refuses a ninth and reports
// a run exactly as the in-process engine does.
func TestAConnectedWorkerRunsEightRunsAtOnce(t *testing.T) {
	const capacity = 8
	c := connectedWorker(t, "team-runtime", capacity)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runCtx, stopRuns := context.WithCancel(ctx)
	var wg sync.WaitGroup
	starts := make([]<-chan struct{}, capacity)
	errs := make([]error, capacity)
	reps := make([]*engine.Report, capacity)
	for i := range capacity {
		req := slowRun(t)
		req.Observer, starts[i] = observed()
		wg.Add(1)
		go func() {
			defer wg.Done()
			reps[i], errs[i] = c.Execute(runCtx, req)
		}()
	}
	for i, started := range starts {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatalf("run %d did not start while the others ran", i)
		}
	}
	_, err := c.Execute(ctx, slowRun(t))
	if !errors.Is(err, engine.ErrInvalidRequest) || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("a ninth run = %v, want a refusal naming the capacity", err)
	}
	stopRuns()
	wg.Wait()
	for i, err := range errs {
		if err != nil || reps[i].Result.Status != runresult.StatusCancelled {
			t.Errorf("cancelled run %d = %v, %v", i, reps[i], err)
		}
	}

	req, err := engine.Prepare(ctx, engine.Request{Config: samplePath("logs-cleanup", "config.yaml"), Fixture: samplePath("logs-cleanup", "input.json"), Normalize: true})
	if err != nil {
		t.Fatal(err)
	}
	want, err := engine.Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := encode(t, want), encode(t, got); !bytes.Equal(a, b) {
		t.Fatalf("connected worker report differs from the in-process report\nin process: %.2000s\nworker:     %.2000s", a, b)
	}
}

// TestAConnectingWorkerStopsOnARefusedToken checks that a wrong
// registration token ends Connect instead of retrying it.
func TestAConnectingWorkerStopsOnARefusedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := Accept(w, r, func(token string) bool { return ConstantTimeEqual(token, testToken) }); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("Accept = %v, want ErrUnauthorized", err)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := Connect(ctx, ConnectOptions{URL: srv.URL, Token: "wrong", Name: "team-runtime", Capacity: 1})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Connect = %v, want ErrUnauthorized", err)
	}
}

func TestConnectOptionsAreChecked(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ConnectOptions
		want string
	}{
		{"no token", ConnectOptions{URL: "http://server", Name: "n", Capacity: 1}, "token"},
		{"no name", ConnectOptions{URL: "http://server", Token: "t", Capacity: 1}, "name"},
		{"no capacity", ConnectOptions{URL: "http://server", Token: "t", Name: "n"}, "capacity"},
		{"no URL", ConnectOptions{Token: "t", Name: "n", Capacity: 1}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Connect(context.Background(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Connect = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

func TestAcceptRefusesWhatIsNotAWorker(t *testing.T) {
	authorize := func(token string) bool { return ConstantTimeEqual(token, testToken) }
	for _, tc := range []struct {
		name   string
		method string
		auth   string
		want   int
	}{
		{"post", http.MethodPost, "Bearer " + testToken, http.StatusMethodNotAllowed},
		{"no token", http.MethodGet, "", http.StatusUnauthorized},
		{"wrong token", http.MethodGet, "Bearer nope", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, ConnectPath, nil)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			if _, err := Accept(w, r, authorize); err == nil {
				t.Fatal("Accept succeeded")
			}
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
	if ConstantTimeEqual("", "") {
		t.Fatal("an empty expected token matched")
	}
}

// objectStore stands for presigned object storage: GET /bundle serves a
// bundle, PUT /result keeps what the worker uploads.
type objectStore struct {
	mu     sync.Mutex
	bundle []byte
	result []byte
}

func (o *objectStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/bundle":
		_, _ = w.Write(o.bundle)
	case r.Method == http.MethodPut && r.URL.Path == "/result":
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		o.result = b.Bytes()
	default:
		http.Error(w, "no such object", http.StatusNotFound)
	}
}

// TestPayloadsTravelThroughPresignedURLs runs a job whose bundle the
// worker downloads and whose report it uploads: the report is the one an
// inline job returns, and a bundle that is not the assigned one is refused.
func TestPayloadsTravelThroughPresignedURLs(t *testing.T) {
	old := inlineLimit
	inlineLimit = 1 // upload every result
	t.Cleanup(func() { inlineLimit = old })
	c := connectedWorker(t, "team-runtime", 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := engine.Prepare(ctx, engine.Request{Config: samplePath("logs-cleanup", "config.yaml"), Fixture: samplePath("logs-cleanup", "input.json"), Normalize: true})
	if err != nil {
		t.Fatal(err)
	}
	target := c.Target()
	req.Runtime = &target
	bundle, _, err := engine.EncodeBundle(req)
	if err != nil {
		t.Fatal(err)
	}
	store := &objectStore{bundle: bundle}
	srv := httptest.NewServer(store)
	defer srv.Close()

	want, err := c.ExecuteJob(ctx, Job{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	var fetched struct{ digest string }
	got, err := c.ExecuteJob(ctx, Job{
		BundleURL: srv.URL + "/bundle", BundleDigest: Digest(bundle), BundleSize: int64(len(bundle)),
		ResultURL: srv.URL + "/result",
		FetchResult: func(_ context.Context, digest string, size int64) ([]byte, error) {
			store.mu.Lock()
			defer store.mu.Unlock()
			if Digest(store.result) != digest || int64(len(store.result)) != size {
				return nil, errors.New("the uploaded result is not the one reported")
			}
			fetched.digest = digest
			return store.result, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fetched.digest == "" {
		t.Fatal("the worker sent its report inline")
	}
	if a, b := encode(t, want), encode(t, got); !bytes.Equal(a, b) {
		t.Fatalf("uploaded report differs from the inline one\ninline:   %.2000s\nuploaded: %.2000s", a, b)
	}

	_, err = c.ExecuteJob(ctx, Job{BundleURL: srv.URL + "/bundle", BundleDigest: Digest([]byte("other")), BundleSize: int64(len(bundle))})
	if !errors.Is(err, engine.ErrBundle) {
		t.Fatalf("a bundle with another digest = %v, want ErrBundle", err)
	}
	_, err = c.ExecuteJob(ctx, Job{BundleURL: srv.URL + "/missing", BundleDigest: Digest(bundle), BundleSize: int64(len(bundle))})
	if !errors.Is(err, engine.ErrRuntimeUnavailable) {
		t.Fatalf("a missing bundle = %v, want ErrRuntimeUnavailable", err)
	}
	if _, err := c.ExecuteJob(ctx, Job{Bundle: bundle, ResultURL: srv.URL + "/result"}); !errors.Is(err, engine.ErrInvalidRequest) {
		t.Fatalf("a result URL without a reader = %v", err)
	}
}

// TestPingsKeepAnIdleConnection holds a connection idle for several idle
// timeouts: the server's pings keep it, and a run on it still works.
func TestPingsKeepAnIdleConnection(t *testing.T) {
	ping, idle := pingInterval, idleTimeout
	t.Cleanup(func() { pingInterval, idleTimeout = ping, idle })
	pingInterval, idleTimeout = 50*time.Millisecond, 200*time.Millisecond
	c := connectedWorker(t, "team-runtime", 1)
	time.Sleep(4 * idleTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	prepared, err := engine.Prepare(ctx, engine.Request{
		Config:  samplePath("logs-cleanup", "config.yaml"),
		Fixture: samplePath("logs-cleanup", "input.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Execute(ctx, prepared); err != nil {
		t.Fatalf("a run after an idle while: %v", err)
	}
}

func TestParseServerURL(t *testing.T) {
	ca := []byte("-----BEGIN CERTIFICATE-----\n")
	for _, tc := range []struct {
		raw  string
		ca   []byte
		want string // the URL, or "" for a refusal
	}{
		{"https://linea.example.com", nil, "https://linea.example.com"},
		{"https://linea.example.com/prefix/", nil, "https://linea.example.com/prefix"},
		{"http://linea-server:7070", nil, "http://linea-server:7070"},
		{"127.0.0.1:7070", nil, "http://127.0.0.1:7070"},
		{"linea.example.com:443", nil, "https://linea.example.com:443"},
		{"", nil, ""},
		{"linea.example.com", nil, ""},
		{"ftp://linea.example.com", nil, ""},
		{"https://user:pass@linea.example.com", nil, ""},
		{"https://linea.example.com?token=x", nil, ""},
		{"http://linea-server:7070", ca, ""},
		{"https://linea.example.com", ca, ""}, // not a PEM certificate
	} {
		u, err := parseServerURL(tc.raw, tc.ca)
		switch {
		case tc.want == "" && err == nil:
			t.Errorf("parseServerURL(%q) = %v, want a refusal", tc.raw, u)
		case tc.want != "" && (err != nil || u.String() != tc.want):
			t.Errorf("parseServerURL(%q) = %v, %v, want %s", tc.raw, u, err, tc.want)
		case tc.want == "" && !errors.Is(err, engine.ErrInvalidRequest):
			t.Errorf("parseServerURL(%q) = %v, want an invalid request", tc.raw, err)
		}
	}
}
